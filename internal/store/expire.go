//go:build !windows

package store

import (
	"fmt"
	"math"
	"slices"
	"time"
)

// wireLeases sets each table's on_warn and on_expire, which need the
// Store's settings and so cannot be declared with the schema.
func (s *Store) wireLeases() {
	t := s.t
	t.identities.onWarn = warnIdentity
	t.identities.onExpire = expireIdentity
	t.threads.onExpire = expireThread
	t.messages.onExpire = expireMessage
	t.deliveries.onExpire = expireDelivery
	t.resv.onWarn = warnReservation
	t.resv.onExpire = expireReservation
	t.links.onExpire = func(w *wtx, k []byte, _ *linkRow) error { return deleteRow(w, t.links, k) }
	t.watches.onExpire = expireWatch
	t.requests.onExpire = func(w *wtx, k []byte, _ *requestRow) error { return deleteRow(w, t.requests, k) }
}

// expireThread deletes a thread. A follow is of the path, not the thread,
// so an agent's outlives the thread; @user's follow of a path thread's
// path ends with it, because @user never expires and has no unfollow, and
// the follow would deliver that path's posts to it for good.
func expireThread(w *wtx, k uint64, v *threadRow) error {
	if v.Kind == kindPath {
		if err := w.unfollowUser(v.Path); err != nil {
			return err
		}
		w.changedPathThread(k, v.Path)
	} else {
		w.changedThread(k)
	}
	return deleteRow(w, w.s.t.threads, k)
}

// expireMessage deletes a message, naming its thread's conversation as
// changed when the message is a post, which the conversation shows.
func expireMessage(w *wtx, k []byte, v *messageRow) error {
	switch {
	case v.Kind == kindPath:
		w.changedPathThread(v.Thread, v.Path)
	case isPost(v.Kind):
		w.changedThread(v.Thread)
	}
	return deleteRow(w, w.s.t.messages, k)
}

// unfollowUser drops p from @user's follows.
func (w *wtx) unfollowUser(p string) error {
	id, _, found, err := w.s.t.byName.Lookup(w.r, []byte(UserName))
	if err != nil || !found {
		return err
	}
	row, found, err := w.s.t.identities.table.Get(w.r, id)
	if err != nil || !found || !slices.Contains(row.Follows, p) {
		return err
	}
	row.Follows = slices.DeleteFunc(row.Follows, func(f string) bool { return f == p })
	return w.putIdentity(id, &row)
}

func deleteRow[K, V any](w *wtx, l *leased[K, V], k K) error {
	_, err := l.table.Delete(w.tx, k)
	return err
}

// expireIdentity makes a live identity dormant, owing a left card for each
// of its paths, handing its reservations to their queues and ending its
// delegates and links; it purges a dormant one, which already left, and
// ends a delegate.
func expireIdentity(w *wtx, k uint64, v *identityRow) error {
	switch {
	case v.Parent != 0:
		return w.endDelegate(v, endExpired)
	case v.State == stateDormant:
		w.forgetSeen(k)
		if err := w.leaveWatches(v); err != nil {
			return err
		}
		return w.deleteIdentity(k)
	}
	if err := w.freeHolder(k); err != nil {
		return err
	}
	if err := w.endDelegates(k, endParent); err != nil {
		return err
	}
	if err := w.deleteLinks(k); err != nil {
		return err
	}
	w.queueCards(kindLeft, v, v.Paths)
	d := dormantRow(v, w.s.dormantTTL)
	return w.putIdentity(k, &d)
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

// warnIdentity owes identity_expiring. The text carries the expiry time
// because a resumed or revived session can read the row much later. A
// delegate renews as its handle, since its parent's session is gated.
func warnIdentity(w *wtx, k uint64, v *identityRow) error {
	left := int(math.Ceil(v.Lease.Expires.Sub(w.now).Minutes()))
	verb := "grelot renew"
	if v.Parent != 0 {
		verb = "grelot as " + v.Name + " renew"
	}
	text := fmt.Sprintf("your identity @%s expires in %d min (at %s); if you are still working on %s, run `%s --ttl=%s`, otherwise do nothing",
		v.Name, left, fmtTime(v.Lease.Expires), v.Description, verb, fmtDur(v.TTL))
	w.oweEvent(k, kindIdentityExpiring, text)
	return nil
}

// owedEvent is a server event a transaction writes once its request is
// done.
type owedEvent struct {
	kind, text string
	to         uint64
}

// oweEvent defers a server event to flushOwed, the only writer of one: an
// event written mid-request bumps its recipient's seq while the request
// may hold an older copy of that row (an eager expiry reached through
// another identity), and storing that copy would reuse the seq.
func (w *wtx) oweEvent(to uint64, kind, text string) {
	w.events = append(w.events, owedEvent{to: to, kind: kind, text: text})
}

// writeEvent writes a server event to identity id under its next seq,
// living the retention from now.
func (w *wtx) writeEvent(id uint64, kind, text string) error {
	row, found, err := w.s.t.identities.table.Get(w.r, id)
	if err != nil || !found {
		return err
	}
	return w.deliver(&row, &deliveryRow{Kind: kind, Text: text, Lease: serverLease(w.now, w.retention)})
}

// deliver writes d to row's identity under its next seq and stores row
// with that seq. row must be this transaction's latest copy: storing an
// older one afterwards would reuse the seq.
func (w *wtx) deliver(row *identityRow, d *deliveryRow) error {
	row.Seq++
	d.Agent, d.Seq, d.Written = row.ID, row.Seq, w.now
	row.Unread.add(d)
	w.wake(row.ID)
	if err := w.putIdentity(row.ID, row); err != nil {
		return err
	}
	return w.s.t.deliveries.table.Put(w.tx, pairKey(row.ID, row.Seq), d)
}
