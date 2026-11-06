package store

import (
	"context"
	"errors"
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
	// Via is the delegate handle of a parent's copy.
	Via     string
	Seq     uint64
	Message uint64
	Thread  uint64
	Wake    bool
	// Truncated is a history entry whose Body was cut to its first line.
	Truncated bool
}

// Counts are deliveries by kind. Cards and a delegate's copies are quiet;
// every other kind wakes.
type Counts struct {
	Direct, Path, Announce, Identity, Reservation, Receipts, Cards, Copies int
}

func (c *Counts) add(d *deliveryRow) { c.bump(d, 1) }

// bump adds n to d's kind.
func (c *Counts) bump(d *deliveryRow, n int) {
	if d.Via != "" {
		c.Copies += n
		return
	}
	switch d.Kind {
	case kindDirect:
		c.Direct += n
	case kindPath:
		c.Path += n
	case kindAnnounce:
		c.Announce += n
	case kindIdentityExpiring:
		c.Identity += n
	case kindReservationGranted, kindReservationExpiring, kindReservationExpired:
		c.Reservation += n
	case kindReceiptsComplete, kindReceiptsDue:
		c.Receipts += n
	case kindJoin, kindLeft:
		c.Cards += n
	}
}

// Wake counts the deliveries that wake an agent.
func (c Counts) Wake() int {
	return c.Direct + c.Path + c.Announce + c.Identity + c.Reservation + c.Receipts
}

// Total counts every delivery, quiet ones included.
func (c Counts) Total() int { return c.Wake() + c.Cards + c.Copies }

// wakes reports whether d falls in a count Wake includes, so a kind no
// count names neither wakes nor counts.
func (d *deliveryRow) wakes() bool {
	var c Counts
	c.bump(d, 1)
	return c.Wake() == 1
}

// Page is an inbox fetch: the oldest unread deliveries, and Unread, every
// delivery past read, the page's included.
type Page struct {
	Entries []Entry
	Unread  Counts
}

// Acked is what an ack answers: the read cursor and the deliveries still
// past it. Count is how many deliveries the ack passed and Oldest the
// earliest of their write times, zero when it passed none.
type Acked struct {
	Oldest   time.Time
	Identity Identity
	Left     Counts
	Read     uint64
	Count    int
}

// Held is what a hook's hold answers: the deliveries past read and shown,
// and shown as the hold left it.
type Held struct {
	Identity Identity
	Counts   Counts
	Shown    uint64
}

// Inbox returns up to limit of c's unexpired deliveries past read,
// oldest first, and moves no cursor: the caller acks what it printed.
// Refusals: KindInvalid for a limit outside 0 to MaxInboxLimit; KindGone
// for a session with no live identity.
func (s *Store) Inbox(ctx context.Context, c Caller, limit int) (Page, error) {
	switch {
	case limit == 0:
		limit = DefaultInboxLimit
	case limit < 0 || limit > MaxInboxLimit:
		return Page{}, refuse(KindInvalid, fmt.Sprintf("the inbox limit must be 1 to %d", MaxInboxLimit))
	}
	if _, err := s.Seen(ctx, c); err != nil {
		return Page{}, err
	}
	var out Page
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		me, err := s.liveFor(r, c, now)
		if err != nil {
			return err
		}
		u, err := s.pastRead(r, &me, now)
		if err != nil {
			return err
		}
		out.Unread = u.Counts
		err = s.scanUnread(r, me.ID, me.Read, now, func(d *deliveryRow) error {
			if len(out.Entries) == limit {
				return errScanDone
			}
			e, eerr := s.entry(r, &me, d)
			out.Entries = append(out.Entries, e)
			return eerr
		})
		if errors.Is(err, errScanDone) {
			err = nil
		}
		return err
	})
	return out, err
}

