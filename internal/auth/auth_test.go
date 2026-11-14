//go:build !windows

package auth_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/access/account"
	"github.com/cplieger/grelot/internal/accounts"
	"github.com/cplieger/grelot/internal/auth"
)

const (
	ownerName = "owner"
	password  = "a long enough test passphrase"
)

var client = account.Client{Peer: netip.MustParseAddr("192.0.2.1"), Via: access.ViaDirect}

// newAuth opens a fresh accounts file and the service on it, closed at the
// test's end.
func newAuth(t *testing.T) (*auth.Auth, *accounts.DB) {
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
	return a, db
}

// setUp creates the first account through a setup token, as the setup page
// does.
func setUp(t *testing.T, a *auth.Auth) {
	t.Helper()
	tok, err := a.SetupToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Service().SetupWithPassword(t.Context(), client, account.SetupPassword{
		Token: access.NewSecret(tok.Value), Password: access.NewSecret(password), Username: ownerName,
	}); err != nil {
		t.Fatalf("setup with the minted token: %v", err)
	}
}

// keyHolder is who key authenticates as on the accounts in db, through a
// verifier of its own; ok is false when the key is refused.
func keyHolder(t *testing.T, db *accounts.DB, key string) (string, bool) {
	t.Helper()
	authn, err := access.New(db.Access(), access.Config{})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/who", http.NoBody)
	r.Header.Set("Authorization", "Bearer "+key)
	p, err := authn.Authenticate(r)
	if err != nil || !p.IsAPIKey() {
		return "", false
	}
	return p.User.Username, true
}

func TestSetupToken_createsTheFirstAccountOnly(t *testing.T) {
	a, _ := newAuth(t)
	before := time.Now()
	tok, err := a.SetupToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if lo, hi := before.Add(24*time.Hour), time.Now().Add(24*time.Hour); tok.Expires.Before(lo) || tok.Expires.After(hi) {
		t.Errorf("Expires %v, want 24 hours from the call", tok.Expires)
	}
	if want := "https://grelot.example.com/setup#token=" + tok.Value; tok.Link != want {
		t.Errorf("Link %q, want %q", tok.Link, want)
	}
	if _, err := a.Service().SetupWithPassword(t.Context(), client, account.SetupPassword{
		Token: access.NewSecret(tok.Value), Password: access.NewSecret(password), Username: ownerName,
	}); err != nil {
		t.Fatalf("setup with the minted token: %v", err)
	}
	if _, err := a.SetupToken(t.Context()); err == nil || !strings.Contains(err.Error(), "grelot recovery") {
		t.Errorf("SetupToken once an account exists = %v, want a refusal pointing at grelot recovery", err)
	}
}

func TestNew_keepsOneOwner(t *testing.T) {
	a, _ := newAuth(t)
	setUp(t, a)
	_, err := a.Service().CreateUser(t.Context(), account.OutOfBand("admin socket"), account.NewUser{Username: "second"})
	pe, ok := errors.AsType[*account.PolicyError](err)
	if !ok || pe.Reason != account.ReasonSingleOwner {
		t.Errorf("CreateUser of a second account = %v, want the single-owner refusal", err)
	}
	if _, err := a.CreateKey(t.Context(), "laptop"); err != nil {
		t.Errorf("CreateKey for the owner: %v", err)
	}
}

func TestRecovery(t *testing.T) {
	a, _ := newAuth(t)
	setUp(t, a)
	t.Run("an unknown name is refused", func(t *testing.T) {
		for _, name := range []string{"ghost", "not a username\x00"} {
			if _, err := a.Recovery(t.Context(), name); err == nil || !strings.Contains(err.Error(), "no account is named") {
				t.Errorf("Recovery(%q) = %v, want no account is named", name, err)
			}
		}
	})
	t.Run("the token starts a recovery of the named account", func(t *testing.T) {
		before := time.Now()
		tok, err := a.Recovery(t.Context(), "Owner")
		if err != nil {
			t.Fatal(err)
		}
		if lo, hi := before.Add(15*time.Minute), time.Now().Add(15*time.Minute); tok.Expires.Before(lo) || tok.Expires.After(hi) {
			t.Errorf("Expires %v, want 15 minutes from the call", tok.Expires)
		}
		if want := "https://grelot.example.com/recover#token=" + tok.Value; tok.Link != want {
			t.Errorf("Link %q, want %q", tok.Link, want)
		}
		rec, err := a.Service().StartRecovery(t.Context(), client, access.NewSecret(tok.Value))
		if err != nil {
			t.Fatalf("StartRecovery with the minted token: %v", err)
		}
		if rec.Username != ownerName {
			t.Errorf("recovery for %q, want %q", rec.Username, ownerName)
		}
	})
}

