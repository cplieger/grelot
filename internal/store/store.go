// Package store holds grelot's state in one bolttable file: identities
// and their delegate links, threads, messages and their deliveries,
// reservations, receipt watches, and the leases that expire them.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/cplieger/bolttable"
	"github.com/cplieger/grelot/internal/obs"
	bolterrors "go.etcd.io/bbolt/errors"
)

var (
	// ErrLocked reports that another process holds the store file.
	ErrLocked = errors.New("store: file held by another process")
	// ErrClosed reports a call on a closed store, as during shutdown.
	ErrClosed = errors.New("store: closed")
)

const (
	msgStoreReset    = "store reset"
	msgStoreCorrupt  = "store corrupt"
	msgCompactFailed = "store compaction failed"
)

// Default lifetimes, used where Options leaves one zero.
const (
	DefaultThreadTTL  = 72 * time.Hour
	DefaultDormantTTL = 72 * time.Hour
	DefaultMaxBytes   = 512 << 20
	// DefaultReservationMaxTTL is RESERVATION_MAX_TTL's default.
	DefaultReservationMaxTTL = 8 * time.Hour
	// DefaultPathRoot is GRELOT_PATH_ROOT's default.
	DefaultPathRoot = "/workspace"
)

// Options configures Open; nil is the zero Options. A nil Logger discards; a nil Now is time.Now;
// a nil Metrics records to a page nobody serves; a zero TTL takes its
// default.
type Options struct {
	Logger *slog.Logger
	Now    func() time.Time
	// Metrics is the /metrics page the store records its opens, sweeps,
	// posts and nudges into.
	Metrics *obs.Metrics
	// Woke, when set, is called after each commit that delivered to an
	// identity, recorded a nudge for it or moved its read, with those ids.
	// It runs in commit order and must not start a write transaction.
	Woke func(ids []uint64)
	// PathRoot is the canonical root every repo lies in, <root>/<repo>,
	// which Who's Repo filter reads; the caller checks it is canonical.
	PathRoot string
	// ThreadTTL is how long a path or direct thread, a post and an event
	// live after they are written.
	ThreadTTL time.Duration
	// DormantTTL is how long an expired identity stays revivable.
	DormantTTL time.Duration
	// MaxBytes is the ceiling on the bytes in use; posts are refused past
	// 90 % of it.
	MaxBytes int64
	// ReservationMaxTTL is the longest TTL a reservation may declare.
	ReservationMaxTTL time.Duration
}

// Store is an open grelot store.
type Store struct {
	db  *bolttable.DB
	t   *tables
	log *slog.Logger
	now func() time.Time
	m   *obs.Metrics
	// space reads the file's space; tests replace it.
	space func() (bolttable.Space, error)
	woke  func(ids []uint64)
	// seen is each identity's newest request time; seenMu guards it.
	seen       map[uint64]time.Time
	pathRoot   string
	resvPolicy leasePolicy
	threadTTL  time.Duration
	dormantTTL time.Duration
	maxBytes   int64
	seenMu     sync.Mutex
}

// Open opens or creates the store file at path. A file another process
// holds fails with ErrLocked, and any other failure leaves the path as it
// was, but for three repairs: a file of another format is reset to empty,
// with a WARN; a file bbolt cannot read is renamed
// <path>.<UTC stamp>.invalid and replaced by an empty store, with an
// ERROR; a file at least half and 4 MiB free is compacted, and a failed
// compaction leaves it whole with a WARN.
func Open(path string, o *Options) (*Store, error) {
	if o == nil {
		o = &Options{}
	}
	s := newStore(o)
	schema, t, err := newSchema()
	if err != nil {
		return nil, err
	}
	db, err := bolttable.Open(path, schema, openOptions)
	if errors.Is(err, bolttable.ErrLocked) {
		return nil, fmt.Errorf("%w: %w", ErrLocked, err)
	}
	if err != nil {
		return nil, err
	}
	rep := db.OpenReport()
	s.logOpen(path, &rep)
	s.db, s.t, s.space = db, t, db.Space
	s.wireLeases()
	if err := s.ensureServerThreads(context.Background()); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return s, nil
}

// openOptions resets every namespace of another format with no copy kept:
// the rows all expire within days and agents register again, so a format
// change keeps nothing.
var openOptions = bolttable.Options{
	Compact:     bolttable.CompactPolicy{MinFreeRatio: 0.5, MinFreeBytes: 4 << 20},
	Reset:       bolttable.ResetPolicy{Allow: func(bolttable.NamespaceDigest) bool { return true }},
	InvalidFile: bolttable.InvalidFileMoveAside,
}

