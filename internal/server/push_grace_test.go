//go:build !windows

package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// grace is how long a failing push still reads as host push, and how long
// push must work before a push identity's watcher ends.
const grace = 2 * time.Minute

// wakes is the wake state each status reader reports for session: the
// status line on GET /v1/me and on GET /v1/inbox, GET /v1/me's wake field,
// the turn-start read's and, when stop, the stop hook's. The turn-start
// read opens a turn once a stop hook was seen; when stop, the stop hook
// shows the mail, which ends a pending retry, and closes that turn.
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

// mustAcceptOnePush posts from writer to name, whose session is to, has
// rcv accept its one push, and acks it, so push is proven and nothing is
// left to re-nudge.
func (h *harness) mustAcceptOnePush(t *testing.T, writer, to, name string, rcv *pushReceiver) {
	t.Helper()
	before := len(rcv.requests())
	h.mustPost(writer, map[string]any{"agent": name}, "first")
	synctest.Wait()
	if n := len(rcv.requests()) - before; n != 1 {
		t.Fatalf("%d pushes for the first mail, want 1", n)
	}
	h.ack(to, h.newestWake(to))
	synctest.Wait()
}

// heldDoor is a notify endpoint answering its kth request with
// replies[k-1], the last one repeating, which holds its nth request until
// release is called, after signalling arrived. Like pushReceiver it lives
// outside the bubble.
type heldDoor struct {
	srv     *httptest.Server
	arrived chan struct{}
	release func()
	seen    atomic.Int32
}

func heldPushes(t *testing.T, n int32, replies ...pushReply) *heldDoor {
	t.Helper()
	d := &heldDoor{arrived: make(chan struct{}, 1)}
	held := make(chan struct{})
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		nth := d.seen.Add(1)
		if nth == n {
			d.arrived <- struct{}{}
			<-held
		}
		reply := replies[min(int(nth), len(replies))-1]
		w.Header().Set("Connection", "close")
		for k, v := range reply.header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(reply.status)
	}))
	t.Cleanup(d.srv.Close)
	var once sync.Once
	d.release = func() { once.Do(func() { close(held) }) }
	t.Cleanup(d.release)
	return d
}

// sends is how many requests the door has had, the held one included.
func (d *heldDoor) sends() int { return int(d.seen.Load()) }

// kiroDoor is a door file serving the kiro-cli identities of every host.
func (d *heldDoor) kiroDoor() []string {
	return []string{"testhost", doorFileAt(d.srv.URL, "testhost", "door_token", "    match: {provider: kiro-cli}\n")}
}

func TestPush_aRetryInFlightPastTheGraceReadsDown(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	// The first send fails at 0 s and its retries at 20 s steps, so the
	// seventh send is the retry at the grace.
	door := heldPushes(t, 7, pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}})
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, door.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()
		time.Sleep(grace)
		<-door.arrived
		h.mustWake(t, "while the retry at the grace is in flight", sessA, wakeDown)
		door.release()
		synctest.Wait()
		if got := h.pushState(sessA); got != string(pushFailing) {
			t.Errorf("push once the held retry failed = %q, want failing", got)
		}
		h.mustWake(t, "once the held retry failed", sessA, wakeDown)
	})
}

// A turn open when the retry is due holds the nudge for the hooks
// (Server.deferred), so no send carries the failing run on.
func TestPush_aRetryHeldForAnOpenTurnEndsTheFailingRun(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t,
		pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}},
		pushReply{status: http.StatusAccepted},
	)
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		// A stop hook seen, so the server tracks the reader's turns.
		h.self(sessA, http.MethodPost, "/v1/events/hold", map[string]any{"for": "stop", "last": true})
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()
		h.prompt(sessA, "turn=start")
		time.Sleep(grace + 30*time.Second)
		synctest.Wait()

		h.mustWake(t, "past the grace in the open turn", sessA, wakePush)
		if got := h.pushState(sessA); got != string(pushOK) {
			t.Errorf("push = %q, want ok", got)
		}
		if got := h.self(sessA, http.MethodPost, "/v1/events/hold", map[string]any{"for": "stop"}).str("wake"); got != wakePush {
			t.Errorf("stop hook: wake %q, want %q", got, wakePush)
		}
		if n := len(rcv.requests()); n != 1 {
			t.Errorf("%d pushes, want only the failed first one", n)
		}
	})
}

