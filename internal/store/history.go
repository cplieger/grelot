package store

import (
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/bolttable"
)

// History page sizes and window: a limit of 0 takes the default, and a
// zero Since the last DefaultHistoryWindow, except with LastRead.
const (
	DefaultHistoryLimit  = 20
	MaxHistoryLimit      = 100
	DefaultHistoryWindow = 24 * time.Hour
)

// HistoryQuery filters History; every filter set must hold. Thread is a
// thread id; Path keeps path threads at or under it; With is the direct
// thread with the identity of that name; Announce the announcements. From
// keeps that identity's posts to the caller and FromMe only the caller's
// own posts. Since and Until bound the time, both inclusive. Grep is a
// substring of the subject, body or event text, ignoring case. LastRead
// keeps the deliveries of the last acked inbox page. Full keeps whole
// bodies instead of their first line.
type HistoryQuery struct {
	Since    time.Time
	Until    time.Time
	Path     string
	With     string
	From     string
	Grep     string
	Thread   uint64
	Limit    int
	FromMe   bool
	Announce bool
	Full     bool
	LastRead bool
}

// History is a history read: the newest matching entries, oldest first.
type History struct {
	Entries  []Entry
	Identity Identity
}

// History reads session's own mailbox, its deliveries read and unread and,
// unless From names another identity or LastRead is set, its own posts in
// the same threads, merged in time order. It moves no cursor. An own post
// carries no Seq and the relation "you". Refusals: KindInvalid for a bad
// limit, path or time window, From with FromMe, or With naming the caller;
// KindNotFound for an unknown name; KindGone for a session with no live
// identity.
func (s *Store) History(ctx context.Context, session string, query *HistoryQuery) (History, error) {
	q := *query
	if err := checkHistory(&q); err != nil {
		return History{}, err
	}
	if _, err := s.Seen(ctx, session); err != nil {
		return History{}, err
	}
	var out History
	err := s.db.View(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		me, err := s.liveFor(r, session, now)
		if err != nil {
			return err
		}
		out.Identity = s.view(&me)
		h, ok, err := s.historyWalk(r, &me, &q, now)
		if err != nil || !ok {
			return err
		}
		out.Entries, err = h.read(r)
		return err
	})
	return out, err
}

func checkHistory(q *HistoryQuery) error {
	switch {
	case q.Limit == 0:
		q.Limit = DefaultHistoryLimit
	case q.Limit < 0 || q.Limit > MaxHistoryLimit:
		return refuse(KindInvalid, fmt.Sprintf("the history limit must be 1 to %d", MaxHistoryLimit))
	}
	if q.Path != "" {
		if _, err := checkPaths([]string{q.Path}); err != nil {
			return err
		}
	}
	if q.FromMe && q.From != "" {
		return refuse(KindInvalid, "--from takes one identity: @me or another's name")
	}
	if !q.Until.IsZero() && q.Since.After(q.Until) {
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
	since       time.Time
	until       time.Time
	grep        string
	path        string
	seqs        seqRange
	thread      uint64
	sender      uint64
	limit       int
	announce    bool
	full        bool
	lastRead    bool
	deliveries  bool
	posts       bool
}

// historyWalk resolves q's names and defaults for me; ok is false when
// no row can match, such as With an identity me shares no thread with.
func (s *Store) historyWalk(r *bolttable.ReadTx, me *identityRow, q *HistoryQuery, now time.Time) (w walk, ok bool, err error) {
	w = walk{
		s: s, me: me, now: now, until: q.Until, grep: strings.ToLower(q.Grep), path: q.Path,
		thread: q.Thread, limit: q.Limit, announce: q.Announce, full: q.Full, lastRead: q.LastRead,
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
	w.deliveries = !fromMe
	w.posts = !q.LastRead && w.sender == 0
	return w, true, nil
}

// historySince is q's lower bound: Since, or the default window before
// now unless q reads the last page. An Until before that default window
// is refused, as an explicit inverted window is.
func historySince(q *HistoryQuery, now time.Time) (time.Time, error) {
	if !q.Since.IsZero() || q.LastRead {
		return q.Since, nil
	}
	since := now.Add(-DefaultHistoryWindow)
	if !q.Until.IsZero() && since.After(q.Until) {
		return since, refuse(KindInvalid, "--until is before the default 24 h window: pass --since too")
	}
	return since, nil
}

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
	return refuse(KindNotFound, fmt.Sprintf("no identity %s: grelot who --all lists them", name))
}

// read walks me's deliveries and posts newest first, each stopping at the
// since bound or at limit matches, and returns the newest limit of both.
func (w *walk) read(r *bolttable.ReadTx) ([]Entry, error) {
	var out []Entry
	if w.deliveries {
		ds, err := w.walkDeliveries(r)
		if err != nil {
			return nil, err
		}
		out = ds
	}
	if w.posts {
		ps, err := w.walkPosts(r)
		if err != nil {
			return nil, err
		}
		out = append(out, ps...)
	}
	slices.SortFunc(out, func(a, b Entry) int {
		return cmp.Or(a.Written.Compare(b.Written), cmp.Compare(a.Message, b.Message), cmp.Compare(a.Seq, b.Seq))
	})
	out = out[max(0, len(out)-w.limit):]
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
		rg.From = pairKey(w.me.ID, w.seqs.To)
	}
	err := w.s.t.deliveries.table.Scan(r, rg, func(_ []byte, d *deliveryRow) (bool, error) {
		if d.Seq < w.seqs.From || d.Written.Before(w.since) {
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
		return len(out) < w.limit, nil
	})
	return out, err
}

func (w *walk) deliveryMatches(r *bolttable.ReadTx, d *deliveryRow) (bool, error) {
	switch {
	case w.thread != 0 && d.Thread != w.thread,
		w.announce && d.Kind != kindAnnounce,
		w.sender != 0 && (!isPost(d.Kind) || d.Sender != w.sender),
		!w.until.IsZero() && d.Written.After(w.until):
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
		if len(ikey) != 16 {
			return false, fmt.Errorf("store: sender index key %x is not 16 bytes", ikey)
		}
		at := time.Unix(0, int64(binary.BigEndian.Uint64(ikey[8:]))) //nolint:gosec // the key holds a UnixNano written from an int64
		if at.Before(w.since) {
			return false, nil
		}
		if !w.until.IsZero() && at.After(w.until) {
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
		return len(out) < w.limit, nil
	})
	return out, err
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
