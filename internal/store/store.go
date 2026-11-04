// Package store holds grelot's state in one bolttable file: identities,
// threads, messages and their deliveries, and the leases that expire them.
package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/cplieger/bolttable"
	bolterrors "go.etcd.io/bbolt/errors"
)

// ErrLocked reports that another process holds the store file.
var ErrLocked = errors.New("store: file held by another process")

const (
	msgStoreReset   = "store reset"
	msgStoreCorrupt = "store corrupt"
)

// Default lifetimes, used where Options leaves one zero.
const (
	DefaultThreadTTL  = 72 * time.Hour
	DefaultDormantTTL = 72 * time.Hour
)

// Options configures Open. A nil Logger discards; a nil Now is time.Now;
// a zero TTL takes its default.
type Options struct {
	Logger *slog.Logger
	Now    func() time.Time
	// ThreadTTL is how long a path or direct thread, a post and an event
	// live after they are written.
	ThreadTTL time.Duration
	// DormantTTL is how long an expired identity stays revivable.
	DormantTTL time.Duration
}

// Store is an open grelot store.
type Store struct {
	db         *bolttable.DB
	t          *tables
	log        *slog.Logger
	now        func() time.Time
	threadTTL  time.Duration
	dormantTTL time.Duration
}

// Open opens or creates the store file at path. A file another process
// holds fails with ErrLocked; any other failure but the two below also
// fails. Either way the file is left untouched. A file of another format is
// reset to empty, with a WARN. A file bbolt reports as invalid is renamed
// <path>.corrupt-<UTC time as 20060102T150405Z> and replaced by an empty
// store, with an ERROR.
func Open(path string, o Options) (*Store, error) {
	s := &Store{log: o.Logger, now: o.Now, threadTTL: o.ThreadTTL, dormantTTL: o.DormantTTL}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.threadTTL == 0 {
		s.threadTTL = DefaultThreadTTL
	}
	if s.dormantTTL == 0 {
		s.dormantTTL = DefaultDormantTTL
	}
	db, t, err := openFile(path)
	if fe := (*bolttable.FormatError)(nil); errors.As(err, &fe) {
		err = s.reset(path, fe)
		if err == nil {
			db, t, err = openFile(path)
		}
	}
	if isCorrupt(err) {
		err = s.moveAside(path, err)
		if err == nil {
			db, t, err = openFile(path)
		}
	}
	if errors.Is(err, bolterrors.ErrTimeout) {
		return nil, fmt.Errorf("%w: %w", ErrLocked, err)
	}
	if err != nil {
		return nil, err
	}
	s.db, s.t = db, t
	s.wireLeases()
	return s, nil
}

func openFile(path string) (*bolttable.DB, *tables, error) {
	schema, t, err := newSchema()
	if err != nil {
		return nil, nil, err
	}
	db, err := bolttable.Open(path, schema, bolttable.Options{})
	return db, t, err
}

// reset empties every namespace fe lists. The store's rows all expire
// within days and agents register again, so a format change keeps nothing.
func (s *Store) reset(path string, fe *bolttable.FormatError) error {
	names := make([]string, 0, len(fe.Namespaces))
	for _, n := range fe.Namespaces {
		names = append(names, n.Name)
	}
	schema, _, err := newSchema()
	if err != nil {
		return err
	}
	if err := bolttable.ResetNamespaces(path, schema, names); err != nil {
		return err
	}
	s.log.Warn(msgStoreReset, "path", path, "namespaces", names)
	return nil
}

// isCorrupt reports bbolt's own verdict on a file that is not a database
// it can open; bolttable passes these errors through wrapped.
func isCorrupt(err error) bool {
	return errors.Is(err, bolterrors.ErrInvalid) ||
		errors.Is(err, bolterrors.ErrChecksum) ||
		errors.Is(err, bolterrors.ErrVersionMismatch)
}

// moveAside renames a corrupt file out of the way, so the store starts
// empty: its data is short-lived, and serving beats keeping a broken file
// in place. It never overwrites an earlier corrupt copy, and leaves alone
// anything but a regular file, which bbolt also reports as invalid.
func (s *Store) moveAside(path string, cause error) error {
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return cause
	}
	dst := path + ".corrupt-" + s.now().UTC().Format("20060102T150405Z")
	if _, err := os.Lstat(dst); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: %s is corrupt (%w) and %s exists", path, cause, dst)
	}
	if err := os.Rename(path, dst); err != nil {
		return fmt.Errorf("store: move aside %s: %w", path, err)
	}
	s.log.Error(msgStoreCorrupt, "path", path, "moved_to", dst, "error", cause.Error())
	return nil
}

// Close closes the file, waiting for open transactions.
func (s *Store) Close() error { return s.db.Close() }

// wtx is one write transaction with the time it runs at.
type wtx struct {
	now time.Time
	s   *Store
	tx  *bolttable.Tx
	r   *bolttable.ReadTx
}

// update runs fn in one write transaction, at one instant of the clock.
func (s *Store) update(ctx context.Context, fn func(*wtx) error) error {
	return s.db.Update(ctx, func(tx *bolttable.Tx) error {
		return fn(&wtx{s: s, tx: tx, r: tx.Read(), now: s.now()})
	})
}

// sweepBatch bounds the rows one sweep transaction handles, so a backlog
// never holds the write lock for long.
const sweepBatch = 256

// Sweep runs every due warning and expiry in batches, one transaction
// each, until none is due, and reports how many rows it handled.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	total := 0
	for {
		n := 0
		err := s.update(ctx, func(w *wtx) error {
			for _, l := range s.t.leased {
				got, err := l.sweep(w, sweepBatch-n)
				n += got
				if err != nil || n >= sweepBatch {
					return err
				}
			}
			return nil
		})
		total += n
		if err != nil || n < sweepBatch {
			return total, err
		}
	}
}
