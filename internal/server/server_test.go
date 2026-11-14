//go:build !windows

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

const (
	wantStopping = "grelot is starting or shutting down"
	wantStale    = "the store sweep has not committed for over a minute"
)

// healthz sends a bare /healthz to the operations listener and returns its
// status and reasons.
func (h *harness) healthz(method string) (int, []string) {
	h.t.Helper()
	resp := h.opsCall(method, "/healthz")
	defer resp.Body.Close()
	var body struct{ Reasons []string }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Reasons
}

// The API listener serves neither operations route, whatever headers a
// request sends, and the operations listener serves nothing else.
func TestHandler_theOperationsRoutesAreOnTheirOwnListener(t *testing.T) {
	h := newHarness(t)
	h.srv.Beat(t.Context())
	for _, path := range []string{"/healthz", "/metrics"} {
		t.Run("the API answers 404 to "+path, func(t *testing.T) {
			if r := h.request(http.MethodGet, path, nil, nil); r.status != http.StatusNotFound {
				t.Errorf("GET %s on the API = %d, want 404", path, r.status)
			}
			if r := h.request(http.MethodGet, path, nil, map[string]string{"Authorization": "", headerVersion: ""}); r.status != http.StatusNotFound {
				t.Errorf("bare GET %s on the API = %d, want 404", path, r.status)
			}
		})
		t.Run("the operations listener answers 200 to "+path, func(t *testing.T) {
			resp := h.opsCall(http.MethodGet, path)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s on the operations listener = %d, want 200", path, resp.StatusCode)
			}
		})
	}
	for _, path := range []string{"/v1/me", "/v1/version", "/api/conversations", "/"} {
		t.Run("the operations listener answers 404 to "+path, func(t *testing.T) {
			resp := h.opsCall(http.MethodGet, path)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s on the operations listener = %d, want 404", path, resp.StatusCode)
			}
		})
	}
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
		h.srv.Release(t.Context())
		if code, reasons := h.healthz(http.MethodGet); code != http.StatusServiceUnavailable || !slices.Equal(reasons, []string{wantStopping}) {
			t.Errorf("after Release: %d %q, want 503 %q", code, reasons, wantStopping)
		}
		h.srv.Beat(t.Context())
		if code, reasons := h.healthz(http.MethodGet); code != http.StatusServiceUnavailable || !slices.Equal(reasons, []string{wantStopping}) {
			t.Errorf("a pass after Release: %d %q, want 503 %q", code, reasons, wantStopping)
		}
	})
}

func TestGate_refusesARequestWithNoValidMachineKey(t *testing.T) {
	h := newSignedIn(t)
	revoked, err := h.srv.auth.CreateKey(t.Context(), "retired")
	if err != nil {
		t.Fatal(err)
	}
	if r := h.request(http.MethodGet, "/v1/version", nil, map[string]string{"Authorization": "Bearer " + revoked}); r.status != http.StatusOK {
		t.Fatalf("GET /v1/version with the key before its revoke: %d %s, want 200", r.status, r.raw)
	}
	if err := h.srv.auth.RevokeKey(t.Context(), "retired"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, header string
	}{{"none", ""}, {"an unknown key", "Bearer grelot_unknown"}, {"another scheme", "Basic " + revoked}, {"a revoked key", "Bearer " + revoked}} {
		t.Run(tc.name, func(t *testing.T) {
			r := h.request(http.MethodGet, "/v1/me", nil, map[string]string{"Authorization": tc.header, headerSession: sessA})
			if r.status != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", r.status)
			}
			if got := r.header.Get("WWW-Authenticate"); !strings.HasPrefix(got, `Bearer realm="grelot"`) {
				t.Errorf("WWW-Authenticate %q, want the Bearer challenge of realm grelot", got)
			}
			if !strings.Contains(r.str("error"), "grelot key create") {
				t.Errorf("error %q, want it to name grelot key create", r.str("error"))
			}
			if _, ok := r.body["path_root"]; ok {
				t.Error("a 401 carries the path root")
			}
			if r.header.Get(headerVersion) != "" {
				t.Error("a 401 names the server version")
			}
		})
	}
	t.Run("a browser session", func(t *testing.T) {
		r := h.request(http.MethodGet, "/v1/me", nil, map[string]string{"Authorization": "", "Cookie": h.browser.header(), headerSession: sessA})
		if r.status != http.StatusForbidden || r.str("code") != "auth_key_required" {
			t.Errorf("GET /v1/me with the signed-in session cookie: %d %s, want 403 auth_key_required", r.status, r.raw)
		}
	})
}

func TestGate_aFailingAccountsFileAnswers503(t *testing.T) {
	a, db := testAuthDB(t)
	h := newHarnessWith(t, store.Options{}, withAuth(a))
	h.setUp()
	h.mustRegister(sessA, "alpha")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	r := h.as(sessA, http.MethodGet, "/v1/me", nil)

	if r.status != http.StatusServiceUnavailable || r.str("code") != "auth_unavailable" || r.header.Get("Retry-After") != "5" {
		t.Errorf("GET /v1/me with the accounts file closed: %d %s Retry-After %q, want 503 auth_unavailable, Retry-After 5",
			r.status, r.raw, r.header.Get("Retry-After"))
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

func TestRoutes_anUnknownRouteIs404AndAWrongMethod405(t *testing.T) {
	h := newHarness(t)
	if r := h.as(sessA, http.MethodGet, "/v1/nope", nil); r.status != http.StatusNotFound || r.str("code") != "auth_not_found" {
		t.Errorf("GET /v1/nope: %d %s, want 404 auth_not_found", r.status, r.raw)
	}
	r := h.as(sessA, http.MethodPut, "/v1/me", nil)
	if r.status != http.StatusMethodNotAllowed || r.str("code") != "auth_method_not_allowed" || r.header.Get("Allow") != "GET, HEAD, PATCH, DELETE" {
		t.Errorf("PUT /v1/me: %d %s Allow %q, want 405 auth_method_not_allowed allowing GET, HEAD, PATCH, DELETE", r.status, r.raw, r.header.Get("Allow"))
	}
	r = h.as(sessA, http.MethodPut, "/api/auth/me", nil)
	if r.status != http.StatusMethodNotAllowed || r.str("code") != "auth_method_not_allowed" || r.header.Get("Allow") != "GET, HEAD" {
		t.Errorf("PUT /api/auth/me: %d %s Allow %q, want 405 auth_method_not_allowed allowing GET, HEAD", r.status, r.raw, r.header.Get("Allow"))
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
