//go:build !windows

package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

// stopHold sends the stop hook's hold from session, with last when set.
func (h *harness) stopHold(session string, last bool) *response {
	h.t.Helper()
	body := map[string]any{"for": "stop"}
	if last {
		body["last"] = true
	}
	r := h.self(session, http.MethodPost, "/v1/events/hold", body)
	if r.status != http.StatusOK {
		h.t.Fatalf("hold: %d %s", r.status, r.raw)
	}
	return r
}

// prompt sends the prompt hook's GET /v1/me with query.
func (h *harness) prompt(session, query string) *response {
	h.t.Helper()
	r := h.self(session, http.MethodGet, "/v1/me?"+query, nil)
	if r.status != http.StatusOK {
		h.t.Fatalf("prompt %s: %d %s", query, r.status, r.raw)
	}
	return r
}

// ack marks session's inbox read up to seq.
func (h *harness) ack(session string, seq float64) {
	h.t.Helper()
	if r := h.as(session, http.MethodPost, "/v1/inbox/ack", map[string]any{"upto": seq}); r.status != http.StatusOK {
		h.t.Fatalf("ack: %d %s", r.status, r.raw)
	}
}

func countOf(r *response, kind string) float64 {
	c, _ := r.body["counts"].(map[string]any)
	n, _ := c[kind].(float64)
	return n
}

func (h *harness) nudgesVia(via string) int {
	n := 0
	for _, r := range h.logs.lines(slog.LevelInfo, "nudge sent") {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "via" && a.Value.String() == via {
				n++
			}
			return true
		})
	}
	return n
}

func TestHold_showsEachWakeEventOnceAndAdvancesShown(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "reader")
	h.mustRegisterAlone(sessB, "writer")
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "first")
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "second")
	newest := h.newestWake(sessA)

	r := h.stopHold(sessA, false)
	if r.body["block"] != true || countOf(r, "direct") != 2 || countOf(r, "wake") != 2 || r.body["shown"] != newest {
		t.Errorf("first hold = %s, want a block for 2 direct, shown %v", r.raw, newest)
	}
	if lines := h.logs.lines(slog.LevelInfo, "hook shown"); len(lines) != 1 {
		t.Errorf("%d hook shown lines, want 1", len(lines))
	}
	if again := h.stopHold(sessA, false); again.body["block"] != false || countOf(again, "total") != 0 {
		t.Errorf("a second hold with no new wake event = %s, want no block and nothing counted", again.raw)
	}
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "third")
	if third := h.stopHold(sessA, false); third.body["block"] != true || countOf(third, "wake") != 1 {
		t.Errorf("a hold after new mail = %s, want a block for the one new post", third.raw)
	}
	if left := entriesOf(t, h.as(sessA, http.MethodGet, "/v1/inbox", nil)); len(left) != 3 {
		t.Errorf("the inbox after the holds has %d entries, want all 3 still unread", len(left))
	}
}

func TestHold_cardsAloneHoldNothing(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "reader")
	h.mustRegister(sessB, "joiner")
	r := h.stopHold(sessA, false)
	if countOf(r, "cards") == 0 {
		t.Fatalf("hold = %s, want the join card counted", r.raw)
	}
	if r.body["block"] != false {
		t.Errorf("a hold with only a card = %s, want no block", r.raw)
	}
	if lines := h.logs.lines(slog.LevelInfo, "hook shown"); len(lines) != 0 {
		t.Errorf("%d hook shown lines for a card, want none", len(lines))
	}
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
	if mail := h.stopHold(sessA, false); mail.body["block"] != true || countOf(mail, "cards") != 1 || countOf(mail, "direct") != 1 {
		t.Errorf("the next hold with mail = %s, want a block counting the post and the card", mail.raw)
	}
}

func TestHold_thatShowsMailRecordsTheNudgeAndNoWatcherLineFollows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.stopHold(sessA, false)
		h.prompt(sessA, "turn=start")
		wait := h.watchAsync(sessA, 55*time.Second)
		synctest.Wait()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		if r := h.stopHold(sessA, true); r.body["block"] != true {
			t.Fatalf("hold = %s, want a block", r.raw)
		}
		if r := wait.answer(); r.status != http.StatusNoContent {
			t.Errorf("the standing wait = %d %s, want 204 with no nudge", r.status, r.raw)
		}
		if hook, watcher := h.nudgesVia("hook"), h.sentByWatcher(); hook != 1 || watcher != 0 {
			t.Errorf("nudge sent lines: %d via hook, %d via watcher; want 1 and 0", hook, watcher)
		}
	})
}

