package store

import (
	"fmt"
	"math"
)

// wireLeases sets each table's on_warn and on_expire, which need the
// Store's settings and so cannot be declared with the schema.
func (s *Store) wireLeases() {
	t := s.t
	t.identities.onWarn = warnIdentity
	t.identities.onExpire = expireIdentity
	t.threads.onExpire = func(w *wtx, k uint64, _ *threadRow) error { return deleteRow(w, t.threads, k) }
	t.messages.onExpire = func(w *wtx, k []byte, _ *messageRow) error { return deleteRow(w, t.messages, k) }
	t.deliveries.onExpire = func(w *wtx, k []byte, _ *deliveryRow) error { return deleteRow(w, t.deliveries, k) }
}

func deleteRow[K, V any](w *wtx, l *leased[K, V], k K) error {
	_, err := l.table.Delete(w.tx, k)
	return err
}

// expireIdentity makes a live identity dormant for DormantTTL, unwarned,
// and purges a dormant one.
func expireIdentity(w *wtx, k uint64, v *identityRow) error {
	if v.State == stateDormant {
		return deleteRow(w, w.s.t.identities, k)
	}
	v.State = stateDormant
	v.DormantSince = w.now
	v.Lease = lease{Expires: w.now.Add(w.s.dormantTTL)}
	return w.s.t.identities.table.Put(w.tx, k, v)
}

// warnIdentity writes identity_expiring. The text carries the expiry time
// because a resumed or revived session can read the row much later.
func warnIdentity(w *wtx, k uint64, v *identityRow) error {
	left := int(math.Ceil(v.Lease.Expires.Sub(w.now).Minutes()))
	text := fmt.Sprintf("your identity %s expires in %d min (at %s); if you are still working on %s, run `grelot renew --ttl %s`, otherwise do nothing",
		v.Name, left, v.Lease.Expires.UTC().Format("2006-01-02 15:04 UTC"), v.Description, fmtDur(v.TTL))
	return w.deliverEvent(k, kindIdentityExpiring, text)
}

// deliverEvent writes a server event to identity id under its next seq,
// living THREAD_TTL from now.
func (w *wtx) deliverEvent(id uint64, kind, text string) error {
	idt := w.s.t.identities.table
	row, found, err := idt.Get(w.r, id)
	if err != nil || !found {
		return err
	}
	row.Seq++
	if err := idt.Put(w.tx, id, &row); err != nil {
		return err
	}
	return w.s.t.deliveries.table.Put(w.tx, pairKey(id, row.Seq), &deliveryRow{
		Agent: id, Seq: row.Seq, Kind: kind, Text: text, Written: w.now,
		Lease: lease{Expires: w.now.Add(w.s.threadTTL)},
	})
}
