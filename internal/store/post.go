//go:build !windows

package store

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/cplieger/bolttable"
	"github.com/cplieger/grelot/internal/protopath"
)

const (
	maxSubject = 120
	maxBody    = 64 << 10
	maxFollows = 32
)

// Target is where a post goes: exactly one of the announcements, a path
// thread, or the direct thread with the identity named Agent.
type Target struct {
	Path     string
	Agent    string
	Announce bool
}

// Posting is a post as Caller's identity. Every identity field is
// required, NoChange keeping it; Where must come when the paths change.
type Posting struct {
	Where      *Where
	Caller     Caller
	Subject    string
	Body       string
	To         Target
	Fields     Fields
	NotifyRead NotifyRead
}

// Posted is what a post answers.
type Posted struct {
	// Notices are lines for the sender, such as a dormant recipient or a
	// follow the post added or could not add.
	Notices  []string
	Identity Identity
	Message  uint64
	Thread   uint64
	// Delivered counts the recipients; SameEnvironment those that share
	// the sender's environment, Other the rest.
	Delivered       int
	SameEnvironment int
	Other           int
}

// Post stores a message from p.Caller's live identity, applies its
// identity fields, and delivers it to each recipient, dormant ones too,
// with a quiet copy to each delegate's parent that would not otherwise see
// it; a path post follows its thread as pathRoute says. Refusals:
// KindInvalid for a bad target, message, field or where; KindGone with no
// live identity; KindForbidden outside a delegate's scope, either way;
// KindNotFound for an unknown @name; KindConflict for a rename to a held
// name; KindFull past 90 % of Options.MaxBytes; ErrClosed after Close.
func (s *Store) Post(ctx context.Context, k Keyed, p *Posting) (Posted, bool, error) {
	if err := checkMessage(p); err != nil {
		return Posted{}, false, err
	}
	return keyed(ctx, s, k, func(w *wtx) (Posted, error) {
		// Measured under the write lock: read before it, concurrent posts
		// all see the space no other one has filled yet and pass the
		// ceiling together.
		full, ferr := s.nearlyFull()
		if ferr != nil {
			return Posted{}, ferr
		}
		if full {
			return Posted{}, refuse(KindFull, "store nearly full: posts resume as older mail expires")
		}
		return w.post(p)
	})
}

func checkMessage(p *Posting) error {
	targets := 0
	for _, set := range []bool{p.To.Announce, p.To.Path != "", p.To.Agent != ""} {
		if set {
			targets++
		}
	}
	if targets != 1 {
		return refuse(KindInvalid, "a post goes to exactly one target: announce, a path or '@name'")
	}
	if p.To.Path != "" {
		if _, err := checkPaths([]string{p.To.Path}); err != nil {
			return err
		}
	}
	if n := utf8.RuneCountInString(p.Subject); n == 0 || n > maxSubject || !utf8.ValidString(p.Subject) || strings.ContainsFunc(p.Subject, isControl) {
		return refuse(KindInvalid, fmt.Sprintf("the subject must be 1 to %d characters of text, on one line", maxSubject))
	}
	if len(p.Body) > maxBody || !utf8.ValidString(p.Body) {
		return refuse(KindInvalid, "the body must be UTF-8 text of at most 64 KiB")
	}
	return p.NotifyRead.check()
}

