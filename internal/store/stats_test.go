//go:build !windows

package store

import (
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func mustStats(t *testing.T, s *Store, rg StatsRange) Stats {
	t.Helper()
	st, err := s.Stats(t.Context(), rg)
	if err != nil {
		t.Fatalf("Stats(%v): %v", rg, err)
	}
	return st
}

func agentNames(as []AgentStats) []string {
	out := make([]string, 0, len(as))
	for i := range as {
		out = append(out, as[i].Name)
	}
	return out
}

func TestStatsWindowAt_endsWithTheBucketNowFillsAndSpansTheRange(t *testing.T) {
	now := time.Date(2026, 11, 4, 5, 39, 30, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		start, end time.Time
		bucket     time.Duration
		buckets    int
		rg         StatsRange
	}{
		{"6h", time.Date(2026, 11, 3, 23, 45, 0, 0, time.UTC), time.Date(2026, 11, 4, 5, 45, 0, 0, time.UTC), 15 * time.Minute, 24, StatsRange6h},
		{"24h", time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC), time.Date(2026, 11, 4, 6, 0, 0, 0, time.UTC), time.Hour, 24, StatsRange24h},
		{"72h", time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC), time.Date(2026, 11, 4, 6, 0, 0, 0, time.UTC), time.Hour, 72, StatsRange72h},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := statsWindowAt(now, tc.rg)
			if !w.Start.Equal(tc.start) || !w.End.Equal(tc.end) || w.Bucket != tc.bucket || w.buckets() != tc.buckets {
				t.Errorf("window %v to %v by %v (%d buckets), want %v to %v by %v (%d)", w.Start, w.End, w.Bucket, w.buckets(), tc.start, tc.end, tc.bucket, tc.buckets)
			}
		})
	}
}

