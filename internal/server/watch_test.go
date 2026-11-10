//go:build !windows

package server

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

// watch holds a standing wait from session for d, as grelot watch sends
// it.
func (h *harness) watch(session string, d time.Duration) *response {
	h.t.Helper()
	return h.watchAsync(session, d).answer()
}

// watchAsync starts a standing wait from session in its own goroutine.
func (h *harness) watchAsync(session string, d time.Duration) *pending {
	return h.async(http.MethodPost, "/v1/events/wait", map[string]any{"wait": d.String()},
		map[string]string{headerSession: session, headerSelf: "1"})
}

// newestWake is the seq of the newest wake entry in session's inbox,
// fetched without an ack.
func (h *harness) newestWake(session string) float64 {
	h.t.Helper()
	r := h.as(session, http.MethodGet, "/v1/inbox", nil)
	entries, _ := r.body["entries"].([]any)
	var seq float64
	for _, e := range entries {
		if m, _ := e.(map[string]any); m["wake"] == true {
			seq, _ = m["seq"].(float64)
		}
	}
	if seq == 0 {
		h.t.Fatalf("no wake entry in %s's inbox: %s", session, r.raw)
	}
	return seq
}

// wantNudge fails unless r returned the nudge {seq, count, attempt}.
func wantNudge(t *testing.T, r *response, seq float64, count, attempt int) {
	t.Helper()
	if r.status != http.StatusOK || r.body["seq"] != seq || r.body["count"] != float64(count) || r.body["attempt"] != float64(attempt) {
		t.Errorf("wait = %d %s, want 200 with seq %v, count %d, attempt %d", r.status, r.raw, seq, count, attempt)
	}
}

func (h *harness) sentByWatcher() int {
	n := 0
	for _, r := range h.logs.lines(slog.LevelInfo, "nudge sent") {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "via" && a.Value.String() == string(store.NudgeByWatcher) {
				n++
			}
			return true
		})
	}
	return n
}

func TestWait_sendsTheDueNudgeAndReturnsIt(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "reader")
	h.mustRegisterAlone(sessB, "writer")
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "first")
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "second")
	seq := h.newestWake(sessA)

	wantNudge(t, h.watch(sessA, 55*time.Second), seq, 2, 1)
	if n := h.sentByWatcher(); n != 1 {
		t.Errorf("%d nudge sent lines via watcher, want 1", n)
	}
	st, _, err := h.store.Nudging(t.Context(), h.idOf(sessA))
	if err != nil {
		t.Fatal(err)
	}
	if want := h.clk.now().Add(3 * time.Minute); st.Due != nil || !st.Next.Equal(want) {
		t.Errorf("nudge state = %+v, want the send recorded: none due, its re-nudge at %v", st, want)
	}
}

func TestWait_aNewerWaitOfTheSessionEndsTheOlderWithReplaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		older := h.watchAsync(sessA, 55*time.Second)
		synctest.Wait()
		start := time.Now()
		newer := h.watchAsync(sessA, 55*time.Second)
		if r := older.answer(); r.status != http.StatusGone || r.str("code") != "replaced" || time.Since(start) != 0 {
			t.Errorf("the older wait = %d %s after %v, want 410 replaced at once", r.status, r.raw, time.Since(start))
		}
		synctest.Wait()
		if !h.srv.presence.online(h.idOf(sessA), time.Now()) {
			t.Error("offline while the newer wait holds")
		}
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		wantNudge(t, newer.answer(), h.newestWake(sessA), 1, 1)
		if n := h.sentByWatcher(); n != 1 {
			t.Errorf("%d nudge sent lines via watcher, want 1", n)
		}
	})
}

func TestWait_anotherSessionsWaitIsNotReplaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustRegisterAlone(sessC, "other")
		first := h.watchAsync(sessA, 55*time.Second)
		synctest.Wait()
		second := h.watchAsync(sessC, 55*time.Second)
		synctest.Wait()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		wantNudge(t, first.answer(), h.newestWake(sessA), 1, 1)
		h.mustPost(sessB, map[string]any{"agent": "other"}, "hello")
		wantNudge(t, second.answer(), h.newestWake(sessC), 1, 1)
	})
}