func TestHold_closesTheTurnUnlessItBlocksWithoutLast(t *testing.T) {
	cases := []struct {
		name     string
		mail     bool
		last     bool
		deferred bool
	}{
		{name: "a hold that does not block", mail: false, last: false, deferred: false},
		{name: "a blocking hold", mail: true, last: false, deferred: true},
		{name: "a blocking last hold", mail: true, last: true, deferred: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newBubbleHarness(t)
				h.mustRegisterAlone(sessA, "reader")
				h.mustRegisterAlone(sessB, "writer")
				h.stopHold(sessA, false)
				h.prompt(sessA, "turn=start")
				if tc.mail {
					h.mustPost(sessB, map[string]any{"agent": "reader"}, "shown at stop")
				}
				if r := h.stopHold(sessA, tc.last); r.body["block"] != tc.mail {
					t.Fatalf("hold = %s, want block %v", r.raw, tc.mail)
				}
				if tc.mail {
					h.ack(sessA, h.newestWake(sessA))
				}
				h.mustPost(sessB, map[string]any{"agent": "reader"}, "after the stop")
				r := h.watch(sessA, 55*time.Second)
				if got := r.status == http.StatusNoContent; got != tc.deferred {
					t.Errorf("a standing wait for mail after the hold = %d %s, want deferred %v", r.status, r.raw, tc.deferred)
				}
			})
		})
	}
}

func TestHold_theDownNoticeIsToldOncePerEpisodeWithoutBlocking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		id := h.idOf(sessA)
		if r := h.stopHold(sessA, false); r.body["down"] != false || r.body["wake"] != wakeDown {
			t.Errorf("a hold before any standing wait = %s, want wake down and no down notice", r.raw)
		}
		h.watch(sessA, time.Second)
		time.Sleep(onlineFor)
		h.prompt(sessA, "turn=start")
		if r := h.stopHold(sessA, false); r.body["down"] != true || r.body["block"] != false {
			t.Errorf("the first hold once the watcher is gone = %s, want the down notice and no block", r.raw)
		}
		if _, open := h.srv.presence.turnOpen(id, time.Now()); open {
			t.Error("the turn is still open after the hold that told the down notice")
		}
		if r := h.stopHold(sessA, false); r.body["down"] != false || r.body["block"] != false {
			t.Errorf("a second hold in the same down episode = %s, want neither down nor block", r.raw)
		}
		h.watch(sessA, time.Second)
		if r := h.stopHold(sessA, false); r.body["down"] != false || r.body["wake"] != wakeWatcher {
			t.Errorf("a hold while the watcher is online = %s, want wake watcher and no down notice", r.raw)
		}
		time.Sleep(onlineFor)
		if r := h.stopHold(sessA, false); r.body["down"] != true || r.body["block"] != false {
			t.Errorf("the first hold of the next down episode = %s, want the down notice and no block", r.raw)
		}
	})
}

func TestHold_refusals(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "reader")
	for _, body := range []map[string]any{{}, {"for": "tool"}, {"for": "STOP"}} {
		t.Run(fmt.Sprint(body), func(t *testing.T) {
			if r := h.self(sessA, http.MethodPost, "/v1/events/hold", body); r.status != http.StatusBadRequest {
				t.Errorf("got %d %s, want 400", r.status, r.raw)
			}
		})
	}
}

func TestPrompt_signalOpensNoTurnOnceReadReachedIt(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "reader")
	h.mustRegisterAlone(sessB, "writer")
	id := h.idOf(sessA)
	h.stopHold(sessA, false)
	for i := range 5 {
		h.mustPost(sessB, map[string]any{"agent": "reader"}, fmt.Sprintf("post %d", i+1))
	}
	page := entriesOf(t, h.as(sessA, http.MethodGet, "/v1/inbox", nil))
	fifth, fourth := page[4]["seq"].(float64), page[3]["seq"].(float64)
	signal := fmt.Sprintf("turn=start&signal=%d", int(fifth))

	h.ack(sessA, fourth)
	h.prompt(sessA, signal)
	if _, open := h.srv.presence.turnOpen(id, h.clk.now()); !open {
		t.Error("no turn opened for a signal whose seq is still unread")
	}
	h.srv.presence.turnClosed(id)
	h.ack(sessA, fifth)
	h.prompt(sessA, signal)
	if _, open := h.srv.presence.turnOpen(id, h.clk.now()); open {
		t.Error("a turn opened for a signal read has reached")
	}
	h.prompt(sessA, "turn=start")
	if _, open := h.srv.presence.turnOpen(id, h.clk.now()); !open {
		t.Error("no turn opened for a prompt that is no signal")
	}
	h.srv.presence.turnClosed(id)
	h.prompt(sessA, "")
	if _, open := h.srv.presence.turnOpen(id, h.clk.now()); open {
		t.Error("a turn opened by GET /v1/me with no turn parameter")
	}
}

