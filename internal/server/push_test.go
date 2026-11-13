//go:build !windows

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

const (
	sessD        = "kiro-cli:session-d"
	sessOther    = "otherhost:session-c"
	doorTokenEnv = "SECRET_DOOR_TOKEN"
)

// pushReply is one scripted answer of a pushReceiver.
type pushReply struct {
	header map[string]string
	body   string
	status int
}

// pushRequest is one request a pushReceiver got.
type pushRequest struct {
	header http.Header
	path   string
	query  string
	body   string
}

// pushReceiver is a host's notify endpoint. It lives outside any synctest
// bubble and closes each connection, so no client goroutine stays parked
// on a socket and the bubble's clock can move.
type pushReceiver struct {
	srv     *httptest.Server
	got     []pushRequest
	replies []pushReply
	mu      sync.Mutex
}

// newPushReceiver answers the nth request with replies[n], the last one
// repeating; no reply answers 202.
func newPushReceiver(t *testing.T, replies ...pushReply) *pushReceiver {
	t.Helper()
	if len(replies) == 0 {
		replies = []pushReply{{status: http.StatusAccepted}}
	}
	rcv := &pushReceiver{replies: replies}
	rcv.srv = httptest.NewServer(http.HandlerFunc(rcv.serve))
	t.Cleanup(rcv.srv.Close)
	return rcv
}

func (r *pushReceiver) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	reply := r.replies[min(len(r.got), len(r.replies)-1)]
	r.got = append(r.got, pushRequest{header: req.Header.Clone(), path: req.URL.EscapedPath(), query: req.URL.RawQuery, body: string(body)})
	r.mu.Unlock()
	w.Header().Set("Connection", "close")
	for k, v := range reply.header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(reply.status)
	_, _ = io.WriteString(w, reply.body)
}

func (r *pushReceiver) requests() []pushRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]pushRequest(nil), r.got...)
}

// keys are the dedupe keys of every request so far, in order.
func (r *pushReceiver) keys(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, req := range r.requests() {
		var b struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal([]byte(req.body), &b); err != nil {
			t.Fatalf("request body %q is not the door's JSON: %v", req.body, err)
		}
		out = append(out, b.Key)
	}
	return out
}

// kiroDoor is a door file serving the kiro-cli identities of every host.
func (r *pushReceiver) kiroDoor() []string {
	return []string{"testhost", doorFileAt(r.srv.URL, "testhost", "door_token", "    match: {provider: kiro-cli}\n")}
}

// newPushHarness is a bubble harness pushing through files, a providers
// folder of name, content pairs. Call it inside the bubble.
func newPushHarness(t *testing.T, files ...string) *harness {
	t.Helper()
	var h *harness
	noSecretLogged(t, &h)
	h = newBubbleHarness(t, pushFrom(files...))
	return h
}

// pushFrom serves the harness through the real sender from files.
func pushFrom(files ...string) func(*Options) {
	fsys := providers(files...)
	return func(o *Options) { o.Push, o.Providers = nil, fsys }
}

// noSecretLogged fails t, once *h is stopped, if any line its server or
// store wrote holds the door secret. Call it before building the harness,
// so the check runs after its Release.
func noSecretLogged(t *testing.T, h **harness) {
	t.Cleanup(func() {
		if *h != nil && strings.Contains((*h).logs.text(), doorSecret) {
			t.Errorf("the door secret is in the log:\n%s", (*h).logs.text())
		}
	})
}

// registerOn registers name from session on host, on a work path of its
// own.
func (h *harness) registerOn(session, name, host string) {
	h.t.Helper()
	body := registerBody(name, "6h", "/workspace/"+name)
	body["host"] = host
	if r := h.as(session, http.MethodPost, "/v1/register", body); r.status != http.StatusCreated {
		h.t.Fatalf("register %s: %d %s", name, r.status, r.raw)
	}
}

func (h *harness) pushState(session string) string {
	h.t.Helper()
	return h.self(session, http.MethodGet, "/v1/me", nil).str("push")
}

