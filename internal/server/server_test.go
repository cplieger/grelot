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

func TestHealthz_takesNoHeadersAndFollowsTheSweeperBeat(t *testing.T) {
	h := newHarness(t)
	type answer struct {
		Status  string   `json:"status"`
		Reasons []string `json:"reasons"`
	}
	get := func(t *testing.T, method string) (int, http.Header, answer) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, h.http.URL+"/healthz", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := h.http.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body answer
		if method == http.MethodGet {
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, resp.Header, body
	}
	wantUp := func(t *testing.T) {
		t.Helper()
		code, _, body := get(t, http.MethodGet)
		if code != http.StatusOK || body.Status != "ok" || body.Reasons != nil {
			t.Errorf("got %d %+v, want 200 ok with no reasons", code, body)
		}
	}
	wantDown := func(t *testing.T) {
		t.Helper()
		code, _, body := get(t, http.MethodGet)
		want := []string{"the store sweep has not committed for over a minute"}
		if code != http.StatusServiceUnavailable || body.Status != "unhealthy" || !slices.Equal(body.Reasons, want) {
			t.Errorf("got %d %+v, want 503 unhealthy %q", code, body, want)
		}
	}

	t.Run("down before the first sweeper pass", wantDown)
	h.srv.Beat(t.Context())
	t.Run("up after a pass, with no bearer and no version", wantUp)
	t.Run("the fleet headers", func(t *testing.T) {
		_, header, _ := get(t, http.MethodGet)
		for k, v := range map[string]string{
			"Content-Type": "application/json", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff",
		} {
			if got := header.Get(k); got != v {
				t.Errorf("%s = %q, want %q", k, got, v)
			}
		}
	})
	t.Run("HEAD answers the same status", func(t *testing.T) {
		if code, _, _ := get(t, http.MethodHead); code != http.StatusOK {
			t.Errorf("HEAD = %d, want 200", code)
		}
	})
	h.clk.advance(4 * store.SweepEvery)
	t.Run("still up at four sweep periods", wantUp)
	h.clk.advance(time.Second)
	t.Run("down once the last pass is older than four sweep periods", wantDown)
	t.Run("HEAD answers down too", func(t *testing.T) {
		if code, _, _ := get(t, http.MethodHead); code != http.StatusServiceUnavailable {
			t.Errorf("HEAD = %d, want 503", code)
		}
	})
	h.srv.Beat(t.Context())
	t.Run("up again after the next pass", wantUp)
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
			if tc.want == http.StatusUpgradeRequired && !strings.Contains(r.str("error"),
				"install grelot v1.1.0 from https://github.com/cplieger/grelot/releases/tag/v1.1.0") {
				t.Errorf("426 %q does not name the server's release download", r.str("error"))
			}
		})
	}
}

func TestRoutes_unknownRouteIs404WithThePathRoot(t *testing.T) {
	h := newHarness(t)
	r := h.as(sessA, http.MethodGet, "/v1/nope", nil)
	if r.status != http.StatusNotFound || r.str("path_root") != DefaultPathRoot {
		t.Errorf("got %d %s, want 404 with path_root %s", r.status, r.raw, DefaultPathRoot)
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
