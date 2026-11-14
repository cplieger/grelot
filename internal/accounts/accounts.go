//go:build !windows

// Package accounts holds what must survive a reset of the message store:
// the operator's settings, the server's generated secrets and access's
// accounts, sessions and API keys, in a bolttable file of their own. It does not build on Windows, where
// bolttable does not run.
package accounts

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/cplieger/bolttable"
)

// Table prefixes are stored bytes: changing one changes the namespace's
// format digest, and the next start refuses the file.
const (
	prefixSettings byte = 1 + iota
	prefixSecrets
	prefixUsers
	prefixSessions
	prefixPasskeys
	prefixKeys
	prefixTokens
	prefixAttributions
	prefixFailures
	prefixLog
	prefixLogStats
)

// rowKey is the key of the one row the settings and secrets tables each hold.
const rowKey uint64 = 1

// secretBytes is the length of each generated secret.
const secretBytes = 32

// Secrets are the keys the server generates at the first open and keeps,
// so what they seal or sign stays valid across restarts.
type Secrets struct {
	FailureKey []byte
	DeviceKey  []byte
	PendingKey []byte
}

type secretsRow struct {
	FailureKey []byte
	DeviceKey  []byte
	PendingKey []byte
}

type tables struct {
	settings *bolttable.Table[uint64, settingsRow]
	secrets  *bolttable.Table[uint64, secretsRow]
}

// DB is an open accounts file.
type DB struct {
	db     *bolttable.DB
	access *Store
	t      tables
	// current is the stored settings; putMu orders Put's commit and store,
	// so a slower Put never leaves an older value here.
	current atomic.Pointer[Settings]
	secrets Secrets
	putMu   sync.Mutex
}

func newSchema() (*bolttable.Schema, tables, *accessTables, error) {
	s := bolttable.NewSchema("grelot-accounts")
	var t tables
	ns, err := s.Namespace("settings")
	if err != nil {
		return nil, t, nil, err
	}
	silent := bolttable.TableOptions{Silent: true}
	if t.settings, err = bolttable.Define(ns, prefixSettings, "settings",
		bolttable.Uint64Key{}, bolttable.JSON[settingsRow](1), silent); err != nil {
		return nil, t, nil, err
	}
	if t.secrets, err = bolttable.Define(ns, prefixSecrets, "secrets",
		bolttable.Uint64Key{}, bolttable.JSON[secretsRow](1), silent); err != nil {
		return nil, t, nil, err
	}
	accessNS, err := s.Namespace("access")
	if err != nil {
		return nil, t, nil, err
	}
	at, err := defineAccess(accessNS)
	if err != nil {
		return nil, t, nil, err
	}
	return s, t, &at, nil
}

// Open opens or creates the accounts file at path and generates the
// secrets on its first open. Unlike the message store it never resets or
// moves the file aside: a file of another format or one bbolt cannot read
// fails with an error naming path, the file left as it was.
func Open(path string) (*DB, error) { return open(path, &bolttable.Options{}) }

func open(path string, o *bolttable.Options) (*DB, error) {
	schema, t, at, err := newSchema()
	if err != nil {
		return nil, err
	}
	db, err := bolttable.Open(path, schema, *o)
	if err != nil {
		return nil, fmt.Errorf("accounts file %s: %w; restore it from a backup, or delete it to start with no accounts", path, err)
	}
	d := &DB{db: db, access: &Store{db: db, t: at}, t: t}
	ctx := context.Background()
	if d.secrets, err = d.ensureSecrets(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("accounts file %s: %w", path, err)
	}
	s, err := d.Get(ctx)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("accounts file %s: %w", path, err)
	}
	d.current.Store(&s)
	return d, nil
}

// Access returns the accounts, sessions and keys as access's account.Store.
func (d *DB) Access() *Store { return d.access }

// Close closes the file, waiting for open transactions.
func (d *DB) Close() error { return d.db.Close() }

// Secrets returns a copy of the generated secrets.
func (d *DB) Secrets() Secrets {
	return Secrets{
		FailureKey: slices.Clone(d.secrets.FailureKey),
		DeviceKey:  slices.Clone(d.secrets.DeviceKey),
		PendingKey: slices.Clone(d.secrets.PendingKey),
	}
}

func (d *DB) ensureSecrets(ctx context.Context) (Secrets, error) {
	var row secretsRow
	err := d.db.Update(ctx, func(tx *bolttable.Tx) error {
		got, found, err := d.t.secrets.Get(tx.Read(), rowKey)
		if err != nil || found {
			row = got
			return err
		}
		row = secretsRow{FailureKey: newSecret(), DeviceKey: newSecret(), PendingKey: newSecret()}
		return d.t.secrets.Put(tx, rowKey, &row)
	})
	return Secrets(row), err
}

// newSecret relies on crypto/rand.Read never returning an error (Go 1.24).
func newSecret() []byte {
	b := make([]byte, secretBytes)
	_, _ = rand.Read(b)
	return b
}
