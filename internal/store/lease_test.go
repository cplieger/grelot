package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

func TestIdentityLease_warnLead(t *testing.T) {
	// identity_expiring goes out min(30 min, ttl/2) before expiry.
	cases := []struct {
		ttl, lead time.Duration
	}{
		{24 * time.Hour, 30 * time.Minute},
		{15 * time.Minute, 7*time.Minute + 30*time.Second},
		{time.Hour, 30 * time.Minute},
		{40 * time.Minute, 20 * time.Minute},
	}
	for _, c := range cases {
		l, err := identityPolicy.grant(testNow, c.ttl, time.Time{})
		if err != nil {
			t.Fatalf("grant(%v): %v", c.ttl, err)
		}
		if want := testNow.Add(c.ttl); !l.Expires.Equal(want) {
			t.Errorf("ttl %v: expires %v, want %v", c.ttl, l.Expires, want)
		}
		if got := l.Expires.Sub(l.WarnAt); got != c.lead {
			t.Errorf("ttl %v: warn lead %v, want %v", c.ttl, got, c.lead)
		}
	}
}

func TestIdentityLease_boundsRefusedWithTheFix(t *testing.T) {
	// --ttl is required, 15 minutes to 24 hours; a missing or
	// out-of-bounds one is a 400 naming the fix.
	for _, ttl := range []time.Duration{0, 14 * time.Minute, 24*time.Hour + time.Second, -time.Hour} {
		_, err := identityPolicy.grant(testNow, ttl, time.Time{})
		var se *Error
		if !errors.As(err, &se) || se.Kind != KindInvalid {
			t.Fatalf("ttl %v: err %v, want a KindInvalid *Error", ttl, err)
		}
		if !strings.Contains(se.Msg, "--ttl") || !strings.Contains(se.Msg, "15m") || !strings.Contains(se.Msg, "24h") {
			t.Errorf("ttl %v: message %q does not name --ttl and its bounds", ttl, se.Msg)
		}
	}
	for _, ttl := range []time.Duration{15 * time.Minute, 24 * time.Hour} {
		if _, err := identityPolicy.grant(testNow, ttl, time.Time{}); err != nil {
			t.Errorf("ttl %v at a bound: %v", ttl, err)
		}
	}
}

func TestLease_capHoldsTheExpiryAndItsWarning(t *testing.T) {
	// a delegate's expires_at never passes its parent's.
	limit := testNow.Add(time.Hour)
	l, err := identityPolicy.grant(testNow, 4*time.Hour, limit)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Expires.Equal(limit) {
		t.Errorf("expires %v, want the cap %v", l.Expires, limit)
	}
	if !l.WarnAt.Before(l.Expires) || l.WarnAt.Before(testNow) {
		t.Errorf("warn_at %v outside [now, expires %v]", l.WarnAt, l.Expires)
	}
}

func TestLease_postsCarryAnExpiryAndNoWarning(t *testing.T) {
	// threads, messages and deliveries expire THREAD_TTL after the post
	// and are never warned.
	s, _ := openTestStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	b := regAt(t, s, "bravo", "/workspace/bravo")
	p := say(t, s, "alpha", Target{Agent: "bravo"}, "hello")
	th, _ := findThread(t, s, p.Thread)
	m, _ := findMessage(t, s, p.Message)
	ds := deliveriesTo(t, s, b.ID)
	if len(ds) != 1 {
		t.Fatalf("%d deliveries, want 1", len(ds))
	}
	want := testNow.Add(DefaultThreadTTL)
	for name, got := range map[string]lease{"thread": th.Lease, "message": m.Lease, "delivery": ds[0].Lease} {
		if !got.Expires.Equal(want) || !got.WarnAt.IsZero() {
			t.Errorf("%s lease %+v, want expiry %v and no warning", name, got, want)
		}
	}
}

// putRows writes rows straight into the tables, for the lease tests that
// need rows before the verbs that create them exist.
func putRows(t *testing.T, s *Store, fn func(w *wtx) error) {
	t.Helper()
	if err := s.update(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

func countRows[K, V any](t *testing.T, s *Store, tb *bolttable.Table[K, V]) int {
	t.Helper()
	n := 0
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		return tb.Scan(r, bolttable.Range{}, func(K, *V) (bool, error) { n++; return true, nil })
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSweep_expiresEachRowAtItsOwnTime(t *testing.T) {
	s, clk := openClockStore(t)
	putRows(t, s, func(w *wtx) error {
		for i, d := range []time.Duration{time.Minute, time.Hour} {
			id := uint64(i + 1)
			if err := s.t.threads.table.Put(w.tx, id, &threadRow{ID: id, Key: "/t" + string(rune('a'+i)), Lease: lease{Expires: testNow.Add(d)}}); err != nil {
				return err
			}
			if err := s.t.messages.table.Put(w.tx, pairKey(id, id), &messageRow{Thread: id, ID: id, Lease: lease{Expires: testNow.Add(d)}}); err != nil {
				return err
			}
			if err := s.t.deliveries.table.Put(w.tx, pairKey(9, id), &deliveryRow{Agent: 9, Seq: id, Lease: lease{Expires: testNow.Add(d)}}); err != nil {
				return err
			}
		}
		return nil
	})

	clk.set(testNow.Add(time.Minute - time.Nanosecond))
	if _, err := s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, s.t.deliveries.table); n != 2 {
		t.Fatalf("before the first expiry: %d deliveries, want 2", n)
	}

	clk.set(testNow.Add(time.Minute))
	if _, err := s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]int{
		"threads":    countRows(t, s, s.t.threads.table),
		"messages":   countRows(t, s, s.t.messages.table),
		"deliveries": countRows(t, s, s.t.deliveries.table),
	} {
		if n != 1 {
			t.Errorf("at the first expiry: %d %s, want 1", n, name)
		}
	}
}

func TestSweep_aThreadTakesItsMessagesWithIt(t *testing.T) {
	// a thread's messages cascade with it, whatever their own
	// expiry says.
	s, clk := openClockStore(t)
	putRows(t, s, func(w *wtx) error {
		if err := s.t.threads.table.Put(w.tx, 1, &threadRow{ID: 1, Key: "/x", Lease: lease{Expires: testNow.Add(time.Minute)}}); err != nil {
			return err
		}
		return s.t.messages.table.Put(w.tx, pairKey(1, 1), &messageRow{Thread: 1, ID: 1, Lease: lease{Expires: testNow.Add(time.Hour)}})
	})
	clk.set(testNow.Add(time.Minute))
	if _, err := s.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, s.t.messages.table); n != 0 {
		t.Errorf("%d messages left after their thread expired", n)
	}
}

