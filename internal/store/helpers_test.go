package store

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

// testNow is the fixed clock every store test starts from.
var testNow = time.Date(2026, 11, 4, 5, 39, 0, 0, time.UTC)

// logSink is a slog.Handler that keeps every record, so a test can assert
// the level and message of the lines a store call wrote.
type logSink struct {
	mu      *sync.Mutex
	records *[]slog.Record
}

func newLogSink() logSink {
	return logSink{mu: &sync.Mutex{}, records: &[]slog.Record{}}
}

func (logSink) Enabled(context.Context, slog.Level) bool { return true }

func (l logSink) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	*l.records = append(*l.records, r.Clone())
	return nil
}

func (l logSink) WithAttrs([]slog.Attr) slog.Handler { return l }

func (l logSink) WithGroup(string) slog.Handler { return l }

// lines returns the records written at level with message msg.
func (l logSink) lines(level slog.Level, msg string) []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []slog.Record
	for _, r := range *l.records {
		if r.Level == level && r.Message == msg {
			out = append(out, r)
		}
	}
	return out
}

// attr returns the string value of the record's attribute key.
func attr(r slog.Record, key string) string {
	var v string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.String()
			return false
		}
		return true
	})
	return v
}

// clock is a settable test clock, safe across goroutines.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *clock) advance(d time.Duration) { c.set(c.now().Add(d)) }

// openClockStore opens a store in a fresh directory on a clock the test
// moves, starting at testNow.
func openClockStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	clk := &clock{t: testNow}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{
		Logger: slog.New(newLogSink()),
		Now:    clk.now,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

func mustSweep(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
}

// commitCount is how many write transactions the store has committed.
func commitCount(t *testing.T, s *Store) uint64 {
	t.Helper()
	st, err := s.db.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return st.Commits
}

// getIdentity reads identity row id, failing the test when it is absent.
func getIdentity(t *testing.T, s *Store, id uint64) identityRow {
	t.Helper()
	var row identityRow
	var found bool
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		var err error
		row, found, err = s.t.identities.table.Get(r, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("identity %d not found", id)
	}
	return row
}

// openTestStore opens a store in a fresh directory with the fixed clock and
// a capturing logger, closing it when the test ends.
func openTestStore(t *testing.T) (*Store, logSink) {
	t.Helper()
	sink := newLogSink()
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{
		Logger: slog.New(sink),
		Now:    func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, sink
}
