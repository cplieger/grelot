package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

// pushLines are the attributes of every line at level with message msg.
func (h *harness) pushLines(level slog.Level, msg string) []map[string]string {
	var out []map[string]string
	for _, r := range h.logs.lines(level, msg) {
		out = append(out, attrs(r))
	}
	return out
}

// mustReturnNow fails unless wait has already answered with a nudge.
func mustReturnNow(t *testing.T, wait <-chan *response) {
	t.Helper()
	select {
	case r := <-wait:
		if r.status != http.StatusOK || r.body["seq"] == nil {
			t.Errorf("wait = %d %s, want the nudge", r.status, r.raw)
		}
	default:
		t.Error("the standing wait still holds, want it to send the nudge at once")
	}
}

// reRegister registers session's identity again; a live one answers 200.
func (h *harness) reRegister(session, name string) {
	h.t.Helper()
	r := h.as(session, http.MethodPost, "/v1/register", registerBody(name, "6h", "/workspace/"+name))
	if r.status != http.StatusOK && r.status != http.StatusCreated {
		h.t.Fatalf("register %s again: %d %s", name, r.status, r.raw)
	}
}

func pathsOf(reqs []pushRequest) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.path
	}
	return out
}

func TestPush_a401Or403TurnsTheDoorOffForEveryIdentityItServesUntilRestart(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			rcv := newPushReceiver(t, pushReply{status: code}, pushReply{status: http.StatusAccepted})
			synctest.Test(t, func(t *testing.T) {
				h := newPushHarness(t, rcv.kiroDoor()...)
				h.mustRegisterAlone(sessA, "reader")
				h.mustRegisterAlone(sessC, "second")
				h.mustRegisterAlone(sessB, "writer")
				waitA, waitC := h.watchAsync(sessA, 55*time.Second), h.watchAsync(sessC, 55*time.Second)
				synctest.Wait()

				h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
				synctest.Wait()
				mustReturnNow(t, waitA)
				h.mustPost(sessB, map[string]any{"agent": "second"}, "hello")
				synctest.Wait()
				mustReturnNow(t, waitC)
				if n := len(rcv.requests()); n != 1 {
					t.Errorf("%d pushes, want only the refused one", n)
				}
				for _, s := range []string{sessA, sessC} {
					if got := h.pushState(s); got != string(pushOff) {
						t.Errorf("%s push = %q, want off", s, got)
					}
				}

				// Registering again does not clear a door's off, and a
				// re-nudge due later is not pushed either.
				h.reRegister(sessA, "reader")
				if got := h.pushState(sessA); got != string(pushOff) {
					t.Errorf("push after registering again = %q, want off", got)
				}
				time.Sleep(3 * time.Minute)
				synctest.Wait()
				if n := len(rcv.requests()); n != 1 {
					t.Errorf("%d pushes with the door off, want 1", n)
				}
				refused := h.pushLines(slog.LevelError, msgPushRefused)
				if len(refused) != 1 {
					t.Fatalf("push refused lines = %v, want one in all", refused)
				}
				if l := refused[0]; l[attrStatus] != strconv.Itoa(code) || l["door"] != "push" || l["provider"] != "testhost" {
					t.Errorf("push refused = %v, want status %d, door push, provider testhost", l, code)
				}
				if n := len(h.pushLines(slog.LevelError, msgPushFailed)); n != 0 {
					t.Errorf("%d push failed ERROR lines, want none beside push refused", n)
				}

				h.restart()
				synctest.Wait()
				want := []string{pushKey(h.idOf(sessA), 0, 2), pushKey(h.idOf(sessC), 0, 2)}
				if got := rcv.keys(t)[1:]; !equalStrings(got, want) {
					t.Errorf("keys after the restart = %v, want both re-nudges %v", got, want)
				}
				if got := h.pushState(sessA); got != string(pushOK) {
					t.Errorf("push after the restart = %q, want ok", got)
				}
			})
		})
	}
}

