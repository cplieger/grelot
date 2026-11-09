package obs

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/measure"
)

var testNow = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

func gather(t *testing.T, m *Metrics) map[string]measure.Family {
	t.Helper()
	fams, err := m.reg.Gather(t.Context())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := map[string]measure.Family{}
	for _, f := range fams {
		out[f.Name] = f
	}
	return out
}

// value is the sample of f whose label values are vals, in declared order.
func value(t *testing.T, f measure.Family, vals ...string) float64 {
	t.Helper()
	for _, s := range f.Samples {
		got := make([]string, 0, len(s.Labels))
		for _, l := range s.Labels {
			got = append(got, l.Value)
		}
		if slices.Equal(got, vals) {
			return s.Value
		}
	}
	t.Fatalf("%s has no sample %v", f.Name, vals)
	return 0
}

func TestNew_servesEverySeriesOfTheSeriesList(t *testing.T) {
	m := New()
	m.ServeState(func(context.Context) (State, bool) { return State{}, true }, func() time.Time { return testNow })
	fams := gather(t, m)
	series := []struct {
		name   string
		typ    measure.Type
		labels []string
	}{
		{"grelot_sweep_last_success_timestamp_seconds", measure.TypeGauge, nil},
		{"grelot_sweep_failures_total", measure.TypeCounter, nil},
		{"grelot_store_in_use_bytes", measure.TypeGauge, nil},
		{"grelot_store_file_bytes", measure.TypeGauge, nil},
		{"grelot_store_free_bytes", measure.TypeGauge, nil},
		{"grelot_store_max_bytes", measure.TypeGauge, nil},
		{"grelot_store_started_empty_total", measure.TypeCounter, []string{"reason"}},
		{"grelot_posts_total", measure.TypeCounter, []string{"thread_kind"}},
		{"grelot_posts_refused_total", measure.TypeCounter, []string{"reason"}},
		{"grelot_writes_replayed_total", measure.TypeCounter, nil},
		{"grelot_push_door_up", measure.TypeGauge, []string{"door"}},
		{"grelot_push_sends_total", measure.TypeCounter, []string{"door", "outcome"}},
		{"grelot_providers_invalid", measure.TypeGauge, nil},
		{"grelot_nudges_total", measure.TypeCounter, []string{"via", "attempt"}},
		{"grelot_nudge_delay_seconds", measure.TypeHistogram, []string{"via"}},
		{"grelot_identities", measure.TypeGauge, []string{"state"}},
		{"grelot_identities_online", measure.TypeGauge, nil},
		{"grelot_threads", measure.TypeGauge, []string{"kind"}},
		{"grelot_reservations", measure.TypeGauge, []string{"state"}},
		{"grelot_unread_backlog", measure.TypeGauge, nil},
		{"grelot_unread_oldest_seconds", measure.TypeGauge, nil},
		{"grelot_http_requests_total", measure.TypeCounter, []string{"route", "code"}},
		{"grelot_http_request_duration_seconds", measure.TypeHistogram, []string{"route"}},
	}
	listed := map[string]bool{}
	for _, want := range series {
		listed[want.name] = true
		t.Run(want.name, func(t *testing.T) {
			f, ok := fams[want.name]
			if !ok {
				t.Fatal("missing")
			}
			if f.Type != want.typ {
				t.Errorf("type %s, want %s", f.Type, want.typ)
			}
			for _, s := range f.Samples {
				var names []string
				for _, l := range s.Labels {
					names = append(names, l.Name)
				}
				if !slices.Equal(names, want.labels) {
					t.Errorf("labels %v, want %v", names, want.labels)
				}
			}
		})
	}
	for name := range fams {
		if strings.HasPrefix(name, "grelot_") && !listed[name] {
			t.Errorf("family %s is not in the series list", name)
		}
	}
}

