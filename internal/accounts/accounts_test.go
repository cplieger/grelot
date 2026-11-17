//go:build !windows

package accounts_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
	"github.com/cplieger/grelot/internal/accounts"
	"github.com/cplieger/grelot/internal/store"
)

func open(t *testing.T, path string) *accounts.DB {
	t.Helper()
	db, err := accounts.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func closeDB(t *testing.T, db *accounts.DB) {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpen_generatesSecretsOnceAndKeepsThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db := open(t, path)
	first := db.Secrets()
	keys := map[string][]byte{
		"FailureKey": first.FailureKey, "DeviceKey": first.DeviceKey, "PendingKey": first.PendingKey, "CursorKey": first.CursorKey,
	}
	seen := map[string]string{}
	for name, k := range keys {
		if len(k) != 32 || bytes.Equal(k, make([]byte, 32)) {
			t.Errorf("%s = %x, want 32 random bytes", name, k)
		}
		if other, dup := seen[string(k)]; dup {
			t.Errorf("%s and %s are the same key, want four different keys", name, other)
		}
		seen[string(k)] = name
	}
	closeDB(t, db)

	again := open(t, path).Secrets()
	if !bytes.Equal(again.FailureKey, first.FailureKey) || !bytes.Equal(again.DeviceKey, first.DeviceKey) ||
		!bytes.Equal(again.PendingKey, first.PendingKey) || !bytes.Equal(again.CursorKey, first.CursorKey) {
		t.Errorf("secrets after reopen %+v, want the first open's %+v", again, first)
	}
}

// TestOpen_aVersion1SecretsFileIsAFormatFile opens a copy of
// testdata/accounts-secrets-v1.db, a file whose secrets row is version 1
// (no cursor key). It was written at e1b8499 by a main that calls
// accounts.Open on the path and closes it, with newSecret patched to
// return the bytes 1, 2 and 3 repeated so gitleaks reads no key in the
// file, then cut to its meta pages' high-water page (10 pages of 4096
// bytes), which bbolt reads unchanged.
func TestOpen_aVersion1SecretsFileIsAFormatFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	if err := os.WriteFile(path, readFile(t, filepath.Join("testdata", "accounts-secrets-v1.db")), 0o600); err != nil {
		t.Fatal(err)
	}
	before := sha256.Sum256(readFile(t, path))

	_, err := accounts.Open(path)

	if !errors.Is(err, bolttable.ErrFormat) {
		t.Errorf("Open of a version 1 secrets file = %v, want bolttable.ErrFormat", err)
	}
	if sha256.Sum256(readFile(t, path)) != before {
		t.Error("the refused file changed")
	}
}

func TestOpen_refusesASecretsRowWithAShortKey(t *testing.T) {
	full := func() accounts.Secrets {
		return accounts.Secrets{
			FailureKey: bytes.Repeat([]byte{1}, 32), DeviceKey: bytes.Repeat([]byte{2}, 32),
			PendingKey: bytes.Repeat([]byte{3}, 32), CursorKey: bytes.Repeat([]byte{4}, 32),
		}
	}
	for _, key := range []string{"FailureKey", "DeviceKey", "PendingKey", "CursorKey"} {
		for _, n := range []int{0, 16, 31} {
			t.Run(fmt.Sprintf("%s of %d bytes", key, n), func(t *testing.T) {
				s := full()
				short := map[string]*[]byte{"FailureKey": &s.FailureKey, "DeviceKey": &s.DeviceKey, "PendingKey": &s.PendingKey, "CursorKey": &s.CursorKey}[key]
				*short = (*short)[:n]
				path := filepath.Join(t.TempDir(), "accounts.db")
				if err := accounts.WriteSecrets(path, s); err != nil {
					t.Fatal(err)
				}
				before := sha256.Sum256(readFile(t, path))

				_, err := accounts.Open(path)

				if !errors.Is(err, accounts.ErrSecretsDamaged) {
					t.Errorf("Open = %v, want accounts.ErrSecretsDamaged", err)
				}
				if sha256.Sum256(readFile(t, path)) != before {
					t.Error("the refused file changed")
				}
			})
		}
	}
}

func TestSecrets_returnsACopy(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "accounts.db"))
	got := db.Secrets()
	want := bytes.Clone(got.FailureKey)
	got.FailureKey[0] ^= 0xff
	if again := db.Secrets().FailureKey; !bytes.Equal(again, want) {
		t.Errorf("FailureKey after the caller changed its copy = %x, want %x", again, want)
	}
}