func TestSweep_runsInBoundedBatches(t *testing.T) {
	// the sweeper works in bounded batches, one Update each, and
	// a pass leaves nothing due behind.
	s, clk := openClockStore(t)
	const n = 3*sweepBatch + 7
	putRows(t, s, func(w *wtx) error {
		for i := range uint64(n) {
			if err := s.t.deliveries.table.Put(w.tx, pairKey(1, i+1), &deliveryRow{Agent: 1, Seq: i + 1, Lease: lease{Expires: testNow}}); err != nil {
				return err
			}
		}
		return nil
	})
	before := commitCount(t, s)
	clk.set(testNow.Add(time.Second))
	got, err := s.Sweep(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Errorf("Sweep expired %d, want %d", got, n)
	}
	if left := countRows(t, s, s.t.deliveries.table); left != 0 {
		t.Errorf("%d deliveries left", left)
	}
	if c := commitCount(t, s) - before; c < 4 {
		t.Errorf("%d commits for %d rows, want at least 4 batches of %d", c, n, sweepBatch)
	}
}

func TestSweep_warnsOnceAndARenewalReArms(t *testing.T) {
	// identity_expiring is written once at warn_at, warn_at is
	// cleared, and a renewal sets it again.
	s, clk := openClockStore(t)
	l, err := identityPolicy.grant(testNow, time.Hour, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	putRows(t, s, func(w *wtx) error {
		return s.t.identities.table.Put(w.tx, 1, &identityRow{ID: 1, Name: "lease-test", Description: "testing leases", TTL: time.Hour, State: stateLive, Lease: l})
	})
	events := func() []deliveryRow {
		var out []deliveryRow
		if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
			return s.t.deliveries.table.Scan(r, bolttable.Range{}, func(_ []byte, d *deliveryRow) (bool, error) {
				if d.Kind == kindIdentityExpiring {
					out = append(out, *d)
				}
				return true, nil
			})
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	clk.set(l.WarnAt.Add(-time.Second))
	mustSweep(t, s)
	if n := len(events()); n != 0 {
		t.Fatalf("%d warnings before warn_at", n)
	}
	clk.set(l.WarnAt)
	mustSweep(t, s)
	clk.set(l.WarnAt.Add(time.Minute))
	mustSweep(t, s)
	ev := events()
	if len(ev) != 1 {
		t.Fatalf("%d warnings after two sweeps past warn_at, want 1", len(ev))
	}
	// The warning names the identity, minutes left, the time, the description and
	// the renew command.
	for _, want := range []string{"lease-test", "expires in 30 min", "2026-11-04 06:39 UTC", "testing leases", "grelot renew --ttl"} {
		if !strings.Contains(ev[0].Text, want) {
			t.Errorf("warning %q lacks %q", ev[0].Text, want)
		}
	}
	if row := getIdentity(t, s, 1); !row.Lease.WarnAt.IsZero() {
		t.Errorf("warn_at %v not cleared after the warning", row.Lease.WarnAt)
	}

	putRows(t, s, func(w *wtx) error {
		row := getIdentity(t, s, 1)
		nl, err := identityPolicy.grant(w.now, time.Hour, time.Time{})
		if err != nil {
			return err
		}
		row.Lease = nl
		return s.t.identities.table.Put(w.tx, 1, &row)
	})
	if row := getIdentity(t, s, 1); row.Lease.WarnAt.IsZero() {
		t.Fatal("a renewal left warn_at clear")
	}
	clk.set(clk.now().Add(30 * time.Minute))
	mustSweep(t, s)
	if n := len(events()); n != 2 {
		t.Errorf("%d warnings after the renewed lease reached warn_at, want 2", n)
	}
}

func TestSettle_runsADueExpiryBeforeTheWriteReadsTheRow(t *testing.T) {
	// a write touching a row runs its due on_expire first, in the
	// same transaction, without waiting for the sweeper.
	s, clk := openClockStore(t)
	var id uint64
	putRows(t, s, func(w *wtx) error {
		var err error
		if id, err = s.t.threads.table.NextID(w.tx); err != nil {
			return err
		}
		return s.t.threads.table.Put(w.tx, id, &threadRow{ID: id, Key: "/x", Lease: lease{Expires: testNow.Add(time.Minute)}})
	})
	clk.set(testNow.Add(time.Minute))
	var found bool
	putRows(t, s, func(w *wtx) error {
		var err error
		_, found, err = s.t.threads.settle(w, id)
		return err
	})
	if found {
		t.Error("settle returned a thread past its expiry")
	}
	if _, ok := findThread(t, s, id); ok {
		t.Error("the thread is still stored after settle")
	}
}
