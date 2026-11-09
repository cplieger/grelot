//go:build !windows

package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

// slowCommits is a store Woke hook that, once armed, holds each commit
// that wakes an identity for delay before the write returns.
type slowCommits struct {
	armed atomic.Bool
	delay time.Duration
}

func (s *slowCommits) woke([]uint64) {
	if s.armed.Load() {
		time.Sleep(s.delay)
	}
}

// pauseNext is a server clock that, once armed, holds the next request to
// read it until release closes. A post first reads the server clock at its
// rate-limit take, after its replay lookup missed.
type pauseNext struct {
	now     func() time.Time
	paused  chan struct{}
	release chan struct{}
	freed   sync.Once
	armed   atomic.Bool
}

// free lets the held request go on, and may run more than once: a test
// registers it as a cleanup, since httptest.Server.Close waits for a held
// request.
func (p *pauseNext) free() { p.freed.Do(func() { close(p.release) }) }

func (p *pauseNext) read() time.Time {
	if p.armed.CompareAndSwap(true, false) {
		close(p.paused)
		<-p.release
	}
	return p.now()
}

// newInProcessHarness is a harness that serves each request in process,
// so a request's own context deadline sits under the route's.
func newInProcessHarness(t *testing.T, o store.Options) *harness {
	t.Helper()
	clk := &clock{t: testNow}
	h := buildHarness(t, o, clk.now)
	h.clk = clk
	return h
}

// keyHeader is the Idempotency-Key header carrying key, quoted.
func keyHeader(session, key string) map[string]string {
	return map[string]string{headerSession: session, headerKey: `"` + key + `"`}
}

// postsTo counts the posts with subject that reached session's history.
func (h *harness) postsTo(session, subject string) int {
	h.t.Helper()
	r := h.as(session, http.MethodGet, "/v1/history", nil)
	if r.status != http.StatusOK {
		h.t.Fatalf("history: %d %s", r.status, r.raw)
	}
	entries, _ := r.body["entries"].([]any)
	n := 0
	for _, e := range entries {
		if m, _ := e.(map[string]any); m["subject"] == subject {
			n++
		}
	}
	return n
}

func TestKeyedPost_pastItsDeadlineIs503TimeoutWithNothingStored(t *testing.T) {
	h := newInProcessHarness(t, store.Options{})
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	r := h.requestCtx(ctx, http.MethodPost, "/v1/posts", postBody(map[string]any{"agent": "reader"}, "late", "a body"),
		map[string]string{headerSession: sessA})

	if r.status != http.StatusServiceUnavailable || r.str("code") != "timeout" || r.header.Get("Retry-After") != "1" {
		t.Errorf("post past its deadline: %d Retry-After %q %s, want 503 timeout after 1 s", r.status, r.header.Get("Retry-After"), r.raw)
	}
	if !strings.Contains(r.str("error"), "nothing changed") {
		t.Errorf("error = %q, want it to say nothing changed", r.str("error"))
	}
	if n := h.postsTo(sessB, "late"); n != 0 {
		t.Errorf("reader's history holds %d of the post, want 0", n)
	}
}

func TestKeyedPost_committedAsItsDeadlinePassesIs201WithItsStatusLine(t *testing.T) {
	slow := &slowCommits{delay: 300 * time.Millisecond}
	h := newInProcessHarness(t, store.Options{Woke: slow.woke})
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	slow.armed.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	r := h.requestCtx(ctx, http.MethodPost, "/v1/posts", postBody(map[string]any{"agent": "reader"}, "slow", "a body"),
		map[string]string{headerSession: sessA})

	slow.armed.Store(false)
	if ctx.Err() == nil {
		t.Fatal("the request's deadline had not passed when it was answered: the fixture did not hold the commit")
	}
	if r.status != http.StatusCreated || r.line() == "" {
		t.Errorf("post committed past its deadline: %d status line %q %s, want 201 with a status line", r.status, r.line(), r.raw)
	}
	if n := h.postsTo(sessB, "slow"); n != 1 {
		t.Errorf("reader's history holds %d of the post, want 1", n)
	}
}