func pushKey(id uint64, read, attempt int) string { return fmt.Sprintf("%d:%d:%d", id, read, attempt) }

func TestPush_any2xxRecordsTheNudgeWhateverItsBody(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	for _, tc := range []struct {
		name  string
		reply pushReply
	}{
		{"200 with a body", pushReply{status: http.StatusOK, body: `{"ok": false, "error": "not delivered"}`}},
		{"202", pushReply{status: http.StatusAccepted}},
		{"204", pushReply{status: http.StatusNoContent}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rcv := newPushReceiver(t, tc.reply)
			synctest.Test(t, func(t *testing.T) {
				h := newPushHarness(t, rcv.kiroDoor()...)
				h.mustRegisterAlone(sessA, "reader")
				h.mustRegisterAlone(sessB, "writer")
				h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
				synctest.Wait()

				if n := len(rcv.requests()); n != 1 {
					t.Fatalf("%d pushes, want 1", n)
				}
				if n := h.nudgesVia(string(store.NudgeByPush)); n != 1 {
					t.Errorf("%d nudge sent lines via push, want 1", n)
				}
				st, _, err := h.store.Nudging(t.Context(), h.idOf(sessA))
				if err != nil || st.Due != nil || !st.Next.Equal(time.Now().Add(3*time.Minute)) {
					t.Errorf("Nudging = %+v, %v; want the nudge outstanding, its re-nudge in 3 min", st, err)
				}
				if line := h.self(sessA, http.MethodGet, "/v1/me", nil).line(); !strings.Contains(line, "wake: host push") {
					t.Errorf("status line %q, want wake: host push", line)
				}
				if got := h.pushState(sessA); got != string(pushOK) {
					t.Errorf("push = %q, want ok", got)
				}
			})
		})
	}
}

func TestPush_theRequestCarriesTheSignalLineAndNothingOfThePost(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegister(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		const leaf = "the build fails when the cache is cold"
		r := h.as(sessB, http.MethodPost, "/v1/posts",
			postBody(map[string]any{"path": repoGrelot + "/" + leaf}, "subject words here", "body words here"))
		if r.status != http.StatusCreated {
			t.Fatalf("post: %d %s", r.status, r.raw)
		}
		synctest.Wait()
		seq := h.newestWake(sessA)

		reqs := rcv.requests()
		if len(reqs) != 1 {
			t.Fatalf("%d pushes, want 1", len(reqs))
		}
		got := reqs[0]
		var body map[string]string
		if err := json.Unmarshal([]byte(got.body), &body); err != nil {
			t.Fatalf("body %q: %v", got.body, err)
		}
		wantLine := fmt.Sprintf("[grelot:%d] grelot (automated, not the user): 1 new; run grelot inbox", int(seq))
		if body["line"] != wantLine {
			t.Errorf("line = %q, want %q", body["line"], wantLine)
		}
		if want := pushKey(h.idOf(sessA), 0, 1); body["key"] != want {
			t.Errorf("key = %q, want %q", body["key"], want)
		}
		if body["name"] != "reader" {
			t.Errorf("name = %q, want reader", body["name"])
		}
		if got.path != "/notify/session-a" {
			t.Errorf("path = %q, want the session id without its provider", got.path)
		}
		if a := got.header.Get("Authorization"); a != "Bearer "+doorSecret {
			t.Errorf("Authorization = %q, want the door's secret", a)
		}
		for _, leak := range []string{"subject words", "body words", "writer", "cache", "/workspace"} {
			all := got.path + got.query + got.body + strings.Join(flatHeaders(got.header), "\n")
			if strings.Contains(all, leak) {
				t.Errorf("the request carries %q: %+v", leak, got)
			}
		}
	})
}