// logOpen logs and counts each repair Open made to the file.
func (s *Store) logOpen(path string, rep *bolttable.OpenReport) {
	if rep.MovedAside != "" {
		s.log.Error(msgStoreCorrupt, "path", path, "moved_to", rep.MovedAside, "error", rep.Invalid.Error())
		s.m.StoreStartedEmpty.Inc(obs.StartedEmptyCorrupt)
	}
	if len(rep.Reset) > 0 {
		s.m.StoreStartedEmpty.Inc(obs.StartedEmptyFormat)
		names := make([]string, 0, len(rep.Reset))
		for _, n := range rep.Reset {
			names = append(names, n.Name)
		}
		s.log.Warn(msgStoreReset, "path", path, "namespaces", names)
	}
	if rep.CompactErr != nil {
		s.log.Warn(msgCompactFailed, "path", path, "error", rep.CompactErr.Error())
	}
}

// newStore is a Store configured by o, each zero option taking its default.
func newStore(o *Options) *Store {
	s := &Store{log: o.Logger, now: o.Now, m: o.Metrics, woke: o.Woke, threadTTL: o.ThreadTTL, dormantTTL: o.DormantTTL, maxBytes: o.MaxBytes, seen: map[uint64]time.Time{}}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.m == nil {
		s.m = obs.New()
	}
	if s.threadTTL == 0 {
		s.threadTTL = DefaultThreadTTL
	}
	if s.dormantTTL == 0 {
		s.dormantTTL = DefaultDormantTTL
	}
	if s.maxBytes == 0 {
		s.maxBytes = DefaultMaxBytes
	}
	s.pathRoot = o.PathRoot
	if s.pathRoot == "" {
		s.pathRoot = DefaultPathRoot
	}
	s.resvPolicy = reservationPolicy
	if o.ReservationMaxTTL != 0 {
		s.resvPolicy.max = o.ReservationMaxTTL
	}
	return s
}

// closed is err as ErrClosed when it is bbolt's closed-database error.
func closed(err error) error {
	if errors.Is(err, bolterrors.ErrDatabaseNotOpen) {
		return fmt.Errorf("%w: %w", ErrClosed, err)
	}
	return err
}

// Close closes the file, waiting for open transactions.
func (s *Store) Close() error { return s.db.Close() }

// wtx is one write transaction with the time it runs at.
type wtx struct {
	now time.Time
	s   *Store
	tx  *bolttable.Tx
	r   *bolttable.ReadTx
	// cards and events are owed so far, written once fn is done.
	cards  []card
	events []owedEvent
	// woke is every identity Options.Woke reports once this commits.
	woke []uint64
}

// wake has Options.Woke report id once this transaction commits.
func (w *wtx) wake(id uint64) {
	if !slices.Contains(w.woke, id) {
		w.woke = append(w.woke, id)
	}
}

// update runs fn in one write transaction, at one instant of the clock,
// then writes the cards and events it owes.
func (s *Store) update(ctx context.Context, fn func(*wtx) error) error {
	return closed(s.db.Update(ctx, func(tx *bolttable.Tx) error {
		w := &wtx{s: s, tx: tx, r: tx.Read(), now: s.now()}
		if err := fn(w); err != nil {
			return err
		}
		if err := w.flushOwed(); err != nil {
			return err
		}
		if ids := w.woke; len(ids) > 0 && s.woke != nil {
			tx.AfterCommit(func() { s.woke(ids) })
		}
		return nil
	}))
}

// read runs fn in one read transaction.
func (s *Store) read(ctx context.Context, fn func(*bolttable.ReadTx) error) error {
	return closed(s.db.View(ctx, fn))
}

// sweepBatch bounds the rows one sweep transaction handles, so a backlog
// never holds the write lock for long.
const sweepBatch = 256

// Sweep runs every due warning and expiry in batches, one transaction
// each, until none is due, and reports how many rows it handled. It also
// forgets the request time of every identity whose row is gone.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	total := 0
	for first := true; ; first = false {
		n := 0
		err := s.update(ctx, func(w *wtx) error {
			if first {
				if err := w.pruneSeen(); err != nil {
					return err
				}
			}
			var err error
			n, err = w.sweepDue()
			return err
		})
		total += n
		if err != nil || n < sweepBatch {
			return total, err
		}
	}
}

// sweepDue runs up to sweepBatch due warnings and expiries across the
// tables and reports how many it ran.
func (w *wtx) sweepDue() (int, error) {
	n := 0
	for _, l := range w.s.t.leased {
		got, err := l.sweep(w, sweepBatch-n)
		n += got
		if err != nil || n >= sweepBatch {
			return n, err
		}
	}
	return n, nil
}