// sendAsWait records id's due nudge as a standing wait's send would, and
// returns it.
func (h *harness) sendAsWait(id uint64) nudgeJSON {
	h.t.Helper()
	st, _, err := h.store.Nudging(h.t.Context(), id)
	if err != nil || st.Due == nil {
		h.t.Fatalf("Nudging = %+v, %v; want a due nudge", st, err)
	}
	got, ok, err := h.store.RecordNudge(h.t.Context(), st.Due, store.NudgeByWatcher)
	if err != nil || !ok {
		h.t.Fatalf("RecordNudge: %v, %v", ok, err)
	}
	return nudgeJSON{Seq: got.Seq, Count: got.Count, Attempt: got.Attempt}
}

func TestWait_aNudgeTheReplacedWaitSentReachesTheNewest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		id := h.idOf(sessA)
		// The test is the older wait, whose look reached the send as the
		// newer one opened.
		older := h.srv.presence.opened(id, sessA)
		h.srv.presence.inTurnOpened(id)
		newer := h.watchAsync(sessA, 55*time.Second)
		synctest.Wait()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		seq := h.newestWake(sessA)
		// The older wait records the nudge while the in-turn hold still
		// defers it for the newer one: closing the hold first lets the
		// newer wait record it itself, and the hand-off has nothing to pass.
		n := h.sendAsWait(id)
		h.srv.presence.inTurnClosed(id, 0, time.Now())
		synctest.Wait()
		start := time.Now()
		if !h.srv.handOff(older, n) {
			t.Error("handOff answered the replaced wait as the newest")
		}
		wantNudge(t, newer.answer(), seq, 1, 1)
		if d := time.Since(start); d != 0 {
			t.Errorf("the newer wait returned %v after the hand-off, want at once", d)
		}
		h.srv.presence.closed(older, time.Now())
	})
}

func TestWait_aNudgeSentBeforeTheNewerWaitOpenedIsTheOlderWaitsAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		id := h.idOf(sessA)
		older := h.srv.presence.opened(id, sessA)
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		if h.srv.handOff(older, h.sendAsWait(id)) {
			t.Error("handOff answered the older wait as replaced before a newer one opened")
		}
		before := time.Now()
		if r := h.watch(sessA, 55*time.Second); r.status != http.StatusNoContent || time.Since(before) != 55*time.Second {
			t.Errorf("the newer wait = %d %s after %v, want 204 after the whole 55s: the older wait returns that nudge", r.status, r.raw, time.Since(before))
		}
		h.srv.presence.closed(older, time.Now())
	})
}

func TestWait_holdsUntilTheReNudgeIsDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		seq := h.newestWake(sessA)
		sent := time.Now()
		wantNudge(t, h.watch(sessA, 55*time.Second), seq, 1, 1)

		time.Sleep(150 * time.Second)
		wantNudge(t, h.watch(sessA, 55*time.Second), seq, 1, 2)
		if d := time.Since(sent); d != 3*time.Minute {
			t.Errorf("the re-nudge returned %v after the first send, want 3m", d)
		}
	})
}

func TestWait_noFirstNudgeWhileTheTurnIsOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		id := h.idOf(sessA)
		opened := time.Now()
		h.srv.presence.sawStop(id)
		h.srv.presence.turnSet(id, opened)
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		seq := h.newestWake(sessA)

		if r := h.watch(sessA, 55*time.Second); r.status != http.StatusNoContent {
			t.Errorf("a wait in an open turn = %d %s, want 204", r.status, r.raw)
		}
		time.Sleep(time.Until(opened.Add(turnLapse - 30*time.Second)))
		wantNudge(t, h.watch(sessA, 55*time.Second), seq, 1, 1)
		if d := time.Since(opened); d != turnLapse {
			t.Errorf("the first nudge went out %v after the turn opened, want at its 5m lapse", d)
		}
	})
}