func TestPush_matchServesItsProviderOnItsHostLabelOnly(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	synctest.Test(t, func(t *testing.T) {
		// The file's name is not the provider it serves.
		h := newPushHarness(t, "relayhost",
			doorFileAt(rcv.srv.URL, "relayhost", "door_token", "    match: {provider: kiro-cli, host: host-a/app}\n"))
		h.registerOn(sessA, "served", "host-a/app")
		h.registerOn(sessB, "elsewhere", "host-b/app")
		h.registerOn(sessOther, "otherkind", "host-a/app")
		h.registerOn(sessD, "writer", "host-b/app")
		for _, to := range []string{"served", "elsewhere", "otherkind"} {
			h.mustPost(sessD, map[string]any{"agent": to}, "hello")
		}
		synctest.Wait()

		reqs := rcv.requests()
		if len(reqs) != 1 || reqs[0].path != "/notify/session-a" {
			t.Fatalf("pushes = %+v, want one, to session-a", reqs)
		}
		for session, want := range map[string]string{sessA: "ok", sessB: "none", sessOther: "none"} {
			if got := h.pushState(session); got != want {
				t.Errorf("%s push = %q, want %q", session, got, want)
			}
		}
		if line := h.self(sessB, http.MethodGet, "/v1/me", nil).line(); !strings.Contains(line, "wake: down") {
			t.Errorf("unserved status line %q, want wake: down", line)
		}
	})
}

func TestPush_aQuietDeliveryIsNotPushed(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegister(sessA, "follower")
		h.mustRegister(sessD, "named")
		h.mustRegisterAlone(sessB, "writer")
		if r := h.as(sessB, http.MethodPost, "/v1/posts",
			postBody(map[string]any{"path": repoGrelot}, "Review", "@named: the routes are ready")); r.status != http.StatusCreated {
			t.Fatalf("post: %d %s", r.status, r.raw)
		}
		synctest.Wait()

		if reqs := rcv.requests(); len(reqs) != 1 || reqs[0].path != "/notify/session-d" {
			t.Errorf("pushes = %+v, want one, to the named session-d", reqs)
		}
		st, _, err := h.store.Nudging(t.Context(), h.idOf(sessA))
		if err != nil || st.Due != nil || !st.Next.IsZero() {
			t.Errorf("the follower's Nudging = %+v, %v; want no nudge due or outstanding", st, err)
		}
	})
}

func TestPush_aServedIdentitysStandingWaitOnlyHolds(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		wait := h.watchAsync(sessA, 55*time.Second)
		synctest.Wait()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()

		if n := len(rcv.requests()); n != 1 {
			t.Errorf("%d pushes, want 1", n)
		}
		if r := wait.answer(); r.status != http.StatusNoContent {
			t.Errorf("wait = %d %s, want 204 at its end", r.status, r.raw)
		}
		if n := h.sentByWatcher(); n != 0 {
			t.Errorf("%d nudges via watcher, want none", n)
		}
	})
}

func TestPush_aHoldThatShowsTheMailLeavesNothingToPush(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.stopHold(sessA, false)
		h.prompt(sessA, "turn=start")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()
		if r := h.stopHold(sessA, false); r.body["block"] != true {
			t.Fatalf("hold = %s, want it to block on the mail", r.raw)
		}
		synctest.Wait()

		if n := len(rcv.requests()); n != 0 {
			t.Errorf("%d pushes, want none for mail the hold showed", n)
		}
		if n := h.nudgesVia("hook"); n != 1 {
			t.Errorf("%d nudges via hook, want 1", n)
		}
	})
}

func TestPush_noFirstPushInsideATurnUntilItLapses(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.stopHold(sessA, false)
		h.prompt(sessA, "turn=start")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()
		if n := len(rcv.requests()); n != 0 {
			t.Fatalf("%d pushes inside the open turn, want none", n)
		}
		time.Sleep(turnLapse - time.Second)
		synctest.Wait()
		if n := len(rcv.requests()); n != 0 {
			t.Fatalf("%d pushes a second before the lapse, want none", n)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if got, want := rcv.keys(t), []string{pushKey(h.idOf(sessA), 0, 1)}; !equalStrings(got, want) {
			t.Errorf("keys at the lapse = %v, want %v", got, want)
		}
	})
}