func TestCreateKey(t *testing.T) {
	a, db := newAuth(t)
	t.Run("no account yet is refused, naming setup-token", func(t *testing.T) {
		if _, err := a.CreateKey(t.Context(), "laptop"); err == nil || !strings.Contains(err.Error(), "grelot setup-token") {
			t.Errorf("CreateKey before setup = %v, want a refusal naming grelot setup-token", err)
		}
	})
	setUp(t, a)
	t.Run("the key authenticates as the admin account", func(t *testing.T) {
		key, err := a.CreateKey(t.Context(), "laptop")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(key, "grelot_") {
			t.Errorf("key %q does not start with grelot_", key)
		}
		if who, ok := keyHolder(t, db, key); !ok || who != ownerName {
			t.Errorf("the key authenticates as %q, %v; want %q", who, ok, ownerName)
		}
	})
	// access's label rule (account.checkKeyLabel, requireLabelFree): one
	// line, not empty, and free among the owner's unexpired keys after NFC
	// and case folding.
	for _, label := range []string{"laptop", "LAPTOP"} {
		t.Run("a label in use is refused as "+label, func(t *testing.T) {
			if _, err := a.CreateKey(t.Context(), label); err == nil || !strings.Contains(err.Error(), "revoke or rotate it") {
				t.Errorf("CreateKey(%s) with laptop in use = %v, want the label-taken refusal", label, err)
			}
		})
	}
	for _, label := range []string{"", "tab\there", "two\nlines"} {
		t.Run("label "+strings.NewReplacer("\t", `\t`, "\n", `\n`).Replace(label)+" is refused", func(t *testing.T) {
			if _, err := a.CreateKey(t.Context(), label); err == nil {
				t.Errorf("CreateKey(%q) = a key, want the label rule's refusal", label)
			}
		})
	}
	for _, label := range []string{"ci.runner-2_b", "two words", "été"} {
		t.Run("label "+label+" is accepted", func(t *testing.T) {
			if _, err := a.CreateKey(t.Context(), label); err != nil {
				t.Errorf("CreateKey(%q) = %v, want a key", label, err)
			}
		})
	}
}

func TestKeys_listsOldestFirstWithLastUse(t *testing.T) {
	a, db := newAuth(t)
	if keys, err := a.Keys(t.Context()); err != nil || len(keys) != 0 {
		t.Fatalf("Keys on a fresh file = %v, %v; want none", keys, err)
	}
	setUp(t, a)
	before := time.Now()
	for _, label := range []string{"zeta", "alpha"} {
		if _, err := a.CreateKey(t.Context(), label); err != nil {
			t.Fatal(err)
		}
	}
	used, err := a.CreateKey(t.Context(), "used")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keyHolder(t, db, used); !ok {
		t.Fatal("the new key does not authenticate")
	}
	keys, err := a.Keys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 {
		t.Fatalf("Keys = %v, want 3", keys)
	}
	for i, want := range []string{"zeta", "alpha", "used"} {
		if keys[i].Label != want || keys[i].Created.Before(before.Truncate(time.Second)) {
			t.Errorf("key %d = %+v, want %s created during the test", i, keys[i], want)
		}
	}
	if !keys[0].LastUsed.IsZero() || keys[2].LastUsed.IsZero() {
		t.Errorf("last used %v and %v, want never for zeta and set for used", keys[0].LastUsed, keys[2].LastUsed)
	}
}

