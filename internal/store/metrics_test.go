//go:build !windows

package store

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/grelot/internal/obs"
)

// page reads m's /metrics page into series name, labels included as the
// page prints them (sorted by name), to value.
func page(t *testing.T, m *obs.Metrics) map[string]float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	out := map[string]float64{}
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("series line %q: %v", line, err)
		}
		out[line[:i]] = v
	}
	return out
}

func wantSeries(t *testing.T, got, want map[string]float64) {
	t.Helper()
	for name, v := range want {
		if g, ok := got[name]; !ok || g != v {
			t.Errorf("%s = %v (present %t), want %v", name, g, ok, v)
		}
	}
}

// openMetricsStore opens a store on a test clock that records into a page
// of its own.
func openMetricsStore(t *testing.T) (*Store, *clock, *obs.Metrics) {
	t.Helper()
	clk := &clock{t: testNow}
	m := obs.New()
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{Logger: slog.New(newLogSink()), Now: clk.now, Metrics: m})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk, m
}

func TestMetrics_anOpenThatStartedEmptyIsCountedByReason(t *testing.T) {
	t.Run("a corrupt file moved aside", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "grelot.db")
		if err := os.WriteFile(path, corruptCopies(t)["invalid"], 0o600); err != nil {
			t.Fatal(err)
		}
		m := obs.New()
		s, err := Open(path, &Options{Logger: slog.New(newLogSink()), Metrics: m})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		wantSeries(t, page(t, m), map[string]float64{
			`grelot_store_started_empty_total{reason="corrupt"}`: 1,
			`grelot_store_started_empty_total{reason="format"}`:  0,
		})
	})
	t.Run("a namespace of another format reset", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "grelot.db")
		writeOtherFormat(t, path)
		m := obs.New()
		s, err := Open(path, &Options{Logger: slog.New(newLogSink()), Metrics: m})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		wantSeries(t, page(t, m), map[string]float64{
			`grelot_store_started_empty_total{reason="corrupt"}`: 0,
			`grelot_store_started_empty_total{reason="format"}`:  1,
		})
	})
	t.Run("a store reopened whole", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "grelot.db")
		first, err := Open(path, &Options{Logger: slog.New(newLogSink())})
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		m := obs.New()
		s, err := Open(path, &Options{Logger: slog.New(newLogSink()), Metrics: m})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		wantSeries(t, page(t, m), map[string]float64{
			`grelot_store_started_empty_total{reason="corrupt"}`: 0,
			`grelot_store_started_empty_total{reason="format"}`:  0,
		})
	})
}

func TestMetrics_runRecordsEachCommittedPassAndCountsEachFailedOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := obs.New()
		s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{Logger: slog.New(newLogSink()), Metrics: m})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		start := time.Now()
		stop := runSweeper(t, s, func() {})
		time.Sleep(SweepEvery)
		synctest.Wait()
		wantSeries(t, page(t, m), map[string]float64{
			`grelot_sweep_last_success_timestamp_seconds`: float64(start.Add(SweepEvery).Unix()),
			`grelot_sweep_failures_total`:                 0,
		})

		if err := s.db.Close(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * SweepEvery)
		synctest.Wait()
		stop()
		wantSeries(t, page(t, m), map[string]float64{
			`grelot_sweep_last_success_timestamp_seconds`: float64(start.Add(SweepEvery).Unix()),
			`grelot_sweep_failures_total`:                 2,
		})
	})
}

