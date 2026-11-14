//go:build !windows

package server

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/obs"
)

func (h *harness) statsReport(span time.Duration) statsReport {
	h.t.Helper()
	r, err := h.srv.statsReport(h.t.Context(), span)
	if err != nil {
		h.t.Fatalf("statsReport(%v): %v", span, err)
	}
	return r
}

func TestSampler_keeps72HoursOfMinutesOldestFirst(t *testing.T) {
	sp := newSampler()
	const minutes = 72*60 + 1
	for i := range minutes + 1 {
		sp.tick(testNow.Add(time.Duration(i)*time.Minute), &obs.Snapshot{Live: i})
	}

	got := sp.series(testNow.Add(-time.Hour), testNow.Add(100*time.Hour))

	// minutes closed, the oldest dropped; each carries the gauges read as
	// it closed.
	if len(got) != 72*60 {
		t.Fatalf("%d samples kept, want %d", len(got), 72*60)
	}
	if first := got[0]; !first.At.Equal(testNow.Add(time.Minute)) || first.Live != 2 {
		t.Errorf("oldest %v live %d, want %v live 2", first.At, first.Live, testNow.Add(time.Minute))
	}
	if last := got[len(got)-1]; !last.At.Equal(testNow.Add(72 * time.Hour)) {
		t.Errorf("newest %v, want %v", last.At, testNow.Add(72*time.Hour))
	}
	if part := sp.series(testNow.Add(72*time.Hour-time.Minute), testNow.Add(72*time.Hour)); len(part) != 1 {
		t.Errorf("a one-minute range holds %d samples, want 1", len(part))
	}
}

func TestSampler_aMinuteCountsItsRequestsErrorsAndP95(t *testing.T) {
	sp := newSampler()
	sp.tick(testNow, &obs.Snapshot{})
	for i := 1; i <= 100; i++ {
		status := http.StatusCreated
		if i == 100 {
			status = http.StatusInternalServerError
		}
		sp.request(httpserver.RequestMetric{Path: "/v1/posts", Status: status, Latency: time.Duration(i) * time.Millisecond})
	}
	for _, route := range []string{"/v1/events/wait", "/v1/inbox", "/v1/events/wait"} {
		sp.request(httpserver.RequestMetric{Path: route, Status: http.StatusOK, Latency: 55 * time.Second})
	}
	sp.tick(testNow.Add(59*time.Second), &obs.Snapshot{})
	if got := sp.series(testNow, testNow.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("samples %+v before the minute turned, want none", got)
	}

	sp.tick(testNow.Add(time.Minute), &obs.Snapshot{})

	got := sp.series(testNow, testNow.Add(time.Hour))
	if len(got) != 1 {
		t.Fatalf("%d samples, want 1", len(got))
	}
	// The held waits count as requests but are no latency.
	if s := got[0]; !s.At.Equal(testNow) || s.Requests != 103 || s.Errors != 1 || s.P95 != 95*time.Millisecond {
		t.Errorf("sample at %v: %d requests, %d errors, p95 %v; want %v, 103, 1, 95ms", s.At, s.Requests, s.Errors, s.P95, testNow)
	}
	sp.tick(testNow.Add(2*time.Minute), &obs.Snapshot{})
	if next := sp.series(testNow.Add(time.Minute), testNow.Add(time.Hour)); len(next) != 1 || next[0].Requests != 0 || next[0].P95 != 0 {
		t.Errorf("the next minute %+v, want one empty sample", next)
	}
}

func TestBeat_samplesTheGaugesOncePerMinute(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "reader")
	h.srv.Beat(t.Context())
	h.clk.advance(time.Minute)
	h.srv.Beat(t.Context())
	h.clk.advance(10 * time.Second)
	h.srv.Beat(t.Context())

	got := h.statsReport(6 * time.Hour).Samples

	if len(got) != 1 || !got[0].At.Equal(testNow) || !got[0].State || got[0].Live != 1 || got[0].MaxBytes == 0 {
		t.Errorf("samples %+v, want one at %v with one live agent and the ceiling", got, testNow)
	}
}

func TestStatsReport_readsTheStoreAtMostEvery30Seconds(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "reader")
	h.mustRegisterAlone(sessB, "writer")
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "first")
	posts := func() int {
		n := 0
		for _, c := range h.statsReport(6 * time.Hour).Posts {
			n += c.Direct
		}
		return n
	}
	if n := posts(); n != 1 {
		t.Fatalf("%d direct posts, want 1", n)
	}
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "second")
	h.clk.advance(29 * time.Second)
	if n := posts(); n != 1 {
		t.Errorf("%d direct posts 29 s on, want the cached 1", n)
	}
	h.clk.advance(time.Second)
	if n := posts(); n != 2 {
		t.Errorf("%d direct posts 30 s on, want 2", n)
	}
}

func TestHold_countsEachHeldTurnPerAgent(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "reader")
	h.mustRegisterAlone(sessB, "writer")
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "first")
	h.stopHold(sessA, false)
	h.stopHold(sessA, false)
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "second")
	h.stopHold(sessA, true)
	h.stopHold(sessB, false)

	got := h.statsReport(6 * time.Hour).HeldTurns

	if len(got) != 1 || got["reader"] != 2 {
		t.Errorf("held turns %v, want reader 2: a hold with nothing new and the writer's hold held none", got)
	}
}

func TestPost_aRefusalIsAnEventNamingTheSenderAndNoLabel(t *testing.T) {
	h := newHarness(t)
	const sender = "quillfeather-sender"
	h.mustRegister(sessA, sender)
	h.mustRegister(sessB, "reader")
	body := postBody(map[string]any{"agent": "reader"}, "hello", "a body")
	for range postsPerMinute {
		h.as(sessA, http.MethodPost, "/v1/posts", body)
	}
	if r := h.as(sessA, http.MethodPost, "/v1/posts", body); r.status != http.StatusTooManyRequests {
		t.Fatalf("post past the limit: %d %s, want 429", r.status, r.raw)
	}

	rep := h.statsReport(6 * time.Hour)

	want := []obs.Event{{At: testNow, Kind: obs.EventPostRefused, Detail: obs.RefusedRateLimited, Agent: sender}}
	if !slices.Equal(rep.Events, want) {
		t.Errorf("events %+v, want %+v", rep.Events, want)
	}
	if rep.EventCounts[obs.EventPostRefused] != 1 {
		t.Errorf("counts %v, want one refused post", rep.EventCounts)
	}
	for series := range h.scrape() {
		if strings.Contains(series, sender) {
			t.Errorf("series %s names the sender", series)
		}
	}
}