func TestWait_noDeferralWithoutASeenStopHook(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "reader")
	h.mustRegisterAlone(sessB, "writer")
	h.srv.presence.turnSet(h.idOf(sessA), h.clk.now())
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
	wantNudge(t, h.watch(sessA, 55*time.Second), h.newestWake(sessA), 1, 1)
}

func TestWait_anInTurnWaitDefersOnlyTheFirstNudge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		id := h.idOf(sessA)
		h.srv.presence.inTurnOpened(id)
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		seq := h.newestWake(sessA)
		if r := h.watch(sessA, 55*time.Second); r.status != http.StatusNoContent {
			t.Fatalf("a wait beside an in-turn wait = %d %s, want 204", r.status, r.raw)
		}
		h.srv.presence.inTurnClosed(id, 0, time.Now())
		wantNudge(t, h.watch(sessA, 55*time.Second), seq, 1, 1)

		h.srv.presence.inTurnOpened(id)
		time.Sleep(3 * time.Minute)
		start := time.Now()
		wantNudge(t, h.watch(sessA, 55*time.Second), seq, 1, 2)
		if d := time.Since(start); d != 0 {
			t.Errorf("the re-nudge beside an in-turn wait waited %v, want none", d)
		}
	})
}

func TestWait_aHoldThatShowsTheMailLeavesTheWaitHolding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		id := h.idOf(sessA)
		h.srv.presence.sawStop(id)
		h.srv.presence.turnSet(id, time.Now())
		wait := h.watchAsync(sessA, 55*time.Second)
		synctest.Wait()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()
		h.srv.presence.turnClosed(id)
		if _, err := h.store.Hold(t.Context(), store.Caller{Session: sessA, Self: true}); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		if r := wait.answer(); r.status != http.StatusNoContent {
			t.Errorf("the wait after a hold showed the mail = %d %s, want 204", r.status, r.raw)
		}
		if n := h.sentByWatcher(); n != 0 {
			t.Errorf("%d nudge sent lines via watcher, want none", n)
		}
	})
}

func TestWait_aPushServedIdentitysWaitOnlyHoldsWhilePushIsOK(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		id := h.idOf(sessA)
		h.push.set(id, pushOK)
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		seq := h.newestWake(sessA)

		wait := h.watchAsync(sessA, 55*time.Second)
		synctest.Wait()
		st, _, err := h.store.Nudging(t.Context(), id)
		if err != nil || st.Due == nil {
			t.Fatalf("Nudging = %+v, %v; want a due nudge", st, err)
		}
		if _, ok, err := h.store.RecordNudge(t.Context(), st.Due, store.NudgeByPush); err != nil || !ok {
			t.Fatalf("RecordNudge by push: %v, %v", ok, err)
		}
		if r := wait.answer(); r.status != http.StatusNoContent {
			t.Errorf("the wait while push works = %d %s, want 204", r.status, r.raw)
		}
		if n := h.sentByWatcher(); n != 0 {
			t.Errorf("%d nudge sent lines via watcher while push works, want none", n)
		}

		for _, state := range []pushState{pushFailing, pushOff} {
			st, _, err := h.store.Nudging(t.Context(), id)
			if err != nil || st.Next.IsZero() {
				t.Fatalf("Nudging = %+v, %v; want a re-nudge pending", st, err)
			}
			time.Sleep(time.Until(st.Next))
			h.push.set(id, pushOK)
			wait := h.watchAsync(sessA, 55*time.Second)
			synctest.Wait()
			start := time.Now()
			h.push.set(id, state)
			h.srv.hub.Woke([]uint64{id})
			if r := wait.answer(); r.status != http.StatusOK || r.body["seq"] != seq {
				t.Errorf("the wait once push is %s = %d %s, want the nudge", state, r.status, r.raw)
			}
			if d := time.Since(start); d != 0 {
				t.Errorf("the wait once push is %s took %v, want at once", state, d)
			}
		}
	})
}

