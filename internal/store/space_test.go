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
	sp, err := s.measure()
	if err != nil {
		t.Fatalf("measure: %v", err)
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
	_, _, err := s.Post(t.Context(), Keyed{}, &p)
	wantRefusal(t, err, KindFull, "", "store nearly full")
	if n := len(mustInbox(t, s, sess("bravo"), 0).Entries); n != 1 {
		t.Errorf("bravo has %d messages, want 1: a refused post stores nothing", n)
	}

	mustAck(t, s, sess("bravo"), 1)
	w := envA()
	if _, err := s.Set(t.Context(), Keyed{}, Caller{Session: sess("alpha")}, Change{TTL: 2 * time.Hour, Where: &w}); err != nil {
		t.Errorf("a renewal on a nearly full store: %v", err)
	}
	if _, _, err := s.Revive(t.Context(), Keyed{}, new(revival(sessA, "", envA()))); err != nil {
		t.Errorf("a revival on a nearly full store: %v", err)
	}
	if _, _, err := s.Retire(t.Context(), Keyed{}, Caller{Session: sess("bravo")}); err != nil {
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
	_, _, err := s.Post(t.Context(), Keyed{}, &p)
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
			_, _, err := s.Post(t.Context(), Keyed{}, &p)
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

func TestFollow_onANearlyFullStoreIsRefusedWithThePostsText(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha", repoGrelot)
	b := regAt(t, s, "bravo", repoSubflux)
	s.space = inUse(DefaultMaxBytes)
	_, _, perr := s.Post(t.Context(), Keyed{}, new(posting(sess("alpha"), Target{Agent: "bravo"})))
	_, ferr := s.Follow(t.Context(), Keyed{}, Caller{Session: sess("bravo")}, "/workspace/marotte")
	wantRefusal(t, ferr, KindFull, "", "store nearly full")
	if perr == nil || ferr.Error() != perr.Error() {
		t.Errorf("follow refused with %q, want the post's %v", ferr, perr)
	}
	if f := getIdentity(t, s, b.ID).Follows; len(f) != 0 {
		t.Errorf("follows %v after a refused follow, want none", f)
	}
	if got := mustFollowed(t, s, sess("bravo"), repoSubflux+"/internal"); len(got.Identity.Follows) != 0 {
		t.Errorf("a follow its work path reaches: follows %v, want none added", got.Identity.Follows)
	}
}

func TestStoreFull_refusesEveryWriteThatAddsRows(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha", repoGrelot)
	regAt(t, s, "bravo", repoSubflux)
	link := mustGrant(t, s, grantFrom("alpha", "rev"))
	mustReserve(t, s, reserving("bravo", pathAPI, time.Hour))
	s.space = inUse(DefaultMaxBytes)
	ctx, alpha, w := t.Context(), Caller{Session: sess("alpha"), Self: true}, envA()
	grant := reserving("alpha", repoGrelot+"/a.go", time.Hour)
	queue := reserving("alpha", pathAPI, time.Hour)
	grant.Caller, queue.Caller, queue.Queue = alpha, alpha, true
	for _, c := range []struct {
		write func() error
		name  string
	}{
		{name: "a post", write: func() error {
			_, _, err := s.Post(ctx, Keyed{}, new(posting(sess("alpha"), Target{Agent: "bravo"})))
			return err
		}},
		{name: "a follow", write: func() error { _, err := s.Follow(ctx, Keyed{}, alpha, "/workspace/marotte"); return err }},
		{name: "a register of a new name", write: func() error {
			_, _, err := s.Register(ctx, Keyed{}, new(registration(sess("charlie"), "charlie", envA(), "/workspace/marotte")))
			return err
		}},
		{name: "a delegate's register", write: func() error {
			_, _, err := s.Register(ctx, Keyed{}, new(delegateReg(sess("rev"), link.Link)))
			return err
		}},
		{name: "a delegate grant", write: func() error { _, _, err := s.Delegate(ctx, Keyed{}, new(grantFrom("alpha", "second"))); return err }},
		{name: "a reservation", write: func() error { _, _, err := s.Reserve(ctx, Keyed{}, &grant); return err }},
		{name: "a queued request", write: func() error { _, _, err := s.Reserve(ctx, Keyed{}, &queue); return err }},
		{name: "a set adding a work path", write: func() error {
			_, err := s.Set(ctx, Keyed{}, alpha, Change{Fields: Fields{Paths: []string{repoGrelot, "/workspace/marotte"}}, Where: &w})
			return err
		}},
		{name: "a take-over adding a work path", write: func() error {
			r := registration(sess("alpha-next"), "alpha", envA(), repoGrelot, "/workspace/marotte")
			r.TakeOver = true
			_, _, err := s.Register(ctx, Keyed{}, &r)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantRefusal(t, c.write(), KindFull, "", "store nearly full")
		})
	}
}

func TestStoreFull_takesARecoveryThatAddsNoPath(t *testing.T) {
	t.Run("a revive", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		old := expireToDormant(t, s, clk, "subflux-dev")
		s.space = inUse(DefaultMaxBytes)
		got, _, err := s.Revive(t.Context(), Keyed{}, new(revival(sessA, "", envA())))
		if err != nil || got.Identity.ID != old.ID || got.Identity.State != stateLive {
			t.Errorf("revive on a nearly full store: id %d state %q, %v; want %d live", got.Identity.ID, got.Identity.State, err, old.ID)
		}
	})
	t.Run("a take-over with its own paths", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		mustRegister(t, s, registration(sessA, "subflux-dev", envA(), repoSubflux, repoGrelot))
		s.space = inUse(DefaultMaxBytes)
		r := registration(sessB, "subflux-dev", envA(), repoGrelot)
		r.TakeOver = true
		got, _, err := s.Register(t.Context(), Keyed{}, &r)
		if err != nil || !got.Moved {
			t.Errorf("take-over on a nearly full store: moved %v, %v; want moved", got.Moved, err)
		}
	})
}

