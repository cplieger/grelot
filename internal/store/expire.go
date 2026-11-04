package store

import (
	"fmt"
	"math"
	"time"
)

// wireLeases sets each table's on_warn and on_expire, which need the
// Store's settings and so cannot be declared with the schema.
func (s *Store) wireLeases() {
	t := s.t
	t.identities.onWarn = warnIdentity
	t.identities.onExpire = expireIdentity
	t.threads.onExpire = expireThread
	t.messages.onExpire = func(w *wtx, k []byte, _ *messageRow) error { return deleteRow(w, t.messages, k) }
	t.deliveries.onExpire = func(w *wtx, k []byte, _ *deliveryRow) error { return deleteRow(w, t.deliveries, k) }
}

func deleteRow[K, V any](w *wtx, l *leased[K, V], k K) error {
	_, err := l.table.Delete(w.tx, k)
	return err
}

// expireIdentity makes a live identity dormant, owing a left card for each
// of its paths, and purges a dormant one, which already left.
func expireIdentity(w *wtx, k uint64, v *identityRow) error {
	if v.State == stateDormant {
		w.s.forgetSeen(k)
		return deleteRow(w, w.s.t.identities, k)
	}
	w.queueCards(kindLeft, v, v.Paths)
	d := dormantRow(v, w.s.dormantTTL)
	return w.s.t.identities.table.Put(w.tx, k, &d)
}

// dormantRow is v once its lease ran out: dormant since its expiry, under
// an unwarned lease until ttl after it. Counting from the expiry, not from
// the sweep, keeps a late sweep from lengthening the dormancy reads show.
func dormantRow(v *identityRow, ttl time.Duration) identityRow {
	d := *v
	d.State, d.DormantSince = stateDormant, v.Lease.Expires
	d.Lease = serverLease(v.Lease.Expires, ttl)
	return d
}

// warnIdentity writes identity_expiring. The text carries the expiry time
// because a resumed or revived session can read the row much later.
func warnIdentity(w *wtx, k uint64, v *identityRow) error {
	left := int(math.Ceil(v.Lease.Expires.Sub(w.now).Minutes()))
	text := fmt.Sprintf("your identity %s expires in %d min (at %s); if you are still working on %s, run `grelot renew --ttl %s`, otherwise do nothing",
		v.Name, left, fmtTime(v.Lease.Expires), v.Description, fmtDur(v.TTL))
	return w.deliverEvent(k, kindIdentityExpiring, text)
}

// deliverEvent writes a server event to identity id under its next seq,
// living THREAD_TTL from now.
func (w *wtx) deliverEvent(id uint64, kind, text string) error {
	row, found, err := w.s.t.identities.table.Get(w.r, id)
	if err != nil || !found {
		return err
	}
	return w.deliver(&row, &deliveryRow{Kind: kind, Text: text, Lease: serverLease(w.now, w.s.threadTTL)})
}

// deliver writes d to row's identity under its next seq and stores row
// with that seq. row must be this transaction's latest copy: storing an
// older one afterwards would reuse the seq.
func (w *wtx) deliver(row *identityRow, d *deliveryRow) error {
	row.Seq++
	d.Agent, d.Seq, d.Written = row.ID, row.Seq, w.now
	if err := w.s.t.identities.table.Put(w.tx, row.ID, row); err != nil {
		return err
	}
	return w.s.t.deliveries.table.Put(w.tx, pairKey(row.ID, row.Seq), d)
}
