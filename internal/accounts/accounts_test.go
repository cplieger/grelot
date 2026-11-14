//go:build !windows

package accounts_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	keys := map[string][]byte{"FailureKey": first.FailureKey, "DeviceKey": first.DeviceKey, "PendingKey": first.PendingKey}
	for name, k := range keys {
		if len(k) != 32 || bytes.Equal(k, make([]byte, 32)) {
			t.Errorf("%s = %x, want 32 random bytes", name, k)
		}
	}
	if bytes.Equal(first.FailureKey, first.DeviceKey) || bytes.Equal(first.DeviceKey, first.PendingKey) ||
		bytes.Equal(first.FailureKey, first.PendingKey) {
		t.Errorf("secrets %x, want three different keys", keys)
	}
	closeDB(t, db)

	again := open(t, path).Secrets()
	if !bytes.Equal(again.FailureKey, first.FailureKey) || !bytes.Equal(again.DeviceKey, first.DeviceKey) ||
		!bytes.Equal(again.PendingKey, first.PendingKey) {
		t.Errorf("secrets after reopen %+v, want the first open's %+v", again, first)
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
	want := accounts.Settings{Retention: 36*time.Hour + 30*time.Minute, Methods: accounts.Methods{Passkeys: true}}
	if err := db.Put(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	closeDB(t, db)

	got, err := open(t, path).Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Get after reopen = %+v, want %+v", got, want)
	}
}

func TestPut_storesARetentionAtEitherBound(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "accounts.db"))
	for _, retention := range []time.Duration{time.Hour, 90 * 24 * time.Hour} {
		t.Run(retention.String(), func(t *testing.T) {
			if err := db.Put(t.Context(), accounts.Settings{Retention: retention}); err != nil {
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
	for _, retention := range []time.Duration{
		time.Hour - time.Minute, 90*24*time.Hour + time.Minute, 2*time.Hour + 30*time.Second, 0, -time.Hour,
	} {
		t.Run(retention.String(), func(t *testing.T) {
			err := db.Put(t.Context(), accounts.Settings{Retention: retention})
			if !errors.Is(err, accounts.ErrInvalid) {
				t.Errorf("Put = %v, want ErrInvalid", err)
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
	if err := db.Put(t.Context(), accounts.Settings{Retention: time.Minute}); !errors.Is(err, accounts.ErrInvalid) {
		t.Fatalf("Put of 1m = %v, want ErrInvalid", err)
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
	if got := db.Current(); got != want {
		t.Errorf("Current after Put = %+v, want %+v", got, want)
	}
	closeDB(t, db)
	if got := open(t, path).Current(); got != want {
		t.Errorf("Current after reopen = %+v, want %+v", got, want)
	}
}