func TestNew_seedsEveryRefusalAndStartedEmptyReasonAtZero(t *testing.T) {
	fams := gather(t, New())
	for _, r := range []string{"corrupt", "format"} {
		if v := value(t, fams["grelot_store_started_empty_total"], r); v != 0 {
			t.Errorf("started empty %s = %v, want 0", r, v)
		}
	}
	for _, r := range []string{"store_full", "rate_limited", "scope"} {
		if v := value(t, fams["grelot_posts_refused_total"], r); v != 0 {
			t.Errorf("refused %s = %v, want 0", r, v)
		}
	}
}

func TestServeState_readsTheStoreAtEachScrape(t *testing.T) {
	m := New()
	st := State{
		Oldest: testNow.Add(-90 * time.Second), Live: 3, Dormant: 2, Online: 1,
		Announce: 1, Path: 4, Direct: 5, Cards: 6, Held: 7, Queued: 8, Unread: 9,
		FileBytes: 4096, FreeBytes: 1024, InUse: 3072, MaxBytes: 512 << 20,
	}
	m.ServeState(func(context.Context) (State, bool) { return st, true }, func() time.Time { return testNow })
	fams := gather(t, m)
	for _, c := range []struct {
		name string
		vals []string
		want float64
	}{
		{"grelot_identities", []string{"live"}, 3},
		{"grelot_identities", []string{"dormant"}, 2},
		{"grelot_identities_online", nil, 1},
		{"grelot_threads", []string{"announce"}, 1},
		{"grelot_threads", []string{"path"}, 4},
		{"grelot_threads", []string{"direct"}, 5},
		{"grelot_threads", []string{"cards"}, 6},
		{"grelot_reservations", []string{"held"}, 7},
		{"grelot_reservations", []string{"queued"}, 8},
		{"grelot_unread_backlog", nil, 9},
		{"grelot_unread_oldest_seconds", nil, 90},
		{"grelot_store_file_bytes", nil, 4096},
		{"grelot_store_free_bytes", nil, 1024},
		{"grelot_store_in_use_bytes", nil, 3072},
		{"grelot_store_max_bytes", nil, 512 << 20},
	} {
		if got := value(t, fams[c.name], c.vals...); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.vals, got, c.want)
		}
	}

	st = State{}
	if got := value(t, gather(t, m)["grelot_unread_oldest_seconds"]); got != 0 {
		t.Errorf("oldest with nothing unread = %v, want 0", got)
	}
}

func TestServeState_aFailedCountLeavesTheGaugesOut(t *testing.T) {
	m := New()
	m.ServeState(func(context.Context) (State, bool) { return State{}, false }, func() time.Time { return testNow })
	if n := len(gather(t, m)["grelot_identities"].Samples); n != 0 {
		t.Errorf("%d identity samples from a failed count, want none", n)
	}
}

func TestRecordHTTP_labelsByRouteAndCode(t *testing.T) {
	m := New()
	m.RecordHTTP(httpserver.RequestMetric{Method: "GET", Path: "/v1/me", Status: 200, Latency: 30 * time.Millisecond})
	m.RecordHTTP(httpserver.RequestMetric{Method: "PATCH", Path: "/v1/me", Status: 200, Latency: time.Millisecond})
	m.RecordHTTP(httpserver.RequestMetric{Method: "POST", Path: "/v1/posts", Status: 507})
	fams := gather(t, m)
	if v := value(t, fams["grelot_http_requests_total"], "/v1/me", "200"); v != 2 {
		t.Errorf("/v1/me 200 = %v, want 2", v)
	}
	if v := value(t, fams["grelot_http_requests_total"], "/v1/posts", "507"); v != 1 {
		t.Errorf("/v1/posts 507 = %v, want 1", v)
	}
	for _, s := range fams["grelot_http_request_duration_seconds"].Samples {
		if s.Labels[0].Value == "/v1/me" && s.Histogram.Count != 2 {
			t.Errorf("/v1/me latency count %d, want 2", s.Histogram.Count)
		}
	}
}
