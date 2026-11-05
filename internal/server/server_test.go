package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

func TestHealthz_takesNoHeadersAndFollowsTheSweeperBeat(t *testing.T) {
	h := newHarness(t)
	get := func() (int, string) {
		resp, err := h.http.Client().Get(h.http.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct{ Status string }
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body.Status
	}

	t.Run("down before the first sweeper pass", func(t *testing.T) {
		if code, st := get(); code != http.StatusServiceUnavailable || st != "down" {
			t.Errorf("got %d %q, want 503 down", code, st)
		}
	})
	h.srv.Beat()
	t.Run("up after a pass, with no bearer and no version", func(t *testing.T) {
		if code, st := get(); code != http.StatusOK || st != "up" {
			t.Errorf("got %d %q, want 200 up", code, st)
		}
	})
	h.clk.advance(4 * store.SweepEvery)
	t.Run("still up at four sweep periods", func(t *testing.T) {
		if code, _ := get(); code != http.StatusOK {
			t.Errorf("got %d, want 200", code)
		}
	})
	h.clk.advance(time.Second)
	t.Run("down once the last pass is older than four sweep periods", func(t *testing.T) {
		if code, st := get(); code != http.StatusServiceUnavailable || st != "down" {
			t.Errorf("got %d %q, want 503 down", code, st)
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