func TestPrompt_signalReadSaysWhetherReadReachedTheSignal(t *testing.T) {
	t.Run("read at the signal with newer wake mail unread", func(t *testing.T) {
		h := newHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.stopHold(sessA, false)
		for i := range 6 {
			h.mustPost(sessB, map[string]any{"agent": "reader"}, fmt.Sprintf("post %d", i+1))
		}
		page := entriesOf(t, h.as(sessA, http.MethodGet, "/v1/inbox", nil))
		fifth := page[4]["seq"].(float64)
		h.ack(sessA, fifth)
		r := h.prompt(sessA, fmt.Sprintf("turn=start&signal=%d", int(fifth)))
		if r.body["signal_read"] != true {
			t.Errorf("GET /v1/me = %s, want signal_read true", r.raw)
		}
		if _, open := h.srv.presence.turnOpen(h.idOf(sessA), h.clk.now()); open {
			t.Error("a turn opened for a signal read has reached")
		}
	})
	t.Run("read below the signal with only quiet deliveries past it", func(t *testing.T) {
		h := newHarness(t)
		h.mustRegister(sessA, "reader")
		h.stopHold(sessA, false)
		h.mustRegister(sessB, "writer")
		page := entriesOf(t, h.as(sessA, http.MethodGet, "/v1/inbox", nil))
		if len(page) != 1 || page[0]["wake"] != false {
			t.Fatalf("inbox = %v, want the writer's quiet join card alone", page)
		}
		r := h.prompt(sessA, fmt.Sprintf("turn=start&signal=%d", int(page[0]["seq"].(float64))))
		if v, set := r.body["signal_read"]; set && v != false {
			t.Errorf("GET /v1/me = %s, want signal_read absent or false", r.raw)
		}
		if _, open := h.srv.presence.turnOpen(h.idOf(sessA), h.clk.now()); !open {
			t.Error("no turn opened for a signal whose seq is still unread")
		}
	})
	t.Run("no signal", func(t *testing.T) {
		h := newHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		for _, q := range []string{"turn=start", ""} {
			if v, set := h.prompt(sessA, q).body["signal_read"]; set && v != false {
				t.Errorf("GET /v1/me?%s answers signal_read %v, want it absent or false", q, v)
			}
		}
	})
}

func TestPrompt_refusals(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "reader")
	for _, q := range []string{"turn=end", "turn=start&signal=x", "turn=start&signal=0", "signal=5"} {
		t.Run(q, func(t *testing.T) {
			if r := h.self(sessA, http.MethodGet, "/v1/me?"+q, nil); r.status != http.StatusBadRequest {
				t.Errorf("got %d %s, want 400", r.status, r.raw)
			}
		})
	}
}

func TestPrompt_anUnreportedTurnEndWaitsForTheLapse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.stopHold(sessA, false)
		opened := time.Now()
		h.prompt(sessA, "turn=start")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		seq := h.newestWake(sessA)
		if r := h.watch(sessA, 55*time.Second); r.status != http.StatusNoContent {
			t.Errorf("a standing wait in the open turn = %d %s, want 204", r.status, r.raw)
		}
		time.Sleep(time.Until(opened.Add(turnLapse - 30*time.Second)))
		wantNudge(t, h.watch(sessA, 55*time.Second), seq, 1, 1)
		if d := time.Since(opened); d != turnLapse {
			t.Errorf("the first nudge went out %v after the prompt, want at the 5m lapse", d)
		}
	})
}

func TestLifecycle_turnEndClosesTheTurnAndTheDueNudgeGoesOutAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.stopHold(sessA, false)
		h.prompt(sessA, "turn=start")
		wait := h.watchAsync(sessA, 55*time.Second)
		synctest.Wait()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		seq := h.newestWake(sessA)
		h.self(sessA, http.MethodPost, "/v1/lifecycle", map[string]any{"event": eventTurnEnd, "agent_id": "sub-1"})
		synctest.Wait()
		if r := wait.now(); r != nil {
			t.Fatalf("the wait returned %d %s after a subagent's turn_end, want it still deferred", r.status, r.raw)
		}
		start := time.Now()
		if r := h.self(sessA, http.MethodPost, "/v1/lifecycle", map[string]any{"event": eventTurnEnd}); r.status != http.StatusOK {
			t.Fatalf("turn_end: %d %s", r.status, r.raw)
		}
		wantNudge(t, wait.answer(), seq, 1, 1)
		if d := time.Since(start); d != 0 {
			t.Errorf("the nudge went out %v after turn_end, want at once", d)
		}
	})
}

func TestLifecycle_turnEndInsideTheAckGraceSendsTheNudgeAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.stopHold(sessA, false)
		h.prompt(sessA, "turn=start")
		watch := h.watchAsync(sessA, 55*time.Second)
		wait := h.inTurn(sessA, 55*time.Second)
		synctest.Wait()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "reply")
		page := entriesOf(t, wait.answer())
		synctest.Wait()
		start := time.Now()
		if r := h.self(sessA, http.MethodPost, "/v1/lifecycle", map[string]any{"event": eventTurnEnd}); r.status != http.StatusOK {
			t.Fatalf("turn_end: %d %s", r.status, r.raw)
		}
		wantNudge(t, watch.answer(), page[0]["seq"].(float64), 1, 1)
		if d := time.Since(start); d != 0 {
			t.Errorf("the nudge went out %v after turn_end, want at once", d)
		}
	})
}

