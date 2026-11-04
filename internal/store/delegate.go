package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/bolttable"
)

const (
	linkPrefix = "grelot-link:"
	// linkBytes is the link's 128 random bits, 26 base32 characters.
	linkBytes = 16
	// maxLinks and maxDelegates bound a parent's unused links and its live
	// delegates, each on its own.
	maxLinks     = 32
	maxDelegates = 32
	// maxScope bounds each allow and deny list of a grant.
	maxScope = 16
	// scopeAnnounce in a thread list names the announcements.
	scopeAnnounce = "announce"
)

var linkEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Grant is a delegate grant from Caller's identity. AllowThreads and
// DenyThreads hold canonical paths or "announce"; AllowAgents and
// DenyAgents hold names of live identities.
type Grant struct {
	Caller       Caller
	Label        string
	AllowThreads []string
	AllowAgents  []string
	DenyThreads  []string
	DenyAgents   []string
	TTL          time.Duration
}

// Granted is what a grant answers: the link, shown once and stored only
// as its SHA-256, the handle it registers, and when the unused link
// expires.
type Granted struct {
	Expires time.Time
	Link    string
	Handle  string
}

// Delegate grants a single-use link that registers the delegate
// <caller's name>/<label> under the grant's scope. The unused link lives
// for the grant's TTL. Refusals: KindInvalid for a bad label, ttl, thread
// or a deny of the caller itself; KindForbidden for a delegate caller;
// KindNotFound for an agent name no live identity holds; KindConflict for
// a handle an identity or an unused link holds, or a cap reached; KindGone
// for a session with no live identity.
func (s *Store) Delegate(ctx context.Context, g *Grant) (Granted, error) {
	if err := checkLabel(g.Label); err != nil {
		return Granted{}, err
	}
	if _, err := identityPolicy.grant(s.now(), g.TTL, time.Time{}); err != nil {
		return Granted{}, err
	}
	var out Granted
	err := s.update(ctx, func(w *wtx) error {
		var gerr error
		out, gerr = w.delegate(g)
		return gerr
	})
	return out, err
}

func (w *wtx) delegate(g *Grant) (Granted, error) {
	me, err := w.liveAs(g.Caller)
	if err != nil {
		return Granted{}, err
	}
	if me.Parent != 0 {
		return Granted{}, refuse(KindForbidden, "a delegate cannot grant delegates: ask your parent for another link")
	}
	sc, err := w.resolveScope(&me, g)
	if err != nil {
		return Granted{}, err
	}
	handle := me.Name + "/" + g.Label
	if ferr := w.handleFree(&me, handle, g.Label); ferr != nil {
		return Granted{}, ferr
	}
	raw := make([]byte, linkBytes)
	_, _ = rand.Read(raw) // crypto/rand.Read never returns an error.
	text := linkEncoding.EncodeToString(raw)
	id, err := w.s.t.links.table.NextID(w.tx)
	if err != nil {
		return Granted{}, err
	}
	link := linkRow{
		Parent: me.ID, ID: id, Label: g.Label, Hash: hashLink(text), Scope: sc, TTL: g.TTL,
		Lease: serverLease(w.now, g.TTL),
	}
	if err := w.s.t.links.table.Put(w.tx, pairKey(me.ID, id), &link); err != nil {
		return Granted{}, err
	}
	me.Ended, me.LastSeen = false, w.now
	if err := w.s.t.identities.table.Put(w.tx, me.ID, &me); err != nil {
		return Granted{}, err
	}
	w.s.log.Info("delegate granted", "name", me.Name, "handle", handle, "ttl", fmtDur(g.TTL))
	return Granted{Link: linkPrefix + text, Handle: handle, Expires: link.Lease.Expires}, nil
}

// handleFree refuses a grant whose handle an identity holds, whose label an
// unused link of me's holds, or past either cap.
func (w *wtx) handleFree(me *identityRow, handle, label string) error {
	if _, held, err := w.identityBy(w.s.t.byName, handle); err != nil || held {
		return orConflict(err, fmt.Sprintf("%s is held by a live or dormant identity: choose another label", handle))
	}
	links, err := w.s.linksOf(w.r, me.ID)
	if err != nil {
		return err
	}
	switch {
	case slices.ContainsFunc(links, func(l linkRow) bool { return l.Label == label }):
		return refuse(KindConflict, fmt.Sprintf("an unused link for %s exists: use it, or grelot delegate revoke %s first", handle, label))
	case len(links) >= maxLinks:
		return refuse(KindConflict, fmt.Sprintf("you hold %d unused delegate links, the most allowed: grelot delegate revoke one first", maxLinks))
	}
	n, err := w.s.delegateCount(w.r, me.ID)
	if err == nil && n >= maxDelegates {
		err = refuse(KindConflict, fmt.Sprintf("you have %d live delegates, the most allowed: grelot delegate revoke one first", maxDelegates))
	}
	return err
}

func orConflict(err error, msg string) error {
	if err != nil {
		return err
	}
	return refuse(KindConflict, msg)
}