// Each row is one site that starts, carries or ends a failing run. The
// first send, at 0 s, fails; at runs 10 s on, before the first retry at
// 20 s. An open run reads host push for the grace from its first failed
// send and down after; an ended one reads host push again.
func TestPush_theFailingRunStartsAtTheFirstFailureAndEndsWithItsRetry(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	retry := func(after string) pushReply {
		return pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": after}}
	}
	accepted := pushReply{status: http.StatusAccepted}
	stopSeen := func(h *harness) {
		h.self(sessA, http.MethodPost, "/v1/events/hold", map[string]any{"for": "stop", "last": true})
	}
	for _, tc := range []struct {
		name    string
		replies []pushReply
		// first runs before the post, at 10 s after it.
		first, at func(*harness)
		// before and after are the wake a second either side of the
		// grace, push the state after it.
		before, after string
		push          pushState
	}{
		{
			name: "a failed retry keeps the run open", replies: []pushReply{retry("20")},
			before: wakePush, after: wakeDown, push: pushFailing,
		},
		{
			name: "a same-session register carries the run", replies: []pushReply{retry("20")},
			at:     func(h *harness) { h.reRegister(sessA, "reader") },
			before: wakePush, after: wakeDown, push: pushFailing,
		},
		{
			name: "an accepted retry ends the run", replies: []pushReply{retry("20"), accepted, retry("600")},
			before: wakePush, after: wakePush, push: pushOK,
		},
		{
			name: "a read of the mail ends the run", replies: []pushReply{retry("600")},
			at:     stopSeen,
			before: wakePush, after: wakePush, push: pushOK,
		},
		{
			name: "an ack ends the run", replies: []pushReply{retry("600")},
			at:     func(h *harness) { h.ack(sessA, h.newestWake(sessA)) },
			before: wakePush, after: wakePush, push: pushOK,
		},
		// The ack moves the read past the first mail, so the nudge still
		// due for the second is a new send, whose failure starts a new run.
		{
			name: "a new send of the nudge ends the run", replies: []pushReply{retry("600")},
			at: func(h *harness) {
				first := h.newestWake(sessA)
				h.mustPost(sessB, map[string]any{"agent": "reader"}, "again")
				synctest.Wait()
				h.ack(sessA, first)
			},
			before: wakePush, after: wakePush, push: pushFailing,
		},
		{
			name: "a retry held for an open turn ends the run", replies: []pushReply{retry("20"), accepted},
			first: stopSeen, at: func(h *harness) { h.prompt(sessA, "turn=start") },
			before: wakePush, after: wakePush, push: pushOK,
		},
		{
			name: "push off ends the run, and a register starts afresh", replies: []pushReply{retry("5"), {status: http.StatusNotFound}, accepted},
			at:     func(h *harness) { h.reRegister(sessA, "reader") },
			before: wakePush, after: wakePush, push: pushOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rcv := newPushReceiver(t, tc.replies...)
			synctest.Test(t, func(t *testing.T) {
				h := newPushHarness(t, rcv.kiroDoor()...)
				h.mustRegisterAlone(sessA, "reader")
				h.mustRegisterAlone(sessB, "writer")
				if tc.first != nil {
					tc.first(h)
				}
				h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
				synctest.Wait()
				time.Sleep(10 * time.Second)
				if tc.at != nil {
					tc.at(h)
				}
				synctest.Wait()
				time.Sleep(grace - 11*time.Second)
				synctest.Wait()
				h.mustWake(t, "a second before the grace", sessA, tc.before)
				time.Sleep(2 * time.Second)
				synctest.Wait()
				h.mustWake(t, "a second after the grace", sessA, tc.after)
				if got := h.pushState(sessA); got != string(tc.push) {
					t.Errorf("push after the grace = %q, want %q", got, tc.push)
				}
			})
		})
	}
}

// forgetNow drops session's push entry, as the sweeper's beat drops a
// lapsed identity's. A lapse needs the bubble's clock to move, which it
// cannot while a send is held on a socket, so the test calls the writer.
func (h *harness) forgetNow(session string) {
	h.t.Helper()
	p := h.srv.push.(*hostPush)
	id := h.idOf(session)
	p.mu.Lock()
	e := p.ids[id]
	p.mu.Unlock()
	p.forget(id, e)
}

