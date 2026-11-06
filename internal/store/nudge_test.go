//go:build !windows

package store

import (
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// nudgeOf is identity id's nudge state, failing the test when id has no
// live identity.
func nudgeOf(t *testing.T, s *Store, id uint64) NudgeState {
	t.Helper()
	st, found, err := s.Nudging(t.Context(), id)
	if err != nil {
		t.Fatalf("Nudging(%d): %v", id, err)
	}
	if !found {
		t.Fatalf("Nudging(%d): no live identity", id)
	}
	return st
}

// mustSend records the send due for id.
func mustSend(t *testing.T, s *Store, id uint64) {
	t.Helper()
	st := nudgeOf(t, s, id)
	if st.Due == nil {
		t.Fatalf("identity %d: no nudge due, state %+v", id, st)
	}
	if _, ok, err := s.RecordNudge(t.Context(), st.Due, NudgeByWatcher); err != nil || !ok {
		t.Fatalf("RecordNudge(%+v): recorded %v, %v", *st.Due, ok, err)
	}
}

// directTo posts a direct message from one name to another and returns
// the recipient's seq for it.
func directTo(t *testing.T, s *Store, from, to string, toID uint64) uint64 {
	t.Helper()
	mustPost(t, s, posting(sess(from), Target{Agent: to}))
	ds := deliveriesTo(t, s, toID)
	return ds[len(ds)-1].Seq
}

func wantNoNudge(t *testing.T, st NudgeState) {
	t.Helper()
	if st.Due != nil {
		t.Errorf("due %+v, want no nudge", *st.Due)
	}
}

func wantDue(t *testing.T, st NudgeState, attempt int, seq uint64) {
	t.Helper()
	if st.Due == nil {
		t.Fatalf("no nudge due, want attempt %d covering %d", attempt, seq)
	}
	if st.Due.Attempt != attempt || st.Due.Seq != seq {
		t.Errorf("due attempt %d seq %d, want attempt %d seq %d", st.Due.Attempt, st.Due.Seq, attempt, seq)
	}
}

// sentNudge is an identity's stored outstanding nudge.
type sentNudge struct {
	at      time.Time
	seq     uint64
	attempt int
}

// outstanding is id's outstanding nudge; ok is false when read has passed
// the last one sent, or none was.
func outstanding(t *testing.T, s *Store, id uint64) (n sentNudge, ok bool) {
	t.Helper()
	row := getIdentity(t, s, id)
	return sentNudge{at: row.NudgedAt, seq: row.Nudged, attempt: row.Nudges}, row.Read < row.Nudged
}

func TestNudge_firstNudgeCoversTheNewestWakeSeq(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	regAt(t, s, "charlie") // writes bravo a join card, which is quiet
	t.Run("a card alone nudges nothing", func(t *testing.T) {
		st := nudgeOf(t, s, bravo.ID)
		wantNoNudge(t, st)
		if n, ok := outstanding(t, s, bravo.ID); ok || !st.Next.IsZero() {
			t.Errorf("outstanding %+v next %v, want neither", n, st.Next)
		}
	})
	directTo(t, s, "alpha", "bravo", bravo.ID)
	second := directTo(t, s, "charlie", "bravo", bravo.ID)
	st := nudgeOf(t, s, bravo.ID)
	t.Run("the first send covers the newest", func(t *testing.T) {
		wantDue(t, st, 1, second)
	})
	t.Run("it counts the wake mail past read, not the card", func(t *testing.T) {
		if st.Due != nil && (st.Due.Count != 2 || st.Due.Read != 0 || st.Due.Identity != bravo.ID) {
			t.Errorf("due count %d read %d identity %d, want 2, 0, %d", st.Due.Count, st.Due.Read, st.Due.Identity, bravo.ID)
		}
	})
	t.Run("nothing is outstanding yet", func(t *testing.T) {
		if n, ok := outstanding(t, s, bravo.ID); ok {
			t.Errorf("outstanding %+v, want none", n)
		}
	})
}

func TestNudge_nothingNewGoesOutWhileOneIsOutstanding(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	first := directTo(t, s, "alpha", "bravo", bravo.ID)
	mustSend(t, s, bravo.ID)
	clk.advance(time.Minute)
	directTo(t, s, "alpha", "bravo", bravo.ID)
	st := nudgeOf(t, s, bravo.ID)
	wantNoNudge(t, st)
	t.Run("the outstanding nudge is the one sent", func(t *testing.T) {
		if n, ok := outstanding(t, s, bravo.ID); !ok || n.seq != first || n.attempt != 1 || !n.at.Equal(testNow) {
			t.Errorf("outstanding %+v %v, want seq %d attempt 1 at %v", n, ok, first, testNow)
		}
	})
	t.Run("the re-nudge is due 3 minutes after the send", func(t *testing.T) {
		if want := testNow.Add(3 * time.Minute); !st.Next.Equal(want) {
			t.Errorf("next %v, want %v", st.Next, want)
		}
	})
}

func TestNudge_reNudgesAt3_15And60MinutesCoveringTheNewestThenStops(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	directTo(t, s, "alpha", "bravo", bravo.ID)
	mustSend(t, s, bravo.ID)
	for i, gap := range []time.Duration{3 * time.Minute, 15 * time.Minute, time.Hour} {
		attempt := i + 2
		newest := directTo(t, s, "alpha", "bravo", bravo.ID)
		sentAt := clk.now()
		clk.advance(gap - time.Second)
		st := nudgeOf(t, s, bravo.ID)
		wantNoNudge(t, st)
		if want := sentAt.Add(gap); !st.Next.Equal(want) {
			t.Errorf("before attempt %d: next %v, want %v", attempt, st.Next, want)
		}
		clk.advance(time.Second)
		wantDue(t, nudgeOf(t, s, bravo.ID), attempt, newest)
		mustSend(t, s, bravo.ID)
	}
	t.Run("nothing after the fourth until the read passes nudged", func(t *testing.T) {
		directTo(t, s, "alpha", "bravo", bravo.ID)
		clk.advance(4 * time.Hour)
		st := nudgeOf(t, s, bravo.ID)
		wantNoNudge(t, st)
		if n, ok := outstanding(t, s, bravo.ID); !st.Next.IsZero() || !ok || n.attempt != 4 {
			t.Errorf("next %v outstanding %+v %v, want no re-nudge after attempt 4", st.Next, n, ok)
		}
	})
	t.Run("a read past nudged ends it, and new mail starts at one", func(t *testing.T) {
		readAll(t, s, "bravo")
		st := nudgeOf(t, s, bravo.ID)
		if n, ok := outstanding(t, s, bravo.ID); st.Due != nil || ok {
			t.Errorf("after the read: due %v outstanding %+v, want neither", st.Due, n)
		}
		newest := directTo(t, s, "alpha", "bravo", bravo.ID)
		wantDue(t, nudgeOf(t, s, bravo.ID), 1, newest)
	})
}

func TestNudge_aReadPastNudgedStopsTheReNudge(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	seq := directTo(t, s, "alpha", "bravo", bravo.ID)
	mustSend(t, s, bravo.ID)
	mustAck(t, s, sess("bravo"), seq)
	clk.advance(time.Hour)
	st := nudgeOf(t, s, bravo.ID)
	if n, ok := outstanding(t, s, bravo.ID); st.Due != nil || ok || !st.Next.IsZero() {
		t.Errorf("due %v outstanding %+v next %v, want nothing once read passed nudged", st.Due, n, st.Next)
	}
}

func TestNudge_aReadShortOfNudgedKeepsItOutstanding(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	first := directTo(t, s, "alpha", "bravo", bravo.ID)
	second := directTo(t, s, "alpha", "bravo", bravo.ID)
	mustSend(t, s, bravo.ID)
	mustAck(t, s, sess("bravo"), first)
	clk.advance(3 * time.Minute)
	st := nudgeOf(t, s, bravo.ID)
	wantDue(t, st, 2, second)
	if st.Due != nil && (st.Due.Read != first || st.Due.Count != 1) {
		t.Errorf("due read %d count %d, want read %d count 1", st.Due.Read, st.Due.Count, first)
	}
}

func TestNudge_identityExpiringTakesPartInAFirstNudgeButIsNeverReNudged(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	r := registration(sess("bravo"), "bravo", envA())
	r.TTL = time.Hour
	bravo := mustRegister(t, s, r)
	clk.advance(30 * time.Minute)
	mustSweep(t, s)
	ds := deliveriesTo(t, s, bravo.ID)
	if len(ds) != 1 || ds[0].Kind != kindIdentityExpiring {
		t.Fatalf("deliveries %+v, want the identity_expiring warning", ds)
	}
	t.Run("the warning gets a first nudge", func(t *testing.T) {
		wantDue(t, nudgeOf(t, s, bravo.ID), 1, ds[0].Seq)
	})
	mustSend(t, s, bravo.ID)
	clk.advance(3 * time.Minute)
	t.Run("the warning alone is never re-nudged", func(t *testing.T) {
		st := nudgeOf(t, s, bravo.ID)
		wantNoNudge(t, st)
		if !st.Next.IsZero() {
			t.Errorf("next %v, want none", st.Next)
		}
	})
	t.Run("other unread wake mail is re-nudged, covering the newest", func(t *testing.T) {
		seq := directTo(t, s, "alpha", "bravo", bravo.ID)
		wantDue(t, nudgeOf(t, s, bravo.ID), 2, seq)
	})
}

func TestNudge_aClosedSessionIsNotNudged(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	seq := directTo(t, s, "alpha", "bravo", bravo.ID)
	if err := s.EndSession(t.Context(), sess("bravo")); err != nil {
		t.Fatal(err)
	}
	t.Run("no first nudge", func(t *testing.T) {
		wantNoNudge(t, nudgeOf(t, s, bravo.ID))
	})
	t.Run("a request from its session lets the due nudge go out", func(t *testing.T) {
		if _, err := s.Seen(t.Context(), plain("bravo")); err != nil {
			t.Fatal(err)
		}
		wantDue(t, nudgeOf(t, s, bravo.ID), 1, seq)
	})
	t.Run("no re-nudge", func(t *testing.T) {
		mustSend(t, s, bravo.ID)
		if err := s.EndSession(t.Context(), sess("bravo")); err != nil {
			t.Fatal(err)
		}
		clk.advance(30 * time.Minute)
		wantNoNudge(t, nudgeOf(t, s, bravo.ID))
	})
}

func TestNudge_aDormantIdentityIsNotNudged(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	old := expireToDormant(t, s, clk, "subflux-dev")
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "subflux-dev"}))
	if n := len(deliveriesTo(t, s, old.ID)); n != 1 {
		t.Fatalf("%d deliveries to the dormant identity, want the direct message", n)
	}
	st, found, err := s.Nudging(t.Context(), old.ID)
	if err != nil || found || st.Due != nil {
		t.Errorf("Nudging = %+v, found %v, %v; want no live identity", st, found, err)
	}
	t.Run("nor before the sweep reaches it", func(t *testing.T) {
		r := registration(sess("bravo"), "bravo", envA())
		r.TTL = 15 * time.Minute
		bravo := mustRegister(t, s, r)
		directTo(t, s, "alpha", "bravo", bravo.ID)
		clk.advance(15 * time.Minute)
		if _, found, err := s.Nudging(t.Context(), bravo.ID); err != nil || found {
			t.Errorf("found %v, %v; want no live identity past its expiry", found, err)
		}
	})
}

