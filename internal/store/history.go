//go:build !windows

package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/bolttable"
)

// defaultHistoryWindow is a nil Since's window, except with LastRead or
// Path.
const defaultHistoryWindow = 24 * time.Hour

// HistoryQuery filters History; every filter set must hold. Thread is a
// thread id; Path keeps path threads at or under it; With is the direct
// thread with the identity of that name; Announce the announcements. From
// keeps that identity's posts to the caller and FromMe only the caller's
// own posts. Since and Until bound the time, both inclusive; nil is open.
// Grep is a substring of the subject, body or event text, ignoring case.
// LastRead keeps the last acked inbox page; Full keeps whole bodies.
type HistoryQuery struct {
	Since *time.Time
	Until *time.Time
	Grep  string
	Path  string
	With  string
	From  string
	// Before and Limit page the read back in time: the entries before
	// Before (the newest at its zero), at most Limit (every one at 0).
	Before   EntryKey
	Thread   uint64
	Limit    int
	FromMe   bool
	Announce bool
	Full     bool
	LastRead bool
}

// EntryKey is an entry's place in a history read's order.
type EntryKey struct {
	Written time.Time
	Message uint64
	Seq     uint64
}

// Key is e's place in a history read's order.
func (e *Entry) Key() EntryKey { return EntryKey{Written: e.Written, Message: e.Message, Seq: e.Seq} }

func (k EntryKey) compare(o EntryKey) int {
	return cmp.Or(k.Written.Compare(o.Written), cmp.Compare(k.Message, o.Message), cmp.Compare(k.Seq, o.Seq))
}

// HistoryPage is a history read: a page of entries, and Since, the lower
// bound the read applied (nil when it read whole). A page after the first
// passes Since back as its query's Since, so the walk keeps one window.
type HistoryPage struct {
	Since *time.Time
	Listing[Entry]
}

// History pages c's own mailbox, read and unread, and unless From names
// another identity or LastRead is set its own posts there, in time order;
// with Path and no LastRead, every post of the live path threads under
// Path too. It moves no cursor. An own post carries no Seq and the
// relation "you". Refusals: KindInvalid for a bad path or time window,
// From with FromMe, or With naming the caller; KindNotFound for an unknown
// name; KindGone for a session with no live identity.
func (s *Store) History(ctx context.Context, c Caller, query *HistoryQuery) (HistoryPage, error) {
	q := *query
	if err := checkHistory(&q); err != nil {
		return HistoryPage{}, err
	}
	if _, err := s.Seen(ctx, c); err != nil {
		return HistoryPage{}, err
	}
	var all []Entry
	var since *time.Time
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		me, err := s.liveFor(r, c, now)
		if err != nil {
			return err
		}
		h, ok, err := s.historyWalk(r, &me, &q, now)
		since = h.since
		if err != nil || !ok {
			return err
		}
		all, err = h.read(r)
		return err
	})
	if err != nil {
		return HistoryPage{}, err
	}
	return HistoryPage{Since: since, Listing: historyPage(all, &q)}, nil
}

// historyPage is the page of all, in order, that q asks for.
func historyPage(all []Entry, q *HistoryQuery) Listing[Entry] {
	before := all
	if q.Before != (EntryKey{}) {
		n, _ := slices.BinarySearchFunc(all, q.Before, func(e Entry, k EntryKey) int { return e.Key().compare(k) })
		before = all[:n]
	}
	start := 0
	if q.Limit != 0 {
		start = max(0, len(before)-q.Limit)
	}
	return Listing[Entry]{Items: before[start:len(before):len(before)], Total: len(all), More: start > 0}
}

func checkHistory(q *HistoryQuery) error {
	if q.Path != "" {
		if _, err := checkPaths([]string{q.Path}); err != nil {
			return err
		}
	}
	if q.FromMe && q.From != "" {
		return refuse(KindInvalid, "--from takes one identity: @me or another's name")
	}
	if q.Since != nil && q.Until != nil && q.Since.After(*q.Until) {
		return refuse(KindInvalid, "--since must not be after --until")
	}
	return nil
}

// walk is one history read's resolved filters.
type walk struct {
	s           *Store
	me          *identityRow
	threadPaths map[uint64]string
	now         time.Time
	since       *time.Time
	until       *time.Time
	grep        string
	path        string
	seqs        seqRange
	thread      uint64
	sender      uint64
	announce    bool
	full        bool
	lastRead    bool
	deliveries  bool
	posts       bool
	fromMe      bool
}