func TestEventHold_committedAsItsDeadlinePassesIs200(t *testing.T) {
	slow := &slowCommits{delay: 300 * time.Millisecond}
	h := newInProcessHarness(t, store.Options{Woke: slow.woke})
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	h.directPost(sessA, "reader")
	slow.armed.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	r := h.requestCtx(ctx, http.MethodPost, "/v1/events/hold", map[string]any{"for": "stop"}, map[string]string{headerSession: sessB})

	slow.armed.Store(false)
	if ctx.Err() == nil {
		t.Fatal("the request's deadline had not passed when it was answered: the fixture did not hold the commit")
	}
	if r.status != http.StatusOK || r.body["block"] != true {
		t.Errorf("hold committed past its deadline: %d %s, want 200 blocking for the wake mail", r.status, r.raw)
	}
}

func TestKeyedPost_reSentAnswersItsFirstAnswerAndStoresOnce(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	body := postBody(map[string]any{"agent": "reader"}, "once", "a body")

	first := h.request(http.MethodPost, "/v1/posts", body, keyHeader(sessA, "post-key-0000001"))
	again := h.request(http.MethodPost, "/v1/posts", body, keyHeader(sessA, "post-key-0000001"))

	if first.status != http.StatusCreated || again.status != http.StatusCreated {
		t.Fatalf("post and re-send: %d, %d %s, want 201 twice", first.status, again.status, again.raw)
	}
	if again.raw != first.raw {
		t.Errorf("re-send answered\n%s\nwant the first answer\n%s", again.raw, first.raw)
	}
	if again.line() == "" {
		t.Errorf("no %s on the re-send's answer", headerStatus)
	}
	if n := h.postsTo(sessB, "once"); n != 1 {
		t.Errorf("reader's history holds %d of the post, want 1", n)
	}
	if n := len(h.logs.lines(slog.LevelInfo, "message posted")); n != 1 {
		t.Errorf("message posted lines = %d, want 1", n)
	}
}

func TestKeyedWrite_aKeySentWithAnotherRequestIs422(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	const key = "reused-key-00001"
	if r := h.request(http.MethodPost, "/v1/posts", postBody(map[string]any{"agent": "reader"}, "first", "a body"), keyHeader(sessA, key)); r.status != http.StatusCreated {
		t.Fatalf("post: %d %s", r.status, r.raw)
	}
	for _, c := range []struct {
		body         any
		name, method string
		path         string
	}{
		{name: "another body", method: http.MethodPost, path: "/v1/posts", body: postBody(map[string]any{"agent": "reader"}, "second", "a body")},
		{name: "another route", method: http.MethodPost, path: "/v1/inbox/ack", body: map[string]any{"upto": 1}},
		{name: "another query", method: http.MethodPost, path: "/v1/posts?x=1", body: postBody(map[string]any{"agent": "reader"}, "first", "a body")},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := h.request(c.method, c.path, c.body, keyHeader(sessA, key))
			if r.status != http.StatusUnprocessableEntity || r.str("code") != "key_reused" {
				t.Errorf("%s %s with a used key: %d %s, want 422 key_reused", c.method, c.path, r.status, r.raw)
			}
		})
	}
	if n := h.postsTo(sessB, "second"); n != 0 {
		t.Errorf("reader's history holds %d of the refused post, want 0", n)
	}
}