func (w *wtx) post(p *Posting) (Posted, error) {
	// Settled first so the route reads the holders an eager hand-off left.
	if p.To.Path != "" {
		if err := w.settleReservations(p.To.Path); err != nil {
			return Posted{}, err
		}
	}
	me, err := w.liveAs(p.Caller)
	if err != nil {
		return Posted{}, err
	}
	if ferr := w.applyFields(&me, p.Fields, p.Where, fieldsEvery, false); ferr != nil {
		return Posted{}, ferr
	}
	w.touch(&me)
	rt, err := w.route(&me, p.To)
	if err != nil {
		return Posted{}, err
	}
	if perr := w.s.t.identities.table.Put(w.tx, me.ID, &me); perr != nil {
		return Posted{}, perr
	}
	where, err := w.s.whereOf(w.r, &me)
	if err != nil {
		return Posted{}, err
	}
	msg, err := w.storeMessage(&me, where, &rt.thread, p)
	if err != nil {
		return Posted{}, err
	}
	w.countPost(rt.thread.Kind)
	v, err := w.viewOf(&me)
	if err != nil {
		return Posted{}, err
	}
	out := Posted{Notices: rt.notices, Message: msg.ID, Thread: rt.thread.ID, Identity: v}
	wt, err := w.newWatch(p.NotifyRead, &msg)
	if err != nil {
		return Posted{}, err
	}
	if err := w.fanOut(&me, &rt, &msg, &out, wt); err != nil {
		return Posted{}, err
	}
	if err := w.putWatch(wt, &out); err != nil {
		return Posted{}, err
	}
	if err := w.hintCopy(&msg, &out); err != nil {
		return Posted{}, err
	}
	return out, nil
}

// fanOut writes msg's delivery to each recipient but the sender, in id
// order, and counts them into out by whether they share the sender's
// environment, then the copies a delegate's parent is owed. A recipient
// whose purge is due is purged here and gets none. A recipient wt waits
// for has its row name wt.
func (w *wtx) fanOut(me *identityRow, rt *route, msg *messageRow, out *Posted, wt *watching) error {
	cp := copies{}
	cp.owe(me, me.ID)
	for _, id := range slices.Sorted(maps.Keys(rt.recipients)) {
		if id == me.ID {
			continue
		}
		r, found, err := w.s.t.identities.settle(w, id)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if err := w.deliver(&r, &deliveryRow{
			Kind: rt.thread.Kind, Path: rt.recipients[id], Name: r.Name, Parent: r.Parent, Watch: wt.add(&r),
			Message: msg.ID, Thread: rt.thread.ID, Sender: me.ID, Lease: lease{Expires: msg.Lease.Expires},
		}); err != nil {
			return err
		}
		out.Delivered++
		cp.owe(&r, me.ID)
		where, werr := w.s.whereOf(w.r, &r)
		if werr != nil {
			return werr
		}
		if shareEnvironment(msg.Where, where) {
			out.SameEnvironment++
		} else {
			out.Other++
		}
	}
	return w.writeCopies(cp, rt, msg)
}

// route is where a post goes: its thread, each recipient with the path
// that matched it ("" outside path threads), and lines for the sender.
type route struct {
	recipients map[uint64]string
	notices    []string
	thread     threadRow
}

// route refuses a delegate's post outside its scope, and directRoute
// also a direct post to a delegate from outside its scope.
func (w *wtx) route(me *identityRow, to Target) (route, error) {
	switch {
	case to.Announce:
		if me.Parent != 0 && !me.Scope.allowsAnnounce() {
			return route{}, w.s.scopeRefusal(w.r, me)
		}
		return w.announceRoute()
	case to.Path != "":
		if me.Parent != 0 && !me.Scope.allowsPath(to.Path) {
			return route{}, w.s.scopeRefusal(w.r, me)
		}
		return w.pathRoute(me, to.Path)
	default:
		return w.directRoute(me, to.Agent)
	}
}

// announceRoute reaches every identity, live and dormant, but a delegate
// whose scope leaves the announcements out.
func (w *wtx) announceRoute() (route, error) {
	th, _, err := w.thread(announceKey)
	if err != nil {
		return route{}, err
	}
	th.Kind = kindAnnounce
	rt := route{thread: th, recipients: map[uint64]string{}}
	err = w.s.t.identities.table.Scan(w.r, bolttable.Range{}, func(id uint64, v *identityRow) (bool, error) {
		if v.Parent == 0 || v.Scope.allowsAnnounce() {
			rt.recipients[id] = ""
		}
		return true, nil
	})
	return rt, err
}