func TestNudge_aDelegateIsNeverNudged(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	_, d := delegateOf(t, s, "parent", "review")
	p := posting(sess("parent"), Target{Agent: d.Name})
	p.Caller.Self = true
	mustPost(t, s, p)
	if n := len(deliveriesTo(t, s, d.ID)); n != 1 {
		t.Fatalf("%d deliveries to the delegate, want 1", n)
	}
	wantNoNudge(t, nudgeOf(t, s, d.ID))
	t.Run("nor does its hold record one", func(t *testing.T) {
		if _, err := s.Hold(t.Context(), as(sess("parent"), d.Name)); err != nil {
			t.Fatal(err)
		}
		if row := getIdentity(t, s, d.ID); row.Nudged != 0 || row.Nudges != 0 {
			t.Errorf("delegate nudged %d nudges %d, want none", row.Nudged, row.Nudges)
		}
	})
}

func TestRecordNudge_ofTwoRacersOnlyOneRecords(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	seq := directTo(t, s, "alpha", "bravo", bravo.ID)
	due := *nudgeOf(t, s, bravo.ID).Due
	t.Run("in turn", func(t *testing.T) {
		got, ok, err := s.RecordNudge(t.Context(), &due, NudgeByPush)
		if err != nil || !ok || got.Seq != seq || got.Attempt != 1 {
			t.Fatalf("first RecordNudge = %+v, %v, %v; want seq %d attempt 1", got, ok, err, seq)
		}
		if n, _ := outstanding(t, s, bravo.ID); !n.at.Equal(testNow) {
			t.Errorf("recorded at %v, want %v", n.at, testNow)
		}
		if _, ok, err := s.RecordNudge(t.Context(), &due, NudgeByWatcher); err != nil || ok {
			t.Errorf("second RecordNudge recorded %v, %v; want nothing", ok, err)
		}
		row := getIdentity(t, s, bravo.ID)
		if row.Nudged != seq || row.Nudges != 1 || !row.NudgedAt.Equal(testNow) {
			t.Errorf("row nudged %d nudges %d at %v, want %d 1 %v", row.Nudged, row.Nudges, row.NudgedAt, seq, testNow)
		}
	})
	t.Run("at once", func(t *testing.T) {
		carol := regAt(t, s, "carol")
		directTo(t, s, "alpha", "carol", carol.ID)
		due := *nudgeOf(t, s, carol.ID).Due
		var wg sync.WaitGroup
		var mu sync.Mutex
		recorded := 0
		for range 8 {
			wg.Go(func() {
				_, ok, err := s.RecordNudge(t.Context(), &due, NudgeByWatcher)
				if err != nil {
					t.Error(err)
				}
				if ok {
					mu.Lock()
					recorded++
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		if recorded != 1 {
			t.Errorf("%d racers recorded, want 1", recorded)
		}
	})
}

func TestRecordNudge_refusesASendThatIsNoLongerDue(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	first := directTo(t, s, "alpha", "bravo", bravo.ID)
	directTo(t, s, "alpha", "bravo", bravo.ID)
	mustSend(t, s, bravo.ID)
	clk.advance(3 * time.Minute)
	stale := *nudgeOf(t, s, bravo.ID).Due
	mustAck(t, s, sess("bravo"), first)
	if _, ok, err := s.RecordNudge(t.Context(), &stale, NudgeByPush); err != nil || ok {
		t.Errorf("RecordNudge of a send computed before the read moved: recorded %v, %v; want nothing", ok, err)
	}
	if row := getIdentity(t, s, bravo.ID); row.Nudges != 1 {
		t.Errorf("nudges %d, want 1", row.Nudges)
	}
}

func TestRecordNudge_refusesASendALaterAttemptOvertook(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	directTo(t, s, "alpha", "bravo", bravo.ID)
	stale := *nudgeOf(t, s, bravo.ID).Due
	mustSend(t, s, bravo.ID)
	clk.advance(3 * time.Minute)
	if _, ok, err := s.RecordNudge(t.Context(), &stale, NudgeByPush); err != nil || ok {
		t.Errorf("RecordNudge of attempt 1 while attempt 2 is due: recorded %v, %v; want nothing", ok, err)
	}
	if row := getIdentity(t, s, bravo.ID); row.Nudges != 1 || !row.NudgedAt.Equal(testNow) {
		t.Errorf("nudges %d at %v, want the first send's 1 at %v", row.Nudges, row.NudgedAt, testNow)
	}
}

func TestRecordNudge_logsTheSend(t *testing.T) {
	s, clk, sink := openIdentityStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	seq := directTo(t, s, "alpha", "bravo", bravo.ID)
	clk.advance(1500 * time.Millisecond)
	mustSend(t, s, bravo.ID)
	lines := sink.lines(slog.LevelInfo, msgNudgeSent)
	if len(lines) != 1 {
		t.Fatalf("%d nudge sent lines, want 1", len(lines))
	}
	for k, want := range map[string]string{"via": "watcher", "attempt": "1", "seq": strconv.FormatUint(seq, 10), "oldest_delay_ms": "1500"} {
		if got := attr(lines[0], k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestHold_recordsWhatItShowsAsTheNudge(t *testing.T) {
	s, clk, sink := openIdentityStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	regAt(t, s, "charlie")
	t.Run("a card alone records nothing", func(t *testing.T) {
		clk.advance(lastSeenEvery) // so the hold writes its row
		if _, err := s.Hold(t.Context(), plain("bravo")); err != nil {
			t.Fatal(err)
		}
		if row := getIdentity(t, s, bravo.ID); row.Nudges != 0 {
			t.Errorf("nudges %d after a hold showing only a card, want 0", row.Nudges)
		}
	})
	directTo(t, s, "alpha", "bravo", bravo.ID)
	seq := directTo(t, s, "charlie", "bravo", bravo.ID)
	heldAt := clk.now()
	if _, err := s.Hold(t.Context(), plain("bravo")); err != nil {
		t.Fatal(err)
	}
	t.Run("it records the newest wake seq shown as the first send", func(t *testing.T) {
		row := getIdentity(t, s, bravo.ID)
		if row.Nudged != seq || row.Nudges != 1 || !row.NudgedAt.Equal(heldAt) {
			t.Errorf("nudged %d nudges %d at %v, want %d 1 %v", row.Nudged, row.Nudges, row.NudgedAt, seq, heldAt)
		}
		lines := sink.lines(slog.LevelInfo, msgNudgeSent)
		if len(lines) != 1 || attr(lines[0], "via") != "hook" || attr(lines[0], "attempt") != "1" {
			t.Errorf("nudge sent lines %v, want one via hook attempt 1", lines)
		}
	})
	t.Run("so neither push nor a watcher sends it", func(t *testing.T) {
		wantNoNudge(t, nudgeOf(t, s, bravo.ID))
	})
	t.Run("an outstanding nudge is left alone", func(t *testing.T) {
		clk.advance(time.Minute)
		directTo(t, s, "alpha", "bravo", bravo.ID)
		if _, err := s.Hold(t.Context(), plain("bravo")); err != nil {
			t.Fatal(err)
		}
		if row := getIdentity(t, s, bravo.ID); row.Nudged != seq || row.Nudges != 1 || !row.NudgedAt.Equal(heldAt) {
			t.Errorf("nudged %d nudges %d at %v, want the hook's send unchanged", row.Nudged, row.Nudges, row.NudgedAt)
		}
	})
	t.Run("the re-nudge covers an agent that saw the counts and did not read", func(t *testing.T) {
		clk.set(heldAt.Add(3 * time.Minute))
		ds := deliveriesTo(t, s, bravo.ID)
		wantDue(t, nudgeOf(t, s, bravo.ID), 2, ds[len(ds)-1].Seq)
	})
}

func TestNudge_theStateOutlivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	clk := &clock{t: testNow}
	open := func() *Store {
		s, err := Open(path, Options{Now: clk.now})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return s
	}
	s := open()
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	seq := directTo(t, s, "alpha", "bravo", bravo.ID)
	mustSend(t, s, bravo.ID)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open()
	t.Cleanup(func() { _ = s.Close() })
	t.Run("the send is not repeated", func(t *testing.T) {
		st := nudgeOf(t, s, bravo.ID)
		wantNoNudge(t, st)
		if n, ok := outstanding(t, s, bravo.ID); !ok || n.seq != seq || !st.Next.Equal(testNow.Add(3*time.Minute)) {
			t.Errorf("outstanding %+v %v next %v, want the send kept and its re-nudge due at %v", n, ok, st.Next, testNow.Add(3*time.Minute))
		}
	})
	t.Run("the due re-nudge is not forgotten", func(t *testing.T) {
		clk.advance(3 * time.Minute)
		wantDue(t, nudgeOf(t, s, bravo.ID), 2, seq)
	})
}

func TestNudge_aRevivalGivesUnreadWakeMailAFirstNudge(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	old := expireToDormant(t, s, clk, "subflux-dev")
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "subflux-dev"}))
	editIdentity(t, s, old.ID, func(row *identityRow) {
		row.Nudged, row.Nudges, row.NudgedAt = row.Seq, 4, testNow
	})
	mustRevive(t, s, revival(sessA, "subflux-dev", envA()))
	ds := deliveriesTo(t, s, old.ID)
	wantDue(t, nudgeOf(t, s, old.ID), 1, ds[len(ds)-1].Seq)
}