func TestKeyedWrite_aMissingOrMalformedKeyIs400(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	body := postBody(map[string]any{"agent": "reader"}, "unkeyed", "a body")
	for _, c := range []struct{ name, key string }{
		{name: "missing", key: ""},
		{name: "unquoted", key: "abcdefghijklmnop"},
		{name: "15 characters", key: `"abcdefghijklmno"`},
		{name: "65 characters", key: `"` + strings.Repeat("a", 65) + `"`},
		{name: "a character outside the set", key: `"abcdefghijklmno+"`}, // gitleaks:allow
		{name: "a list of two", key: `"abcdefghijklmnop", "bcdefghijklmnopq"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := h.request(http.MethodPost, "/v1/posts", body, map[string]string{headerSession: sessA, headerKey: c.key})
			if r.status != http.StatusBadRequest || !strings.Contains(r.str("error"), "Idempotency-Key") {
				t.Errorf("post with key %q: %d %s, want 400 naming the Idempotency-Key", c.key, r.status, r.raw)
			}
		})
	}
	for _, c := range []struct{ name, key string }{
		{name: "16 characters", key: `"abcdefghijklmnop"`},
		{name: "64 characters of every kind", key: `"` + strings.Repeat("aZ0-_", 12) + `abcd"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if r := h.request(http.MethodPost, "/v1/posts", body, map[string]string{headerSession: sessA, headerKey: c.key}); r.status != http.StatusCreated {
				t.Errorf("post with key %s: %d %s, want 201", c.key, r.status, r.raw)
			}
		})
	}
	if n := h.postsTo(sessB, "unkeyed"); n != 2 {
		t.Errorf("reader's history holds %d of the post, want only the 2 sent with a valid key", n)
	}
}

func TestUnkeyedRoute_aKeyIs400(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	for _, c := range []struct {
		body         any
		method, path string
	}{
		{method: http.MethodPost, path: "/v1/events/wait", body: map[string]any{"wait": "1s"}},
		{method: http.MethodPost, path: "/v1/events/hold", body: map[string]any{"for": "stop"}},
		{method: http.MethodPost, path: "/v1/lifecycle", body: map[string]any{"event": "turn_end"}},
		{method: http.MethodGet, path: "/v1/me"},
	} {
		t.Run(strings.TrimPrefix(c.path, "/v1/"), func(t *testing.T) {
			r := h.request(c.method, c.path, c.body, keyHeader(sessA, "unwanted-key-0001"))
			if r.status != http.StatusBadRequest || !strings.Contains(r.str("error"), "takes no Idempotency-Key") {
				t.Errorf("%s %s with a key: %d %s, want 400", c.method, c.path, r.status, r.raw)
			}
		})
	}
}

func TestKeyedPost_reSentWithTheRateBucketEmptyAnswersItsFirstResult(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	body := postBody(map[string]any{"agent": "reader"}, "hello", "a body")
	first := h.request(http.MethodPost, "/v1/posts", body, keyHeader(sessA, "first-post-key-01"))
	if first.status != http.StatusCreated {
		t.Fatalf("first post: %d %s", first.status, first.raw)
	}
	for i := range postsPerMinute - 1 {
		if r := h.as(sessA, http.MethodPost, "/v1/posts", body); r.status != http.StatusCreated {
			t.Fatalf("post %d: %d %s", i+2, r.status, r.raw)
		}
	}
	if r := h.as(sessA, http.MethodPost, "/v1/posts", body); r.status != http.StatusTooManyRequests {
		t.Fatalf("post %d: %d %s, want 429 with the bucket empty", postsPerMinute+1, r.status, r.raw)
	}

	again := h.request(http.MethodPost, "/v1/posts", body, keyHeader(sessA, "first-post-key-01"))

	if again.status != http.StatusCreated || again.raw != first.raw {
		t.Errorf("re-send with the bucket empty: %d\n%s\nwant 201 with the first answer\n%s", again.status, again.raw, first.raw)
	}
}