func TestStoreFull_takesEveryWriteThatAddsNothing(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha", repoGrelot, "/workspace/marotte")
	regAt(t, s, "bravo", repoSubflux)
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	mustFollow(t, s, sess("bravo"), "/workspace/bolttable")
	held := mustReserve(t, s, reserving("alpha", repoGrelot+"/a.go", time.Hour))
	mustGrant(t, s, grantFrom("alpha", "rev"))
	seq := mustInbox(t, s, sess("bravo"), 0).Entries[0].Seq
	s.space = inUse(DefaultMaxBytes)
	ctx, alpha, bravo, w := t.Context(), Caller{Session: sess("alpha"), Self: true}, Caller{Session: sess("bravo")}, envA()
	for _, c := range []struct {
		write func() error
		name  string
	}{
		{name: "an ack", write: func() error { _, _, err := s.Ack(ctx, Keyed{}, bravo, seq); return err }},
		{name: "an unfollow", write: func() error { _, err := s.Unfollow(ctx, Keyed{}, bravo, "/workspace/bolttable"); return err }},
		{name: "a follow already reached", write: func() error { _, err := s.Follow(ctx, Keyed{}, bravo, repoSubflux+"/internal"); return err }},
		{name: "a repeated register", write: func() error {
			r := registration(sess("alpha"), "alpha", envA(), repoGrelot)
			r.Self = true
			_, _, err := s.Register(ctx, Keyed{}, &r)
			return err
		}},
		{name: "a lease renewal", write: func() error { _, err := s.Set(ctx, Keyed{}, alpha, Change{TTL: 2 * time.Hour, Where: &w}); return err }},
		{name: "a set narrowing the work paths", write: func() error {
			_, err := s.Set(ctx, Keyed{}, alpha, Change{Fields: Fields{Paths: []string{repoGrelot}}, Where: &w})
			return err
		}},
		{name: "a reservation renewal", write: func() error {
			_, err := s.RenewReservation(ctx, Keyed{}, alpha, held.Reservation.ID, 2*time.Hour)
			return err
		}},
		{name: "a release", write: func() error { _, err := s.Release(ctx, Keyed{}, alpha, held.Reservation.ID); return err }},
		{name: "a revoke", write: func() error { _, err := s.Revoke(ctx, Keyed{}, alpha, "rev"); return err }},
		{name: "a session end", write: func() error { return s.EndSession(ctx, sess("alpha"), 0) }},
		{name: "a bye", write: func() error { _, _, err := s.Retire(ctx, Keyed{}, bravo); return err }},
		{name: "a sweep", write: func() error { clk.advance(7 * time.Hour); _, err := s.sweep(ctx); return err }},
	} {
		if err := c.write(); err != nil {
			t.Errorf("%s on a nearly full store: %v", c.name, err)
		}
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
		_, _, err := s.Post(t.Context(), Keyed{}, &p)
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
		Logger: slog.New(newLogSink()), Now: clk.now, Retention: retainFor(time.Hour), MaxBytes: testMaxBytes,
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
	_, _, err = s.Post(t.Context(), Keyed{}, &p)
	wantRefusal(t, err, KindFull, "", "store nearly full")
}