func TestRotateKey_theOldSecretWorksUntilTheNewOnesFirstUse(t *testing.T) {
	a, db := newAuth(t)
	setUp(t, a)
	old, err := a.CreateKey(t.Context(), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keyHolder(t, db, old); !ok {
		t.Fatal("the new key does not authenticate")
	}

	rotated, err := a.RotateKey(t.Context(), "laptop")

	if err != nil || rotated == old || !strings.HasPrefix(rotated, "grelot_") {
		t.Fatalf("RotateKey(laptop) = %q, %v; want a new grelot_ key", rotated, err)
	}
	if _, ok := keyHolder(t, db, old); !ok {
		t.Error("the old secret is refused before the new one is used, want it to work in the overlap")
	}
	if who, ok := keyHolder(t, db, rotated); !ok || who != ownerName {
		t.Errorf("the rotated key authenticates as %q, %v; want %q", who, ok, ownerName)
	}
	if _, ok := keyHolder(t, db, old); ok {
		t.Error("the old secret still works after the new one's first use")
	}
	if keys, _ := a.Keys(t.Context()); len(keys) != 1 || keys[0].Label != "laptop" {
		t.Errorf("keys after the rotation = %v, want laptop alone", keys)
	}
	if _, err := a.RotateKey(t.Context(), "nope"); err == nil || !strings.Contains(err.Error(), "no key is labelled") {
		t.Errorf("RotateKey(nope) = %v, want no key is labelled", err)
	}
}

func TestRevokeKey(t *testing.T) {
	a, db := newAuth(t)
	setUp(t, a)
	gone, err := a.CreateKey(t.Context(), "gone")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := a.CreateKey(t.Context(), "kept")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RevokeKey(t.Context(), "gone"); err != nil {
		t.Fatalf("RevokeKey(gone) = %v", err)
	}
	if _, ok := keyHolder(t, db, gone); ok {
		t.Error("the revoked key still authenticates")
	}
	if _, ok := keyHolder(t, db, kept); !ok {
		t.Error("revoking gone also refused kept's key")
	}
	if keys, _ := a.Keys(t.Context()); len(keys) != 1 || keys[0].Label != "kept" {
		t.Errorf("Keys after the revoke = %v, want kept alone", keys)
	}
	if err := a.RevokeKey(t.Context(), "gone"); err == nil || !strings.Contains(err.Error(), "no key is labelled") {
		t.Errorf("RevokeKey(gone) again = %v; want no key is labelled", err)
	}
}

// A label finds its key whatever its case, as access refuses a second key
// under it: the unexpired key first, then an expired one.
func TestRevokeKey_findsTheLabelAsAccessComparesIt(t *testing.T) {
	a, db := newAuth(t)
	setUp(t, a)
	live, err := a.CreateKey(t.Context(), "Laptop")
	if err != nil {
		t.Fatal(err)
	}
	storeKey(t, db, "laptop", "grelot_"+strings.Repeat("4", 64), time.Now().Add(-time.Minute))

	if err := a.RevokeKey(t.Context(), "LAPTOP"); err != nil {
		t.Fatalf("RevokeKey(LAPTOP) = %v, want Laptop revoked", err)
	}
	if _, ok := keyHolder(t, db, live); ok {
		t.Error("the unexpired Laptop still authenticates")
	}
	if keys, _ := a.Keys(t.Context()); len(keys) != 1 || keys[0].Label != "laptop" {
		t.Errorf("keys after the first revoke = %v, want the expired laptop alone", keys)
	}
	if err := a.RevokeKey(t.Context(), "Laptop"); err != nil {
		t.Fatalf("RevokeKey(Laptop) over the expired key = %v, want it revoked", err)
	}
	if keys, _ := a.Keys(t.Context()); len(keys) != 0 {
		t.Errorf("keys after the second revoke = %v, want none", keys)
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

// keyID is the ID of the key labelled label in db.
func keyID(t *testing.T, db *accounts.DB, label string) int64 {
	t.Helper()
	keys, err := db.Access().ListAPIKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i := range keys {
		if keys[i].Label == label {
			return keys[i].ID
		}
	}
	t.Fatalf("no key labelled %s", label)
	return 0
}

// storeKey writes key labelled label for the first account straight into
// db, as an accounts file written before the label rule held may carry it.
func storeKey(t *testing.T, db *accounts.DB, label, key string, expires time.Time) {
	t.Helper()
	u, found, err := db.Access().UserByUsername(t.Context(), ownerName)
	if err != nil || !found {
		t.Fatalf("owner: %v %v", found, err)
	}
	if err := db.Access().CreateAPIKey(t.Context(), &access.Key{
		UserID: u.ID, KeyHash: access.APIKeyHash(key), KeyPrefix: "grelot_", KeySuffix: key[len(key)-4:],
		Label: label, CreatedAt: time.Now(), ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeKey_twoKeysOfOneLabelAreBothKept(t *testing.T) {
	a, db := newAuth(t)
	setUp(t, a)
	first, second := "grelot_"+strings.Repeat("1", 64), "grelot_"+strings.Repeat("2", 64)
	storeKey(t, db, "laptop", first, time.Time{})
	storeKey(t, db, "laptop", second, time.Time{})

	err := a.RevokeKey(t.Context(), "laptop")

	if err == nil || !strings.Contains(err.Error(), `2 keys are labelled "laptop"`) {
		t.Errorf("RevokeKey(laptop) over two keys = %v; want a refusal naming both", err)
	}
	for _, k := range []string{first, second} {
		if _, ok := keyHolder(t, db, k); !ok {
			t.Errorf("key ...%s was revoked by the refused call", k[len(k)-4:])
		}
	}
}

// KeyLive answers as access's verifier admits a key: it exists, has not
// expired and its owner is enabled.
func TestKeyLive(t *testing.T) {
	a, db := newAuth(t)
	setUp(t, a)
	for _, label := range []string{"live", "gone"} {
		if _, err := a.CreateKey(t.Context(), label); err != nil {
			t.Fatal(err)
		}
	}
	storeKey(t, db, "expired", "grelot_"+strings.Repeat("3", 64), time.Now().Add(-time.Minute))
	live, gone, expired := keyID(t, db, "live"), keyID(t, db, "gone"), keyID(t, db, "expired")
	if err := a.RevokeKey(t.Context(), "gone"); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		id   int64
		want bool
	}{
		{"a live key", live, true},
		{"a revoked key", gone, false},
		{"an expired key", expired, false},
		{"an ID no key ever had", 9999, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, err := a.KeyLive(c.id); err != nil || got != c.want {
				t.Errorf("KeyLive = %v, %v; want %v", got, err, c.want)
			}
		})
	}
	t.Run("a key of a disabled owner", func(t *testing.T) {
		u, _, err := db.Access().UserByUsername(t.Context(), ownerName)
		if err != nil {
			t.Fatal(err)
		}
		u.Enabled = false
		if err := db.Access().UpdateUser(t.Context(), u); err != nil {
			t.Fatal(err)
		}
		if got, err := a.KeyLive(live); err != nil || got {
			t.Errorf("KeyLive = %v, %v; want false", got, err)
		}
	})
}