func TestInTurnWait_closingWithNoMailWakesAStandingWaitItDeferred(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		id := h.idOf(sessA)
		// An in-turn wait whose client left before it saw the post: open
		// while the standing wait looks, then closed having returned none.
		h.srv.presence.inTurnOpened(id)
		watch := h.watchAsync(sessA, 55*time.Second)
		synctest.Wait()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		seq := h.newestWake(sessA)
		synctest.Wait()
		if r := watch.now(); r != nil {
			t.Fatalf("the standing wait returned %d %s while the in-turn wait was open, want it deferred", r.status, r.raw)
		}
		start := time.Now()
		h.srv.inTurnEnded(id, 0)
		wantNudge(t, watch.answer(), seq, 1, 1)
		if d := time.Since(start); d != 0 {
			t.Errorf("the nudge went out %v after the in-turn wait closed, want at once", d)
		}
	})
}

// inTurn holds GET /v1/inbox?wait= from session in its own goroutine.
func (h *harness) inTurn(session string, wait time.Duration) *pending {
	return h.async(http.MethodGet, "/v1/inbox?wait="+wait.String(), nil, map[string]string{headerSession: session})
}

func TestInTurnWait_returnsMailPastReadWhateverTheNudgeState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "first")
		seq := h.newestWake(sessA)
		wantNudge(t, h.watch(sessA, 55*time.Second), seq, 1, 1)

		page := entriesOf(t, h.inTurn(sessA, 55*time.Second).answer())
		if len(page) != 1 || page[0]["subject"] != "first" {
			t.Fatalf("the in-turn wait beside the watcher's nudge = %v, want the first post", page)
		}
		h.ack(sessA, seq)
		wait := h.inTurn(sessA, 55*time.Second)
		synctest.Wait()
		start := time.Now()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "second")
		page = entriesOf(t, wait.answer())
		if len(page) != 1 || page[0]["subject"] != "second" || time.Since(start) != 0 {
			t.Errorf("the in-turn wait = %v after %v, want the second post at once", page, time.Since(start))
		}
	})
}

func TestInTurnWait_aCardDoesNotReturnIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegister(sessA, "reader")
		h.mustRegister(sessB, "early-joiner")
		start := time.Now()
		wait := h.inTurn(sessA, 55*time.Second)
		synctest.Wait()
		h.mustRegister(sessC, "late-joiner")
		page := entriesOf(t, wait.answer())
		if d := time.Since(start); d != 55*time.Second {
			t.Errorf("the in-turn wait returned after %v with only cards, want the whole 55s", d)
		}
		if len(page) != 2 || page[0]["kind"] != "join" || page[1]["kind"] != "join" {
			t.Errorf("the page at the wait's end = %v, want both join cards", page)
		}
	})
}

func TestInTurnWait_holdsTheFirstNudgeForItsAck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		watch := h.watchAsync(sessA, 55*time.Second)
		wait := h.inTurn(sessA, 55*time.Second)
		synctest.Wait()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "reply")
		page := entriesOf(t, wait.answer())
		synctest.Wait()
		h.ack(sessA, page[0]["seq"].(float64))
		if r := watch.answer(); r.status != http.StatusNoContent {
			t.Errorf("the standing wait beside the in-turn wait = %d %s, want 204", r.status, r.raw)
		}
		if n := h.sentByWatcher(); n != 0 {
			t.Errorf("%d nudge sent lines via watcher for mail the in-turn wait returned, want none", n)
		}
	})
}

func TestInTurnWait_mailLeftUnackedIsNudgedAfterTheGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		wait := h.inTurn(sessA, 55*time.Second)
		synctest.Wait()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "reply")
		page := entriesOf(t, wait.answer())
		returned := time.Now()
		wantNudge(t, h.watch(sessA, 55*time.Second), page[0]["seq"].(float64), 1, 1)
		if d := time.Since(returned); d != ackGrace {
			t.Errorf("the first nudge went out %v after the in-turn wait returned, want %v", d, ackGrace)
		}
	})
}

func TestInTurnWait_refusals(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "reader")
	for _, w := range []string{"56s", "-1s", "soon"} {
		t.Run(w, func(t *testing.T) {
			if r := h.as(sessA, http.MethodGet, "/v1/inbox?wait="+w, nil); r.status != http.StatusBadRequest {
				t.Errorf("got %d %s, want 400", r.status, r.raw)
			}
		})
	}
}
