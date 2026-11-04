package store

import (
	"context"
	"fmt"
	"time"

	"github.com/cplieger/bolttable"
)

// Inbox page sizes: a limit of 0 takes the default.
const (
	DefaultInboxLimit = 20
	MaxInboxLimit     = 100
)

// Entry is one delivery, or in history one of the reader's own posts, as
// its reader sees it. A post or card carries its message and sender; an
// event carries only Text. Relation is computed at the read from the
// message's sender snapshot and the reader's where, so it outlives the
// sender.
type Entry struct {
	PostedAt time.Time
	Written  time.Time
	ReadAt   time.Time
	Kind     string
	Subject  string
	Body     string
	Text     string
	// ThreadPath is a path thread's or a card's path; Matched is the
	// reader's work path or follow that a path post matched.
	ThreadPath string
	Matched    string
	From       string
	Host       string
	OS         string
	Arch       string
	Relation   string
	Seq        uint64
	Message    uint64
	Thread     uint64
	Wake       bool
	// Truncated is a history entry whose Body was cut to its first line.
	Truncated bool
}

// Counts are deliveries by kind. Cards are quiet; every other kind wakes.
type Counts struct {
	Direct, Path, Announce, Identity, Cards int
}

func (c *Counts) add(kind string) {
	switch kind {
	case kindDirect:
		c.Direct++
	case kindPath:
		c.Path++
	case kindAnnounce:
		c.Announce++
	case kindIdentityExpiring:
		c.Identity++
	case kindJoin, kindLeft:
		c.Cards++
	}
}

// Wake counts the deliveries that wake an agent.
func (c Counts) Wake() int { return c.Direct + c.Path + c.Announce + c.Identity }

func isWake(kind string) bool { return kind != kindJoin && kind != kindLeft }

// Page is an inbox fetch: the oldest unread deliveries, and Unread, every
// delivery past read, the page's included.
type Page struct {
	Entries  []Entry
	Identity Identity
	Unread   Counts
}

// Acked is what an ack answers: the read cursor and the deliveries still
// past it.
type Acked struct {
	Identity Identity
	Left     Counts
	Read     uint64
}

// Held is what a hook's hold answers: the deliveries past read and shown,
// and shown as the hold left it.
type Held struct {
	Identity Identity
	Counts   Counts
	Shown    uint64
}

// Inbox returns up to limit of session's unexpired deliveries past read,
// oldest first, and moves no cursor: the caller acks what it printed.
// Refusals: KindInvalid for a limit outside 0 to MaxInboxLimit; KindGone
// for a session with no live identity.
func (s *Store) Inbox(ctx context.Context, session string, limit int) (Page, error) {
	switch {
	case limit == 0:
		limit = DefaultInboxLimit
	case limit < 0 || limit > MaxInboxLimit:
		return Page{}, refuse(KindInvalid, fmt.Sprintf("the inbox limit must be 1 to %d", MaxInboxLimit))
	}
	if _, err := s.Seen(ctx, session); err != nil {
		return Page{}, err
	}
	var out Page
	err := s.db.View(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		me, err := s.liveFor(r, session, now)
		if err != nil {
			return err
		}
		out.Identity = s.view(&me)
		return s.scanUnread(r, me.ID, me.Read, now, func(d *deliveryRow) error {
			out.Unread.add(d.Kind)
			if len(out.Entries) == limit {
				return nil
			}
			e, err := s.entry(r, &me, d)
			out.Entries = append(out.Entries, e)
			return err
		})
	})
	return out, err
}

// entry is d as me reads it.
func (s *Store) entry(r *bolttable.ReadTx, me *identityRow, d *deliveryRow) (Entry, error) {
	e := Entry{
		Written: d.Written, ReadAt: d.ReadAt, Kind: d.Kind, Text: d.Text, Matched: d.Path,
		Seq: d.Seq, Message: d.Message, Thread: d.Thread, Wake: isWake(d.Kind),
	}
	if d.Message == 0 {
		return e, nil
	}
	m, found, err := s.t.messages.table.Get(r, pairKey(d.Thread, d.Message))
	if err != nil {
		return e, err
	}
	if !found {
		// A delivery expires with its message and a thread no earlier than
		// its last message, so an unexpired delivery always has one.
		return e, fmt.Errorf("store: delivery %d of identity %d names message %d, which is gone", d.Seq, d.Agent, d.Message)
	}
	fillMessage(&e, me, &m)
	return e, nil
}

