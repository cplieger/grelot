//go:build !windows

package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

func openAt(t *testing.T, path string, sink logSink) (*Store, error) {
	t.Helper()
	s, err := Open(path, &Options{Logger: slog.New(sink), Now: func() time.Time { return testNow }})
	if err == nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	return s, err
}

// siblings lists the names beside path that start with its base name,
// but for <path>.lock, which bolttable's Open creates and never removes.
func siblings(t *testing.T, path string) []string {
	t.Helper()
	m, err := filepath.Glob(path + ".*")
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(m, func(p string) bool { return p == path+".lock" })
}

func TestOpen_keepsRowsAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	s, err := openAt(t, path, newLogSink())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.db.Update(t.Context(), func(tx *bolttable.Tx) error {
		return s.t.identities.table.Put(tx, 7, &identityRow{ID: 7, Name: "kept"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	sink := newLogSink()
	s2, err := openAt(t, path, sink)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	var got identityRow
	var found bool
	if err := s2.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		got, found, err = s2.t.identities.table.Get(r, 7)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !found || got.Name != "kept" {
		t.Errorf("row after restart = %+v, found %v; want the row named kept", got, found)
	}
	if n := len(sink.lines(slog.LevelWarn, msgStoreReset)) + len(sink.lines(slog.LevelError, msgStoreCorrupt)); n != 0 {
		t.Errorf("a healthy file logged %d reset or corrupt lines, want 0", n)
	}
}

func TestOpen_heldLockFailsAndLeavesTheFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	if _, err := openAt(t, path, newLogSink()); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	_, err = openAt(t, path, newLogSink())

	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open error = %v, want ErrLocked", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the held file changed")
	}
	if got := siblings(t, path); len(got) != 0 {
		t.Errorf("files beside the held store = %v, want none", got)
	}
}

// writeOtherFormat leaves at path a file whose grelot namespace holds one
// identities row under another codec version, so its format digest differs.
func writeOtherFormat(t *testing.T, path string) {
	t.Helper()
	s := bolttable.NewSchema("grelot")
	ns := bolttable.Must(s.Namespace(namespace))
	tbl := bolttable.Must(bolttable.Define(ns, prefixIdentities, "identities",
		bolttable.Uint64Key{}, bolttable.JSON[identityRow](99), bolttable.TableOptions{Silent: true}))
	db, err := bolttable.Open(path, s, bolttable.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(t.Context(), func(tx *bolttable.Tx) error {
		return tbl.Put(tx, 1, &identityRow{ID: 1, Name: "old"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// freshRows is what an empty store holds after Open: the announcement and
// cards threads, created at first start.
const freshRows = 2

func rowCount(ctx context.Context, t *testing.T, s *Store) int {
	t.Helper()
	st, err := s.db.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ns := range st.Namespaces {
		for _, tb := range ns.Tables {
			n += tb.Rows
		}
	}
	return n
}

func TestOpen_formatMismatchResetsAndWarns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	writeOtherFormat(t, path)
	sink := newLogSink()

	s, err := openAt(t, path, sink)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if n := rowCount(t.Context(), t, s); n != freshRows {
		t.Errorf("rows after the reset = %d, want %d", n, freshRows)
	}
	warns := sink.lines(slog.LevelWarn, msgStoreReset)
	if len(warns) != 1 {
		t.Fatalf("reset WARN lines = %d, want 1", len(warns))
	}
	if got := attr(warns[0], "path"); got != path {
		t.Errorf("reset line path = %q, want %q", got, path)
	}
	if got := siblings(t, path); len(got) != 0 {
		t.Errorf("files beside the reset store = %v, want none: a format change is not corruption", got)
	}
}

// metaOffset is where bbolt's meta struct starts in each of the first two
// pages, after the 16-byte page header; version sits 4 bytes in and the
// checksum 56 bytes in (https://github.com/etcd-io/bbolt/blob/v1.5.0/internal/common/meta.go).
const metaOffset = 16

func corruptCopies(t *testing.T) map[string][]byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "valid.db")
	s, err := openAt(t, path, newLogSink())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	flip := func(at int) []byte {
		b := bytes.Clone(valid)
		for _, page := range []int{0, os.Getpagesize()} {
			b[page+metaOffset+at] ^= 0xff
		}
		return b
	}
	return map[string][]byte{
		"invalid":  bytes.Repeat([]byte{0xab}, 32<<10),
		"checksum": flip(60),
		"version":  flip(4),
	}
}

func TestOpen_corruptFileIsMovedAsideAndTheStoreStartsEmpty(t *testing.T) {
	for name, data := range corruptCopies(t) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "grelot.db")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			sink := newLogSink()

			s, err := openAt(t, path, sink)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}

			moved := siblings(t, path)
			if len(moved) != 1 || !movedAsideName.MatchString(strings.TrimPrefix(moved[0], path)) {
				t.Fatalf("files beside the store = %v, want one <path>.<UTC stamp>.invalid", moved)
			}
			got, err := os.ReadFile(moved[0])
			if err != nil {
				t.Fatalf("moved-aside file: %v", err)
			}
			if !bytes.Equal(got, data) {
				t.Error("the moved-aside file differs from the corrupt one")
			}
			if n := rowCount(t.Context(), t, s); n != freshRows {
				t.Errorf("rows in the fresh store = %d, want %d", n, freshRows)
			}
			errs := sink.lines(slog.LevelError, msgStoreCorrupt)
			if len(errs) != 1 {
				t.Fatalf("corrupt ERROR lines = %d, want 1", len(errs))
			}
			if got := attr(errs[0], "moved_to"); got != moved[0] {
				t.Errorf("corrupt line moved_to = %q, want %q", got, moved[0])
			}
		})
	}
}

