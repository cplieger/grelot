//go:build !windows

package server

import (
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// grace is how long a failing push still reads as host push, and how long
// push must work before a push identity's watcher ends.
const grace = 2 * time.Minute

// wakes is the wake state each status reader reports for session: the
// status line on GET /v1/me and on GET /v1/inbox, GET /v1/me's wake field,
// the turn-start read's and, when stop, the stop hook's. The stop hook
// shows the mail, which ends a pending retry, and closes the turn the
// turn-start read opened; without it no turn is open, since no stop hook
// was seen.
func (h *harness) wakes(session string, stop bool) map[string]string {
	h.t.Helper()
	fromLine := func(r *response) string {
		_, after, _ := strings.Cut(r.line(), " · wake: ")
		before, _, _ := strings.Cut(after, " · ")
		return before
	}
	me := h.self(session, http.MethodGet, "/v1/me", nil)
	out := map[string]string{"status line": fromLine(me), "me wake": me.str("wake")}
	out["inbox line"] = fromLine(h.as(session, http.MethodGet, "/v1/inbox", nil))
	out["turn start"] = h.prompt(session, "turn=start").str("wake")
	if stop {
		out["stop hook"] = h.self(session, http.MethodPost, "/v1/events/hold", map[string]any{"for": "stop", "last": true}).str("wake")
	}
	return out
}

// mustWake fails unless every status reader of session but the stop hook
// reports want at the test's step when.
func (h *harness) mustWake(t *testing.T, when, session, want string) {
	t.Helper()
	h.mustWakeAll(t, when, session, want, false)
}

// mustWakeAll is mustWake, the stop hook too when stop.
func (h *harness) mustWakeAll(t *testing.T, when, session, want string, stop bool) {
	t.Helper()
	for reader, got := range h.wakes(session, stop) {
		if got != want {
			t.Errorf("%s, %s: wake %q, want %q", when, reader, got, want)
		}
	}
}

// waitForRequests sleeps on the bubble's clock, a second at a time for at
// most limit, until rcv has had n requests, and answers when the nth came.
func waitForRequests(t *testing.T, rcv *pushReceiver, n int, limit time.Duration) time.Time {
	t.Helper()
	for end := time.Now().Add(limit); len(rcv.requests()) < n; {
		if time.Now().After(end) {
			t.Fatalf("%d pushes after %v, want %d", len(rcv.requests()), limit, n)
		}
		time.Sleep(time.Second)
		synctest.Wait()
	}
	return time.Now()
}

func TestPush_aFailingSendStillReadsHostPushForTwoMinutes(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t, pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}})
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()
		if got := h.pushState(sessA); got != string(pushFailing) {
			t.Fatalf("push = %q, want failing", got)
		}

		h.mustWake(t, "host push after one failed send", sessA, wakePush)
		time.Sleep(grace - time.Second)
		synctest.Wait()
		if n := len(rcv.requests()); n < 5 {
			t.Errorf("%d pushes, want a retry every 20 s", n)
		}
		h.mustWake(t, "a second before the grace ends, every retry failing", sessA, wakePush)
		time.Sleep(time.Second)
		synctest.Wait()
		h.mustWake(t, "down once the run has failed for the grace", sessA, wakeDown)
		wait := h.watchAsync(sessA, 55*time.Second)
		synctest.Wait()
		mustReturnNow(t, wait)
		// The watcher's nudge made the retry obsolete, which ends the run.
		h.mustWakeAll(t, "host push once the watcher sent the nudge", sessA, wakePush, true)
		if got := h.pushState(sessA); got != string(pushOK) {
			t.Errorf("push after the watcher's nudge = %q, want ok", got)
		}
	})
}

// The stop hook shows the mail, which ends the pending retry, and with it
// the failing run, so push is ok again with no accepted send.
func TestPush_theStopHookReadsTheGracedWake(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t, pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}})
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()
		h.mustWakeAll(t, "after one failed send", sessA, wakePush, true)
		time.Sleep(grace - time.Second)
		synctest.Wait()
		h.mustWakeAll(t, "a second before the grace ends", sessA, wakePush, true)
		time.Sleep(time.Second)
		synctest.Wait()
		h.mustWakeAll(t, "a grace after the first failure, no retry pending", sessA, wakePush, true)
		if got := h.pushState(sessA); got != string(pushOK) {
			t.Errorf("push = %q, want ok once the stop hook read the mail", got)
		}
	})
}