// entry is d as me reads it.
func (s *Store) entry(r *bolttable.ReadTx, me *identityRow, d *deliveryRow) (Entry, error) {
	e := Entry{
		Written: d.Written, ReadAt: d.ReadAt, Kind: d.Kind, Text: d.Text, Matched: d.Path,
		Seq: d.Seq, Message: d.Message, Thread: d.Thread, Via: d.Via, Wake: d.wakes(),
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

// Ack moves c's read cursor to max(read, upto), never past its
// highest seq, stamps read_at on each delivery it passes and records the
// range as last_page; a stale upto changes nothing. A session with no
// live identity is KindGone.
func (s *Store) Ack(ctx context.Context, c Caller, upto uint64) (Acked, error) {
	var out Acked
	err := s.update(ctx, func(w *wtx) error {
		me, err := w.liveAs(c)
		if err != nil {
			return err
		}
		if to := min(upto, me.Seq); to > me.Read {
			from := me.Read + 1
			rows, err := w.stampRead(&me, to)
			if err != nil {
				return err
			}
			if len(rows) > 0 {
				from, out.Count, out.Oldest = rows[0].Seq, len(rows), rows[0].Written
			}
			me.LastPage = seqRange{From: from, To: to}
			w.wake(me.ID)
		}
		w.touch(&me)
		if err := s.t.identities.table.Put(w.tx, me.ID, &me); err != nil {
			return err
		}
		v, verr := w.viewOf(&me)
		if verr != nil {
			return verr
		}
		out.Identity, out.Read = v, me.Read
		u, perr := s.pastRead(w.r, &me, w.now)
		out.Left = u.Counts
		return perr
	})
	return out, err
}

// stampRead moves me's read to to, sets read_at on the unexpired
// deliveries it passes, takes me out of each receipt watch they name, and
// returns them in seq order.
func (w *wtx) stampRead(me *identityRow, to uint64) ([]deliveryRow, error) {
	rows, err := w.passRead(me, to)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].ReadAt = w.now
		if err := w.s.t.deliveries.table.Put(w.tx, pairKey(me.ID, rows[i].Seq), &rows[i]); err != nil {
			return nil, err
		}
		if err := w.leaveWatch(rows[i].Watch, me, exitRead); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// Hold counts c's deliveries past max(read, shown) and, when one of
// them wakes, advances shown to the highest seq, so the next hold shows
// only what commits after this one; cards alone advance nothing, and wait
// for the next hold that shows a wake event. It never moves read, and
// commits only when shown moves or seenDue asks for a write. A session with no live identity is
// KindGone.
func (s *Store) Hold(ctx context.Context, c Caller) (Held, error) {
	var out Held
	write := false
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		me, err := s.liveFor(r, c, now)
		if err != nil {
			return err
		}
		if out, _, err = s.held(r, &me, now); err != nil {
			return err
		}
		if write = seenDue(&me, now) || out.Counts.Wake() > 0; !write {
			s.noteSeen(me.ID, now)
			out.Identity = s.view(&me)
		}
		return nil
	})
	if err != nil || !write {
		return out, err
	}
	err = s.update(ctx, func(w *wtx) error {
		var herr error
		out, herr = w.hold(c)
		return herr
	})
	return out, err
}

// hold is Hold's write: it advances shown and, when no nudge is
// outstanding, records what it showed as the nudge.
func (w *wtx) hold(c Caller) (Held, error) {
	me, err := w.liveAs(c)
	if err != nil {
		return Held{}, err
	}
	out, shown, err := w.s.held(w.r, &me, w.now)
	if err != nil {
		return out, err
	}
	w.seen(&me)
	if shown.count > 0 && me.Parent == 0 && me.Read >= me.Nudged {
		// What a hold shows inside the turn is the nudge, so no push or
		// watcher sends it; the re-nudge still covers an unread one.
		w.recordNudge(&me, &Nudge{Seq: shown.newest, Oldest: shown.oldest, Attempt: 1}, nudgeByHook)
	}
	if out.Identity, err = w.viewOf(&me); err != nil {
		return out, err
	}
	return out, w.s.t.identities.table.Put(w.tx, me.ID, &me)
}

// held counts me's deliveries past max(read, shown) and, when one of them
// wakes, advances me's shown to its highest seq. shown is the wake
// deliveries among them.
func (s *Store) held(r *bolttable.ReadTx, me *identityRow, now time.Time) (out Held, shown wakeBatch, err error) {
	err = s.scanUnread(r, me.ID, max(me.Read, me.Shown), now, func(d *deliveryRow) error {
		out.Counts.add(d)
		shown.add(d)
		return nil
	})
	if shown.count > 0 {
		me.Shown = me.Seq
	}
	out.Shown = me.Shown
	return out, shown, err
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