// pathRoute reaches the identities with a work path, a held reservation
// or a follow at p or above it, and the delegates whose scope allows p.
// Posts go down the tree only, so a work path under p does not match.
// Walking p's ancestors shortest first leaves each recipient labelled with
// its most specific match. The sender follows p unless its work path or
// a follow already reaches p, it is at the cap, or it is a delegate,
// whose scope already delivers p. Unlike grelot follow, this follow keeps
// the sender's follows under p, so a post never removes one it chose.
func (w *wtx) pathRoute(me *identityRow, p string) (route, error) {
	th, _, err := w.thread(p)
	if err != nil {
		return route{}, err
	}
	th.Kind, th.Path = kindPath, p
	rt := route{thread: th, recipients: map[uint64]string{}}
	for _, a := range ancestors(p) {
		if err := w.matchAt(&rt, a, p); err != nil {
			return route{}, err
		}
	}
	if me.Parent == 0 {
		me.Threaded = true
	}
	if me.Parent != 0 || reachedBy(me, p) != "" {
		return rt, nil
	}
	if len(me.Follows) >= maxFollows {
		rt.notices = append(rt.notices, fmt.Sprintf(
			"you follow %d threads, the most allowed, so this post did not follow %s: grelot unfollow one to follow it", maxFollows, p,
		))
		return rt, nil
	}
	me.Follows = append(me.Follows, p)
	n, werr := w.wideFollow(me, p)
	if n != "" {
		rt.notices = append(rt.notices, n)
	}
	return rt, werr
}

// wideFollow is the notice for a post's new follow of p when it reaches
// more than p's own thread, a live path thread or a follow of me's under
// p; "" otherwise.
func (w *wtx) wideFollow(me *identityRow, p string) (string, error) {
	inside := slices.DeleteFunc(slices.Clone(me.Follows), func(f string) bool { return f == p || !under(f, p) })
	if len(inside) == 0 {
		live, err := w.liveThreadUnder(p)
		if err != nil || !live {
			return "", err
		}
	}
	stop := "unfollow it"
	if word, ok := protopath.Word(p); ok {
		stop = "grelot unfollow " + word
	}
	n := fmt.Sprintf("this post follows %s so its replies reach you, and that follow also reaches every thread under it: %s to stop hearing them", p, stop)
	if len(inside) > 0 {
		n += "; your follows under it stay: " + strings.Join(inside, ", ")
	}
	return n, nil
}

// liveThreadUnder reports whether a live path thread lives strictly under p.
func (w *wtx) liveThreadUnder(p string) (bool, error) {
	found := false
	err := w.s.t.threadKey.Scan(w.r, bolttable.Range{Prefix: []byte(strings.TrimSuffix(p, "/") + "/")}, func(_ []byte, id uint64, _ []byte) (bool, error) {
		th, ok, err := w.s.t.threads.table.Get(w.r, id)
		found = err == nil && ok && th.Kind == kindPath && !th.Lease.due(w.now)
		return err == nil && !found, err
	})
	return found, err
}

// matchAt labels with a the recipients of a post at p that a matches: a
// work path, a follow, a held reservation, or a delegate's allowed thread
// whose scope does not deny p.
func (w *wtx) matchAt(rt *route, a, p string) error {
	label := func(id uint64) (bool, error) {
		rt.recipients[id] = a
		return true, nil
	}
	if err := scanExact(w.r, w.s.t.byPath, a, label); err != nil {
		return err
	}
	if err := scanExact(w.r, w.s.t.byFollow, a, label); err != nil {
		return err
	}
	if err := scanExact(w.r, w.s.t.resvPath, a, func(id uint64) (bool, error) {
		v, found, err := w.s.t.resv.table.Get(w.r, id)
		if found && v.State == reservationHeld {
			rt.recipients[v.Holder] = a
		}
		return err == nil, err
	}); err != nil {
		return err
	}
	return scanExact(w.r, w.s.t.byAllow, a, func(id uint64) (bool, error) {
		v, found, err := w.s.t.identities.table.Get(w.r, id)
		if found && v.Scope.allowsPath(p) {
			rt.recipients[id] = a
		}
		return err == nil, err
	})
}

