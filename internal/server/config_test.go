//go:build !windows

package server

import (
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/auth"
	"github.com/cplieger/grelot/internal/store"
)

// clearConfigEnv unsets every key LoadConfig and RetiredSettings read.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GRELOT_TOKEN", "GRELOT_TOKEN_FILE", "GRELOT_PATH_ROOT", "STORE_PATH", "ACCOUNTS_PATH",
		"STORE_MAX_BYTES", "THREAD_TTL", "DORMANT_TTL", "RESERVATION_MAX_TTL", "PROVIDERS_DIR",
		"PUBLIC_URL", "TRUSTED_PROXIES", "OIDC_ISSUER", "OIDC_CLIENT_ID",
		"OIDC_CLIENT_SECRET", "OIDC_CLIENT_SECRET_FILE", "OIDC_LABEL",
	} {
		t.Setenv(k, "")
	}
}

// setRequired sets the keys LoadConfig needs, to values a test may
// override.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("PUBLIC_URL", "https://grelot.example.com")
}

func TestLoadConfig_defaults(t *testing.T) {
	clearConfigEnv(t)
	setRequired(t)
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		PathRoot: "/workspace", StorePath: "/data/grelot.db", AccountsPath: "/data/accounts.db",
		MaxBytes: 512 << 20, DormantTTL: 72 * time.Hour, ReservationMaxTTL: 8 * time.Hour,
		ProvidersDir: "/providers", PublicURL: &url.URL{Scheme: "https", Host: "grelot.example.com"},
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("config %+v, want %+v", c, want)
	}
	if want.MaxBytes != store.DefaultMaxBytes {
		t.Errorf("the default ceiling is not the store's")
	}
}

func TestLoadConfig_readsEachKey(t *testing.T) {
	clearConfigEnv(t)
	secretFile := filepath.Join(t.TempDir(), "oidc-secret")
	if err := os.WriteFile(secretFile, []byte("client-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRELOT_PATH_ROOT", "/src")
	t.Setenv("STORE_PATH", "/var/lib/grelot/g.db")
	t.Setenv("ACCOUNTS_PATH", "/var/lib/grelot/a.db")
	t.Setenv("STORE_MAX_BYTES", "1048576")
	t.Setenv("DORMANT_TTL", "12h")
	t.Setenv("RESERVATION_MAX_TTL", "2h")
	t.Setenv("PROVIDERS_DIR", "/etc/grelot/providers")
	t.Setenv("PUBLIC_URL", " https://grelot.example.com:8443/ ")
	t.Setenv("TRUSTED_PROXIES", "172.18.0.0/16, 10.0.0.5,::ffff:192.0.2.0/120")
	t.Setenv("OIDC_ISSUER", "https://id.example.com/realms/home")
	t.Setenv("OIDC_CLIENT_ID", "grelot")
	t.Setenv("OIDC_CLIENT_SECRET_FILE", secretFile)
	t.Setenv("OIDC_LABEL", "Home ID")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		PathRoot: "/src", StorePath: "/var/lib/grelot/g.db", AccountsPath: "/var/lib/grelot/a.db",
		MaxBytes: 1 << 20, DormantTTL: 12 * time.Hour, ReservationMaxTTL: 2 * time.Hour,
		ProvidersDir: "/etc/grelot/providers", PublicURL: &url.URL{Scheme: "https", Host: "grelot.example.com:8443"},
		TrustedProxies: []netip.Prefix{
			netip.MustParsePrefix("172.18.0.0/16"), netip.MustParsePrefix("10.0.0.5/32"), netip.MustParsePrefix("192.0.2.0/24"),
		},
		OIDC: auth.OIDC{Issuer: "https://id.example.com/realms/home", ClientID: "grelot", ClientSecret: "client-secret", Label: "Home ID"},
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("config %+v, want %+v", c, want)
	}
}