func TestPush_noFirstPushForMailAnInTurnWaitReturnedUntilTheAckGraceEnds(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		in := h.inTurn(sessA, 55*time.Second)
		synctest.Wait()
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		if r := in.answer(); r.status != http.StatusOK {
			t.Fatalf("in-turn wait = %d %s", r.status, r.raw)
		}
		synctest.Wait()
		if n := len(rcv.requests()); n != 0 {
			t.Fatalf("%d pushes while the in-turn wait held the mail, want none", n)
		}
		time.Sleep(ackGrace - time.Second)
		synctest.Wait()
		if n := len(rcv.requests()); n != 0 {
			t.Fatalf("%d pushes inside the ack grace, want none", n)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if n := len(rcv.requests()); n != 1 {
			t.Errorf("%d pushes once the unacked grace ended, want 1", n)
		}
	})
}

func TestPush_theReNudgeIsPushedAtThreeMinutesWithAttempt2(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()
		id := h.idOf(sessA)
		time.Sleep(3*time.Minute - time.Second)
		synctest.Wait()
		if got, want := rcv.keys(t), []string{pushKey(id, 0, 1)}; !equalStrings(got, want) {
			t.Fatalf("keys before 3 min = %v, want %v", got, want)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if got, want := rcv.keys(t), []string{pushKey(id, 0, 1), pushKey(id, 0, 2)}; !equalStrings(got, want) {
			t.Errorf("keys at 3 min = %v, want %v", got, want)
		}
		if n := h.nudgesVia(string(store.NudgeByPush)); n != 2 {
			t.Errorf("%d nudges via push, want 2", n)
		}
	})
}

func TestPush_neverForADormantIdentityAnEndedSessionOrADelegate(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	t.Run("an ended session", func(t *testing.T) {
		rcv := newPushReceiver(t)
		synctest.Test(t, func(t *testing.T) {
			h := newPushHarness(t, rcv.kiroDoor()...)
			h.mustRegisterAlone(sessA, "reader")
			h.mustRegisterAlone(sessB, "writer")
			h.self(sessA, http.MethodPost, "/v1/lifecycle", map[string]any{"event": eventSessionEnd})
			h.directPost(sessB, "reader")
			synctest.Wait()
			if n := len(rcv.requests()); n != 0 {
				t.Errorf("%d pushes, want none", n)
			}
		})
	})
	t.Run("a dormant identity", func(t *testing.T) {
		rcv := newPushReceiver(t)
		synctest.Test(t, func(t *testing.T) {
			h := newPushHarness(t, rcv.kiroDoor()...)
			h.mustRegisterAlone(sessA, "reader")
			time.Sleep(7 * time.Hour)
			h.mustRegisterAlone(sessB, "writer")
			h.directPost(sessB, "reader")
			synctest.Wait()
			if n := len(rcv.requests()); n != 0 {
				t.Errorf("%d pushes, want none", n)
			}
		})
	})
	t.Run("a delegate", func(t *testing.T) {
		rcv := newPushReceiver(t)
		synctest.Test(t, func(t *testing.T) {
			h := newPushHarness(t, rcv.kiroDoor()...)
			h.mustRegisterAlone(sessA, "parent")
			h.mustRegisterAlone(sessB, "writer")
			g := h.self(sessA, http.MethodPost, "/v1/delegates",
				map[string]any{"label": "review", "ttl": "1h", "allow_agents": []string{"writer"}})
			if g.status != http.StatusCreated {
				t.Fatalf("grant: %d %s", g.status, g.raw)
			}
			if r := h.as(sessD, http.MethodPost, "/v1/register", map[string]any{"delegate": g.str("link"), "description": "reviewing"}); r.status != http.StatusCreated {
				t.Fatalf("register the delegate from its own session: %d %s", r.status, r.raw)
			}
			h.directPost(sessB, g.str("handle"))
			synctest.Wait()
			if n := len(rcv.requests()); n != 0 {
				t.Errorf("%d pushes, want none", n)
			}
		})
	})
}

