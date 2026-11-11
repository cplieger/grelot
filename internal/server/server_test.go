//go:build !windows

package server

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/store"
)

const (
	wantStopping = "grelot is starting or shutting down"
	wantStale    = "the store sweep has not committed for over a minute"
)

// healthz sends a bare /healthz, with no bearer and no version, and returns
// its status and reasons.
func (h *harness) healthz(method string) (int, []string) {
	h.t.Helper()
	r := h.request(method, "/healthz", nil, map[string]string{"Authorization": "", headerVersion: ""})
	var reasons []string
	if list, ok := r.body["reasons"].([]any); ok {
		for _, v := range list {
			reasons = append(reasons, v.(string))
		}
	}
	return r.status, reasons
}

func TestHealthz_takesNoHeadersAndFollowsTheSweeperBeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		// t.Run panics inside a bubble, so each phase names itself.
		want := func(phase string, code int, reasons ...string) {
			t.Helper()
			if got, gotReasons := h.healthz(http.MethodGet); got != code || !slices.Equal(gotReasons, reasons) {
				t.Errorf("%s: GET = %d %q, want %d %q", phase, got, gotReasons, code, reasons)
			}
			if got, _ := h.healthz(http.MethodHead); got != code {
				t.Errorf("%s: HEAD = %d, want %d", phase, got, code)
			}
		}

		want("before the first sweeper pass", http.StatusServiceUnavailable, wantStopping)
		h.srv.Beat(t.Context())
		want("after a pass", http.StatusOK)
		time.Sleep(4 * store.SweepEvery)
		want("at four sweep periods", http.StatusOK)
		time.Sleep(time.Nanosecond)
		want("past four sweep periods", http.StatusServiceUnavailable, wantStale)
		h.srv.Beat(t.Context())
		want("after the next pass", http.StatusOK)
	})
}

func TestHealthz_aReleaseAnswers503AtOnceAndForGood(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.srv.Beat(t.Context())
		h.srv.Release()
		if code, reasons := h.healthz(http.MethodGet); code != http.StatusServiceUnavailable || !slices.Equal(reasons, []string{wantStopping}) {
			t.Errorf("after Release: %d %q, want 503 %q", code, reasons, wantStopping)
		}
		h.srv.Beat(t.Context())
		if code, reasons := h.healthz(http.MethodGet); code != http.StatusServiceUnavailable || !slices.Equal(reasons, []string{wantStopping}) {
			t.Errorf("a pass after Release: %d %q, want 503 %q", code, reasons, wantStopping)
		}
	})
}

func TestAuthorize_bearer(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name, header string
	}{{"none", ""}, {"wrong token", "Bearer other"}, {"another scheme", "Basic " + testToken}} {
		t.Run(tc.name, func(t *testing.T) {
			r := h.request(http.MethodGet, "/v1/me", nil, map[string]string{"Authorization": tc.header, headerSession: sessA})
			if r.status != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", r.status)
			}
			if r.header.Get("WWW-Authenticate") == "" {
				t.Error("no WWW-Authenticate header")
			}
			if _, ok := r.body["path_root"]; ok {
				t.Error("a 401 carries the path root")
			}
			if r.header.Get(headerVersion) != "" {
				t.Error("a 401 names the server version")
			}
		})
	}
}

func TestAuthorize_version(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		version string
		want    int
	}{
		{"", http.StatusUpgradeRequired},
		{"v1.0.9", http.StatusUpgradeRequired},
		{"v1.1.0-rc.1", http.StatusUpgradeRequired},
		{"1.1.0", http.StatusUpgradeRequired},
		{"v1.1", http.StatusUpgradeRequired},
		{"dev", http.StatusOK},
		{"v1.1.0", http.StatusOK},
		{"v1.1.0-dev.3", http.StatusOK},
		{"v1.2.0", http.StatusOK},
		{"v2.0.0", http.StatusOK},
	} {
		t.Run("version "+tc.version, func(t *testing.T) {
			r := h.request(http.MethodGet, "/v1/me", nil, map[string]string{headerVersion: tc.version, headerSession: sessA})
			if r.status != tc.want {
				t.Fatalf("status %d, want %d: %s", r.status, tc.want, r.raw)
			}
			if got := r.header.Get(headerVersion); got != testVersion {
				t.Errorf("answer names server version %q, want %q", got, testVersion)
			}
			if tc.want == http.StatusUpgradeRequired && !strings.Contains(r.str("error"),
				"install grelot v1.1.0-dev.4 with go install github.com/cplieger/grelot@v1.1.0-dev.4") {
				t.Errorf("426 %q does not name the server's go install", r.str("error"))
			}
		})
	}
}

func TestVersionRoute(t *testing.T) {
	h := newHarness(t)
	t.Run("any caller reads the server's version", func(t *testing.T) {
		r := h.request(http.MethodGet, "/v1/version", nil, map[string]string{headerVersion: "v1.1.0"})
		if r.status != http.StatusOK || r.str("version") != testVersion || r.header.Get(headerVersion) != testVersion {
			t.Errorf("status %d, body %s, Grelot-Version %q; want 200 and %s in both", r.status, r.raw, r.header.Get(headerVersion), testVersion)
		}
	})
	t.Run("a client it does not speak gets the 426 naming grelot version", func(t *testing.T) {
		r := h.request(http.MethodGet, "/v1/version", nil, map[string]string{headerVersion: "v1.0.0"})
		if r.status != http.StatusUpgradeRequired || r.header.Get(headerVersion) != testVersion ||
			!strings.HasSuffix(r.str("error"), "go install github.com/cplieger/grelot@"+testVersion+", then check both with grelot version") {
			t.Errorf("status %d, error %q, Grelot-Version %q; want the 426 naming grelot version and the server's version", r.status, r.str("error"), r.header.Get(headerVersion))
		}
	})
}

func TestRoutes_unknownRouteIs404WithThePathRoot(t *testing.T) {
	h := newHarness(t)
	r := h.as(sessA, http.MethodGet, "/v1/nope", nil)
	if r.status != http.StatusNotFound || r.str("path_root") != protopath.DefaultRoot {
		t.Errorf("got %d %s, want 404 with path_root %s", r.status, r.raw, protopath.DefaultRoot)
	}
}

func TestDecode_bodyOver128KiBIs413(t *testing.T) {
	h := newHarness(t)
	body := registerBody("grelot-dev", "6h")
	body["description"] = strings.Repeat("x", 128<<10)
	if r := h.as(sessA, http.MethodPost, "/v1/register", body); r.status != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413: %s", r.status, r.raw)
	}
}

func TestSweeper_aServerAtRestLogsNoPeriodicLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		if r := h.as(sessA, http.MethodPost, "/v1/reservations", map[string]any{"path": "/workspace/reader/main.go", "reason": "editing", "ttl": "1h"}); r.status != http.StatusCreated {
			t.Fatalf("reserve: %d %s", r.status, r.raw)
		}
		// The first committed pass logs the one health transition, which is
		// an event, not a periodic line.
		h.srv.Beat(t.Context())
		before := len(h.logs.atLeast(slog.LevelDebug))

		ctx, cancel := context.WithCancel(t.Context())
		swept := make(chan error, 1)
		go func() { swept <- h.store.Run(ctx, func() { h.srv.Beat(ctx) }) }()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		cancel()
		if err := <-swept; err != nil {
			t.Fatalf("Run: %v", err)
		}

		if lines := h.logs.atLeast(slog.LevelDebug)[before:]; len(lines) != 0 {
			t.Errorf("%d lines over ten quiet minutes, the first %s %q, want none", len(lines), lines[0].Level, lines[0].Message)
		}
	})
}
