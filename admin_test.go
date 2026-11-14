//go:build !windows

package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/grelot/internal/accounts"
	"github.com/cplieger/grelot/internal/auth"
)

// runAdmin runs one admin command line and returns its exit code and output.
func runAdmin(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// authenticates reports whether key passes a verifier of its own over db.
func authenticates(t *testing.T, db *accounts.DB, key string) bool {
	t.Helper()
	authn, err := access.New(db.Access(), access.Config{})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/who", http.NoBody)
	r.Header.Set("Authorization", "Bearer "+key)
	p, err := authn.Authenticate(r)
	return err == nil && p.IsAPIKey()
}

func TestAdminCommands_eachVerbThroughTheSocket(t *testing.T) {
	db, err := accounts.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a, err := auth.New(t.Context(), db, testAuthOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	sock := testListeners(t).admin
	stop, err := serveAdmin(context.Background(), sock, a, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	t.Setenv("ADMIN_SOCKET", sock)

	if code, out, _ := runAdmin(t, "key", "list"); code != 0 || !strings.Contains(out, "No keys") {
		t.Errorf("key list on no keys: exit %d, %q; want 0 and No keys", code, out)
	}
	if code, _, errOut := runAdmin(t, "key", "create", "laptop"); code != 1 || !strings.Contains(errOut, "grelot setup-token") {
		t.Errorf("key create before setup: exit %d, %q; want 1 naming grelot setup-token", code, errOut)
	}
	code, out, _ := runAdmin(t, "setup-token")
	if token, rest, _ := strings.Cut(out, "\n"); code != 0 || token == "" || !strings.Contains(rest, "Open https://grelot.example.com/setup#token="+token+" ") {
		t.Errorf("setup-token: exit %d, %q; want 0, the token alone on line 1, then the setup link carrying it", code, out)
	}
	if err := db.Access().CreateInitialUser(t.Context(), &access.User{
		Username: "owner", Role: access.RoleAdmin, Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runAdmin(t, "setup-token"); code != 1 || !strings.Contains(errOut, "setup is done") {
		t.Errorf("setup-token once an account exists: exit %d, %q; want 1, setup is done", code, errOut)
	}

	code, out, _ = runAdmin(t, "key", "create", "laptop")
	key, _, _ := strings.Cut(out, "\n")
	if code != 0 || !strings.Contains(out, "laptop's key, shown only once") {
		t.Fatalf("key create: exit %d, %q; want 0 and the key", code, out)
	}
	if !authenticates(t, db, key) {
		t.Errorf("the printed key %q does not authenticate", key)
	}
	if code, out, _ := runAdmin(t, "key", "list"); code != 0 || !strings.Contains(out, "LAST USED") || !strings.Contains(out, "laptop") {
		t.Errorf("key list: exit %d, %q; want a table naming laptop", code, out)
	}
	code, out, _ = runAdmin(t, "recovery", "owner")
	if token, rest, _ := strings.Cut(out, "\n"); code != 0 || !strings.Contains(rest, "Open https://grelot.example.com/recover#token="+token+" ") || !strings.Contains(rest, "for owner") {
		t.Errorf("recovery owner: exit %d, %q; want 0, a token for owner and the recovery link carrying it", code, out)
	}
	if code, _, errOut := runAdmin(t, "recovery", "ghost"); code != 1 || !strings.Contains(errOut, `no account is named "ghost"`) {
		t.Errorf("recovery ghost: exit %d, %q; want 1, no account is named", code, errOut)
	}
	code, out, _ = runAdmin(t, "key", "rotate", "laptop")
	rotated, _, _ := strings.Cut(out, "\n")
	if code != 0 || rotated == key || !strings.Contains(out, "laptop's new key, shown only once") {
		t.Fatalf("key rotate: exit %d, %q; want 0 and a new key", code, out)
	}
	if !authenticates(t, db, rotated) {
		t.Errorf("the rotated key %q does not authenticate", rotated)
	}
	key = rotated
	if code, out, _ := runAdmin(t, "key", "revoke", "laptop"); code != 0 || out != "Revoked the key labelled laptop.\n" {
		t.Errorf("key revoke: exit %d, %q; want 0 and one line", code, out)
	}
	if authenticates(t, db, key) {
		t.Error("the revoked key still authenticates")
	}
}

func TestAdminCommands_usage(t *testing.T) {
	t.Setenv("ADMIN_SOCKET", "/nonexistent/admin.sock")
	for _, args := range [][]string{
		{"setup-token", "extra"},
		{"recovery"},
		{"recovery", "a", "b"},
		{"key"},
		{"key", "create"},
		{"key", "list", "extra"},
		{"key", "revoke"},
		{"key", "rotate"},
		{"key", "rotate", "a", "b"},
		{"key", "rename", "laptop"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if code, _, errOut := runAdmin(t, args...); code != 2 || !strings.HasPrefix(errOut, "usage: grelot "+args[0]) {
				t.Errorf("exit %d, stderr %q; want 2 and the usage line", code, errOut)
			}
		})
	}
}

func TestAdminCommands_noServer(t *testing.T) {
	t.Setenv("ADMIN_SOCKET", filepath.Join(t.TempDir(), "admin.sock"))
	code, _, errOut := runAdmin(t, "key", "list")
	if code != 1 || !strings.Contains(errOut, "it is not running, or ADMIN_SOCKET differs") {
		t.Errorf("exit %d, stderr %q; want 1 saying the server is not running", code, errOut)
	}
	t.Setenv("ADMIN_SOCKET", "admin.sock")
	if code, _, errOut := runAdmin(t, "key", "list"); code != 1 || !strings.Contains(errOut, "ADMIN_SOCKET must be") {
		t.Errorf("a relative ADMIN_SOCKET: exit %d, stderr %q; want 1 naming the key", code, errOut)
	}
}

func TestServe_answersTheAdminSocketUntilItStops(t *testing.T) {
	addrs, _, cancel, done := startServe(t)
	t.Setenv("ADMIN_SOCKET", addrs.admin)
	if code, out, errOut := runAdmin(t, "key", "list"); code != 0 || !strings.Contains(out, "seed ") {
		t.Errorf("key list while serving: exit %d, %q %q; want 0 listing the seeded key", code, out, errOut)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return within 5 s of cancel")
	}
	if _, err := os.Lstat(addrs.admin); !os.IsNotExist(err) {
		t.Errorf("Lstat of the admin socket after serve = %v, want it removed", err)
	}
	if code, _, _ := runAdmin(t, "key", "list"); code != 1 {
		t.Errorf("key list after serve: exit %d, want 1", code)
	}
}

// testAuthOptions serves the sign-in at an https origin, its refusals
// written nowhere.
func testAuthOptions() *auth.Options {
	return &auth.Options{
		Logger: slog.New(slog.DiscardHandler), PublicURL: &url.URL{Scheme: "https", Host: "grelot.example.com"},
		Errors: func(http.ResponseWriter, *http.Request, access.Error) {},
	}
}