// resolveScope checks a grant's threads and resolves its agents to ids.
func (w *wtx) resolveScope(me *identityRow, g *Grant) (scope, error) {
	var sc scope
	var err error
	if sc.AllowThreads, sc.Announce, err = scopeThreads("--allow-thread", g.AllowThreads); err != nil {
		return sc, err
	}
	if sc.DenyThreads, sc.DenyAnnounce, err = scopeThreads("--deny-thread", g.DenyThreads); err != nil {
		return sc, err
	}
	if slices.Contains(g.DenyAgents, me.Name) {
		return sc, refuse(KindInvalid, "a deny cannot name your own identity: your delegate exists to report to you")
	}
	if sc.AllowAgents, err = w.scopeAgents("--allow-agent", g.AllowAgents); err != nil {
		return sc, err
	}
	sc.DenyAgents, err = w.scopeAgents("--deny-agent", g.DenyAgents)
	return sc, err
}

// scopeThreads splits a thread list into its paths and whether it names
// the announcements.
func scopeThreads(flag string, ts []string) (paths []string, announce bool, err error) {
	if len(ts) > maxScope {
		return nil, false, refuse(KindInvalid, fmt.Sprintf("at most %d %s values", maxScope, flag))
	}
	for _, t := range ts {
		if t == scopeAnnounce {
			announce = true
			continue
		}
		paths = append(paths, t)
	}
	if paths, err = checkPaths(paths); err != nil {
		return nil, false, err
	}
	return paths, announce, nil
}

func (w *wtx) scopeAgents(flag string, names []string) ([]uint64, error) {
	if len(names) > maxScope {
		return nil, refuse(KindInvalid, fmt.Sprintf("at most %d %s values", maxScope, flag))
	}
	var ids []uint64
	for _, n := range names {
		row, found, err := w.identityBy(w.s.t.byName, n)
		if err != nil {
			return nil, err
		}
		if !found || row.State != stateLive {
			return nil, refuse(KindNotFound, fmt.Sprintf("%s %s: no live identity has that name", flag, n))
		}
		if !slices.Contains(ids, row.ID) {
			ids = append(ids, row.ID)
		}
	}
	return ids, nil
}

// registerDelegate registers the delegate r.Link grants and consumes the
// link. The delegate stores no where, paths or labels of its own: it runs
// in its parent's environment, and reads take the parent's. Registered
// from its parent's session it binds no session; from any other it binds
// that one, as a Codex subagent's own thread does.
func (w *wtx) registerDelegate(r *Registration) (Registered, error) {
	if err := checkDelegateFields(r); err != nil {
		return Registered{}, err
	}
	link, err := w.linkOf(r.Link)
	if err != nil {
		return Registered{}, err
	}
	parent, found, err := w.s.t.identities.settle(w, link.Parent)
	if err != nil {
		return Registered{}, err
	}
	// The parent's expiry, run by that settle, deletes the link.
	if !found || parent.State != stateLive {
		return Registered{}, linkGone()
	}
	session, err := w.delegateSession(r.Session, &parent)
	if err != nil {
		return Registered{}, err
	}
	handle := parent.Name + "/" + link.Label
	if nerr := w.nameFree(handle); nerr != nil {
		return Registered{}, nerr
	}
	if n, cerr := w.s.delegateCount(w.r, parent.ID); cerr != nil || n >= maxDelegates {
		return Registered{}, orConflict(cerr, fmt.Sprintf("@%s has %d live delegates, the most allowed: ask it to revoke one", parent.Name, maxDelegates))
	}
	ls, err := identityPolicy.grant(w.now, link.TTL, parent.Lease.Expires)
	if err != nil {
		return Registered{}, err
	}
	id, err := w.s.t.identities.table.NextID(w.tx)
	if err != nil {
		return Registered{}, err
	}
	sc := link.Scope
	row := identityRow{
		ID: id, Name: handle, Description: r.Fields.Description, State: stateLive, RegisteredAt: w.now, LastSeen: w.now,
		Host: parent.Host, OS: parent.OS, Arch: parent.Arch, Provider: parent.Provider, Session: session,
		Parent: parent.ID, Scope: &sc, TTL: link.TTL, Lease: ls,
	}
	if err := w.s.t.identities.table.Put(w.tx, id, &row); err != nil {
		return Registered{}, err
	}
	if _, err := w.s.t.links.table.Delete(w.tx, pairKey(link.Parent, link.ID)); err != nil {
		return Registered{}, err
	}
	w.s.log.Info("delegate registered", "handle", handle, "id", id, "parent", parent.ID, "own_session", session != "")
	v, verr := w.viewOf(&row)
	return Registered{Identity: v}, verr
}

// checkDelegateFields refuses what a delegate's registration cannot set:
// its name is its handle, its scope replaces paths, and its ttl is the
// grant's.
func checkDelegateFields(r *Registration) error {
	switch {
	case r.Fields.Name != "" || len(r.Fields.Paths) > 0:
		return refuse(KindInvalid, "a delegate's name is its handle and its scope replaces paths: pass only --delegate <link> and --desc")
	case r.TTL != 0:
		return refuse(KindInvalid, "a delegate's ttl is its grant's: drop --ttl")
	case r.TakeOver:
		return refuse(KindInvalid, "--take-over does not apply to a delegate")
	case r.Fields.Description == "":
		return missingField(fieldsNew, "description", "--desc <your task>")
	}
	return checkDescription(r.Fields.Description)
}

