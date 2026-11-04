package store

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustMeasure(t *testing.T, s *Store) Space {
	t.Helper()
	sp, err := s.Measure(t.Context())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if sp.InUseBytes != sp.FileBytes-sp.FreeBytes {
		t.Errorf("in use %d, want file %d minus free %d", sp.InUseBytes, sp.FileBytes, sp.FreeBytes)
	}
	return sp
}

func TestPost_refusedOnlyPastNinetyPercentOfTheDefaultCeiling(t *testing.T) {
	// 90 % of the default 512 MiB is 483,183,820.8 bytes.
	s, clk := openClockStore(t)
	expireToDormant(t, s, clk, "carol")
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")

	s.inUse.Store(483_183_820)
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))

	s.inUse.Store(483_183_821)
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	_, err := s.Post(t.Context(), &p)
	wantRefusal(t, err, KindFull, "", "store nearly full")
	if n := len(mustInbox(t, s, sess("bravo"), 0).Entries); n != 1 {
		t.Errorf("bravo has %d messages, want 1: a refused post stores nothing", n)
	}

	mustAck(t, s, sess("bravo"), 1)
	w := envA()
	if _, err := s.Set(t.Context(), sess("alpha"), Change{TTL: 2 * time.Hour, Where: &w}); err != nil {
		t.Errorf("a renewal on a nearly full store: %v", err)
	}
	if _, err := s.Revive(t.Context(), new(revival(sessA, "", envA()))); err != nil {
		t.Errorf("a revival on a nearly full store: %v", err)
	}
	if err := s.Retire(t.Context(), sess("bravo")); err != nil {
		t.Errorf("a retirement on a nearly full store: %v", err)
	}
}

func TestPost_atExactlyNinetyPercentStillGoesThrough(t *testing.T) {
	s, _ := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	s.maxBytes = 1000
	s.inUse.Store(900)
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	s.inUse.Store(901)
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	_, err := s.Post(t.Context(), &p)
	wantRefusal(t, err, KindFull, "", "store nearly full")
}

// fillUntilFull posts 64 KiB messages from alpha to bravo, measuring every
// 16 posts as the server's minute does, until a post is refused as nearly
// full; it fails the test if none is by limit posts.
func fillUntilFull(t *testing.T, s *Store, limit int) Space {
	t.Helper()
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	p.Body = strings.Repeat("x", maxBody)
	for i := range limit {
		_, err := s.Post(t.Context(), &p)
		if err != nil {
			wantRefusal(t, err, KindFull, "", "store nearly full")
			return mustMeasure(t, s)
		}
		if i%16 == 15 {
			mustMeasure(t, s)
		}
	}
	t.Fatalf("no post refused after %d posts of 64 KiB", limit)
	return Space{}
}

const testMaxBytes = 32 << 20

func TestMeasure_inUseFallsWhenABurstExpiresThoughTheFileDoesNot(t *testing.T) {
	clk := &clock{t: testNow}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), Options{
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

func TestOpen_measuresTheStoreItOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	o := Options{Logger: slog.New(newLogSink()), Now: func() time.Time { return testNow }, MaxBytes: testMaxBytes}
	s, err := Open(path, o)
	if err != nil {
		t.Fatal(err)
	}
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	fillUntilFull(t, s, 2000)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	_, err = s.Post(t.Context(), &p)
	wantRefusal(t, err, KindFull, "", "store nearly full")
}