// reachedBy names me's work path or follow at or above p, which already
// delivers p's threads, or is "" when none does.
func reachedBy(me *identityRow, p string) string {
	if i := slices.IndexFunc(me.Paths, func(q string) bool { return under(p, q) }); i >= 0 {
		return "your work path " + me.Paths[i]
	}
	if i := slices.IndexFunc(me.Follows, func(f string) bool { return under(p, f) }); i >= 0 {
		return "your follow of " + me.Follows[i]
	}
	return ""
}

// ancestors lists p and every path above it, shortest first.
func ancestors(p string) []string {
	out := []string{p}
	for p != "/" {
		p = path.Dir(p)
		out = append(out, p)
	}
	slices.Reverse(out)
	return out
}

// directRoute reaches the identity named name, which a dormant one
// receives too, telling the sender until when it can revive.
func (w *wtx) directRoute(me *identityRow, name string) (route, error) {
	peer, found, err := w.identityBy(w.s.t.byName, name)
	switch {
	case err != nil:
		return route{}, err
	case !found:
		return route{}, unknownIdentity(name)
	case peer.ID == me.ID:
		return route{}, refuse(KindInvalid, "a direct post goes to another identity, not yourself")
	case me.Parent != 0 && !allowsAgent(me, &peer):
		return route{}, w.s.scopeRefusal(w.r, me)
	case peer.Parent != 0 && !allowsAgent(&peer, me):
		return route{}, w.s.scopeRefusal(w.r, &peer)
	}
	th, _, err := w.thread(directKey(me.ID, peer.ID))
	if err != nil {
		return route{}, err
	}
	th.Kind = kindDirect
	rt := route{thread: th, recipients: map[uint64]string{peer.ID: ""}}
	if peer.State == stateDormant {
		rt.notices = append(rt.notices, fmt.Sprintf("@%s is dormant since %s; it gets this if it revives before %s",
			peer.Name, fmtTime(peer.DormantSince), fmtTime(peer.Lease.Expires)))
	}
	return rt, nil
}

// storeMessage stores the post with the sender's labels and where as they
// stand now, where being whereOf the sender, and moves a path or direct thread's expiry to the post's.
// The expiry only ever moves later: a shorter THREAD_TTL after a restart,
// or a clock step back, would otherwise expire the thread, and with it an
// older message whose deliveries are still unread.
func (w *wtx) storeMessage(me *identityRow, where Where, th *threadRow, p *Posting) (messageRow, error) {
	ttl := announceTTL
	if th.Kind != kindAnnounce {
		ttl = w.s.threadTTL
		th.Lease = lease{Expires: later(th.Lease.Expires, w.now.Add(ttl))}
	}
	if err := w.s.t.threads.table.Put(w.tx, th.ID, th); err != nil {
		return messageRow{}, err
	}
	ls := serverLease(w.now, ttl)
	id, err := w.s.t.messages.table.NextID(w.tx)
	if err != nil {
		return messageRow{}, err
	}
	m := messageRow{
		ID: id, Thread: th.ID, Kind: th.Kind, Path: th.Path, PostedAt: w.now, Lease: ls,
		Sender: me.ID, SenderName: me.Name, Host: me.Host, OS: me.OS, Arch: me.Arch, Where: cloneWhere(where),
		Subject: p.Subject, Body: p.Body,
	}
	if err := w.s.t.messages.table.Put(w.tx, pairKey(th.ID, id), &m); err != nil {
		return messageRow{}, err
	}
	return m, nil
}
