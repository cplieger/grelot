//go:build !windows

package server

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

// postBody is a post to to that changes no identity field.
func postBody(to any, subject, body string) map[string]any {
	return map[string]any{
		"to": to, "subject": subject, "body": body,
		"identity": map[string]any{"name": store.NoChange, "description": store.NoChange, "paths": []string{store.NoChange}},
	}
}

func TestPost_countsRecipientsByRelation(t *testing.T) {
	h := newHarness(t)
	checkout := []any{map[string]any{"repo": repoGrelot, "id": strings.Repeat("c", 32), "remote": strings.Repeat("e", 32)}}
	for _, a := range []struct {
		session, name, env string
		checkouts          []any
	}{
		{sessA, "sender", "a", checkout},
		{sessB, "same-checkout", "a", checkout},
		{sessC, "elsewhere", "b", []any{}},
	} {
		b := registerBody(a.name, "6h")
		w := where(a.env)
		w["checkouts"] = a.checkouts
		b["where"] = w
		if r := h.as(a.session, http.MethodPost, "/v1/register", b); r.status != http.StatusCreated {
			t.Fatalf("register %s: %d %s", a.name, r.status, r.raw)
		}
	}
	r := h.as(sessA, http.MethodPost, "/v1/posts", postBody(map[string]any{"path": repoGrelot}, "Start: the routes", "a body"))
	if r.status != http.StatusCreated {
		t.Fatalf("post: %d %s", r.status, r.raw)
	}
	got, _ := r.body["recipients"].(map[string]any)
	if got["total"] != 2.0 || got["same_environment"] != 1.0 || got["other"] != 1.0 {
		t.Errorf("recipients = %v, want 2 in all, the same-checkout one under same environment and 1 other", got)
	}
	if id, _ := r.body["message"].(float64); id == 0 {
		t.Errorf("message id missing: %s", r.raw)
	}
	if r.line() == "" {
		t.Errorf("no %s on the post's answer", headerStatus)
	}
}

func TestPost_logLineIsOneRedactedCappedLine(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	key, err := h.machineKey()
	if err != nil {
		t.Fatal(err)
	}
	body := "token " + key + "\nsecond line " + strings.Repeat("x", 5000)
	if r := h.as(sessA, http.MethodPost, "/v1/posts", postBody(map[string]any{"agent": "reader"}, "with "+key, body)); r.status != http.StatusCreated {
		t.Fatalf("post: %d %s", r.status, r.raw)
	}
	lines := h.logs.lines(slog.LevelInfo, "message posted")
	if len(lines) != 1 {
		t.Fatalf("message posted lines = %d, want 1", len(lines))
	}
	attrs := map[string]string{}
	lines[0].Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	if attrs["to"] != "reader" {
		t.Errorf("to = %q, want the direct post's recipient, reader", attrs["to"])
	}
	for _, k := range []string{"subject", "body"} {
		if strings.Contains(attrs[k], key) {
			t.Errorf("%s carries the request's machine key: %q", k, attrs[k])
		}
	}
	if b := attrs["body"]; len(b) > logTextCap || strings.Contains(b, "\n") || !strings.HasPrefix(b, "token ") {
		t.Errorf("body = %d bytes %q…, want one line of at most %d bytes", len(b), b[:min(len(b), 40)], logTextCap)
	}
	if attrs["thread_kind"] != "direct" || attrs["sender"] != "sender" || attrs["recipients"] != "1" {
		t.Errorf("line = %v, want a direct post from sender to 1 recipient", attrs)
	}
}