func TestPush_aResumedSessionIsPushedTheMailThatWaited(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	for name, resume := range map[string]func(h *harness) *response{
		"GET /v1/me": func(h *harness) *response { return h.self(sessA, http.MethodGet, "/v1/me", nil) },
		"PATCH /v1/me": func(h *harness) *response {
			return h.as(sessA, http.MethodPatch, "/v1/me", map[string]any{"description": "resumed"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			rcv := newPushReceiver(t)
			synctest.Test(t, func(t *testing.T) {
				h := newPushHarness(t, rcv.kiroDoor()...)
				h.mustRegisterAlone(sessA, "reader")
				h.mustRegisterAlone(sessB, "writer")
				h.self(sessA, http.MethodPost, "/v1/lifecycle", map[string]any{"event": eventSessionEnd})
				h.directPost(sessB, "reader")
				synctest.Wait()
				if n := len(rcv.requests()); n != 0 {
					t.Fatalf("%d pushes while the session was ended, want none", n)
				}
				if r := resume(h); r.status != http.StatusOK {
					t.Fatalf("%s from the ended session: %d %s", name, r.status, r.raw)
				}
				synctest.Wait()
				if got, want := rcv.keys(t), []string{pushKey(h.idOf(sessA), 0, 1)}; !equalStrings(got, want) {
					t.Errorf("keys after the resume = %v, want %v", got, want)
				}
			})
		})
	}
}

func TestPush_aResumedSessionsPromptDefersTheMailThatWaitedToItsTurnEnd(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	for name, tc := range map[string]struct {
		end              func(h *harness)
		hookNudges, push int
	}{
		"stop hold": {end: func(h *harness) {
			if r := h.stopHold(sessA, false); r.body["block"] != true {
				t.Fatalf("hold = %s, want it to block on the mail", r.raw)
			}
		}, hookNudges: 1},
		"lapse": {end: func(*harness) { time.Sleep(turnLapse) }, push: 1},
	} {
		t.Run(name, func(t *testing.T) {
			rcv := newPushReceiver(t)
			synctest.Test(t, func(t *testing.T) {
				h := newPushHarness(t, rcv.kiroDoor()...)
				h.mustRegisterAlone(sessA, "reader")
				h.mustRegisterAlone(sessB, "writer")
				h.stopHold(sessA, false)
				h.self(sessA, http.MethodPost, "/v1/lifecycle", map[string]any{"event": eventSessionEnd})
				h.directPost(sessB, "reader")
				open := h.atEachWake(sessA, h.turnOpenNow, func() { h.prompt(sessA, "turn=start") })
				if len(open) == 0 || slices.Contains(open, false) {
					t.Fatalf("turn open at each wake of the reader during the prompt = %v, want at least one, all true", open)
				}
				synctest.Wait()
				if n := len(rcv.requests()); n != 0 {
					t.Fatalf("%d pushes into the resumed session's open turn, want none", n)
				}
				tc.end(h)
				synctest.Wait()
				if n := h.nudgesVia("hook"); n != tc.hookNudges {
					t.Errorf("%d nudges via hook at the turn's end, want %d", n, tc.hookNudges)
				}
				if n := len(rcv.requests()); n != tc.push {
					t.Errorf("%d pushes at the turn's end, want %d", n, tc.push)
				}
			})
		})
	}
}

// atEachWake runs during and returns, for each wake naming session's
// identity meanwhile, what read reported as the sender told of that wake
// would see it. The sender may run only after during returns, so its
// pushes alone cannot show a wake that came too early.
func (h *harness) atEachWake(session string, read func(id uint64) bool, during func()) []bool {
	h.t.Helper()
	id, hub := h.idOf(session), h.srv.hub
	hub.mu.Lock()
	sender := hub.listener
	hub.mu.Unlock()
	var mu sync.Mutex
	var got []bool
	hub.listen(func(ids []uint64) {
		if slices.Contains(ids, id) {
			v := read(id)
			mu.Lock()
			got = append(got, v)
			mu.Unlock()
		}
		sender(ids)
	})
	during()
	hub.listen(sender)
	mu.Lock()
	defer mu.Unlock()
	return got
}

func (h *harness) turnOpenNow(id uint64) bool {
	_, open := h.srv.presence.turnOpen(id, h.srv.now())
	return open
}

// firstNudgeHeld reports whether the first nudge due to id now waits for
// the hooks, as the sender reads it; false when none is due.
func (h *harness) firstNudgeHeld(id uint64) bool {
	st, found, err := h.store.Nudging(h.t.Context(), id)
	if err != nil {
		h.t.Errorf("Nudging(%d): %v", id, err)
	}
	if !found || st.Due == nil {
		return false
	}
	_, held := h.srv.deferred(id, st.Due)
	return held
}

func TestPush_aResumedSessionsInTurnWaitHoldsTheMailItReturnsForItsAck(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.self(sessA, http.MethodPost, "/v1/lifecycle", map[string]any{"event": eventSessionEnd})
		h.directPost(sessB, "reader")
		synctest.Wait()
		if n := len(rcv.requests()); n != 0 {
			t.Fatalf("%d pushes while the session was ended, want none", n)
		}
		start := time.Now()
		var got *response
		held := h.atEachWake(sessA, h.firstNudgeHeld, func() { got = h.inTurn(sessA, 55*time.Second).answer() })
		page := entriesOf(t, got)
		if len(page) != 1 || time.Since(start) != 0 {
			t.Fatalf("the in-turn wait = %v after %v, want the waiting mail at once", page, time.Since(start))
		}
		if len(held) == 0 || slices.Contains(held, false) {
			t.Fatalf("first nudge held at each wake of the reader during the wait = %v, want at least one, all true", held)
		}
		synctest.Wait()
		if n := len(rcv.requests()); n != 0 {
			t.Fatalf("%d pushes for the mail the in-turn wait returned, before its ack, want none", n)
		}
		h.ack(sessA, page[0]["seq"].(float64))
		time.Sleep(ackGrace)
		synctest.Wait()
		if n := len(rcv.requests()); n != 0 {
			t.Errorf("%d pushes once the mail was acked, want none", n)
		}
	})
}