// movedAsideName is the suffix bolttable's InvalidFileMoveAside gives a
// moved file: .<UTC time as 20060102T150405.000000000Z>.invalid.
var movedAsideName = regexp.MustCompile(`^\.\d{8}T\d{6}\.\d{9}Z\.invalid$`)

func TestOpen_compactsAFileMostlyFreeAndKeepsItsRows(t *testing.T) {
	clk := &clock{t: testNow}
	path := filepath.Join(t.TempDir(), "grelot.db")
	o := Options{Logger: slog.New(newLogSink()), Now: clk.now, ThreadTTL: time.Hour}
	s, err := Open(path, &o)
	if err != nil {
		t.Fatal(err)
	}
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	// About 40 MiB of mail: bbolt grows the file in 16 MiB steps whose
	// unwritten tail is not free, so the burst must be well past one step
	// for its expiry to free half the file.
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	p.Body = strings.Repeat("x", maxBody)
	for range 640 {
		mustPost(t, s, p)
	}
	clk.advance(time.Hour)
	mustSweep(t, s)
	before := mustMeasure(t, s)
	// Open compacts only a file at least half and 4 MiB free.
	if before.FreeBytes*2 < before.FileBytes || before.FreeBytes < 4<<20 {
		t.Fatalf("free %d of %d bytes before the restart: the fixture does not reach the thresholds", before.FreeBytes, before.FileBytes)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	sink := newLogSink()
	o.Logger = slog.New(sink)
	s, err = Open(path, &o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size()*2 > before.FileBytes {
		t.Errorf("file %d bytes after the restart, want at most half of %d", fi.Size(), before.FileBytes)
	}
	if n := len(mustInbox(t, s, sess("bravo"), 0).Entries); n != 0 {
		t.Errorf("bravo has %d messages after the compaction, want 0: they expired", n)
	}
	if _, err := s.Status(t.Context(), Caller{Session: sess("alpha")}); err != nil {
		t.Errorf("alpha after the compaction: %v", err)
	}
	if n := len(sink.lines(slog.LevelWarn, msgCompactFailed)); n != 0 {
		t.Errorf("compaction WARN lines = %d, want 0", n)
	}
}

func TestLogOpen_aFailedCompactionIsOneWarn(t *testing.T) {
	sink := newLogSink()
	s := newStore(&Options{Logger: slog.New(sink)})

	s.logOpen("/data/grelot.db", &bolttable.OpenReport{CompactErr: errors.New("no space left on device")})

	warns := sink.lines(slog.LevelWarn, msgCompactFailed)
	if len(warns) != 1 {
		t.Fatalf("compaction WARN lines = %d, want 1", len(warns))
	}
	if got := attr(warns[0], "error"); got != "no space left on device" {
		t.Errorf("compaction line error = %q, want the cause", got)
	}
	if n := len(sink.lines(slog.LevelError, msgStoreCorrupt)) + len(sink.lines(slog.LevelWarn, msgStoreReset)); n != 0 {
		t.Errorf("a report with only a compaction failure logged %d corrupt or reset lines, want 0", n)
	}
}

func TestPost_onAClosedStoreReportsErrClosed(t *testing.T) {
	s, _ := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	p := posting(sess("alpha"), Target{Agent: "bravo"})
	_, _, err := s.Post(t.Context(), Keyed{}, &p)

	if !errors.Is(err, ErrClosed) {
		t.Errorf("Post after Close: %v, want ErrClosed", err)
	}
	w := envA()
	if _, _, err := s.Set(t.Context(), Keyed{}, Caller{Session: sess("alpha")}, Change{TTL: 2 * time.Hour, Where: &w}); !errors.Is(err, ErrClosed) {
		t.Errorf("Set after Close: %v, want ErrClosed", err)
	}
}

func TestOpen_otherErrorsLeaveThePathUntouched(t *testing.T) {
	t.Run("path is a directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "grelot.db")
		if err := os.MkdirAll(filepath.Join(path, "inside"), 0o700); err != nil {
			t.Fatal(err)
		}

		_, err := openAt(t, path, newLogSink())

		if err == nil || errors.Is(err, ErrLocked) {
			t.Fatalf("Open error = %v, want a plain failure", err)
		}
		if fi, err := os.Stat(filepath.Join(path, "inside")); err != nil || !fi.IsDir() {
			t.Errorf("the directory's content changed: %v", err)
		}
		if got := siblings(t, path); len(got) != 0 {
			t.Errorf("files beside the path = %v, want none", got)
		}
	})
	t.Run("parent is a file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "notadir")
		if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := openAt(t, filepath.Join(parent, "grelot.db"), newLogSink())

		if err == nil || errors.Is(err, ErrLocked) {
			t.Fatalf("Open error = %v, want a plain failure", err)
		}
		if b, err := os.ReadFile(parent); err != nil || string(b) != "x" {
			t.Errorf("the parent file changed: %q, %v", b, err)
		}
	})
}