// historyWalk resolves q's names and defaults for me; ok is false when
// no row can match, such as With an identity me shares no thread with.
func (s *Store) historyWalk(r *bolttable.ReadTx, me *identityRow, q *HistoryQuery, now time.Time) (w walk, ok bool, err error) {
	w = walk{
		s: s, me: me, now: now, until: q.Until, grep: strings.ToLower(q.Grep), path: q.Path,
		thread: q.Thread, announce: q.Announce, full: q.Full, lastRead: q.LastRead,
		threadPaths: map[uint64]string{},
	}
	if w.since, err = historySince(q, now); err != nil {
		return w, false, err
	}
	fromMe := q.FromMe || q.From == me.Name
	if q.From != "" && !fromMe {
		peer, err := s.historyPeer(r, q.From)
		if err != nil {
			return w, false, err
		}
		w.sender = peer.ID
	}
	if q.With != "" {
		if found, err := w.withThread(r, q.With); err != nil || !found {
			return w, false, err
		}
	}
	if q.LastRead {
		w.seqs = me.LastPage
	}
	w.fromMe = fromMe
	w.deliveries = !fromMe
	w.posts = !q.LastRead && w.sender == 0
	return w, true, nil
}

// historySince is q's lower bound: Since, or the default window before
// now unless q reads the last page or a path, whose threads a follower
// reads whole. An Until before that default window is refused, as an
// explicit inverted window is.
func historySince(q *HistoryQuery, now time.Time) (*time.Time, error) {
	if q.Since != nil || q.LastRead || q.Path != "" {
		return q.Since, nil
	}
	since := now.Add(-defaultHistoryWindow)
	if q.Until != nil && since.After(*q.Until) {
		return &since, refuse(KindInvalid, "--until is before the default 24 h window: pass --since too")
	}
	return &since, nil
}

// beforeSince reports whether t is older than w's lower bound.
func (w *walk) beforeSince(t time.Time) bool { return w.since != nil && t.Before(*w.since) }

// afterUntil reports whether t is newer than w's upper bound.
func (w *walk) afterUntil(t time.Time) bool { return w.until != nil && t.After(*w.until) }

// withThread narrows w to the direct thread with the identity named name.
func (w *walk) withThread(r *bolttable.ReadTx, name string) (bool, error) {
	if name == w.me.Name {
		return false, refuse(KindInvalid, "--with names another identity's direct thread, not yourself")
	}
	peer, err := w.s.historyPeer(r, name)
	if err != nil {
		return false, err
	}
	id, _, found, err := w.s.t.threadKey.Lookup(r, []byte(directKey(w.me.ID, peer.ID)))
	if err != nil || !found || (w.thread != 0 && w.thread != id) {
		return false, err
	}
	w.thread = id
	return true, nil
}

func (s *Store) historyPeer(r *bolttable.ReadTx, name string) (identityRow, error) {
	peer, found, err := s.lookupIdentity(r, s.t.byName, name)
	if err == nil && !found {
		err = unknownIdentity(name)
	}
	return peer, err
}

func unknownIdentity(name string) *Error {
	return notFound(ReasonIdentity, fmt.Sprintf("no identity %s: grelot who --all lists them", name))
}

// read walks me's deliveries, and its posts or with a path the path
// threads, newest first, each stopping at the since bound, and returns
// every match in key order.
func (w *walk) read(r *bolttable.ReadTx) ([]Entry, error) {
	var out []Entry
	if w.deliveries {
		ds, err := w.walkDeliveries(r)
		if err != nil {
			return nil, err
		}
		out = ds
	}
	switch {
	case w.path != "" && !w.lastRead:
		ts, err := w.walkThreads(r, messageIDs(out))
		if err != nil {
			return nil, err
		}
		out = append(out, ts...)
	case w.posts:
		ps, err := w.walkPosts(r)
		if err != nil {
			return nil, err
		}
		out = append(out, ps...)
	}
	slices.SortFunc(out, func(a, b Entry) int { return a.Key().compare(b.Key()) })
	if !w.full {
		for i := range out {
			firstLine(&out[i])
		}
	}
	return out, nil
}

// walkDeliveries relies on seq order being write order, so the walk can
// stop at the first delivery written before since.
func (w *walk) walkDeliveries(r *bolttable.ReadTx) ([]Entry, error) {
	var out []Entry
	rg := bolttable.Range{Prefix: be64(w.me.ID), Reverse: true}
	if w.lastRead {
		rg.To = pairKey(w.me.ID, w.seqs.To+1)
	}
	err := w.s.t.deliveries.table.Scan(r, rg, func(_ []byte, d *deliveryRow) (bool, error) {
		if d.Seq < w.seqs.From || w.beforeSince(d.Written) {
			return false, nil
		}
		if d.Lease.due(w.now) {
			return true, nil
		}
		ok, err := w.deliveryMatches(r, d)
		if err != nil || !ok {
			return err == nil, err
		}
		e, err := w.s.entry(r, w.me, d)
		if err != nil {
			return false, err
		}
		if w.grepMatches(&e) {
			out = append(out, e)
		}
		return true, nil
	})
	return out, err
}

func (w *walk) deliveryMatches(r *bolttable.ReadTx, d *deliveryRow) (bool, error) {
	switch {
	case w.thread != 0 && d.Thread != w.thread,
		w.announce && d.Kind != kindAnnounce,
		w.sender != 0 && (!isPost(d.Kind) || d.Sender != w.sender),
		w.afterUntil(d.Written):
		return false, nil
	case w.path == "":
		return true, nil
	case d.Kind != kindPath:
		return false, nil
	}
	p, err := w.threadPath(r, d.Thread)
	return under(p, w.path), err
}

