package server

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// scrape reads /metrics with no headers into series name, labels
// included as the page prints them (sorted by name), to value.
func (h *harness) scrape() map[string]float64 {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodGet, h.base()+"/metrics", http.NoBody)
	if err != nil {
		h.t.Fatal(err)
	}
	resp := h.do(req)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		h.t.Fatalf("GET /metrics: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out := map[string]float64{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			h.t.Fatalf("series line %q: %v", line, err)
		}
		out[line[:i]] = v
	}
	return out
}

func (h *harness) base() string {
	if h.http == nil {
		return ""
	}
	return h.http.URL
}

func TestMetrics_takesNoHeadersAndCarriesTheRuntimeFamilies(t *testing.T) {
	h := newHarness(t)
	got := h.scrape()
	for _, name := range []string{`grelot_identities{state="live"}`, `grelot_sweep_failures_total`, `go_goroutines`} {
		if _, ok := got[name]; !ok {
			t.Errorf("no %s on the page", name)
		}
	}
}

func TestMetrics_gaugesCountTheStoreAtTheScrape(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		for _, sess := range []string{sessA, sessB} {
			if r := h.watch(sess, time.Second); r.status != http.StatusNoContent {
				t.Fatalf("wait from %s: %d %s", sess, r.status, r.raw)
			}
		}
		if r := h.as(sessB, http.MethodPost, "/v1/lifecycle", map[string]any{"event": "session_end"}); r.status >= 300 {
			t.Fatalf("session_end: %d %s", r.status, r.raw)
		}
		h.mustPost(sessA, map[string]any{"agent": "writer"}, "hello")
		time.Sleep(30 * time.Second)

		got := h.scrape()
		for name, want := range map[string]float64{
			`grelot_identities{state="live"}`:    2,
			`grelot_identities{state="dormant"}`: 0,
			// writer's session ended, so only reader is online.
			`grelot_identities_online`:            1,
			`grelot_threads{kind="announce"}`:     1,
			`grelot_threads{kind="path"}`:         0,
			`grelot_threads{kind="direct"}`:       1,
			`grelot_threads{kind="cards"}`:        1,
			`grelot_reservations{state="held"}`:   0,
			`grelot_reservations{state="queued"}`: 0,
			`grelot_unread_backlog`:               1,
			`grelot_unread_oldest_seconds`:        30,
			`grelot_store_max_bytes`:              512 << 20,
		} {
			if v, ok := got[name]; !ok || v != want {
				t.Errorf("%s = %v (present %t), want %v", name, v, ok, want)
			}
		}
		file, free, inUse := got["grelot_store_file_bytes"], got["grelot_store_free_bytes"], got["grelot_store_in_use_bytes"]
		if file <= 0 || inUse != file-free {
			t.Errorf("bytes: file %v free %v in use %v, want a file and in use = file - free", file, free, inUse)
		}

		time.Sleep(time.Minute)
		if v := h.scrape()[`grelot_unread_oldest_seconds`]; v != 90 {
			t.Errorf("oldest unread a minute later = %v, want 90", v)
		}
	})
}

func TestMetrics_requestsAreCountedByTheirRoute(t *testing.T) {
	h := newHarness(t)
	h.as(sessA, http.MethodGet, "/v1/me", nil)
	h.as(sessA, http.MethodGet, "/v1/me", nil)
	h.request(http.MethodGet, "/v1/me", nil, map[string]string{"Authorization": "", headerSession: sessA})
	h.request(http.MethodGet, "/v1/no-such-route", nil, nil)
	h.request(http.MethodGet, "/no-such-page", nil, nil)
	got := h.scrape()
	for name, want := range map[string]float64{
		`grelot_http_requests_total{code="200",route="/v1/me"}`:      2,
		`grelot_http_requests_total{code="401",route="/v1/me"}`:      1,
		`grelot_http_requests_total{code="404",route="/v1/"}`:        1,
		`grelot_http_requests_total{code="404",route="unmatched"}`:   1,
		`grelot_http_request_duration_seconds_count{route="/v1/me"}`: 3,
	} {
		if v := got[name]; v != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}
	for name := range got {
		if strings.Contains(name, "no-such") {
			t.Errorf("a request path became a label: %s", name)
		}
	}
}