func TestPush_a400TurnsPushOffForThatIdentityAloneUntilItIsClear(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	for _, tc := range []struct {
		clear func(h *harness) (writer string)
		name  string
	}{
		{name: "it registers again", clear: func(h *harness) string {
			h.reRegister(sessA, "reader")
			return sessB
		}},
		{name: "it is revived", clear: func(h *harness) string {
			time.Sleep(7 * time.Hour)
			if r := h.self(sessA, http.MethodPost, "/v1/revive", reviveBody("", "6h")); r.status != http.StatusOK {
				h.t.Fatalf("revive: %d %s", r.status, r.raw)
			}
			h.mustRegisterAlone(sessD, "writer2")
			return sessD
		}},
		{name: "the server restarts", clear: func(h *harness) string {
			h.restart()
			return sessB
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rcv := newPushReceiver(t, pushReply{status: http.StatusBadRequest}, pushReply{status: http.StatusAccepted})
			synctest.Test(t, func(t *testing.T) {
				h := newPushHarness(t, rcv.kiroDoor()...)
				h.mustRegisterAlone(sessA, "reader")
				h.mustRegisterAlone(sessC, "second")
				h.mustRegisterAlone(sessB, "writer")
				waitA := h.watchAsync(sessA, 55*time.Second)
				synctest.Wait()

				h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
				synctest.Wait()
				mustReturnNow(t, waitA)
				h.mustPost(sessB, map[string]any{"agent": "second"}, "hello")
				synctest.Wait()
				time.Sleep(3 * time.Minute)
				synctest.Wait()
				// The reader's re-nudge is due too, and only the second's goes out.
				want := []string{"/notify/session-a", "/notify/session-c", "/notify/session-c"}
				if got := pathsOf(rcv.requests()); !equalStrings(got, want) {
					t.Fatalf("pushes = %v, want %v", got, want)
				}
				if got, want := h.pushState(sessA)+" "+h.pushState(sessC), "off ok"; got != want {
					t.Errorf("push states = %q, want %q", got, want)
				}
				failed := h.pushLines(slog.LevelError, msgPushFailed)
				if len(failed) != 1 || failed[0][attrStatus] != "400" || failed[0]["name"] != "reader" {
					t.Errorf("push failed ERROR lines = %v, want one, status 400, for reader", failed)
				}
				if n := len(h.pushLines(slog.LevelError, msgPushRefused)); n != 0 {
					t.Errorf("%d push refused lines, want none for a 400", n)
				}

				writer := tc.clear(h)
				synctest.Wait()
				if got := h.pushState(sessA); got != string(pushOK) {
					t.Errorf("push once %s = %q, want ok", tc.name, got)
				}
				before := len(rcv.requests())
				h.ack(sessA, h.newestWake(sessA))
				h.mustPost(writer, map[string]any{"agent": "reader"}, "again")
				synctest.Wait()
				if got := pathsOf(rcv.requests()[before:]); !equalStrings(got, []string{"/notify/session-a"}) {
					t.Errorf("pushes for new mail once %s = %v, want one to session-a", tc.name, got)
				}
			})
		})
	}
}

func TestPush_aGoneStatusTurnsPushOffForThatIdentityAtInfo(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t, pushReply{status: http.StatusNotFound})
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		wait := h.watchAsync(sessA, 55*time.Second)
		synctest.Wait()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()

		mustReturnNow(t, wait)
		gone := h.pushLines(slog.LevelInfo, msgPushGone)
		if len(gone) != 1 || gone[0][attrStatus] != "404" || gone[0]["name"] != "reader" {
			t.Errorf("push gone lines = %v, want one at INFO, status 404, for reader", gone)
		}
		for _, level := range []slog.Level{slog.LevelWarn, slog.LevelError} {
			if n := len(h.pushLines(level, msgPushFailed)) + len(h.pushLines(level, msgPushRefused)); n != 0 {
				t.Errorf("%d push lines at %v, want none for a gone status", n, level)
			}
		}
		if got := h.pushState(sessA); got != string(pushOff) {
			t.Errorf("push = %q, want off", got)
		}
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		if n := len(rcv.requests()); n != 1 {
			t.Errorf("%d pushes, want none after the gone answer", n)
		}
	})
}

