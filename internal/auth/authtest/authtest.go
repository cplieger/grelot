//go:build !windows

// Package authtest builds the accounts a test server needs: an accounts
// file holding one admin account and a known machine key, the account
// service over it, and more keys the test's clients send.
package authtest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/grelot/internal/accounts"
	"github.com/cplieger/grelot/internal/auth"
)

// PublicURL is the origin of the service New builds.
const PublicURL = "http://localhost:9737"

// SeedKey is the machine key Seed stores, so a test whose server opens the
// accounts file itself knows a key it admits. It has the shape access
// generates, the prefix and 64 hex digits, which its verifier requires.
var SeedKey = "grelot_" + strings.Repeat("5", 64)

// Seed writes one enabled admin account and SeedKey, owned by it, into a
// new accounts file at path, and closes the file.
func Seed(t testing.TB, path string) {
	t.Helper()
	SeedKeys(t, path, 0)
}

// SeedKeys is Seed with n more keys of the same owner, labelled seed-1 to
// seed-n, which it returns in that order.
func SeedKeys(t testing.TB, path string, n int) []string {
	t.Helper()
	db, err := accounts.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, now := context.WithoutCancel(t.Context()), time.Now()
	admin := &access.User{
		Username: "owner", Role: access.RoleAdmin, Enabled: true,
		CreatedAt: now, UpdatedAt: now, WebAuthnHandle: access.GenerateWebAuthnHandle(),
	}
	if err := db.Access().CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, n+1)
	keys = append(keys, SeedKey)
	for i := range n {
		keys = append(keys, fmt.Sprintf("grelot_%064x", i+1))
	}
	for i, k := range keys {
		label := "seed"
		if i > 0 {
			label += "-" + strconv.Itoa(i)
		}
		key := &access.Key{
			UserID: admin.ID, KeyHash: access.APIKeyHash(k), KeyPrefix: "grelot_", KeySuffix: k[len(k)-4:],
			Label: label, CreatedAt: now,
		}
		if err := db.Access().CreateAPIKey(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	return keys[1:]
}

// New is an account service over a new accounts file that Seed wrote,
// closed at the test's end. errs writes the sign-in routes' refusals; nil
// writes the status and code alone.
func New(t testing.TB, errs access.ErrorWriter) *auth.Auth {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accounts.db")
	Seed(t, path)
	db, err := accounts.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if errs == nil {
		errs = func(w http.ResponseWriter, _ *http.Request, e access.Error) {
			http.Error(w, string(e.Code), e.Status)
		}
	}
	public, _ := url.Parse(PublicURL)
	a, err := auth.New(t.Context(), db, &auth.Options{Logger: slog.New(slog.DiscardHandler), PublicURL: public, Errors: errs})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.WithoutCancel(t.Context())) })
	return a
}

// Key mints a machine key labelled label on a, owned by its first admin.
func Key(t testing.TB, a *auth.Auth, label string) string {
	t.Helper()
	key, err := a.CreateKey(context.WithoutCancel(t.Context()), label)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