// threadPath is path thread id's path, read once per walk.
func (w *walk) threadPath(r *bolttable.ReadTx, id uint64) (string, error) {
	if p, ok := w.threadPaths[id]; ok {
		return p, nil
	}
	th, found, err := w.s.t.threads.table.Get(r, id)
	if err != nil {
		return "", err
	}
	if !found {
		// A delivery expires with its message, and a thread no earlier
		// than its last message.
		return "", fmt.Errorf("store: a delivery names thread %d, which is gone", id)
	}
	w.threadPaths[id] = th.Path
	return th.Path, nil
}

// walkPosts reads the sender index, whose key carries the post time, so
// the walk stops at since before reading the message.
func (w *walk) walkPosts(r *bolttable.ReadTx) ([]Entry, error) {
	var out []Entry
	err := w.s.t.bySender.Scan(r, bolttable.Range{Prefix: be64(w.me.ID), Reverse: true}, func(ikey, k, _ []byte) (bool, error) {
		at, err := senderPostedAt(ikey)
		if err != nil || w.beforeSince(at) {
			return false, err
		}
		if w.afterUntil(at) {
			return true, nil
		}
		m, found, err := w.s.t.messages.table.Get(r, k)
		if err != nil {
			return false, err
		}
		if !found || m.Lease.due(w.now) || !w.postMatches(&m) {
			return true, nil
		}
		e := Entry{Kind: m.Kind, Written: m.PostedAt, Message: m.ID, Thread: m.Thread}
		fillMessage(&e, w.me, &m)
		if w.grepMatches(&e) {
			out = append(out, e)
		}
		return true, nil
	})
	return out, err
}

// walkThreads reads the posts of the live path threads at or under w.path
// that readsThread lets me read, delivered to me or not, but the messages
// in seen.
func (w *walk) walkThreads(r *bolttable.ReadTx, seen map[uint64]bool) ([]Entry, error) {
	var out []Entry
	err := w.s.t.threadKey.Scan(r, bolttable.Range{Prefix: []byte(w.path)}, func(_ []byte, id uint64, _ []byte) (bool, error) {
		th, found, err := w.s.t.threads.table.Get(r, id)
		if err != nil || !found || !w.readsThread(&th) {
			return err == nil, err
		}
		ps, err := w.threadPosts(r, th.ID, seen)
		out = append(out, ps...)
		return err == nil, err
	})
	return out, err
}

// readsThread is false for a thread outside w.path, an expired or
// non-path thread, and one a delegate's scope leaves out.
func (w *walk) readsThread(th *threadRow) bool {
	return th.Kind == kindPath && !th.Lease.due(w.now) && under(th.Path, w.path) &&
		(w.me.Parent == 0 || w.me.Scope.allowsPath(th.Path))
}

// threadPosts reads thread's matching posts newest first. It
// relies on id order being post-time order, so the walk can stop at the
// first post before since, as walkDeliveries relies on seq order.
func (w *walk) threadPosts(r *bolttable.ReadTx, thread uint64, seen map[uint64]bool) ([]Entry, error) {
	author := w.sender
	if w.fromMe {
		author = w.me.ID
	}
	var out []Entry
	err := w.s.t.messages.table.Scan(r, bolttable.Range{Prefix: be64(thread), Reverse: true}, func(_ []byte, m *messageRow) (bool, error) {
		if w.beforeSince(m.PostedAt) {
			return false, nil
		}
		if seen[m.ID] || m.Lease.due(w.now) || !w.postMatches(m) || (author != 0 && m.Sender != author) ||
			w.afterUntil(m.PostedAt) {
			return true, nil
		}
		e := Entry{Kind: m.Kind, Written: m.PostedAt, Message: m.ID, Thread: m.Thread}
		fillMessage(&e, w.me, m)
		if w.grepMatches(&e) {
			out = append(out, e)
		}
		return true, nil
	})
	return out, err
}

func messageIDs(es []Entry) map[uint64]bool {
	ids := make(map[uint64]bool, len(es))
	for i := range es {
		ids[es[i].Message] = true
	}
	return ids
}

func (w *walk) postMatches(m *messageRow) bool {
	switch {
	case w.thread != 0 && m.Thread != w.thread,
		w.announce && m.Kind != kindAnnounce,
		w.path != "" && (m.Kind != kindPath || !under(m.Path, w.path)):
		return false
	}
	return true
}

func (w *walk) grepMatches(e *Entry) bool {
	if w.grep == "" {
		return true
	}
	return slices.ContainsFunc([]string{e.Subject, e.Body, e.Text}, func(s string) bool {
		return strings.Contains(strings.ToLower(s), w.grep)
	})
}

// firstLine cuts e's body to its first line.
func firstLine(e *Entry) {
	if i := strings.IndexByte(e.Body, '\n'); i >= 0 {
		e.Body, e.Truncated = strings.TrimSuffix(e.Body[:i], "\r"), true
	}
}
