//go:build !windows

package store

import (
	"errors"
	"slices"
	"time"

	"github.com/cplieger/bolttable"
)

// unreadSummary is an identity's delivery rows past read, kept in its row
// so no reader of the backlog scans it. Three writes keep it: deliver adds
// a row, passRead takes out the rows an ack passes, and recount counts
// again after a delivery past read expires. It counts a row until the
// sweep deletes it, so it holds only before Due. A row with no expiry is
// never due and never moves Due.
type unreadSummary struct {
	// Oldest is the write time of the first wake row, Newest the last
	// one's seq.
	Oldest time.Time `json:"oldest,omitzero"`
	// Due is at or before the earliest expiry of a row past read.
	Due    time.Time `json:"due,omitzero"`
	Counts Counts    `json:"counts"`
	Newest uint64    `json:"newest,omitempty"`
}

// add counts d, which comes after every row already counted.
func (u *unreadSummary) add(d *deliveryRow) {
	u.Counts.bump(d, 1)
	if d.wakes() {
		if u.Newest == 0 {
			u.Oldest = d.Written
		}
		u.Newest = d.Seq
	}
	if e := d.Lease.Expires; !e.IsZero() && (u.Due.IsZero() || e.Before(u.Due)) {
		u.Due = e
	}
}

// holds reports whether no row u counts is due at now.
func (u *unreadSummary) holds(now time.Time) bool { return u.Due.IsZero() || u.Due.After(now) }

func (u *unreadSummary) wake() wakeBatch {
	n := u.Counts.Wake()
	if n == 0 {
		return wakeBatch{}
	}
	return wakeBatch{oldest: u.Oldest, newest: u.Newest, count: n, renudges: n > u.Counts.Identity}
}

// pastRead is me's unexpired deliveries past read at now: its summary
// while that holds, else a scan.
func (s *Store) pastRead(r *bolttable.ReadTx, me *identityRow, now time.Time) (unreadSummary, error) {
	if me.Unread.holds(now) {
		return me.Unread, nil
	}
	var u unreadSummary
	err := s.scanUnread(r, me.ID, me.Read, now, func(d *deliveryRow) error {
		u.add(d)
		return nil
	})
	return u, err
}

// unread counts row's unexpired deliveries past read.
func (s *Store) unread(r *bolttable.ReadTx, row *identityRow, now time.Time) (int, error) {
	u, err := s.pastRead(r, row, now)
	return u.Counts.Total(), err
}

// pastRange is identity id's rows past after, up to and with to; a zero
// to has no upper end.
func pastRange(id, after, to uint64) bolttable.Range {
	rg := bolttable.Range{Prefix: be64(id), From: pairKey(id, after+1)}
	if to != 0 {
		rg.To = pairKey(id, to+1)
	}
	return rg
}

// passRead takes me's rows past read up to to out of its summary, moves
// read to to, and returns the unexpired ones in seq order.
func (w *wtx) passRead(me *identityRow, to uint64) ([]deliveryRow, error) {
	var rows []deliveryRow
	err := w.s.t.deliveries.table.Scan(w.r, pastRange(me.ID, me.Read, to), func(_ []byte, d *deliveryRow) (bool, error) {
		me.Unread.Counts.bump(d, -1)
		if !d.Lease.due(w.now) {
			rows = append(rows, *d)
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	me.Read = to
	u := &me.Unread
	switch {
	case me.Read == me.Seq:
		*u = unreadSummary{}
	case u.Counts.Wake() == 0:
		u.Oldest, u.Newest = time.Time{}, 0
	default:
		err = w.s.t.deliveries.table.Scan(w.r, pastRange(me.ID, to, 0), func(_ []byte, d *deliveryRow) (bool, error) {
			if d.wakes() {
				u.Oldest = d.Written
			}
			return !d.wakes(), nil
		})
	}
	return rows, err
}

// expireDelivery deletes d and, when it was past its identity's read or
// that summary no longer holds, has the summary counted again from the
// rows once the transaction's work is done.
func expireDelivery(w *wtx, k []byte, d *deliveryRow) error {
	row, found, err := w.s.t.identities.table.Get(w.r, d.Agent)
	if err != nil {
		return err
	}
	if found && (d.Seq > row.Read || !row.Unread.holds(w.now)) && !slices.Contains(w.recounts, d.Agent) {
		w.recounts = append(w.recounts, d.Agent)
	}
	return deleteRow(w, w.s.t.deliveries, k)
}

// recount rebuilds each owed summary from every row past read, due ones
// included, since the summary counts a row until its delete.
func (w *wtx) recount() error {
	for _, id := range w.recounts {
		row, found, err := w.s.t.identities.table.Get(w.r, id)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		var u unreadSummary
		if err := w.s.t.deliveries.table.Scan(w.r, pastRange(id, row.Read, 0), func(_ []byte, d *deliveryRow) (bool, error) {
			u.add(d)
			return true, nil
		}); err != nil {
			return err
		}
		row.Unread = u
		if err := w.s.t.identities.table.Put(w.tx, id, &row); err != nil {
			return err
		}
	}
	w.recounts = nil
	return nil
}

// errScanDone ends a scanUnread early.
var errScanDone = errors.New("scan done")
