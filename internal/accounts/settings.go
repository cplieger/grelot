//go:build !windows

package accounts

import (
	"context"
	"errors"
	"time"

	"github.com/cplieger/bolttable"
	"github.com/cplieger/grelot/internal/store"
)

// The bounds Put holds the history retention to; its refusal names them in
// words.
const (
	MinRetention = time.Hour
	MaxRetention = 90 * 24 * time.Hour
)

// ErrInvalid reports settings Put refuses, in words a user can act on.
var ErrInvalid = errors.New("the history retention must be a whole number of minutes from 1 hour to 90 days")

// Methods are the sign-in methods the operator has left on.
type Methods struct {
	Password bool
	Passkeys bool
	OIDC     bool
}

// Settings are what the operator changes in the web UI.
type Settings struct {
	// Retention is how long a thread, post or event lives after it is
	// written.
	Retention time.Duration
	Methods   Methods
}

// DefaultSettings is what Get answers before the first Put.
func DefaultSettings() Settings {
	return Settings{
		Retention: store.DefaultRetention,
		Methods:   Methods{Password: true, Passkeys: true, OIDC: true},
	}
}

type settingsRow struct {
	RetentionMS int64
	Methods     Methods
}

// Get returns the stored settings, or DefaultSettings when none are.
func (d *DB) Get(ctx context.Context) (Settings, error) {
	s := DefaultSettings()
	err := d.db.View(ctx, func(r *bolttable.ReadTx) error {
		row, found, err := d.t.settings.Get(r, rowKey)
		if found {
			s = Settings{Retention: time.Duration(row.RetentionMS) * time.Millisecond, Methods: row.Methods}
		}
		return err
	})
	return s, err
}

// Put stores s, refusing with ErrInvalid a retention outside MinRetention
// and MaxRetention or not a whole number of minutes.
func (d *DB) Put(ctx context.Context, s Settings) error {
	if s.Retention < MinRetention || s.Retention > MaxRetention || s.Retention%time.Minute != 0 {
		return ErrInvalid
	}
	row := settingsRow{RetentionMS: s.Retention.Milliseconds(), Methods: s.Methods}
	d.putMu.Lock()
	defer d.putMu.Unlock()
	if err := d.db.Update(ctx, func(tx *bolttable.Tx) error {
		return d.t.settings.Put(tx, rowKey, &row)
	}); err != nil {
		return err
	}
	d.current.Store(&s)
	return nil
}

// Current is the stored settings as of the last committed Put. It never
// blocks, so access reads it once per operation.
func (d *DB) Current() Settings { return *d.current.Load() }

// Retention is Current's history retention; it never blocks, so the store
// reads it inside each write transaction.
func (d *DB) Retention() time.Duration { return d.current.Load().Retention }