func TestGet_beforeAnyPutAnswersTheDefaults(t *testing.T) {
	got, err := open(t, filepath.Join(t.TempDir(), "accounts.db")).Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := accounts.Settings{Retention: 72 * time.Hour, Methods: accounts.Methods{Password: true, Passkeys: true, OIDC: true}}
	if got != want {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
}

func TestPut_keepsTheValuesAcrossAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db := open(t, path)
	put := accounts.Settings{Retention: 36*time.Hour + 30*time.Minute, Methods: accounts.Methods{Passkeys: true}}
	if err := db.Put(t.Context(), put); err != nil {
		t.Fatal(err)
	}
	closeDB(t, db)

	got, err := open(t, path).Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := put
	want.Revision = 1
	if got != want {
		t.Errorf("Get after reopen = %+v, want %+v", got, want)
	}
}

func TestPut_bumpsTheRevisionItWasGiven(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "accounts.db"))
	for rev := range uint64(3) {
		got, err := db.Get(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if got.Revision != rev || db.Current().Revision != rev {
			t.Fatalf("before save %d: Get revision %d, Current revision %d; want %d", rev, got.Revision, db.Current().Revision, rev)
		}
		if err := db.Put(t.Context(), accounts.Settings{Revision: rev, Retention: time.Duration(rev+2) * time.Hour}); err != nil {
			t.Fatalf("Put of revision %d: %v", rev, err)
		}
	}
	if got := db.Current(); got.Revision != 3 || got.Retention != 4*time.Hour {
		t.Errorf("Current after three saves = %+v, want revision 3 and the last retention, 4h", got)
	}
}

func TestPut_refusesAStaleRevisionAndKeepsTheStoredSettings(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "accounts.db"))
	first := accounts.Settings{Retention: 5 * time.Hour, Methods: accounts.Methods{Password: true}}
	if err := db.Put(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	for _, rev := range []uint64{0, 2} {
		err := db.Put(t.Context(), accounts.Settings{Revision: rev, Retention: 9 * time.Hour})
		if !errors.Is(err, accounts.ErrStaleRevision) {
			t.Errorf("Put of revision %d over revision 1 = %v, want ErrStaleRevision", rev, err)
		}
	}
	want := first
	want.Revision = 1
	if got, err := db.Get(t.Context()); err != nil || got != want {
		t.Errorf("Get after the refused saves = %+v, %v; want %+v", got, err, want)
	}
	if got := db.Current(); got != want {
		t.Errorf("Current after the refused saves = %+v, want %+v", got, want)
	}
}

func TestPut_ConcurrentSavesOfOneRevisionStoreOne(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "accounts.db"))
	const savers = 8
	errs := make([]error, savers)
	var wg sync.WaitGroup
	for i := range savers {
		wg.Go(func() {
			errs[i] = db.Put(t.Context(), accounts.Settings{Retention: time.Duration(i+1) * time.Hour})
		})
	}
	wg.Wait()
	stored, stale := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			stored++
		case errors.Is(err, accounts.ErrStaleRevision):
			stale++
		default:
			t.Errorf("saver %d: %v, want nil or ErrStaleRevision", i, err)
		}
	}
	if stored != 1 || stale != savers-1 {
		t.Errorf("%d saves of revision 0: %d stored and %d stale, want 1 and %d", savers, stored, stale, savers-1)
	}
	if got := db.Current().Revision; got != 1 {
		t.Errorf("revision after the race = %d, want 1", got)
	}
}

func TestPut_storesARetentionAtEitherBound(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "accounts.db"))
	for _, retention := range []time.Duration{time.Hour, 90 * 24 * time.Hour} {
		t.Run(retention.String(), func(t *testing.T) {
			if err := db.Put(t.Context(), accounts.Settings{Revision: db.Current().Revision, Retention: retention}); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := db.Get(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if got.Retention != retention {
				t.Errorf("retention after Put = %v, want %v", got.Retention, retention)
			}
		})
	}
}

func TestPut_refusesARetentionOutOfBounds(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "accounts.db"))
	for _, c := range []struct {
		retention time.Duration
		whole     bool
	}{
		{time.Hour - time.Minute, false},
		{90*24*time.Hour + time.Minute, false},
		{0, false},
		{-time.Hour, false},
		{2*time.Hour + 30*time.Second, true},
		{90*24*time.Hour - time.Millisecond, true},
	} {
		t.Run(c.retention.String(), func(t *testing.T) {
			err := db.Put(t.Context(), accounts.Settings{Retention: c.retention})
			var re *accounts.RangeError
			if !errors.As(err, &re) {
				t.Fatalf("Put = %v, want a *RangeError", err)
			}
			if want := (accounts.RangeError{Min: time.Hour, Max: 90 * 24 * time.Hour, Whole: c.whole}); *re != want {
				t.Errorf("RangeError = %+v, want %+v", *re, want)
			}
			got, gerr := db.Get(t.Context())
			if gerr != nil {
				t.Fatal(gerr)
			}
			if want := accounts.DefaultSettings(); got != want {
				t.Errorf("settings after a refused Put = %+v, want the defaults %+v", got, want)
			}
		})
	}
}