func TestLoadConfig_refusalsNameTheKey(t *testing.T) {
	for _, tc := range []struct {
		key, value string
	}{
		{"GRELOT_PATH_ROOT", "workspace"},
		{"GRELOT_PATH_ROOT", "/workspace/"},
		{"GRELOT_PATH_ROOT", "/workspace/../src"},
		{"STORE_PATH", "/data/"},
		{"STORE_PATH", "/data/."},
		{"STORE_PATH", "/data/.."},
		{"STORE_PATH", ".."},
		{"ACCOUNTS_PATH", "/data/"},
		{"ACCOUNTS_PATH", ".."},
		{"ACCOUNTS_PATH", "/data/grelot.db"},
		{"ACCOUNTS_PATH", "/data/./grelot.db"},
		{"STORE_MAX_BYTES", "lots"},
		{"STORE_MAX_BYTES", "0"},
		{"DORMANT_TTL", "-1h"},
		{"RESERVATION_MAX_TTL", "soon"},
		{"PROVIDERS_DIR", "providers"},
		{"PROVIDERS_DIR", "/providers/"},
		{"PROVIDERS_DIR", "/srv/../providers"},
		{"PUBLIC_URL", ""},
		{"PUBLIC_URL", "grelot.example.com"},
		{"PUBLIC_URL", "http://grelot.example.com"},
		{"PUBLIC_URL", "https://192.0.2.7"},
		{"PUBLIC_URL", "https://Grelot.example.com"},
		{"PUBLIC_URL", "https://grelot.example.com/grelot"},
		{"PUBLIC_URL", "https://grelot.example.com/?a=1"},
		{"PUBLIC_URL", "https://user@grelot.example.com"},
		{"TRUSTED_PROXIES", "172.18.0.0/16,proxy"},
		{"OIDC_ISSUER", "http://id.example.com"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearConfigEnv(t)
			setRequired(t)
			if strings.HasPrefix(tc.key, "OIDC_") {
				t.Setenv("OIDC_CLIENT_ID", "grelot")
				t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
			}
			t.Setenv(tc.key, tc.value)
			_, err := LoadConfig()
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("err %v, want one naming %s", err, tc.key)
			}
			if strings.Contains(err.Error(), "client-secret") {
				t.Errorf("err %q carries the client secret", err)
			}
		})
	}
}

// Single sign-on takes its three keys together: one or two set is refused
// naming the missing ones.
func TestLoadConfig_singleSignOnNeedsEveryKey(t *testing.T) {
	t.Run("issuer alone", func(t *testing.T) {
		clearConfigEnv(t)
		setRequired(t)
		t.Setenv("OIDC_ISSUER", "https://id.example.com")
		_, err := LoadConfig()
		if err == nil || !strings.Contains(err.Error(), "OIDC_CLIENT_ID and OIDC_CLIENT_SECRET unset") {
			t.Errorf("err %v, want one naming OIDC_CLIENT_ID and OIDC_CLIENT_SECRET as unset", err)
		}
	})
	t.Run("an issuer or client id file is not read", func(t *testing.T) {
		clearConfigEnv(t)
		setRequired(t)
		t.Setenv("OIDC_ISSUER_FILE", filepath.Join(t.TempDir(), "issuer"))
		t.Setenv("OIDC_CLIENT_ID_FILE", filepath.Join(t.TempDir(), "client-id"))
		t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
		_, err := LoadConfig()
		if err == nil || !strings.Contains(err.Error(), "OIDC_ISSUER and OIDC_CLIENT_ID unset") {
			t.Errorf("err %v, want one naming OIDC_ISSUER and OIDC_CLIENT_ID as unset", err)
		}
	})
	t.Run("all three", func(t *testing.T) {
		clearConfigEnv(t)
		setRequired(t)
		t.Setenv("OIDC_ISSUER", "https://id.example.com")
		t.Setenv("OIDC_CLIENT_ID", "grelot")
		t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
		c, err := LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		want := auth.OIDC{Issuer: "https://id.example.com", ClientID: "grelot", ClientSecret: "client-secret"}
		if c.OIDC != want {
			t.Errorf("OIDC %+v, want %+v", c.OIDC, want)
		}
	})
	t.Run("http to localhost", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("PUBLIC_URL", "http://localhost:9737")
		t.Setenv("OIDC_ISSUER", "http://127.0.0.1:5556/dex")
		t.Setenv("OIDC_CLIENT_ID", "grelot")
		t.Setenv("OIDC_CLIENT_SECRET", "client-secret")
		if _, err := LoadConfig(); err != nil {
			t.Errorf("LoadConfig with a loopback issuer and a localhost origin: %v", err)
		}
	})
}

