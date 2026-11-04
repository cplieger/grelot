package store

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestDelivery_outlivesTheReadAndExpiresWithItsMessage(t *testing.T) {
	s, clk := openClockStore(t)
	a := regAt(t, s, "alpha", "/workspace/alpha")
	b := regAt(t, s, "bravo", "/workspace/bravo")
	keepAlive(t, s, a.ID, b.ID)
	first := say(t, s, "alpha", Target{Agent: "bravo"}, "first")
	clk.advance(time.Hour)
	say(t, s, "alpha", Target{Agent: "bravo"}, "second")
	mustInbox(t, s, sess("bravo"), 0)
	mustAck(t, s, sess("bravo"), 2)

	clk.set(testNow.Add(DefaultThreadTTL - time.Nanosecond))
	mustSweep(t, s)
	ds := deliveriesTo(t, s, b.ID)
	if len(ds) != 2 || ds[0].ReadAt.IsZero() || ds[1].ReadAt.IsZero() {
		t.Fatalf("deliveries %+v, want both kept, read, until their message expires", ds)
	}
	wantSubjects(t, "history of read mail", historySubjects(t, s, sess("bravo"), HistoryQuery{Since: testNow}), "first", "second")

	clk.set(testNow.Add(DefaultThreadTTL))
	mustSweep(t, s)
	if _, found := findMessage(t, s, first.Message); found {
		t.Error("the first message outlived its expiry")
	}
	if _, found := findThread(t, s, first.Thread); !found {
		t.Fatal("the thread went with its first message, though the second keeps it")
	}
	if ds := deliveriesTo(t, s, b.ID); len(ds) != 1 || ds[0].Seq != 2 {
		t.Errorf("deliveries %+v, want only seq 2: the first expires by its own index with its message", ds)
	}
	wantSubjects(t, "history after the first expiry", historySubjects(t, s, sess("bravo"), HistoryQuery{Since: testNow}), "second")
}

func TestSweep_catchesUpAfterARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	clk := &clock{t: testNow}
	o := Options{Logger: slog.New(newLogSink()), Now: clk.now, ThreadTTL: time.Hour}
	s, err := Open(path, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "bravo"} {
		r := registration(sess(name), name, envA(), "/workspace/"+name)
		r.TTL = time.Hour
		mustRegister(t, s, r)
	}
	r := registration(sess("carol"), "carol", envA(), "/workspace/carol")
	r.TTL = 24 * time.Hour
	carol := mustRegister(t, s, r)
	dm := say(t, s, "alpha", Target{Agent: "bravo"}, "while you were out")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	clk.set(testNow.Add(23*time.Hour + 40*time.Minute))
	s, err = Open(path, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Inbox(t.Context(), sess("bravo"), 0)
	wantRefusal(t, err, KindGone, "dormant", "your identity bravo expired 22 h ago with 0 unread")

	mustSweep(t, s)
	if _, found := findMessage(t, s, dm.Message); found {
		t.Error("the message outlived its expiry across the restart")
	}
	if _, found := findThread(t, s, dm.Thread); found {
		t.Error("the direct thread outlived its post across the restart")
	}
	for _, name := range []string{"alpha", "bravo"} {
		_, err := s.Inbox(t.Context(), sess(name), 0)
		wantRefusal(t, err, KindGone, "dormant")
	}
	p := mustInbox(t, s, sess("carol"), 0)
	if len(p.Entries) != 1 || p.Entries[0].Kind != kindIdentityExpiring ||
		p.Entries[0].Text != "your identity carol expires in 20 min (at 2026-11-05 05:39 UTC); if you are still working on moving the run surface, run `grelot renew --ttl 24h`, otherwise do nothing" {
		t.Errorf("carol's inbox %+v, want the one identity_expiring written by the sweep after the restart", p.Entries)
	}
	mustSweep(t, s)
	if ds := allDeliveriesTo(t, s, carol.ID); len(ds) != 1 {
		t.Errorf("%d deliveries to carol after a second sweep, want the warning once", len(ds))
	}
}