func TestPost_anAddressedPathPostAnswersWhoItWoke(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "named")
	h.mustRegister(sessC, "follower")
	body := postBody(map[string]any{"path": repoGrelot}, "Review the routes", "@named: the routes are ready")
	r := h.request(http.MethodPost, "/v1/posts", body, keyHeader(sessA, "addressed-post-key-1"))
	if r.status != http.StatusCreated {
		t.Fatalf("post: %d %s", r.status, r.raw)
	}
	got, _ := r.body["recipients"].(map[string]any)
	if woken, _ := got["woken"].([]any); got["total"] != 2.0 || len(woken) != 1 || woken[0] != "named" || got["quiet"] != 1.0 {
		t.Errorf("recipients = %v, want 2 in all: named woken, 1 quiet", got)
	}
	again, _ := h.request(http.MethodPost, "/v1/posts", body, keyHeader(sessA, "addressed-post-key-1")).body["recipients"].(map[string]any)
	if woken, _ := again["woken"].([]any); len(woken) != 1 || woken[0] != "named" || again["quiet"] != 1.0 {
		t.Errorf("the re-sent post's recipients = %v, want the first answer's split: named woken, 1 quiet", again)
	}

	plain := h.as(sessA, http.MethodPost, "/v1/posts", postBody(map[string]any{"path": repoGrelot}, "Status", "the routes are ready"))
	all, _ := plain.body["recipients"].(map[string]any)
	_, hasWoken := all["woken"]
	_, hasQuiet := all["quiet"]
	if all["total"] != 2.0 || hasWoken || hasQuiet {
		t.Errorf("an unaddressed post's recipients = %v, want 2 and no woken or quiet field", all)
	}

	var woken []string
	for _, line := range h.logs.lines(slog.LevelInfo, "message posted") {
		line.Attrs(func(a slog.Attr) bool {
			if a.Key == "woken" {
				woken = append(woken, a.Value.String())
			}
			return true
		})
	}
	if !slices.Equal(woken, []string{"1", "2"}) {
		t.Errorf("message posted woken = %v, want 1 for the addressed post and 2 for the other", woken)
	}
}

func TestPost_aDirectPostToADormantIdentityNamesItsDormancy(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	if r := h.as(sessB, http.MethodPost, "/v1/register", registerBody("sleeper", "15m")); r.status != http.StatusCreated {
		t.Fatalf("register: %d %s", r.status, r.raw)
	}
	h.clk.advance(16 * time.Minute)
	r := h.as(sessA, http.MethodPost, "/v1/posts", postBody(map[string]any{"agent": "sleeper"}, "hello", "a body"))
	if r.status != http.StatusCreated {
		t.Fatalf("post: %d %s", r.status, r.raw)
	}
	notices, _ := r.body["notices"].([]any)
	if len(notices) != 1 || !strings.Contains(notices[0].(string), "dormant") {
		t.Errorf("notices = %v, want the dormancy notice", notices)
	}
}

// grantDelegate grants session's identity a delegate labelled label and
// registers it from session, returning its handle.
func (h *harness) grantDelegate(session, label string) string {
	h.t.Helper()
	g := h.request(http.MethodPost, "/v1/delegates", map[string]any{"label": label, "ttl": "1h"},
		map[string]string{headerSession: session, headerSelf: "1"})
	if g.status != http.StatusCreated {
		h.t.Fatalf("grant: %d %s", g.status, g.raw)
	}
	r := h.as(session, http.MethodPost, "/v1/register", map[string]any{"delegate": g.str("link"), "description": "reviewing"})
	if r.status != http.StatusCreated {
		h.t.Fatalf("register the delegate: %d %s", r.status, r.raw)
	}
	return g.str("handle")
}

func TestPost_aDelegatesPostOutsideItsScopeIs403(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "parent")
	handle := h.grantDelegate(sessA, "review")
	r := h.request(http.MethodPost, "/v1/posts", postBody(map[string]any{"path": repoGrelot}, "hello", "a body"),
		map[string]string{headerSession: sessA, headerAs: handle})
	if r.status != http.StatusForbidden {
		t.Fatalf("post outside the scope: %d %s, want 403", r.status, r.raw)
	}
	if n := len(h.logs.lines(slog.LevelInfo, "post refused")); n != 1 {
		t.Errorf("post refused lines = %d, want 1", n)
	}
}