func TestLoadConfig_aRetiredSettingIsNeitherReadNorRefused(t *testing.T) {
	for _, tc := range []struct{ key, value, line string }{
		{"THREAD_TTL", "24h", "THREAD_TTL is no longer read: set the history retention in Settings"},
		{"THREAD_TTL", "72", "THREAD_TTL is no longer read: set the history retention in Settings"},
		{"GRELOT_TOKEN", "old-shared-token", "GRELOT_TOKEN is no longer read: each machine sends its own key, from grelot key create"},
		{"GRELOT_TOKEN_FILE", "/run/secrets/token", "GRELOT_TOKEN_FILE is no longer read: each machine sends its own key, from grelot key create"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearConfigEnv(t)
			setRequired(t)
			t.Setenv(tc.key, tc.value)
			if _, err := LoadConfig(); err != nil {
				t.Errorf("LoadConfig with %s=%s: %v, want it ignored", tc.key, tc.value, err)
			}
			if got := RetiredSettings(); !slices.Equal(got, []string{tc.line}) {
				t.Errorf("RetiredSettings() = %q, want %q", got, tc.line)
			}
		})
	}
	t.Run("unset", func(t *testing.T) {
		clearConfigEnv(t)
		if got := RetiredSettings(); len(got) != 0 {
			t.Errorf("RetiredSettings() = %q, want none", got)
		}
	})
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

func TestListenAddr(t *testing.T) {
	for _, tc := range []struct{ name, env, want string }{
		{"unset gives the image's port", "", ":9737"},
		{"a value is kept", "127.0.0.1:9999", "127.0.0.1:9999"},
		{"edge spaces are cut", "  0.0.0.0:9999 \t", "0.0.0.0:9999"},
		{"invisible padding is cut", "\u200b:9999\u00ad", ":9999"},
		{"only blanks gives the image's port", " \ufeff ", ":9737"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LISTEN_ADDR", tc.env)
			if got := ListenAddr(); got != tc.want {
				t.Errorf("ListenAddr() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOpsListenAddr(t *testing.T) {
	for _, tc := range []struct{ name, env, want string }{
		{"unset gives the image's operations port", "", ":9738"},
		{"a value is kept", "127.0.0.1:9999", "127.0.0.1:9999"},
		{"edge spaces are cut", "  0.0.0.0:9999 \t", "0.0.0.0:9999"},
		{"invisible padding is cut", "\u200b:9999\u00ad", ":9999"},
		{"only blanks gives the image's operations port", " \ufeff ", ":9738"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LISTEN_ADDR", "127.0.0.1:1234")
			t.Setenv("OPS_LISTEN_ADDR", tc.env)
			if got := OpsListenAddr(); got != tc.want {
				t.Errorf("OpsListenAddr() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAdminSocket(t *testing.T) {
	longest := "/" + strings.Repeat("s", 102)
	for _, tc := range []struct{ name, env, want string }{
		{"unset gives the data folder's socket", "", "/data/admin.sock"},
		{"a value is kept", "/run/grelot/admin.sock", "/run/grelot/admin.sock"},
		{"edge spaces are cut", " \t/run/admin.sock ", "/run/admin.sock"},
		{"a path of 103 bytes fits a socket address", longest, longest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ADMIN_SOCKET", tc.env)
			got, err := AdminSocket()
			if err != nil || got != tc.want {
				t.Errorf("AdminSocket() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, tc := range []struct{ name, env string }{
		{"a relative path", "admin.sock"},
		{"a folder", "/run/grelot/"},
		{"a path of 104 bytes, over macOS's socket address", longest + "s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ADMIN_SOCKET", tc.env)
			if got, err := AdminSocket(); err == nil || !strings.Contains(err.Error(), "ADMIN_SOCKET") {
				t.Errorf("AdminSocket() = %q, %v; want an error naming ADMIN_SOCKET", got, err)
			}
		})
	}
}

func TestCheckListenAddrs(t *testing.T) {
	for _, tc := range []struct {
		name, api, ops string
		refused        bool
	}{
		{"the image's two ports", ":9737", ":9738", false},
		{"the same port", ":9737", ":9737", true},
		{"the same port on two hosts", "127.0.0.1:9737", "0.0.0.0:9737", true},
		{"two hosts on their own ports", "10.0.0.1:9000", "127.0.0.1:9001", false},
		{"one port spelled with a leading zero", "127.0.0.1:9737", "127.0.0.2:09737", true},
		{"one port spelled as its service name", ":80", "127.0.0.2:http", true},
		{"two free ports", "127.0.0.1:0", "127.0.0.1:0", false},
		{"no port, which the listen refuses", "9737", "9737", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckListenAddrs(tc.api, tc.ops)
			if (err != nil) != tc.refused {
				t.Fatalf("CheckListenAddrs(%q, %q) = %v, want refused %v", tc.api, tc.ops, err, tc.refused)
			}
			if tc.refused && !strings.Contains(err.Error(), "OPS_LISTEN_ADDR must name another port than LISTEN_ADDR") {
				t.Errorf("error %q names neither key", err)
			}
		})
	}
}