// A send's answer belongs to its receiver, the door and the session it
// went to. Each row holds a push to @reader through the door serving
// host-a while a writer changes its entry, then lets the door answer;
// every later push through either door is accepted. doorRefused and
// identityOff run only on the sender goroutine, one send at a time, so
// they never interleave with another send's answer.
func TestPush_anAnswerAppliesOnlyToTheReceiverItsSendWentTo(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	accepted := pushReply{status: http.StatusAccepted}
	retry := pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}}
	gone := pushReply{status: http.StatusNotFound}
	gaveUp := pushReply{status: http.StatusBadRequest}
	refused := pushReply{status: http.StatusUnauthorized}

	// A writer runs while the push is held and answers the session
	// @reader is on after it.
	sameSession := func(h *harness) string { h.reRegister(sessA, "reader"); return sessA }
	takeOver := func(h *harness) string { h.takeOverOn(sessC, "reader", "host-a/app"); return sessC }
	doorMove := func(h *harness) string { h.takeOverOn(sessC, "reader", "host-b/app"); return sessC }
	forget := func(h *harness) string { h.forgetNow(sessA); return sessA }
	forgetThenRegister := func(h *harness) string {
		h.forgetNow(sessA)
		h.reRegister(sessA, "reader")
		return sessA
	}

	// A watcher started 20 s after the answer ends restored a grace after
	// the later of its start and the receiver's first accepted push.
	// proved is that for a push accepted by then, reNudged for the
	// re-nudge accepted 3 minutes after the nudge was recorded.
	const proved, reNudged = 20*time.Second + grace, 3*time.Minute + grace
	for _, tc := range []struct {
		name   string
		writer func(*harness) string
		reply  pushReply
		push   pushState
		// sends counts the pushes after the held one, through either door,
		// at the answer, 19 s on and at the Retry-After, 20 s on.
		sends [3]int
		// restored is when after the answer the watcher ends restored,
		// zero for not within its six waits.
		restored time.Duration
	}{
		{"same-session register, accepted", sameSession, accepted, pushOK, [3]int{}, proved},
		{"same-session register, retry", sameSession, retry, pushFailing, [3]int{0, 0, 1}, proved},
		{"same-session register, gone", sameSession, gone, pushOff, [3]int{}, 0},
		{"same-session register, gave up", sameSession, gaveUp, pushOff, [3]int{}, 0},
		{"same-session register, refused", sameSession, refused, pushOff, [3]int{}, 0},
		// The old chat's 2xx still records the nudge as pushed, so the new
		// chat is pushed the mail at the re-nudge.
		{"take-over on the same door, accepted", takeOver, accepted, pushOK, [3]int{}, reNudged},
		// A dropped answer leaves the nudge due, which the new receiver is
		// pushed at once.
		{"take-over on the same door, retry", takeOver, retry, pushOK, [3]int{1, 1, 1}, proved},
		{"take-over on the same door, gone", takeOver, gone, pushOK, [3]int{1, 1, 1}, proved},
		{"take-over on the same door, gave up", takeOver, gaveUp, pushOK, [3]int{1, 1, 1}, proved},
		{"door move, accepted", doorMove, accepted, pushOK, [3]int{}, reNudged},
		{"door move, retry", doorMove, retry, pushOK, [3]int{1, 1, 1}, proved},
		{"door move, gone", doorMove, gone, pushOK, [3]int{1, 1, 1}, proved},
		{"door move, gave up", doorMove, gaveUp, pushOK, [3]int{1, 1, 1}, proved},
		// A refused credential turns off the old door, which no longer
		// serves the identity.
		{"door move, refused", doorMove, refused, pushOK, [3]int{1, 1, 1}, proved},
		{"forget, accepted", forget, accepted, pushNone, [3]int{}, 0},
		{"forget, retry", forget, retry, pushNone, [3]int{}, 0},
		{"forget, gone", forget, gone, pushNone, [3]int{}, 0},
		{"forget, gave up", forget, gaveUp, pushNone, [3]int{}, 0},
		{"forget then register, accepted", forgetThenRegister, accepted, pushOK, [3]int{}, reNudged},
		{"forget then register, retry", forgetThenRegister, retry, pushOK, [3]int{1, 1, 1}, proved},
		{"forget then register, gone", forgetThenRegister, gone, pushOK, [3]int{1, 1, 1}, proved},
		{"forget then register, gave up", forgetThenRegister, gaveUp, pushOK, [3]int{1, 1, 1}, proved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := heldPushes(t, 1, tc.reply, accepted)
			rcv := newPushReceiver(t)
			synctest.Test(t, func(t *testing.T) {
				h := newPushHarness(t,
					"olddoor", doorFileAt(old.srv.URL, "olddoor", "door_token", "    match: {provider: kiro-cli, host: host-a/app}\n"),
					"newdoor", doorFileAt(rcv.srv.URL, "newdoor", "door_token", "    match: {provider: kiro-cli, host: host-b/app}\n"))
				h.mustRegisterAlone(sessA, "reader")
				h.registerOn(sessB, "writer", "host-c/app")
				h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
				<-old.arrived
				session := tc.writer(h)
				old.release()
				synctest.Wait()
				answered := time.Now()

				if got := h.pushState(session); got != string(tc.push) {
					t.Errorf("push after the answer = %q, want %q", got, tc.push)
				}
				var sends [3]int
				for i, step := range []time.Duration{0, 19 * time.Second, time.Second} {
					time.Sleep(step)
					synctest.Wait()
					sends[i] = old.sends() + len(rcv.requests()) - 1
				}
				if sends != tc.sends {
					t.Errorf("pushes after the held one at 0 s, 19 s and 20 s = %v, want %v", sends, tc.sends)
				}
				var restored time.Duration
				if end, at := h.watchUntilEnd(t, session, 6); end != nil {
					restored = at.Sub(answered)
					if end.str("code") != "restored" {
						t.Errorf("the watcher ended %s, want restored or no end", end.raw)
					}
				}
				if restored != tc.restored {
					t.Errorf("the watcher ended restored %v after the answer, want %v (0 is never)", restored, tc.restored)
				}
			})
		})
	}
}