func TestWait_sessionEndedAnswersEndedAndLeavesItSet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		self := map[string]string{headerSession: sessA, headerSelf: "1"}
		wait := h.watchAsync(sessA, 55*time.Second)
		synctest.Wait()
		start := time.Now()
		if r := h.request(http.MethodPost, "/v1/lifecycle", map[string]any{"event": eventSessionEnd}, self); r.status != http.StatusOK {
			t.Fatalf("session_end: %d %s", r.status, r.raw)
		}
		ended := func() bool {
			id, err := h.store.Watcher(t.Context(), store.Caller{Session: sessA, Self: true})
			if err != nil {
				t.Fatalf("Watcher: %v", err)
			}
			return id.Ended
		}
		if r := wait.answer(); r.status != http.StatusGone || r.str("code") != "ended" || time.Since(start) != 0 {
			t.Errorf("the open wait at session_end = %d %s after %v, want 410 ended at once", r.status, r.raw, time.Since(start))
		}
		if r := h.watch(sessA, 55*time.Second); r.status != http.StatusGone || r.str("code") != "ended" {
			t.Errorf("a wait while ended = %d %s, want 410 ended", r.status, r.raw)
		}
		if !ended() {
			t.Fatal("the standing wait cleared ended")
		}
		h.request(http.MethodGet, "/v1/me", nil, self)
		if ended() {
			t.Fatal("GET /v1/me left ended set")
		}
		if r := h.watch(sessA, time.Second); r.status != http.StatusNoContent {
			t.Errorf("a wait after the session came back = %d %s, want 204", r.status, r.raw)
		}
	})
}

func TestWait_410ReasonWords(t *testing.T) {
	t.Run("ended, holding nothing", func(t *testing.T) {
		h := newHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.self(sessA, http.MethodPost, "/v1/lifecycle", map[string]any{"event": eventSessionEnd})
		if r := h.watch(sessA, 55*time.Second); r.status != http.StatusGone || r.str("code") != "ended" {
			t.Errorf("got %d %s, want 410 ended", r.status, r.raw)
		}
		if m := h.srv.presence.get(h.idOf(sessA)); m.held || m.open != 0 {
			t.Errorf("the refused wait left held %v, open %d; want neither", m.held, m.open)
		}
	})
	t.Run("dormant", func(t *testing.T) {
		h := newHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.clk.advance(7 * time.Hour)
		if r := h.watch(sessA, 55*time.Second); r.status != http.StatusGone || r.str("code") != "dormant" {
			t.Errorf("got %d %s, want 410 dormant", r.status, r.raw)
		}
	})
	t.Run("none after bye, at once for a wait already open", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newBubbleHarness(t)
			h.mustRegisterAlone(sessA, "reader")
			wait := h.watchAsync(sessA, 55*time.Second)
			synctest.Wait()
			start := time.Now()
			if r := h.self(sessA, http.MethodDelete, "/v1/me", nil); r.status != http.StatusOK {
				t.Fatalf("bye: %d %s", r.status, r.raw)
			}
			if r := wait.answer(); r.status != http.StatusGone || r.str("code") != "none" || time.Since(start) != 0 {
				t.Errorf("the open wait = %d %s after %v, want 410 none at once", r.status, r.raw, time.Since(start))
			}
			if r := h.watch(sessA, 55*time.Second); r.status != http.StatusGone || r.str("code") != "none" {
				t.Errorf("a new wait = %d %s, want 410 none", r.status, r.raw)
			}
		})
	})
	t.Run("none after a take-over, at once for a wait already open", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newBubbleHarness(t)
			h.mustRegister(sessA, "grelot-dev")
			wait := h.watchAsync(sessA, 55*time.Second)
			synctest.Wait()
			start := time.Now()
			body := registerBody("grelot-dev", "6h")
			body["take_over"] = true
			if r := h.as(sessB, http.MethodPost, "/v1/register", body); r.body["moved"] != true {
				t.Fatalf("take-over: %d %s", r.status, r.raw)
			}
			if r := wait.answer(); r.status != http.StatusGone || r.str("code") != "none" || time.Since(start) != 0 {
				t.Errorf("the open wait = %d %s after %v, want 410 none at once", r.status, r.raw, time.Since(start))
			}
			if r := h.watch(sessA, 55*time.Second); r.status != http.StatusGone || r.str("code") != "none" {
				t.Errorf("a new wait = %d %s, want 410 none", r.status, r.raw)
			}
		})
	})
}