func TestRetention_followsTheStoredSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db := open(t, path)
	if got := db.Retention(); got != 72*time.Hour {
		t.Errorf("Retention before any Put = %v, want 72h", got)
	}
	if err := db.Put(t.Context(), accounts.Settings{Retention: 5 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if got := db.Retention(); got != 5*time.Hour {
		t.Errorf("Retention after Put = %v, want 5h", got)
	}
	var re *accounts.RangeError
	if err := db.Put(t.Context(), accounts.Settings{Revision: 1, Retention: time.Minute}); !errors.As(err, &re) {
		t.Fatalf("Put of 1m = %v, want a *RangeError", err)
	}
	if got := db.Retention(); got != 5*time.Hour {
		t.Errorf("Retention after a refused Put = %v, want 5h kept", got)
	}
	closeDB(t, db)
	if got := open(t, path).Retention(); got != 5*time.Hour {
		t.Errorf("Retention after reopen = %v, want 5h", got)
	}
}

func TestOpen_aFileOfAnotherFormatFailsNamingItAndKeepsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	s := bolttable.NewSchema("grelot-accounts")
	ns := bolttable.Must(s.Namespace("settings"))
	tbl := bolttable.Must(bolttable.Define(ns, 1, "settings",
		bolttable.Uint64Key{}, bolttable.JSON[struct{ Old bool }](99), bolttable.TableOptions{Silent: true}))
	other, err := bolttable.Open(path, s, bolttable.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Update(t.Context(), func(tx *bolttable.Tx) error {
		return tbl.Put(tx, 1, &struct{ Old bool }{Old: true})
	}); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)

	_, err = accounts.Open(path)

	if !errors.Is(err, bolttable.ErrFormat) || !strings.Contains(err.Error(), path) {
		t.Errorf("Open = %v, want a format error naming %s", err, path)
	}
	if !bytes.Equal(readFile(t, path), before) {
		t.Error("the refused file changed")
	}
}

func TestOpen_anUnreadableFileFailsNamingItAndKeepsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	junk := bytes.Repeat([]byte{0xa5}, 64<<10)
	if err := os.WriteFile(path, junk, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := accounts.Open(path)

	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("Open = %v, want an error naming %s", err, path)
	}
	if !bytes.Equal(readFile(t, path), junk) {
		t.Error("the refused file changed")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".invalid") {
			t.Errorf("Open moved the file aside to %s", e.Name())
		}
	}
}

// A message store opened on a file of another format resets it; the
// accounts file beside it keeps its settings.
func TestOpen_aStoreResetLeavesTheAccountsFile(t *testing.T) {
	dir := t.TempDir()
	accPath, storePath := filepath.Join(dir, "accounts.db"), filepath.Join(dir, "grelot.db")
	db := open(t, accPath)
	want := accounts.Settings{Retention: 5 * time.Hour, Methods: accounts.Methods{Password: true}}
	if err := db.Put(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	want.Revision = 1
	secrets := db.Secrets()
	closeDB(t, db)

	s := bolttable.NewSchema("grelot")
	ns := bolttable.Must(s.Namespace("grelot"))
	bolttable.Must(bolttable.Define(ns, 1, "identities",
		bolttable.Uint64Key{}, bolttable.JSON[struct{}](99), bolttable.TableOptions{Silent: true}))
	other, err := bolttable.Open(storePath, s, bolttable.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	st, err := store.Open(storePath, &store.Options{Logger: slog.New(slog.NewJSONHandler(&log, nil))})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), `"msg":"store reset"`) {
		t.Fatalf("store.Open logged %s, want a store reset", log.String())
	}

	again := open(t, accPath)
	got, err := again.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != want || !bytes.Equal(again.Secrets().FailureKey, secrets.FailureKey) {
		t.Errorf("after the store reset: settings %+v and the failure key changed=%v, want %+v and unchanged",
			got, !bytes.Equal(again.Secrets().FailureKey, secrets.FailureKey), want)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCurrent_followsTheStoredSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	db := open(t, path)
	if got := db.Current(); got != accounts.DefaultSettings() {
		t.Errorf("Current before any Put = %+v, want the defaults", got)
	}
	want := accounts.Settings{Retention: 5 * time.Hour, Methods: accounts.Methods{Passkeys: true}}
	if err := db.Put(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	want.Revision = 1
	if got := db.Current(); got != want {
		t.Errorf("Current after Put = %+v, want %+v", got, want)
	}
	closeDB(t, db)
	if got := open(t, path).Current(); got != want {
		t.Errorf("Current after reopen = %+v, want %+v", got, want)
	}
}