func TestPush_aReadThatEndsTheRetryEndsTheFailingRun(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t, pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "600"}})
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()
		if got := h.pushState(sessA); got != string(pushFailing) {
			t.Fatalf("push = %q, want failing", got)
		}
		h.mustWakeAll(t, "after one failed send, the stop hook reading the mail", sessA, wakePush, true)
		synctest.Wait()
		if got := h.pushState(sessA); got != string(pushOK) {
			t.Fatalf("push after the read = %q, want ok", got)
		}
		time.Sleep(grace + time.Second)
		synctest.Wait()
		h.mustWakeAll(t, "past the grace with no retry pending", sessA, wakePush, true)
		if got := h.pushState(sessA); got != string(pushOK) {
			t.Errorf("push past the grace = %q, want ok", got)
		}

		h.mustPost(sessB, map[string]any{"agent": "reader"}, "again")
		synctest.Wait()
		waitForRequests(t, rcv, 2, 5*time.Minute)
		if got := h.pushState(sessA); got != string(pushFailing) {
			t.Fatalf("push after the new mail's send = %q, want failing", got)
		}
		h.mustWake(t, "a new single failure", sessA, wakePush)
		time.Sleep(grace - time.Second)
		synctest.Wait()
		h.mustWake(t, "a second before the new failure's grace ends", sessA, wakePush)
		time.Sleep(time.Second)
		synctest.Wait()
		h.mustWake(t, "down once the new failure has gone the grace", sessA, wakeDown)
	})
}

func TestPush_anAcceptedSendEndsTheFailingRun(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t,
		pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}},
		pushReply{status: http.StatusAccepted},
		pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "600"}},
	)
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()
		waitForRequests(t, rcv, 2, time.Minute)
		if got := h.pushState(sessA); got != string(pushOK) {
			t.Fatalf("push after the accepted retry = %q, want ok", got)
		}

		// The re-nudge fails 3 minutes on, past the first failure's grace.
		waitForRequests(t, rcv, 3, 5*time.Minute)
		if got := h.pushState(sessA); got != string(pushFailing) {
			t.Fatalf("push after the re-nudge = %q, want failing", got)
		}
		h.mustWake(t, "host push: the new run began at the re-nudge", sessA, wakePush)
		time.Sleep(grace - time.Second)
		synctest.Wait()
		h.mustWake(t, "host push a second before the new run's grace ends", sessA, wakePush)
		time.Sleep(time.Second)
		synctest.Wait()
		h.mustWakeAll(t, "down once the new run has failed for the grace", sessA, wakeDown, true)
	})
}

func TestPush_aGoneOrRefusedDoorIsNotGraced(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	for _, code := range []int{http.StatusNotFound, http.StatusUnauthorized} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			rcv := newPushReceiver(t, pushReply{status: code})
			synctest.Test(t, func(t *testing.T) {
				h := newPushHarness(t, rcv.kiroDoor()...)
				h.mustRegisterAlone(sessA, "reader")
				h.mustRegisterAlone(sessB, "writer")
				h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
				synctest.Wait()
				if got := h.pushState(sessA); got != string(pushOff) {
					t.Fatalf("push = %q, want off", got)
				}
				h.mustWakeAll(t, "after the one push", sessA, wakeDown, true)
			})
		})
	}
}

// watchUntilEnd takes session's standing waits back to back, as a watcher
// does, for at most n waits or until one answers 410, and answers that
// one with the instant it came, or nil.
func (h *harness) watchUntilEnd(t *testing.T, session string, n int) (*response, time.Time) {
	t.Helper()
	for range n {
		r := h.watchAsync(session, 55*time.Second).answer()
		switch r.status {
		case http.StatusGone:
			return r, time.Now()
		case http.StatusOK, http.StatusNoContent:
		default:
			t.Fatalf("standing wait = %d %s, want 200, 204 or 410", r.status, r.raw)
		}
	}
	return nil, time.Time{}
}