func TestStats_bucketsRankingAndReads(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	clk.set(testNow.Add(-5*time.Hour - time.Minute))
	alpha := regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	charlie := regAt(t, s, "charlie", pathMarotte)
	dave := regAt(t, s, "dave", "/workspace/dave")
	keepAlive(t, s, alpha.ID, bravo.ID, charlie.ID, dave.ID)
	clk.set(testNow.Add(-5 * time.Hour))
	announce := say(t, s, "alpha", Target{Announce: true}, "a1")
	clk.set(testNow.Add(-20 * time.Minute))
	path := say(t, s, "bravo", Target{Path: pathMarotte}, "p1")
	clk.set(testNow.Add(-10 * time.Minute))
	readAll(t, s, "charlie")
	clk.set(testNow)
	direct := say(t, s, "alpha", Target{Agent: "bravo"}, "d1")
	mustPost(t, s, userPosting(Target{Announce: true}))

	st := mustStats(t, s, StatsRange6h)

	t.Run("posts by bucket and kind", func(t *testing.T) {
		// The window runs 23:45 to 05:45 in 15-minute buckets.
		want := make([]PostCounts, 24)
		want[3] = PostCounts{Announce: 1}
		want[22] = PostCounts{Path: 1}
		want[23] = PostCounts{Announce: 1, Direct: 1}
		if !slices.Equal(st.Posts, want) {
			t.Errorf("posts %v, want %v", st.Posts, want)
		}
	})
	t.Run("agents by posts then reads then name, silent ones kept, @user left out", func(t *testing.T) {
		if got := agentNames(st.Agents); !slices.Equal(got, []string{"alpha", "bravo", "charlie", "dave"}) {
			t.Fatalf("agents %q, want alpha, bravo, charlie, dave", got)
		}
		a, c, d := st.Agents[0], st.Agents[2], st.Agents[3]
		if a.Posts != 2 || !slices.Equal(a.PostTimes, []time.Time{testNow.Add(-5 * time.Hour), testNow}) {
			t.Errorf("alpha %+v, want 2 posts at -5h and now", a)
		}
		// charlie read the announcement after 4h50m and the path post after 10m.
		if c.Posts != 0 || c.Read != 2 || c.LongestWait != 4*time.Hour+50*time.Minute {
			t.Errorf("charlie %+v, want 2 reads, the longest 4h50m", c)
		}
		if d.Posts != 0 || d.Read != 0 || len(d.PostTimes) != 0 {
			t.Errorf("dave %+v, want a silent row", d)
		}
	})
	t.Run("threads by posts then deliveries", func(t *testing.T) {
		if len(st.Threads) != 3 {
			t.Fatalf("threads %+v, want 3", st.Threads)
		}
		for i, want := range []struct {
			id                              uint64
			title                           string
			posts, participants, deliveries int
		}{
			// alpha's announcement reached bravo, charlie and dave; @user's
			// reached all four agents.
			{announce.Thread, "announce", 2, 5, 7},
			{path.Thread, pathMarotte, 1, 2, 1},
			{direct.Thread, "@alpha and @bravo", 1, 2, 1},
		} {
			got := st.Threads[i]
			if got.ID != want.id || got.Title != want.title || got.Posts != want.posts || got.Participants != want.participants || got.Deliveries != want.deliveries {
				t.Errorf("thread %d: %+v, want %+v", i, got, want)
			}
		}
	})
	t.Run("delays", func(t *testing.T) {
		d := st.Delays
		if d.Read != 2 || d.Median != 10*time.Minute || d.P95 != 4*time.Hour+50*time.Minute {
			t.Errorf("delays read %d, median %v, p95 %v; want 2, 10m, 4h50m", d.Read, d.Median, d.P95)
		}
		c, err := s.Census(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if d.Unread != c.Unread || d.Unread == 0 {
			t.Errorf("unread %d, want the census's %d", d.Unread, c.Unread)
		}
	})
}

func TestStats_leavesOutExpiredPostsAndAgentsDormantBeforeTheWindow(t *testing.T) {
	clk := &clock{t: testNow.Add(-3 * time.Hour)}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{Logger: slog.New(newLogSink()), Now: clk.now, Retention: retainFor(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	alpha := regAt(t, s, "alpha", "/workspace/alpha")
	early := regAt(t, s, "early-sleeper", "/workspace/early")
	late := regAt(t, s, "late-sleeper", "/workspace/late")
	keepAlive(t, s, alpha.ID, early.ID, late.ID)
	say(t, s, "alpha", Target{Agent: "late-sleeper"}, "expired")
	// The second post lands before the first expires, so no write has
	// removed the first when Stats reads.
	clk.set(testNow.Add(-90 * time.Minute))
	say(t, s, "alpha", Target{Agent: "late-sleeper"}, "kept")
	clk.set(testNow)
	editIdentity(t, s, early.ID, func(r *identityRow) { r.State, r.DormantSince = stateDormant, testNow.Add(-7*time.Hour) })
	editIdentity(t, s, late.ID, func(r *identityRow) { r.State, r.DormantSince = stateDormant, testNow.Add(-2*time.Hour) })

	st := mustStats(t, s, StatsRange6h)

	if got := agentNames(st.Agents); !slices.Equal(got, []string{"alpha", "late-sleeper"}) {
		t.Errorf("agents %q, want alpha and late-sleeper", got)
	}
	if st.Agents[0].Posts != 1 || len(st.Threads) != 1 || st.Threads[0].Posts != 1 || st.Threads[0].Deliveries != 1 {
		t.Errorf("alpha %+v, threads %+v; want only the unexpired post", st.Agents[0], st.Threads)
	}
}

func TestStats_threadsRankByPostsBeforeDeliveries(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	for _, n := range []string{"alpha", "bravo", "charlie", "dave"} {
		regAt(t, s, n, "/workspace/"+n)
	}
	announce := say(t, s, "alpha", Target{Announce: true}, "to all")
	direct := say(t, s, "alpha", Target{Agent: "bravo"}, "d1")
	say(t, s, "alpha", Target{Agent: "bravo"}, "d2")

	st := mustStats(t, s, StatsRange6h)

	// The announcement has 3 deliveries from 1 post, the direct thread 2
	// from 2 posts.
	if len(st.Threads) != 2 || st.Threads[0].ID != direct.Thread || st.Threads[1].ID != announce.Thread || st.Threads[1].Deliveries != 3 {
		t.Errorf("threads %+v, want the direct thread first, then the announcement with 3 deliveries", st.Threads)
	}
}

func TestStats_aParentsCopyIsNoDelivery(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	review := scopedDelegate(t, s, "alpha", "review", func(g *Grant) { g.AllowAgents = []string{"bravo"} })
	say(t, s, "bravo", Target{Agent: review.Name}, "for review")

	st := mustStats(t, s, StatsRange6h)

	if len(st.Threads) != 1 || st.Threads[0].Deliveries != 1 || st.Threads[0].Participants != 2 {
		t.Errorf("threads %+v, want one delivery to the delegate and two participants", st.Threads)
	}
}

func TestHistogram_binsOnTheDurationLadderUpToRetentionOrTheLongestDelay(t *testing.T) {
	const day = 24 * time.Hour
	ladder := []time.Duration{
		0, time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second,
		time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute,
		time.Hour, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour, day, 3 * day, 7 * day, 14 * day, 30 * day, 90 * day,
	}
	for _, tc := range []struct {
		name      string
		delays    []time.Duration
		retention time.Duration
		top       time.Duration
		lastCount int
	}{
		{"the default retention", nil, 72 * time.Hour, 3 * day, 0},
		{"a retention between edges", nil, 90 * time.Minute, 2 * time.Hour, 0},
		{"the longest retention", nil, 90 * day, 90 * day, 0},
		{"a delay past the retention", []time.Duration{7 * time.Hour}, time.Hour, 12 * time.Hour, 1},
		{"a delay on the last edge, which is closed", []time.Duration{12 * time.Hour}, time.Hour, 12 * time.Hour, 1},
		{"a delay past the ladder", []time.Duration{100 * day}, 90 * day, 90 * day, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := histogram(tc.delays, tc.retention)
			want := ladder[:slices.Index(ladder, tc.top)+1]
			if !slices.Equal(h.Edges, want) {
				t.Fatalf("edges %v, want %v", h.Edges, want)
			}
			if len(h.Counts) != len(want)-1 || h.Counts[len(h.Counts)-1] != tc.lastCount {
				t.Errorf("counts %v, want %d bins, the last %d", h.Counts, len(want)-1, tc.lastCount)
			}
		})
	}
}

func TestHistogram_countsBinsAndNearestRankPercentiles(t *testing.T) {
	var delays []time.Duration
	for i := 20; i >= 1; i-- {
		delays = append(delays, time.Duration(i)*time.Second)
	}

	h := histogram(delays, 30*time.Second)

	// Bins [0,1s) [1s,2s) [2s,5s) [5s,10s) [10s,30s]: an edge's delay
	// opens its bin.
	if !slices.Equal(h.Counts, []int{0, 1, 3, 5, 11}) {
		t.Errorf("counts %v, want [0 1 3 5 11]", h.Counts)
	}
	if h.Median != 10*time.Second || h.P95 != 19*time.Second || h.Read != 20 {
		t.Errorf("median %v, p95 %v, read %d; want 10s, 19s, 20", h.Median, h.P95, h.Read)
	}
	if none := histogram(nil, time.Hour); none.Median != 0 || none.P95 != 0 || none.Read != 0 {
		t.Errorf("no delays: %+v, want zero percentiles", none)
	}
}