func TestPush_aRetryLaterStatusRetriesTheSameKeyAfterRetryAfter(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	for _, tc := range []struct {
		header map[string]string
		name   string
		status int
		after  time.Duration
		level  slog.Level
	}{
		{name: "503", status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}, after: 20 * time.Second, level: slog.LevelWarn},
		{name: "429", status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "7"}, after: 7 * time.Second, level: slog.LevelWarn},
		{name: "500", status: http.StatusInternalServerError, after: 15 * time.Second, level: slog.LevelError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rcv := newPushReceiver(t, pushReply{status: tc.status, header: tc.header}, pushReply{status: http.StatusAccepted})
			synctest.Test(t, func(t *testing.T) {
				h := newPushHarness(t, rcv.kiroDoor()...)
				h.mustRegisterAlone(sessA, "reader")
				h.mustRegisterAlone(sessB, "writer")
				h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
				synctest.Wait()

				failed := h.pushLines(tc.level, msgPushFailed)
				if len(failed) != 1 || failed[0][attrStatus] != tc.name || failed[0]["name"] != "reader" {
					t.Errorf("push failed lines at %v = %v, want one, status %s", tc.level, failed, tc.name)
				}
				if got := h.pushState(sessA); got != string(pushFailing) {
					t.Errorf("push = %q, want failing", got)
				}
				time.Sleep(tc.after - time.Second)
				synctest.Wait()
				if n := len(rcv.requests()); n != 1 {
					t.Fatalf("%d pushes a second before Retry-After, want 1", n)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				key := pushKey(h.idOf(sessA), 0, 1)
				if got := rcv.keys(t); !equalStrings(got, []string{key, key}) {
					t.Errorf("keys = %v, want %s twice", got, key)
				}
				if n := h.nudgesVia(string(store.NudgeByPush)); n != 1 {
					t.Errorf("%d nudges via push, want the retry's", n)
				}
				if got := h.pushState(sessA); got != string(pushOK) {
					t.Errorf("push after the retry = %q, want ok", got)
				}
			})
		})
	}
}

func TestPush_aStandingWaitSendsANudgeWhosePushFailedAndTheRetryStops(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t, pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}})
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		wait := h.watchAsync(sessA, 55*time.Second)
		synctest.Wait()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()

		mustReturnNow(t, wait)
		if n := h.sentByWatcher(); n != 1 {
			t.Errorf("%d nudges via watcher, want 1", n)
		}
		time.Sleep(21 * time.Second)
		synctest.Wait()
		if n := len(rcv.requests()); n != 1 {
			t.Errorf("%d pushes, want no retry once the wait sent the nudge", n)
		}
		if got := h.pushState(sessA); got != string(pushOK) {
			t.Errorf("push = %q, want ok with no retry pending", got)
		}
		// A standing wait opened while push is failing sends at once too.
		h.mustRegisterAlone(sessC, "second")
		h.mustPost(sessB, map[string]any{"agent": "second"}, "hello")
		synctest.Wait()
		late := h.watchAsync(sessC, 55*time.Second)
		synctest.Wait()
		mustReturnNow(t, late)
	})
}

func TestPush_aNetworkErrorIsRetriedAt15sWithStatus0(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	rcv.srv.Close()
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()

		failed := h.pushLines(slog.LevelWarn, msgPushFailed)
		if len(failed) != 1 {
			t.Fatalf("push failed WARN lines = %v, want one", failed)
		}
		if l := failed[0]; l[attrStatus] != "0" || l["error"] == "" || strings.Contains(l["error"], "notify") {
			t.Errorf("line = %v, want status 0 and an error without the URL", l)
		}
		if got := h.pushState(sessA); got != string(pushFailing) {
			t.Errorf("push = %q, want failing", got)
		}
		time.Sleep(15*time.Second - time.Second)
		synctest.Wait()
		if n := len(h.pushLines(slog.LevelWarn, msgPushFailed)); n != 1 {
			t.Fatalf("%d tries a second before 15 s, want 1", n)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if n := len(h.pushLines(slog.LevelWarn, msgPushFailed)); n != 2 {
			t.Errorf("%d tries at 15 s, want 2", n)
		}
	})
}

func TestPush_aDoorThatDoesNotRenderForOneIdentityTurnsItOff(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	// The store refuses a control character in a session id or a name, so
	// a template that fails on a short session id stands for any door that
	// renders for some identities only.
	const shortSession = "kiro-cli:short"
	synctest.Test(t, func(t *testing.T) {
		file := strings.Replace(doorFileAt(rcv.srv.URL, "testhost", "door_token", "    match: {provider: kiro-cli}\n"),
			"{{path .Session}}", "{{path (slice .Session 0 9)}}", 1)
		h := newPushHarness(t, "testhost", file)
		h.mustRegisterAlone(shortSession, "broken")
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "broken"}, "hello")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()

		if got := pathsOf(rcv.requests()); !equalStrings(got, []string{"/notify/session-a"}) {
			t.Errorf("pushes = %v, want only the reader's", got)
		}
		failed := h.pushLines(slog.LevelError, msgPushFailed)
		if len(failed) != 1 || failed[0]["name"] != "broken" || failed[0][attrStatus] != "0" || failed[0]["error"] == "" {
			t.Errorf("push failed lines = %v, want one ERROR for broken with status 0 and the error", failed)
		}
		if got := h.pushState(shortSession) + " " + h.pushState(sessA); got != "off ok" {
			t.Errorf("push states = %q, want off ok", got)
		}
	})
}
