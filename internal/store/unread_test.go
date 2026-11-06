package store

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// openSummaryStore opens a store whose threads live 25 minutes, so a
// server event expires inside one test.
func openSummaryStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	clk := &clock{t: testNow}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{
		Logger: slog.New(newLogSink()), Now: clk.now, ThreadTTL: 25 * time.Minute, DormantTTL: 40 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

func TestUnreadSummary_ackPastExpiredWarningsKeepsTheLaterPost(t *testing.T) {
	s, clk := openSummaryStore(t)
	reader := regAt(t, s, "reader", repoSubflux)
	regAt(t, s, "writer", repoGrelot)
	if _, err := s.Reserve(t.Context(), &Reserving{
		Caller: Caller{Session: sess("reader")}, Path: repoSubflux, Reason: "editing", TTL: 2 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	mustSweep(t, s) // reservation_expiring, living until 26 min
	clk.advance(time.Minute)
	mustSweep(t, s) // reservation_expired, until 27 min
	clk.advance(8 * time.Minute)
	post := mustPost(t, s, posting(sess("writer"), Target{Agent: "reader"})) // until 35 min
	if got := eventsTo(t, s, reader.ID, kindReservationExpired); len(got) != 1 {
		t.Fatalf("reservation_expired events %q, want one", got)
	}

	// Both warnings are past their expiry and not yet swept; the ack
	// passes them and stops before the post.
	clk.advance(18 * time.Minute)
	rows := deliveriesTo(t, s, reader.ID)
	last := rows[len(rows)-1]
	if last.Message != post.Message {
		t.Fatalf("last delivery %+v, want the post", last)
	}
	acked, err := s.Ack(t.Context(), Caller{Session: sess("reader")}, last.Seq-1)
	if err != nil {
		t.Fatal(err)
	}
	if acked.Left != (Counts{Direct: 1}) {
		t.Errorf("ack left %+v, want the one direct post", acked.Left)
	}
	checkSummaries(t, s, clk.now(), "after the ack")
	mustSweep(t, s)
	checkSummaries(t, s, clk.now(), "after the sweep")
	st, err := s.Status(t.Context(), Caller{Session: sess("reader")})
	if err != nil {
		t.Fatal(err)
	}
	if st.Unread != (Counts{Direct: 1}) {
		t.Errorf("status unread %+v, want the one direct post", st.Unread)
	}
}

func TestUnreadSummary_anIdentityAndItsWarningExpireInOneSweep(t *testing.T) {
	s, clk := openSummaryStore(t)
	reg := registration(sess("reader"), "reader", envA(), repoSubflux)
	reg.TTL = 15 * time.Minute // warned 7.5 min ahead
	reader := mustRegister(t, s, reg)
	regAt(t, s, "writer", repoGrelot)
	clk.advance(8 * time.Minute)
	mustSweep(t, s) // identity_expiring, living until 33 min
	if got := eventsTo(t, s, reader.ID, kindIdentityExpiring); len(got) != 1 {
		t.Fatalf("identity_expiring events %q, want one", got)
	}
	clk.advance(2 * time.Minute)
	mustPost(t, s, posting(sess("writer"), Target{Agent: "reader"})) // until 35 min
	checkSummaries(t, s, clk.now(), "before the expiry")

	// One sweep makes reader dormant and deletes its warning.
	clk.advance(24 * time.Minute)
	mustSweep(t, s)
	if got := eventsTo(t, s, reader.ID, kindIdentityExpiring); len(got) != 0 {
		t.Fatalf("identity_expiring events %q after the sweep, want none", got)
	}
	checkSummaries(t, s, clk.now(), "after the sweep")
	if got := mustCensus(t, s, func(*Identity) bool { return false }); got.Dormant != 1 {
		t.Errorf("dormant %d, want reader", got.Dormant)
	}
}

// A row with no expiry is never due, so the summary and the scan both
// count it.
func TestUnreadSummary_aRowWithNoExpiryCountsInBoth(t *testing.T) {
	now := testNow.Add(1000 * time.Hour)
	d := deliveryRow{Kind: kindDirect, Seq: 1, Written: testNow}
	if d.Lease.due(now) {
		t.Fatal("a zero lease is due")
	}
	var u unreadSummary
	u.add(&d)
	if !u.holds(now) || u.Counts != (Counts{Direct: 1}) || !u.Due.IsZero() {
		t.Errorf("summary %+v holds %t, want one direct row, no Due, holding", u, u.holds(now))
	}
}

func TestWakes_followsTheCountsWakeIncludes(t *testing.T) {
	for kind, want := range map[string]bool{
		kindDirect: true, kindPath: true, kindAnnounce: true, kindIdentityExpiring: true,
		kindReservationGranted: true, kindReservationExpiring: true, kindReservationExpired: true,
		kindReceiptsComplete: true, kindReceiptsDue: true,
		kindJoin: false, kindLeft: false, "unknown": false,
	} {
		if got := (&deliveryRow{Kind: kind}).wakes(); got != want {
			t.Errorf("%s wakes %t, want %t", kind, got, want)
		}
	}
	if (&deliveryRow{Kind: kindDirect, Via: "parent/review"}).wakes() {
		t.Error("a delegate's copy wakes")
	}
	var c Counts
	c.add(&deliveryRow{Kind: "unknown"})
	if c.Total() != 0 {
		t.Errorf("an unknown kind counted %+v", c)
	}
}