// delegateSession is the session a delegate registered from session binds:
// none from its parent's, else that session, which must have no live
// identity. A dormant one there is detached, as a fresh registration
// detaches it.
func (w *wtx) delegateSession(session string, parent *identityRow) (string, error) {
	if session == parent.Session {
		return "", nil
	}
	own, found, err := w.identityBy(w.s.t.bySession, session)
	switch {
	case err != nil:
		return "", err
	case found && own.State == stateLive:
		return "", refuse(KindConflict, fmt.Sprintf(
			"this session has the live identity %s; a delegate registers from its parent's session or a session with none", own.Name,
		))
	case found:
		if err := w.detach(&own); err != nil {
			return "", err
		}
	}
	return session, nil
}

// linkOf finds the unused link text names, settling its expiry first.
func (w *wtx) linkOf(text string) (linkRow, error) {
	raw, ok := strings.CutPrefix(text, linkPrefix)
	if !ok || len(raw) != linkEncoding.EncodedLen(linkBytes) {
		return linkRow{}, refuse(KindInvalid, "not a delegate link: expected grelot-link:<26 characters> from your parent")
	}
	if _, err := linkEncoding.DecodeString(raw); err != nil {
		return linkRow{}, refuse(KindInvalid, "not a delegate link: expected grelot-link:<26 characters> from your parent")
	}
	key, _, found, err := w.s.t.linkHash.Lookup(w.r, hashLink(raw))
	if err != nil || !found {
		return linkRow{}, orGone(err)
	}
	link, found, err := w.s.t.links.settle(w, key)
	if err != nil || !found {
		return linkRow{}, orGone(err)
	}
	return link, nil
}

func hashLink(raw string) []byte {
	h := sha256.Sum256([]byte(raw))
	return h[:]
}

func orGone(err error) error {
	if err != nil {
		return err
	}
	return linkGone()
}

// linkGone answers every link that is not stored: the store keeps only
// unused links, so a used, revoked, expired or orphaned one reads alike.
func linkGone() *Error {
	return &Error{
		Kind: KindGone, Why: "link_gone",
		Msg: "link already used, revoked or expired, or its parent has gone: ask your parent for a new link",
	}
}

func checkLabel(l string) error {
	if !namePattern.MatchString(l) {
		return refuse(KindInvalid, fmt.Sprintf("label %q must be 2 to 48 lowercase letters, digits and hyphens, starting with a letter or digit", l))
	}
	return nil
}

// linksOf lists parent's unused links, expired ones the sweep has not
// reached included.
func (s *Store) linksOf(r *bolttable.ReadTx, parent uint64) ([]linkRow, error) {
	var out []linkRow
	err := s.t.links.table.Scan(r, bolttable.Range{Prefix: be64(parent)}, func(_ []byte, v *linkRow) (bool, error) {
		out = append(out, *v)
		return true, nil
	})
	return out, err
}

// delegatesOf lists the ids of parent's live delegates.
func (s *Store) delegatesOf(r *bolttable.ReadTx, parent uint64) ([]uint64, error) {
	var out []uint64
	err := s.t.byParent.Scan(r, bolttable.Range{Prefix: be64(parent)}, func(_ []byte, id uint64, _ []byte) (bool, error) {
		out = append(out, id)
		return true, nil
	})
	return out, err
}

func (s *Store) delegateCount(r *bolttable.ReadTx, parent uint64) (int, error) {
	ids, err := s.delegatesOf(r, parent)
	return len(ids), err
}

// endDelegate deletes delegate d's row: a delegate never goes dormant, and
// its parent holds every message it received.
func (w *wtx) endDelegate(d *identityRow, why string) error {
	if err := w.leaveWatches(d); err != nil {
		return err
	}
	if _, err := w.s.t.identities.table.Delete(w.tx, d.ID); err != nil {
		return err
	}
	w.s.forgetSeen(d.ID)
	w.s.log.Info("delegate ended", "handle", d.Name, "id", d.ID, "why", why)
	return nil
}

// endDelegates ends every delegate of parent.
func (w *wtx) endDelegates(parent uint64, why string) error {
	ids, err := w.s.delegatesOf(w.r, parent)
	if err != nil {
		return err
	}
	for _, id := range ids {
		d, found, err := w.s.t.identities.table.Get(w.r, id)
		if err != nil {
			return err
		}
		if found {
			if err := w.endDelegate(&d, why); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteLinks deletes parent's unused links; an expiry keeps the parent's
// row, so no cascade runs.
func (w *wtx) deleteLinks(parent uint64) error {
	links, err := w.s.linksOf(w.r, parent)
	if err != nil {
		return err
	}
	for i := range links {
		if _, err := w.s.t.links.table.Delete(w.tx, pairKey(parent, links[i].ID)); err != nil {
			return err
		}
	}
	return nil
}