func TestPost_thirtyAMinuteADelegateCountingAsItsParent(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "parent")
	h.mustRegister(sessB, "reader")
	handle := h.grantDelegate(sessA, "review")
	self := map[string]string{headerSession: sessA, headerSelf: "1"}
	post := func(headers map[string]string, to, subject string) *response {
		return h.request(http.MethodPost, "/v1/posts", postBody(map[string]any{"agent": to}, subject, "a body"), headers)
	}
	if r := post(self, "reader", strings.Repeat("s", 121)); r.status != http.StatusBadRequest {
		t.Fatalf("over-long subject: %d %s, want 400", r.status, r.raw)
	}
	if r := post(map[string]string{headerSession: sessA, headerAs: handle}, "parent", "from the delegate"); r.status != http.StatusCreated {
		t.Fatalf("delegate post: %d %s", r.status, r.raw)
	}
	for i := range 29 {
		if r := post(self, "reader", "hello"); r.status != http.StatusCreated {
			t.Fatalf("post %d: %d %s", i+2, r.status, r.raw)
		}
	}
	r := post(self, "reader", "one too many")
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") != "60" {
		t.Fatalf("31st post: %d Retry-After %q %s, want 429 after 60 s", r.status, r.header.Get("Retry-After"), r.raw)
	}
	h.clk.advance(29600 * time.Millisecond)
	r = post(self, "reader", "still too many")
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") != "31" {
		t.Fatalf("post with the oldest 29.6 s old: %d Retry-After %q, want 429 after 31 s, rounded up", r.status, r.header.Get("Retry-After"))
	}
	h.clk.advance(31 * time.Second)
	if r := post(self, "reader", "a new minute"); r.status != http.StatusCreated {
		t.Errorf("post after the Retry-After: %d %s", r.status, r.raw)
	}
}

func TestRetryAfter_roundsUpWithAOneSecondFloor(t *testing.T) {
	for _, c := range []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "whole seconds", in: 60 * time.Second, want: 60 * time.Second},
		{name: "a fraction past a second", in: 30400 * time.Millisecond, want: 31 * time.Second},
		{name: "under a second", in: 200 * time.Millisecond, want: time.Second},
		{name: "nothing left", in: 0, want: time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := retryAfter(c.in); got != c.want {
				t.Errorf("retryAfter(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestStoreNearlyFull_aPostAndAFollowAre507(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	h.fill()
	r := h.as(sessA, http.MethodPost, "/v1/posts", postBody(map[string]any{"agent": "reader"}, "hello", "a body"))
	if r.status != http.StatusInsufficientStorage || r.str("code") != "store_full" {
		t.Errorf("post: %d %s, want 507 store_full", r.status, r.raw)
	}
	if f := h.as(sessB, http.MethodPut, "/v1/follows?path=/workspace/other", nil); f.status != http.StatusInsufficientStorage || f.str("code") != "store_full" {
		t.Errorf("follow: %d %s, want 507 store_full", f.status, f.raw)
	}
}

func TestPost_duringShutdownIs503WithNoErrorLine(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}

	r := h.as(sessA, http.MethodPost, "/v1/posts", postBody(map[string]any{"agent": "reader"}, "hello", "a body"))

	if r.status != http.StatusServiceUnavailable || r.str("code") != "shutting_down" {
		t.Errorf("post: %d %s, want 503 shutting_down", r.status, r.raw)
	}
	if n := len(h.logs.lines(slog.LevelError, "request failed")); n != 0 {
		t.Errorf("request failed ERROR lines = %d, want 0", n)
	}
}

func TestPost_refusesABadTargetOrReceiptList(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	for _, tc := range []struct {
		to     any
		notify any
		name   string
	}{
		{name: "an unknown word", to: "everyone"},
		{name: "both a path and an agent", to: map[string]any{"path": repoGrelot, "agent": "reader"}},
		{name: "no target", to: map[string]any{}},
		{name: "a receipt list of another shape", to: map[string]any{"agent": "reader"}, notify: 5},
		{name: "a receipt name that is not a recipient", to: map[string]any{"agent": "reader"}, notify: []string{"sender"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := postBody(tc.to, "hello", "a body")
			if tc.notify != nil {
				b["notify_read"] = tc.notify
			}
			if r := h.as(sessA, http.MethodPost, "/v1/posts", b); r.status != http.StatusBadRequest {
				t.Errorf("post: %d %s, want 400", r.status, r.raw)
			}
		})
	}
	b := postBody(map[string]any{"agent": "reader"}, "hello", "a body")
	b["notify_read"], b["by"] = "all", "2h"
	if r := h.as(sessA, http.MethodPost, "/v1/posts", b); r.status != http.StatusCreated {
		t.Errorf("notify_read all with by: %d %s", r.status, r.raw)
	}
}
