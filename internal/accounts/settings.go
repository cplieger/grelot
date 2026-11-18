//go:build !windows

package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cplieger/bolttable"
	"github.com/cplieger/grelot/internal/store"
)

// The bounds Put holds the history retention to; RangeError's sentence
// names them.
const (
	MinRetention = time.Hour
	MaxRetention = 90 * 24 * time.Hour
)

// ErrStaleRevision refuses a Put whose Revision is not the stored one,
// leaving the stored settings as they were.
var ErrStaleRevision = errors.New("the settings changed since they were read: read them again")

// RangeError refuses a retention outside Min to Max, or, when Whole, one
// within them that is not a whole number of minutes.
type RangeError struct {
	Min, Max time.Duration
	Whole    bool
}

func (e *RangeError) Error() string {
	if e.Whole {
		return "the history retention must be a whole number of minutes"
	}
	return fmt.Sprintf("the history retention must be from %s to %s", spanWords(e.Min), spanWords(e.Max))
}

// spanWords says d in whole days when it is a number of days, else in
// hours.
func spanWords(d time.Duration) string {
	n, unit := int64(d/time.Hour), "hour"
	if d%(24*time.Hour) == 0 {
		n, unit = int64(d/(24*time.Hour)), "day"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}

// Methods are the sign-in methods the operator has left on.
type Methods struct {
	Password bool
	Passkeys bool
	OIDC     bool
}

// Settings are what the operator changes in the web UI.
type Settings struct {
	// Revision counts the stored saves, 0 before the first; Put stores
	// only over the revision it is given.
	Revision uint64
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

// settingsRow's Revision is absent from a row written before it, which
// reads as 0.
type settingsRow struct {
	RetentionMS int64
	Methods     Methods
	Revision    uint64
}

// Get returns the stored settings, or DefaultSettings when none are.
func (d *DB) Get(ctx context.Context) (Settings, error) {
	s := DefaultSettings()
	err := d.db.View(ctx, func(r *bolttable.ReadTx) error {
		row, found, err := d.t.settings.Get(r, rowKey)
		if found {
			s = Settings{Revision: row.Revision, Retention: time.Duration(row.RetentionMS) * time.Millisecond, Methods: row.Methods}
		}
		return err
	})
	return s, err
}

// Put stores s as revision s.Revision+1. It refuses with *RangeError a
// retention outside MinRetention and MaxRetention or not a whole number
// of minutes, and with ErrStaleRevision an s.Revision that is not the
// stored one; the revision is compared in the update that writes, so of
// two Puts of one revision only the first stores.
func (d *DB) Put(ctx context.Context, s Settings) error {
	switch {
	case s.Retention < MinRetention || s.Retention > MaxRetention:
		return &RangeError{Min: MinRetention, Max: MaxRetention}
	case s.Retention%time.Minute != 0:
		return &RangeError{Min: MinRetention, Max: MaxRetention, Whole: true}
	}
	row := settingsRow{RetentionMS: s.Retention.Milliseconds(), Methods: s.Methods, Revision: s.Revision + 1}
	d.putMu.Lock()
	defer d.putMu.Unlock()
	if err := d.db.Update(ctx, func(tx *bolttable.Tx) error {
		stored, _, err := d.t.settings.Get(tx.Read(), rowKey)
		switch {
		case err != nil:
			return err
		case stored.Revision != s.Revision:
			return ErrStaleRevision
		}
		return d.t.settings.Put(tx, rowKey, &row)
	}); err != nil {
		return err
	}
	s.Revision = row.Revision
	d.current.Store(&s)
	return nil
}

// Current is the stored settings as of the last committed Put. It never
// blocks, so access reads it once per operation.
func (d *DB) Current() Settings { return *d.current.Load() }

// Retention is Current's history retention; it never blocks, so the store
// reads it inside each write transaction.
func (d *DB) Retention() time.Duration { return d.current.Load().Retention }