// fillMessage sets e's message fields from m as me reads it.
func fillMessage(e *Entry, me *identityRow, m *messageRow) {
	e.PostedAt, e.Subject, e.Body, e.ThreadPath = m.PostedAt, m.Subject, m.Body, m.Path
	e.From, e.Host, e.OS, e.Arch = m.SenderName, m.Host, m.OS, m.Arch
	e.Relation = party{where: me.Where, id: me.ID}.relationTo(party{where: m.Where, id: m.Sender}, m.Path)
}

// Ack moves session's read cursor to max(read, upto), never past its
// highest seq, stamps read_at on each delivery it passes and records the
// range as last_page; a stale upto changes nothing. A session with no
// live identity is KindGone.
func (s *Store) Ack(ctx context.Context, session string, upto uint64) (Acked, error) {
	var out Acked
	err := s.update(ctx, func(w *wtx) error {
		me, err := w.liveBySession(session)
		if err != nil {
			return err
		}
		if to := min(upto, me.Seq); to > me.Read {
			from, err := w.stampRead(&me, to)
			if err != nil {
				return err
			}
			me.Read, me.LastPage = to, seqRange{From: from, To: to}
		}
		me.Ended, me.LastSeen = false, w.now
		if err := s.t.identities.table.Put(w.tx, me.ID, &me); err != nil {
			return err
		}
		out = Acked{Identity: s.view(&me), Read: me.Read}
		return s.scanUnread(w.r, me.ID, me.Read, w.now, func(d *deliveryRow) error {
			out.Left.add(d.Kind)
			return nil
		})
	})
	return out, err
}

// stampRead sets read_at on me's unexpired deliveries past read up to to,
// and returns the first one's seq, or read+1 when every one has expired.
func (w *wtx) stampRead(me *identityRow, to uint64) (uint64, error) {
	var rows []deliveryRow
	err := w.s.scanUnread(w.r, me.ID, me.Read, w.now, func(d *deliveryRow) error {
		if d.Seq <= to {
			rows = append(rows, *d)
		}
		return nil
	})
	if err != nil || len(rows) == 0 {
		return me.Read + 1, err
	}
	for i := range rows {
		rows[i].ReadAt = w.now
		if err := w.s.t.deliveries.table.Put(w.tx, pairKey(me.ID, rows[i].Seq), &rows[i]); err != nil {
			return 0, err
		}
	}
	return rows[0].Seq, nil
}

// Hold counts session's deliveries past max(read, shown) and, when one of
// them wakes, advances shown to the highest seq, so the next hold shows
// only what commits after this one; cards alone advance nothing, and wait
// for the next hold that shows a wake event. It never moves read. A
// session with no live identity is KindGone.
func (s *Store) Hold(ctx context.Context, session string) (Held, error) {
	var out Held
	err := s.update(ctx, func(w *wtx) error {
		me, err := w.liveBySession(session)
		if err != nil {
			return err
		}
		if err := s.scanUnread(w.r, me.ID, max(me.Read, me.Shown), w.now, func(d *deliveryRow) error {
			out.Counts.add(d.Kind)
			return nil
		}); err != nil {
			return err
		}
		if out.Counts.Wake() > 0 {
			me.Shown = me.Seq
		}
		me.Ended, me.LastSeen = false, w.now
		out.Identity, out.Shown = s.view(&me), me.Shown
		return s.t.identities.table.Put(w.tx, me.ID, &me)
	})
	return out, err
}

// scanUnread calls fn, in seq order, for each of identity id's unexpired
// deliveries past after. fn must not write the deliveries table.
func (s *Store) scanUnread(r *bolttable.ReadTx, id, after uint64, now time.Time, fn func(*deliveryRow) error) error {
	return s.t.deliveries.table.Scan(r, bolttable.Range{Prefix: be64(id), From: pairKey(id, after+1)},
		func(_ []byte, d *deliveryRow) (bool, error) {
			if d.Lease.due(now) {
				return true, nil
			}
			return true, fn(d)
		})
}

// unread counts identity id's unexpired deliveries past read.
func (s *Store) unread(r *bolttable.ReadTx, id, read uint64, now time.Time) (int, error) {
	n := 0
	err := s.scanUnread(r, id, read, now, func(*deliveryRow) error {
		n++
		return nil
	})
	return n, err
}