func TestPush_aPromptTheDelegateGateRefusesOpensNoTurn(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.stopHold(sessA, false)
		if _, _, err := h.store.Delegate(t.Context(), store.Keyed{}, &store.Grant{Caller: store.Caller{Session: sessA}, Label: "tests", TTL: time.Hour}); err != nil {
			t.Fatal(err)
		}
		if r := h.as(sessA, http.MethodGet, "/v1/me?turn=start", nil); r.status != http.StatusConflict || r.str("code") != "delegate_gate" {
			t.Fatalf("prompt naming neither handle nor self: %d %s, want 409 delegate_gate", r.status, r.raw)
		}
		h.directPost(sessB, "reader")
		synctest.Wait()
		if got, want := rcv.keys(t), []string{pushKey(h.idOf(sessA), 0, 1)}; !equalStrings(got, want) {
			t.Errorf("keys after the refused prompt and new mail = %v, want %v at once", got, want)
		}
	})
}

func TestPush_aRevivalWithMailWaitingIsPushedAtOnce(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		time.Sleep(7 * time.Hour)
		h.mustRegisterAlone(sessB, "writer")
		h.directPost(sessB, "reader")
		synctest.Wait()
		if n := len(rcv.requests()); n != 0 {
			t.Fatalf("%d pushes while dormant, want none", n)
		}
		if r := h.self(sessA, http.MethodPost, "/v1/revive", reviveBody("", "6h")); r.status != http.StatusOK {
			t.Fatalf("revive: %d %s", r.status, r.raw)
		}
		synctest.Wait()
		if got, want := rcv.keys(t), []string{pushKey(h.idOf(sessA), 0, 1)}; !equalStrings(got, want) {
			t.Errorf("keys after the revival = %v, want %v", got, want)
		}
	})
}

func equalStrings(a, b []string) bool { return strings.Join(a, "\x00") == strings.Join(b, "\x00") }