func TestMetrics_eachStoredPostIsCountedByItsThreadKind(t *testing.T) {
	s, _, m := openMetricsStore(t)
	regAt(t, s, "alpha")
	// bravo's join card goes to alpha, who shares its path; alpha's own
	// had nobody to go to and was not stored.
	regAt(t, s, "bravo")
	mustPost(t, s, posting(sess("alpha"), Target{Announce: true}))
	mustPost(t, s, posting(sess("alpha"), Target{Path: repoSubflux}))
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	mustPost(t, s, posting(sess("bravo"), Target{Agent: "alpha"}))
	refused := posting(sess("alpha"), Target{Agent: "bravo"})
	refused.Subject = ""
	if _, _, err := s.Post(t.Context(), Keyed{}, &refused); !isKind(err, KindInvalid) {
		t.Fatalf("post with no subject: %v, want KindInvalid", err)
	}
	wantSeries(t, page(t, m), map[string]float64{
		`grelot_posts_total{thread_kind="announce"}`: 1,
		`grelot_posts_total{thread_kind="path"}`:     1,
		`grelot_posts_total{thread_kind="direct"}`:   2,
		`grelot_posts_total{thread_kind="cards"}`:    1,
	})
}

func TestMetrics_aPostTheStoreRefusesCountsNothing(t *testing.T) {
	m := obs.New()
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{Logger: slog.New(newLogSink()), Now: func() time.Time { return testNow }, Metrics: m, MaxBytes: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	regAt(t, s, "alpha")
	if _, _, err := s.Post(t.Context(), Keyed{}, new(posting(sess("alpha"), Target{Announce: true}))); !isKind(err, KindFull) {
		t.Fatalf("post past the ceiling: %v, want KindFull", err)
	}
	if v, ok := page(t, m)[`grelot_posts_total{thread_kind="announce"}`]; ok {
		t.Errorf("announce posts = %v, want no series for a refused post", v)
	}
}

func TestMetrics_eachRecordedNudgeIsCountedByLayerAndAttemptWithItsDelay(t *testing.T) {
	s, clk, m := openMetricsStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	charlie := regAt(t, s, "charlie")
	directTo(t, s, "alpha", "bravo", bravo.ID)
	clk.advance(1500 * time.Millisecond)
	first := nudgeOf(t, s, bravo.ID).Due
	mustSend(t, s, bravo.ID)
	if _, recorded, err := s.RecordNudge(t.Context(), first, NudgeByPush); err != nil || recorded {
		t.Fatalf("a second record of the same send: recorded %v, %v; want false", recorded, err)
	}

	clk.advance(3 * time.Minute)
	repeat := nudgeOf(t, s, bravo.ID).Due
	if repeat == nil || repeat.Attempt != 2 {
		t.Fatalf("due at 3 minutes = %+v, want attempt 2", repeat)
	}
	if _, ok, err := s.RecordNudge(t.Context(), repeat, NudgeByPush); err != nil || !ok {
		t.Fatalf("RecordNudge(attempt 2): %v, %v", ok, err)
	}

	directTo(t, s, "alpha", "charlie", charlie.ID)
	clk.advance(2 * time.Second)
	if _, err := s.Hold(t.Context(), plain("charlie")); err != nil {
		t.Fatal(err)
	}

	got := page(t, m)
	wantSeries(t, got, map[string]float64{
		`grelot_nudges_total{attempt="first",via="watcher"}`: 1,
		`grelot_nudges_total{attempt="repeat",via="push"}`:   1,
		`grelot_nudges_total{attempt="first",via="hook"}`:    1,
		`grelot_nudge_delay_seconds_count{via="watcher"}`:    1,
		`grelot_nudge_delay_seconds_sum{via="watcher"}`:      1.5,
		`grelot_nudge_delay_seconds_count{via="push"}`:       1,
		// The re-nudge's oldest unread is the first post, 1.5 s plus 3 min old.
		`grelot_nudge_delay_seconds_sum{via="push"}`:   181.5,
		`grelot_nudge_delay_seconds_count{via="hook"}`: 1,
		`grelot_nudge_delay_seconds_sum{via="hook"}`:   2,
	})
	if v, ok := got[`grelot_nudges_total{attempt="first",via="push"}`]; ok {
		t.Errorf("first nudges via push = %v, want none: the racing record wrote nothing", v)
	}
}