func TestWait_aPushIdentitysWatcherEndsOnceHostPushHasWorkedForTwoMinutes(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)

	t.Run("push ok from the watcher's start", func(t *testing.T) {
		rcv := newPushReceiver(t)
		synctest.Test(t, func(t *testing.T) {
			h := newPushHarness(t, rcv.kiroDoor()...)
			h.mustRegisterAlone(sessA, "reader")
			h.mustRegisterAlone(sessB, "writer")
			start := time.Now()
			end, at := h.watchUntilEnd(t, sessA, 5)
			if end == nil {
				t.Fatal("the watcher never ended, want it to end 2 min after its start")
			}
			if end.str("code") != "restored" {
				t.Errorf("end = %s, want code restored", end.raw)
			}
			if d := at.Sub(start); d != grace {
				t.Errorf("the watcher ended %v after its start, want %v", d, grace)
			}
			h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
			synctest.Wait()
			if n := len(rcv.requests()); n != 1 {
				t.Errorf("%d pushes after the watcher ended, want its mail pushed once", n)
			}
			if n := h.nudgesVia("push"); n != 1 {
				t.Errorf("%d nudges via push, want 1", n)
			}
			h.mustWakeAll(t, "after the watcher ended", sessA, wakePush, true)
		})
	})

	t.Run("a nudge due at the hand-back goes by push", func(t *testing.T) {
		rcv := newPushReceiver(t)
		synctest.Test(t, func(t *testing.T) {
			h := newPushHarness(t, rcv.kiroDoor()...)
			h.mustRegisterAlone(sessA, "reader")
			h.mustRegisterAlone(sessB, "writer")
			for range 2 {
				if r := h.watchAsync(sessA, 55*time.Second).answer(); r.status != http.StatusNoContent {
					t.Fatalf("standing wait = %d %s, want 204", r.status, r.raw)
				}
			}
			last := h.watchAsync(sessA, 55*time.Second)
			time.Sleep(grace - 110*time.Second - time.Second)
			h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
			synctest.Wait()
			if r := last.now(); r != nil {
				t.Fatalf("the wait answered %d %s at the post, want it to hold while push sends", r.status, r.raw)
			}
			time.Sleep(time.Second)
			synctest.Wait()
			if r := last.now(); r == nil || r.status != http.StatusGone || r.str("code") != "restored" {
				t.Fatalf("the wait at the grace = %v, want a 410 restored", r)
			}
			time.Sleep(3 * time.Minute)
			synctest.Wait()
			if n, w := h.nudgesVia("push"), h.sentByWatcher(); n != 2 || w != 0 {
				t.Errorf("%d nudges via push and %d via watcher, want the nudge and its re-nudge by push", n, w)
			}
		})
	})

	t.Run("measured from the accepted send that ended a failing run", func(t *testing.T) {
		rcv := newPushReceiver(t,
			pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}},
			pushReply{status: http.StatusAccepted},
		)
		synctest.Test(t, func(t *testing.T) {
			h := newPushHarness(t, rcv.kiroDoor()...)
			h.mustRegisterAlone(sessA, "reader")
			h.mustRegisterAlone(sessB, "writer")
			first := h.watchAsync(sessA, 55*time.Second)
			synctest.Wait()
			h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
			synctest.Wait()
			mustReturnNow(t, first)
			if n := h.sentByWatcher(); n != 1 {
				t.Fatalf("%d nudges via watcher, want the failed push's", n)
			}
			synctest.Wait()
			if got := h.pushState(sessA); got != string(pushOK) {
				t.Fatalf("push after the watcher's nudge = %q, want ok, the retry obsolete", got)
			}
			nudged := time.Now()
			end, at := h.watchUntilEnd(t, sessA, 9)
			if end == nil || end.str("code") != "restored" {
				t.Fatalf("end = %v, want a 410 restored", end)
			}
			if n := len(rcv.requests()); n != 2 {
				t.Fatalf("%d pushes, want the failed one and the accepted re-nudge", n)
			}
			// The re-nudge goes out 3 minutes after the nudge the watcher sent.
			if d := at.Sub(nudged); d != 3*time.Minute+grace {
				t.Errorf("the watcher ended %v after its nudge, want the re-nudge's 3m plus %v", d, grace)
			}
		})
	})
}

func TestWait_aWatcherNeverEndsAsRestoredWithoutWorkingPush(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	for _, tc := range []struct {
		name  string
		reply pushReply
	}{
		{name: "every push failing", reply: pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}}},
		{name: "push off for the identity", reply: pushReply{status: http.StatusNotFound}},
		{name: "the door off", reply: pushReply{status: http.StatusUnauthorized}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rcv := newPushReceiver(t, tc.reply)
			synctest.Test(t, func(t *testing.T) {
				h := newPushHarness(t, rcv.kiroDoor()...)
				h.mustRegisterAlone(sessA, "reader")
				h.mustRegisterAlone(sessB, "writer")
				h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
				synctest.Wait()
				if end, _ := h.watchUntilEnd(t, sessA, 6); end != nil {
					t.Errorf("the watcher ended with %s, want it to run on", end.raw)
				}
			})
		})
	}
	t.Run("no door serves the identity", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newBubbleHarness(t)
			h.mustRegisterAlone(sessA, "reader")
			if end, _ := h.watchUntilEnd(t, sessA, 6); end != nil {
				t.Errorf("the watcher ended with %s, want it to run on", end.raw)
			}
		})
	})
}