func TestWait_refusals(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "parent")
	handle := h.grantDelegate(sessA, "review")
	t.Run("a delegate is 400", func(t *testing.T) {
		r := h.request(http.MethodPost, "/v1/events/wait", map[string]any{"wait": "55s"},
			map[string]string{headerSession: sessA, headerAs: handle})
		if r.status != http.StatusBadRequest {
			t.Errorf("got %d %s, want 400", r.status, r.raw)
		}
	})
	for _, wait := range []any{nil, "0s", "56s", "-1s"} {
		t.Run("wait "+fmtAny(wait), func(t *testing.T) {
			r := h.self(sessA, http.MethodPost, "/v1/events/wait", map[string]any{"wait": wait})
			if r.status != http.StatusBadRequest {
				t.Errorf("got %d %s, want 400", r.status, r.raw)
			}
		})
	}
}

func fmtAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "missing"
}

func TestWait_refreshesLastSeenAndPresence(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "reader")
	h.mustRegisterAlone(sessB, "writer")
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
	h.clk.advance(10 * time.Minute)
	r := h.watch(sessA, 55*time.Second)
	if r.status != http.StatusOK {
		t.Fatalf("wait: %d %s", r.status, r.raw)
	}
	if line := r.line(); line == "" || !strings.Contains(line, "wake: "+wakeWatcher) {
		t.Errorf("status line %q, want wake: %s", line, wakeWatcher)
	}
	who := h.request(http.MethodGet, "/v1/identities?q=reader", nil, nil)
	entries, _ := who.body["identities"].([]any)
	if len(entries) != 1 {
		t.Fatalf("who: %s", who.raw)
	}
	e, _ := entries[0].(map[string]any)
	if e["last_seen"] != h.clk.now().Format(time.RFC3339) || e["online"] != true {
		t.Errorf("reader = %v, want last_seen %s and online", e, h.clk.now().Format(time.RFC3339))
	}
}

func TestRelease_answersEveryHeldWaitAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		standing := h.watchAsync(sessA, 55*time.Second)
		inTurn := h.inTurn(sessA, 55*time.Second)
		synctest.Wait()
		start := time.Now()
		h.srv.Release()
		if r := standing.answer(); r.status != http.StatusServiceUnavailable || r.str("code") != "shutting_down" || time.Since(start) != 0 {
			t.Errorf("the held standing wait = %d %s after %v, want 503 shutting_down at once", r.status, r.raw, time.Since(start))
		}
		if r := inTurn.answer(); r.status != http.StatusOK || r.body["entries"] == nil || time.Since(start) != 0 {
			t.Errorf("the held in-turn wait = %d %s after %v, want 200 with the page at once", r.status, r.raw, time.Since(start))
		}
		if r := h.watch(sessA, 55*time.Second); r.status != http.StatusServiceUnavailable || time.Since(start) != 0 {
			t.Errorf("a wait opened after Release = %d %s after %v, want 503 at once", r.status, r.raw, time.Since(start))
		}
		if got := h.logs.lines(slog.LevelError, "request failed"); len(got) != 0 {
			t.Errorf("request failed lines: %d, want none", len(got))
		}
	})
}
