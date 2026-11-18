//go:build !windows

package main

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
	_, ok := keyScopes(t, db, key)
	return ok
}

// keyScopes are the scopes key authenticates with on db; ok is false when
// it does not authenticate.
func keyScopes(t *testing.T, db *accounts.DB, key string) (scopes []string, ok bool) {
	t.Helper()
	authn, err := access.New(db.Access(), access.Config{})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/who", http.NoBody)
	r.Header.Set("Authorization", "Bearer "+key)
	p, err := authn.Authenticate(r)
	return p.Scopes, err == nil && p.IsAPIKey()
}

// adminServer is a running admin socket over a fresh accounts file, named
// by ADMIN_SOCKET, and the file.
func adminServer(t *testing.T) *accounts.DB {
	t.Helper()
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
	return db
}

func createOwner(t *testing.T, db *accounts.DB) {
	t.Helper()
	if err := db.Access().CreateInitialUser(t.Context(), &access.User{
		Username: "owner", Role: access.RoleAdmin, Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdminCommands_eachVerbThroughTheSocket(t *testing.T) {
	db := adminServer(t)

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
	createOwner(t, db)
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
	if scopes, _ := keyScopes(t, db, key); !slices.Equal(scopes, []string{"agent"}) {
		t.Errorf("key create with no --scope minted scopes %v, want [agent]", scopes)
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

// listedScopes is each label's scope in key list's table.
func listedScopes(t *testing.T) map[string]string {
	t.Helper()
	code, out, errOut := runAdmin(t, "key", "list")
	if code != 0 {
		t.Fatalf("key list: exit %d, %q %q", code, out, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if head := strings.Fields(lines[0]); !slices.Equal(head, []string{"LABEL", "SCOPE", "CREATED", "LAST", "USED"}) {
		t.Fatalf("key list's header is %q, want LABEL SCOPE CREATED LAST USED", lines[0])
	}
	got := map[string]string{}
	for _, line := range lines[1:] {
		if f := strings.Fields(line); len(f) >= 2 {
			got[f[0]] = f[1]
		}
	}
	return got
}

func TestAdminCommands_keyCreateTakesAScope(t *testing.T) {
	db := adminServer(t)
	createOwner(t, db)
	minted := map[string]string{}
	for _, c := range []struct{ label, flag, scope string }{
		{"laptop", "", "agent"}, {"runner", "--scope=agent", "agent"}, {"ops", "--scope=owner", "owner"},
	} {
		args := []string{"key", "create", c.label}
		if c.flag != "" {
			args = append(args, c.flag)
		}
		code, out, errOut := runAdmin(t, args...)
		key, _, _ := strings.Cut(out, "\n")
		if code != 0 {
			t.Fatalf("%s: exit %d, %q %q; want 0 and the key", strings.Join(args, " "), code, out, errOut)
		}
		if scopes, ok := keyScopes(t, db, key); !ok || !slices.Equal(scopes, []string{c.scope}) {
			t.Errorf("%s minted a key of scopes %v (authenticates %v), want [%s]", strings.Join(args, " "), scopes, ok, c.scope)
		}
		minted[c.label] = c.scope
	}
	if got := listedScopes(t); !maps.Equal(got, minted) {
		t.Errorf("key list's scopes = %v, want %v", got, minted)
	}
	code, out, _ := runAdmin(t, "key", "rotate", "ops")
	rotated, _, _ := strings.Cut(out, "\n")
	if code != 0 {
		t.Fatalf("key rotate ops: exit %d, %q", code, out)
	}
	if scopes, _ := keyScopes(t, db, rotated); !slices.Equal(scopes, []string{"owner"}) {
		t.Errorf("the rotated owner key's scopes = %v, want [owner]", scopes)
	}
	if got := listedScopes(t)["ops"]; got != "owner" {
		t.Errorf("key list after the rotation lists ops as %q, want owner", got)
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
		{"key", "create", "laptop", "--scope=admin"},
		{"key", "create", "laptop", "--scope="},
		{"key", "create", "laptop", "--scope", "owner"},
		{"key", "create", "laptop", "--scope=owner", "--scope=agent"},
		{"key", "create", "--scope=owner"},
		{"key", "list", "extra"},
		{"key", "revoke"},
		{"key", "rotate"},
		{"key", "rotate", "a", "b"},
		{"key", "rename", "laptop"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, _, errOut := runAdmin(t, args...)
			if code != 2 || !strings.HasPrefix(errOut, "usage: grelot "+args[0]) {
				t.Errorf("exit %d, stderr %q; want 2 and the usage line", code, errOut)
			}
			if args[0] == "key" && !strings.Contains(errOut, "grelot key create <label> [--scope=agent|owner]") {
				t.Errorf("stderr %q, want the usage to name --scope in its parse-safe form", errOut)
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
