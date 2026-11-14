//go:build !windows

package store

import (
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

func mustCensus(t *testing.T, s *Store, online func(*Identity) bool) Census {
	t.Helper()
	c, err := s.Census(t.Context(), online)
	if err != nil {
		t.Fatalf("Census: %v", err)
	}
	return c
}

// censusFixture: carol dormant with one direct post waiting; alpha, bravo
// and delta live; bravo with a direct post from alpha at the returned time
// and a path post two minutes later, alpha with a direct post from bravo
// then too, delta with nothing; alpha holding /workspace/alpha/x
// with bravo queued behind it.
func censusFixture(t *testing.T) (s *Store, clk *clock, firstWake time.Time) {
	t.Helper()
	s, clk = openClockStore(t)
	expireToDormant(t, s, clk, "carol")
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	regAt(t, s, "delta", "/workspace/delta")
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "carol"}))
	clk.advance(time.Minute)
	firstWake = clk.now()
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	clk.advance(2 * time.Minute)
	mustPost(t, s, posting(sess("alpha"), Target{Path: "/workspace/bravo/README.md"}))
	mustPost(t, s, posting(sess("bravo"), Target{Agent: "alpha"}))
	mustReserve(t, s, reserving("alpha", "/workspace/alpha/x", time.Hour))
	mustQueue(t, s, reserving("bravo", "/workspace/alpha/x", time.Hour))
	return s, clk, firstWake
}

func TestCensus_countsWhatTheStateLineReports(t *testing.T) {
	s, _, firstWake := censusFixture(t)
	s.space = func() (bolttable.Space, error) {
		return bolttable.Space{FileBytes: 16384, InUseBytes: 3072, FreeBytes: 1024}, nil
	}

	c := mustCensus(t, s, func(id *Identity) bool { return id.Name == "alpha" })

	t.Run("identities", func(t *testing.T) {
		if c.Live != 3 || c.Dormant != 1 || c.Online != 1 {
			t.Errorf("live %d dormant %d online %d, want 3, 1 and 1", c.Live, c.Dormant, c.Online)
		}
	})
	t.Run("threads", func(t *testing.T) {
		want := ThreadCounts{Announce: 1, Path: 1, Direct: 2, Cards: 1}
		if c.Threads != want {
			t.Errorf("threads %+v, want %+v", c.Threads, want)
		}
	})
	t.Run("reservations", func(t *testing.T) {
		if c.Held != 1 || c.Queued != 1 {
			t.Errorf("held %d queued %d, want 1 and 1", c.Held, c.Queued)
		}
	})
	t.Run("backlog", func(t *testing.T) {
		if c.Unread != 3 {
			t.Errorf("unread %d, want bravo's 2 wake posts and alpha's 1, the dormant carol's left out", c.Unread)
		}
		if !c.Oldest.Equal(firstWake) {
			t.Errorf("oldest %v, want bravo's first post at %v", c.Oldest, firstWake)
		}
	})
	t.Run("space", func(t *testing.T) {
		want := Space{FileBytes: 16384, FreeBytes: 1024, InUseBytes: 3072}
		if c.Space != want {
			t.Errorf("space %+v, want %+v", c.Space, want)
		}
		if c.MaxBytes != 512<<20 {
			t.Errorf("ceiling %d, want the default 512 MiB", c.MaxBytes)
		}
	})
}

func TestCensus_onlineCountsLiveIdentitiesOnly(t *testing.T) {
	s, _, _ := censusFixture(t)
	if c := mustCensus(t, s, func(*Identity) bool { return true }); c.Online != 3 {
		t.Errorf("online %d, want 3: the dormant carol is never online", c.Online)
	}
	if c := mustCensus(t, s, nil); c.Online != 0 {
		t.Errorf("online %d with no predicate, want 0", c.Online)
	}
}

// seqOf is the seq of session's unread delivery of kind.
func seqOf(t *testing.T, s *Store, session, kind string) uint64 {
	t.Helper()
	for _, e := range mustInbox(t, s, session, 0).Entries {
		if e.Kind == kind {
			return e.Seq
		}
	}
	t.Fatalf("%s has no unread %s delivery", session, kind)
	return 0
}

func TestCensus_aReadMovesTheBacklog(t *testing.T) {
	s, clk, _ := censusFixture(t)
	secondWake := clk.now()
	mustAck(t, s, sess("bravo"), seqOf(t, s, sess("bravo"), kindDirect))
	c := mustCensus(t, s, nil)
	if c.Unread != 2 || !c.Oldest.Equal(secondWake) {
		t.Errorf("unread %d oldest %v after bravo read its first post, want 2 and %v", c.Unread, c.Oldest, secondWake)
	}
	mustAck(t, s, sess("bravo"), seqOf(t, s, sess("bravo"), kindPath))
	mustAck(t, s, sess("alpha"), seqOf(t, s, sess("alpha"), kindDirect))
	if c = mustCensus(t, s, nil); c.Unread != 0 || !c.Oldest.IsZero() {
		t.Errorf("unread %d oldest %v after alpha and bravo read everything, want 0 and zero", c.Unread, c.Oldest)
	}
}

func TestCensus_leavesOutRowsPastTheirExpiry(t *testing.T) {
	s, clk, _ := censusFixture(t)
	mustReserve(t, s, reserving("bravo", "/workspace/bravo/y", time.Minute))
	if c := mustCensus(t, s, nil); c.Held != 2 {
		t.Fatalf("held %d, want 2 before the expiry", c.Held)
	}
	clk.advance(time.Minute)
	if c := mustCensus(t, s, nil); c.Held != 1 {
		t.Errorf("held %d once bravo's one-minute grant is due and unswept, want 1", c.Held)
	}
	clk.advance(6 * time.Hour)
	c := mustCensus(t, s, nil)
	if c.Live != 0 || c.Dormant != 4 {
		t.Errorf("live %d dormant %d once the live leases are due and unswept, want 0 and 4", c.Live, c.Dormant)
	}
	clk.advance(DefaultRetention)
	want := ThreadCounts{Announce: 1, Cards: 1}
	if c = mustCensus(t, s, nil); c.Threads != want {
		t.Errorf("threads %+v once the path and direct threads are due and unswept, want %+v", c.Threads, want)
	}
}
