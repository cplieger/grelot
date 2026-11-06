//go:build !windows

package store

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

func mustMeasure(t *testing.T, s *Store) Space {
	t.Helper()
	sp, err := s.Measure()
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if sp.InUseBytes <= 0 || sp.InUseBytes+sp.FreeBytes > sp.FileBytes {
		t.Errorf("in use %d and free %d, want both within the %d byte file", sp.InUseBytes, sp.FreeBytes, sp.FileBytes)
	}
	return sp
}

func TestPost_refusedOnlyPastNinetyPercentOfTheDefaultCeiling(t *testing.T) {
	// 90 % of the default 512 MiB is 483,183,820.8 bytes.
	s, clk := openClockStore(t)
	expireToDormant(t, s, clk, "carol")
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")

	s.space = inUse(483_183_820)
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))

	s.space = inUse(483_183_821)
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	_, err := s.Post(t.Context(), &p)
	wantRefusal(t, err, KindFull, "", "store nearly full")
	if n := len(mustInbox(t, s, sess("bravo"), 0).Entries); n != 1 {
		t.Errorf("bravo has %d messages, want 1: a refused post stores nothing", n)
	}

	mustAck(t, s, sess("bravo"), 1)
	w := envA()
	if _, err := s.Set(t.Context(), Caller{Session: sess("alpha")}, Change{TTL: 2 * time.Hour, Where: &w}); err != nil {
		t.Errorf("a renewal on a nearly full store: %v", err)
	}
	if _, err := s.Revive(t.Context(), new(revival(sessA, "", envA()))); err != nil {
		t.Errorf("a revival on a nearly full store: %v", err)
	}
	if err := s.Retire(t.Context(), Caller{Session: sess("bravo")}); err != nil {
		t.Errorf("a retirement on a nearly full store: %v", err)
	}
}

func TestPost_atExactlyNinetyPercentStillGoesThrough(t *testing.T) {
	s, _ := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	s.maxBytes = 1000
	s.space = inUse(900)
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	s.space = inUse(901)
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	_, err := s.Post(t.Context(), &p)
	wantRefusal(t, err, KindFull, "", "store nearly full")
}

func TestPost_concurrentPostsStopAtTheFirstPastTheCeiling(t *testing.T) {
	s, _ := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	s.maxBytes = 1000

	// The reading is at the ceiling until a commit moves the file's free
	// and pending pages, which every post does: bbolt writes each page it
	// changes to a new one. The barrier holds every reading until all the
	// posters read, so readings taken before any commit all pass; readings
	// the write lock serializes wait out its 300 ms once.
	const posters = 8
	measure := s.space
	before, err := measure()
	if err != nil {
		t.Fatal(err)
	}
	var arrived atomic.Int32
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	s.space = func() (bolttable.Space, error) {
		if arrived.Add(1) == posters {
			open()
		}
		select {
		case <-gate:
		case <-time.After(300 * time.Millisecond):
			open()
		}
		now, err := measure()
		if now == before {
			return bolttable.Space{InUseBytes: 900}, err
		}
		return bolttable.Space{InUseBytes: 901}, err
	}

	var landed, refused atomic.Int32
	var wg sync.WaitGroup
	for range posters {
		wg.Go(func() {
			p := posting(sess("alpha"), Target{Agent: "bravo"})
			p.Body = strings.Repeat("x", maxBody)
			_, err := s.Post(t.Context(), &p)
			var se *Error
			switch {
			case err == nil:
				landed.Add(1)
			case errors.As(err, &se) && se.Kind == KindFull:
				refused.Add(1)
			default:
				t.Errorf("post: %v", err)
			}
		})
	}
	wg.Wait()
	if landed.Load() != 1 || refused.Load() != posters-1 {
		t.Errorf("%d posts landed and %d were refused, want 1 and %d", landed.Load(), refused.Load(), posters-1)
	}
}

// inUse is a space reading of n bytes in use.
func inUse(n int64) func() (bolttable.Space, error) {
	return func() (bolttable.Space, error) { return bolttable.Space{FileBytes: n, InUseBytes: n}, nil }
}

// fillUntilFull posts 64 KiB messages from alpha to bravo until a post is
// refused as nearly full; it fails the test if none is by limit posts.
func fillUntilFull(t *testing.T, s *Store, limit int) Space {
	t.Helper()
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	p.Body = strings.Repeat("x", maxBody)
	for range limit {
		_, err := s.Post(t.Context(), &p)
		if err != nil {
			wantRefusal(t, err, KindFull, "", "store nearly full")
			return mustMeasure(t, s)
		}
	}
	t.Fatalf("no post refused after %d posts of 64 KiB", limit)
	return Space{}
}

const testMaxBytes = 32 << 20

func TestMeasure_inUseFallsWhenABurstExpiresThoughTheFileDoesNot(t *testing.T) {
	clk := &clock{t: testNow}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{
		Logger: slog.New(newLogSink()), Now: clk.now, ThreadTTL: time.Hour, MaxBytes: testMaxBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	peak := fillUntilFull(t, s, 2000)
	if peak.InUseBytes*10 <= testMaxBytes*9 {
		t.Fatalf("refused at %d bytes in use, not past 90 %% of %d", peak.InUseBytes, testMaxBytes)
	}

	clk.advance(time.Hour)
	mustSweep(t, s)
	after := mustMeasure(t, s)
	if after.FileBytes < peak.FileBytes {
		t.Errorf("file %d bytes after the expiry, below its high-water mark %d: the test no longer shows why in-use is measured", after.FileBytes, peak.FileBytes)
	}
	if after.InUseBytes*10 > testMaxBytes*9 {
		t.Errorf("in use %d of a %d file after the burst expired, want under 90 %% of %d", after.InUseBytes, after.FileBytes, testMaxBytes)
	}
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
}

func TestPost_refusedOnAFullStoreAfterARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	o := Options{Logger: slog.New(newLogSink()), Now: func() time.Time { return testNow }, MaxBytes: testMaxBytes}
	s, err := Open(path, &o)
	if err != nil {
		t.Fatal(err)
	}
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	fillUntilFull(t, s, 2000)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path, &o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	_, err = s.Post(t.Context(), &p)
	wantRefusal(t, err, KindFull, "", "store nearly full")
}
