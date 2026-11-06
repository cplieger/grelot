package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

// clearConfigEnv unsets every key LoadConfig reads.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GRELOT_TOKEN", "GRELOT_TOKEN_FILE", "GRELOT_PATH_ROOT", "STORE_PATH",
		"STORE_MAX_BYTES", "THREAD_TTL", "DORMANT_TTL", "RESERVATION_MAX_TTL", "PROVIDERS_DIR",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadConfig_defaults(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("GRELOT_TOKEN", "test-token")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Token: "test-token", PathRoot: "/workspace", StorePath: "/data/grelot.db",
		MaxBytes: 512 << 20, ThreadTTL: 72 * time.Hour, DormantTTL: 72 * time.Hour, ReservationMaxTTL: 8 * time.Hour,
		ProvidersDir: "/providers",
	}
	if c != want {
		t.Errorf("config %+v, want %+v", c, want)
	}
	if want.MaxBytes != store.DefaultMaxBytes {
		t.Errorf("the default ceiling is not the store's")
	}
}

func TestLoadConfig_readsEachKey(t *testing.T) {
	clearConfigEnv(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRELOT_TOKEN_FILE", tokenFile)
	t.Setenv("GRELOT_PATH_ROOT", "/src")
	t.Setenv("STORE_PATH", "/var/lib/grelot/g.db")
	t.Setenv("STORE_MAX_BYTES", "1048576")
	t.Setenv("THREAD_TTL", "24h")
	t.Setenv("DORMANT_TTL", "12h")
	t.Setenv("RESERVATION_MAX_TTL", "2h")
	t.Setenv("PROVIDERS_DIR", "/etc/grelot/providers")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Token: "file-token", PathRoot: "/src", StorePath: "/var/lib/grelot/g.db",
		MaxBytes: 1 << 20, ThreadTTL: 24 * time.Hour, DormantTTL: 12 * time.Hour, ReservationMaxTTL: 2 * time.Hour,
		ProvidersDir: "/etc/grelot/providers",
	}
	if c != want {
		t.Errorf("config %+v, want %+v", c, want)
	}
}

func TestLoadConfig_refusalsNameTheKey(t *testing.T) {
	for _, tc := range []struct {
		key, value string
	}{
		{"GRELOT_TOKEN", ""},
		{"GRELOT_PATH_ROOT", "workspace"},
		{"GRELOT_PATH_ROOT", "/workspace/"},
		{"GRELOT_PATH_ROOT", "/workspace/../src"},
		{"STORE_MAX_BYTES", "lots"},
		{"STORE_MAX_BYTES", "0"},
		{"THREAD_TTL", "72"},
		{"DORMANT_TTL", "-1h"},
		{"RESERVATION_MAX_TTL", "soon"},
		{"PROVIDERS_DIR", "providers"},
		{"PROVIDERS_DIR", "/providers/"},
		{"PROVIDERS_DIR", "/srv/../providers"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("GRELOT_TOKEN", "a-secret-value")
			t.Setenv(tc.key, tc.value)
			_, err := LoadConfig()
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("err %v, want one naming %s", err, tc.key)
			}
			if strings.Contains(err.Error(), "a-secret-value") {
				t.Errorf("err %q carries the token", err)
			}
		})
	}
}

// A fresh file is bbolt's first 16 MiB growth step, nearly all unwritten,
// so the ceiling must count only the pages written.
func TestStoreMaxBytes_aSmallCeilingTakesPostsOnAFreshStore(t *testing.T) {
	h := newHarnessWith(t, store.Options{MaxBytes: 16 << 20})
	h.mustRegister(sessA, "writer")
	h.mustRegister(sessB, "reader")
	r := h.as(sessA, http.MethodPost, "/v1/posts", postBody(map[string]any{"agent": "reader"}, "first", "a body"))
	if r.status != http.StatusCreated {
		t.Errorf("first post on a fresh store under a 16 MiB ceiling: %d %s, want 201", r.status, r.raw)
	}
}