func TestKeyedPost_aReSendTakesNoRateLimitToken(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	body := postBody(map[string]any{"agent": "reader"}, "hello", "a body")
	if r := h.request(http.MethodPost, "/v1/posts", body, keyHeader(sessA, "resent-post-key-1")); r.status != http.StatusCreated {
		t.Fatalf("first post: %d %s", r.status, r.raw)
	}
	for range 3 {
		if r := h.request(http.MethodPost, "/v1/posts", body, keyHeader(sessA, "resent-post-key-1")); r.status != http.StatusCreated {
			t.Fatalf("re-send: %d %s", r.status, r.raw)
		}
	}
	for i := range postsPerMinute - 1 {
		if r := h.as(sessA, http.MethodPost, "/v1/posts", body); r.status != http.StatusCreated {
			t.Fatalf("post %d of the minute after 3 re-sends: %d %s, want 201", i+2, r.status, r.raw)
		}
	}
	if r := h.as(sessA, http.MethodPost, "/v1/posts", body); r.status != http.StatusTooManyRequests {
		t.Errorf("post %d of the minute: %d %s, want 429", postsPerMinute+1, r.status, r.raw)
	}
}

func TestKeyedPost_aConcurrentReSendGivesItsRateLimitTokenBack(t *testing.T) {
	pause := &pauseNext{paused: make(chan struct{}), release: make(chan struct{})}
	h := newHarnessWith(t, store.Options{}, func(o *Options) { pause.now, o.Now = o.Now, pause.read })
	t.Cleanup(pause.free)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	body := postBody(map[string]any{"agent": "reader"}, "raced", "a body")
	headers := keyHeader(sessA, "raced-post-key-01")
	pause.armed.Store(true)
	second := h.async(http.MethodPost, "/v1/posts", body, headers)
	select {
	case <-pause.paused:
	case res := <-second.answers:
		second.got(res)
		t.Fatal("the re-send answered before it reached the rate-limit take")
	case <-time.After(10 * time.Second):
		t.Fatal("the re-send never reached the rate-limit take")
	}

	first := h.request(http.MethodPost, "/v1/posts", body, headers)
	pause.free()
	var again *response
	select {
	case res := <-second.answers:
		again = second.got(res)
	case <-time.After(10 * time.Second):
		t.Fatal("the re-send held between its replay lookup and its write never answered")
	}

	if first.status != http.StatusCreated || again.status != http.StatusCreated || again.raw != first.raw {
		t.Fatalf("post and its concurrent re-send: %d %s and %d %s, want 201 twice with one answer", first.status, first.raw, again.status, again.raw)
	}
	if n := h.postsTo(sessB, "raced"); n != 1 {
		t.Errorf("reader's history holds %d of the post, want 1", n)
	}
	for i := range postsPerMinute - 1 {
		if r := h.as(sessA, http.MethodPost, "/v1/posts", body); r.status != http.StatusCreated {
			t.Fatalf("post %d of the minute after a concurrent re-send: %d %s, want 201", i+2, r.status, r.raw)
		}
	}
	if r := h.as(sessA, http.MethodPost, "/v1/posts", body); r.status != http.StatusTooManyRequests {
		t.Errorf("post %d of the minute: %d %s, want 429", postsPerMinute+1, r.status, r.raw)
	}
}