// takeOverOn moves the live identity name to session, on host.
func (h *harness) takeOverOn(session, name, host string) {
	h.t.Helper()
	body := registerBody(name, "6h", "/workspace/"+name)
	body["host"], body["take_over"] = host, true
	if r := h.as(session, http.MethodPost, "/v1/register", body); r.status != http.StatusCreated || r.body["moved"] != true {
		h.t.Fatalf("take over %s on %s: %d %s", name, host, r.status, r.raw)
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
			h.mustAcceptOnePush(t, sessB, sessA, "reader", rcv)
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
			if n := len(rcv.requests()); n != 2 {
				t.Errorf("%d pushes after the watcher ended, want its mail pushed once more", n)
			}
			if n := h.nudgesVia("push"); n != 2 {
				t.Errorf("%d nudges via push, want 2", n)
			}
			h.mustWakeAll(t, "after the watcher ended", sessA, wakePush, true)
		})
	})

	t.Run("a fresh entry, one grace after its first accepted send", func(t *testing.T) {
		rcv := newPushReceiver(t)
		synctest.Test(t, func(t *testing.T) {
			h := newPushHarness(t, rcv.kiroDoor()...)
			h.mustRegisterAlone(sessA, "reader")
			h.mustRegisterAlone(sessB, "writer")
			first := h.watchAsync(sessA, 55*time.Second)
			time.Sleep(30 * time.Second)
			h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
			synctest.Wait()
			posted := time.Now()
			if n := len(rcv.requests()); n != 1 {
				t.Fatalf("%d pushes, want the mail's", n)
			}
			if r := first.answer(); r.status != http.StatusNoContent {
				t.Fatalf("the first wait = %d %s, want 204 while push sends", r.status, r.raw)
			}
			end, at := h.watchUntilEnd(t, sessA, 5)
			if end == nil || end.str("code") != "restored" {
				t.Fatalf("end = %v, want a 410 restored", end)
			}
			if d := at.Sub(posted); d != grace {
				t.Errorf("the watcher ended %v after the first accepted send, want %v", d, grace)
			}
		})
	})

	t.Run("after a register that cleared push off, from the next accepted send", func(t *testing.T) {
		rcv := newPushReceiver(t, pushReply{status: http.StatusNotFound}, pushReply{status: http.StatusAccepted})
		synctest.Test(t, func(t *testing.T) {
			h := newPushHarness(t, rcv.kiroDoor()...)
			h.mustRegisterAlone(sessA, "reader")
			h.mustRegisterAlone(sessB, "writer")
			first := h.watchAsync(sessA, 55*time.Second)
			synctest.Wait()
			h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
			synctest.Wait()
			mustReturnNow(t, first)
			nudged := time.Now()
			h.reRegister(sessA, "reader")
			synctest.Wait()
			if got := h.pushState(sessA); got != string(pushOK) {
				t.Fatalf("push after the register = %q, want ok", got)
			}
			end, at := h.watchUntilEnd(t, sessA, 9)
			if end == nil || end.str("code") != "restored" {
				t.Fatalf("end = %v, want a 410 restored", end)
			}
			if n := len(rcv.requests()); n != 2 {
				t.Fatalf("%d pushes, want the gone one and the accepted re-nudge", n)
			}
			// The re-nudge goes out 3 minutes after the nudge the watcher sent.
			if d := at.Sub(nudged); d != 3*time.Minute+grace {
				t.Errorf("the watcher ended %v after its nudge, want the re-nudge's 3m plus %v", d, grace)
			}
		})
	})

	t.Run("a nudge due at the hand-back goes by push", func(t *testing.T) {
		rcv := newPushReceiver(t)
		synctest.Test(t, func(t *testing.T) {
			h := newPushHarness(t, rcv.kiroDoor()...)
			h.mustRegisterAlone(sessA, "reader")
			h.mustRegisterAlone(sessB, "writer")
			h.mustAcceptOnePush(t, sessB, sessA, "reader", rcv)
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
			if n, w := h.nudgesVia("push"), h.sentByWatcher(); n != 3 || w != 0 {
				t.Errorf("%d nudges via push and %d via watcher, want the first mail's, then the nudge and its re-nudge, by push", n, w)
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
	t.Run("a restart while the door never accepts", func(t *testing.T) {
		rcv := newPushReceiver(t, pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}})
		synctest.Test(t, func(t *testing.T) {
			h := newPushHarness(t, rcv.kiroDoor()...)
			h.mustRegisterAlone(sessA, "reader")
			h.mustRegisterAlone(sessB, "writer")
			h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
			synctest.Wait()
			wait := h.watchAsync(sessA, 55*time.Second)
			synctest.Wait()
			mustReturnNow(t, wait)
			h.restart()
			synctest.Wait()
			if end, _ := h.watchUntilEnd(t, sessA, 8); end != nil {
				t.Errorf("the watcher ended with %s after the restart, want it to run on", end.raw)
			}
		})
	})
	t.Run("a door no push has reached yet", func(t *testing.T) {
		rcv := newPushReceiver(t)
		synctest.Test(t, func(t *testing.T) {
			h := newPushHarness(t, rcv.kiroDoor()...)
			h.mustRegisterAlone(sessA, "reader")
			if end, _ := h.watchUntilEnd(t, sessA, 8); end != nil {
				t.Errorf("the watcher ended with %s, want it to run on", end.raw)
			}
		})
	})
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

// A restored watcher exits for good, so a door gone after its end reads
// down at once, while a watcher's ordinary end still reads watcher for
// onlineFor, the bridge to its restart.
func TestPush_aDoorGoneAfterARestoredEndReadsDown(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	for _, tc := range []struct {
		end  func(t *testing.T, h *harness)
		name string
		want string
	}{
		{name: "restored", want: wakeDown, end: func(t *testing.T, h *harness) {
			if end, _ := h.watchUntilEnd(t, sessA, 5); end == nil || end.str("code") != "restored" {
				t.Fatalf("end = %v, want a 410 restored", end)
			}
		}},
		{name: "ordinary", want: wakeWatcher, end: func(t *testing.T, h *harness) {
			if r := h.watch(sessA, time.Second); r.status != http.StatusNoContent {
				t.Fatalf("standing wait = %d %s, want 204", r.status, r.raw)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rcv := newPushReceiver(t, pushReply{status: http.StatusAccepted}, pushReply{status: http.StatusNotFound})
			synctest.Test(t, func(t *testing.T) {
				h := newPushHarness(t, rcv.kiroDoor()...)
				h.mustRegisterAlone(sessA, "reader")
				h.mustRegisterAlone(sessB, "writer")
				h.mustAcceptOnePush(t, sessB, sessA, "reader", rcv)
				tc.end(t, h)
				h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
				synctest.Wait()
				if got := h.pushState(sessA); got != string(pushOff) {
					t.Fatalf("push after the 404 = %q, want off", got)
				}
				h.mustWakeAll(t, "the door gone after a "+tc.name+" end", sessA, tc.want, true)
			})
		})
	}
}