func TestKeyedWrites_reSendAnswersTheFirstAnswerWhereARunWouldRefuse(t *testing.T) {
	resend := func(t *testing.T, h *harness, method, path string, body any, headers map[string]string) (first, again *response) {
		t.Helper()
		first = h.request(method, path, body, headers)
		again = h.request(method, path, body, headers)
		if again.status != first.status || again.raw != first.raw {
			t.Errorf("%s %s re-sent: %d %s, want the first answer %d %s", method, path, again.status, again.raw, first.status, first.raw)
		}
		return first, again
	}
	t.Run("register", func(t *testing.T) {
		h := newHarness(t)
		first, _ := resend(t, h, http.MethodPost, "/v1/register", registerBody("alpha", "6h"), keyHeader(sessA, "register-key-0001"))
		if first.status != http.StatusCreated {
			t.Errorf("register: %d %s, want 201", first.status, first.raw)
		}
	})
	t.Run("retire", func(t *testing.T) {
		h := newHarness(t)
		h.mustRegister(sessA, "alpha")
		first, _ := resend(t, h, http.MethodDelete, "/v1/me", nil, keyHeader(sessA, "retire-key-00001"))
		if first.status != http.StatusOK {
			t.Errorf("retire: %d %s, want 200, and the re-send 200, not 410", first.status, first.raw)
		}
	})
	t.Run("release", func(t *testing.T) {
		h := newHarness(t)
		h.mustRegister(sessA, "alpha")
		g := h.as(sessA, http.MethodPost, "/v1/reservations", map[string]any{"path": repoGrelot + "/main.go", "reason": "editing", "ttl": "1h"})
		if g.status != http.StatusCreated {
			t.Fatalf("reserve: %d %s", g.status, g.raw)
		}
		id := g.body["reservation"].(map[string]any)["id"].(float64)
		first, _ := resend(t, h, http.MethodDelete, "/v1/reservations/"+strconv.FormatUint(uint64(id), 10), nil, keyHeader(sessA, "release-key-0001"))
		if first.status != http.StatusOK {
			t.Errorf("release: %d %s, want 200, and the re-send 200, not 404", first.status, first.raw)
		}
	})
	t.Run("reserve", func(t *testing.T) {
		h := newHarness(t)
		h.mustRegister(sessA, "alpha")
		body := map[string]any{"path": repoGrelot + "/main.go", "reason": "editing", "ttl": "1h"}
		resend(t, h, http.MethodPost, "/v1/reservations", body, keyHeader(sessA, "reserve-key-0001"))
		r := h.as(sessA, http.MethodGet, "/v1/reservations", nil)
		if got, _ := r.body["reservations"].([]any); len(got) != 1 {
			t.Errorf("reservations = %s, want the one grant", r.raw)
		}
	})
	t.Run("grant", func(t *testing.T) {
		h := newHarness(t)
		h.mustRegister(sessA, "alpha")
		headers := keyHeader(sessA, "grant-key-000001")
		headers[headerSelf] = "1"
		first, _ := resend(t, h, http.MethodPost, "/v1/delegates", map[string]any{"label": "review", "ttl": "1h"}, headers)
		if first.status != http.StatusCreated || first.str("link") == "" {
			t.Errorf("grant: %d %s, want 201 with a link", first.status, first.raw)
		}
	})
}

// A renewal alone needs no key: a re-sent one only moves the expiry, so a
// client older than the key rule still renews; every other change to the
// identity still needs its key.
func TestPatchMe_aRenewalAloneTakesNoKey(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "renewer")
	noKey := map[string]string{headerSession: sessA, headerKey: ""}
	for _, c := range []struct {
		body any
		name string
		want int
	}{
		{name: "ttl and where", body: map[string]any{"ttl": "3h", "where": where("a")}, want: http.StatusOK},
		{name: "sent again", body: map[string]any{"ttl": "4h", "where": where("a")}, want: http.StatusOK},
		{name: "ttl and a description", body: map[string]any{"ttl": "4h", "description": "other work"}, want: http.StatusBadRequest},
		{name: "a description", body: map[string]any{"description": "other work"}, want: http.StatusBadRequest},
		{name: "not an object", body: []string{"ttl"}, want: http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := h.request(http.MethodPatch, "/v1/me", c.body, noKey)
			if r.status != c.want {
				t.Fatalf("%d %s, want %d", r.status, r.raw, c.want)
			}
			if c.want == http.StatusBadRequest && !strings.Contains(r.str("error"), "requires an Idempotency-Key") {
				t.Errorf("refusal %q, want it to name the missing key", r.str("error"))
			}
		})
	}
	me := h.as(sessA, http.MethodGet, "/v1/me", nil)
	if got := me.body["identity"].(map[string]any)["description"]; got != "building the HTTP API" {
		t.Errorf("description %v, want the unkeyed change refused", got)
	}
	if got := me.body["identity"].(map[string]any)["ttl"]; got != "4h0m0s" {
		t.Errorf("ttl %v, want the last renewal's 4h0m0s", got)
	}
}
