package server

import (
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/grelot/internal/store"
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

func wantSeries(t *testing.T, got, want map[string]float64) {
	t.Helper()
	for name, v := range want {
		if g, ok := got[name]; !ok || g != v {
			t.Errorf("%s = %v (present %t), want %v", name, g, ok, v)
		}
	}
}

func TestMetrics_postsAreCountedByThreadKindAndRefusalsByReason(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	h.mustPost(sessA, announce, "news")
	h.mustPost(sessA, map[string]any{"path": repoGrelot}, "on grelot")
	if r := h.as(sessA, http.MethodPost, "/v1/posts", postBody(map[string]any{"agent": "reader"}, "", "a body")); r.status != http.StatusBadRequest {
		t.Fatalf("post with no subject: %d %s, want 400", r.status, r.raw)
	}
	for range 28 {
		h.mustPost(sessA, map[string]any{"agent": "reader"}, "hello")
	}
	if r := h.as(sessA, http.MethodPost, "/v1/posts", postBody(map[string]any{"agent": "reader"}, "hello", "a body")); r.status != http.StatusTooManyRequests {
		t.Fatalf("31st post: %d %s, want 429", r.status, r.raw)
	}
	wantSeries(t, h.scrape(), map[string]float64{
		`grelot_posts_total{thread_kind="announce"}`:        1,
		`grelot_posts_total{thread_kind="path"}`:            1,
		`grelot_posts_total{thread_kind="direct"}`:          28,
		`grelot_posts_refused_total{reason="rate_limited"}`: 1,
		`grelot_posts_refused_total{reason="store_full"}`:   0,
		`grelot_posts_refused_total{reason="scope"}`:        0,
	})

	t.Run("a delegate's post outside its scope", func(t *testing.T) {
		h := newHarness(t)
		h.mustRegister(sessA, "parent")
		handle := h.grantDelegate(sessA, "review")
		r := h.request(http.MethodPost, "/v1/posts", postBody(map[string]any{"path": repoGrelot}, "hello", "a body"),
			map[string]string{headerSession: sessA, headerAs: handle})
		if r.status != http.StatusForbidden {
			t.Fatalf("post outside the scope: %d %s, want 403", r.status, r.raw)
		}
		wantSeries(t, h.scrape(), map[string]float64{
			`grelot_posts_refused_total{reason="scope"}`:        1,
			`grelot_posts_refused_total{reason="rate_limited"}`: 0,
		})
	})
	t.Run("a post past 90 % of the ceiling", func(t *testing.T) {
		h := newHarnessWith(t, store.Options{MaxBytes: 1})
		h.mustRegister(sessA, "sender")
		h.mustRegister(sessB, "reader")
		if r := h.as(sessA, http.MethodPost, "/v1/posts", postBody(map[string]any{"agent": "reader"}, "hello", "a body")); r.status != http.StatusInsufficientStorage {
			t.Fatalf("post: %d %s, want 507", r.status, r.raw)
		}
		got := h.scrape()
		wantSeries(t, got, map[string]float64{`grelot_posts_refused_total{reason="store_full"}`: 1})
		if v, ok := got[`grelot_posts_total{thread_kind="direct"}`]; ok {
			t.Errorf("direct posts = %v, want no series for a refused post", v)
		}
	})
}

func TestMetrics_providersInvalidCountsTheFilesSkippedAtLoad(t *testing.T) {
	t.Setenv("SECRET_DOOR_TOKEN", doorSecret)
	for _, tc := range []struct {
		fsys fs.FS
		name string
		want float64
	}{
		{name: "two broken files beside a good one", want: 2, fsys: providers(
			"broken", "name: broken\nno_such_key: 1\n",
			"alpha", doorFile("alpha", "door_token", "    match: {provider: testhost}\n"),
			"testhost", doorFile("testhost", "door_token", "    match: {host: host-a/app}\n"),
		)},
		{name: "an unreadable folder", want: 1, fsys: unreadable{}},
		{name: "every file loads", want: 0, fsys: providers("testhost", doorFile("testhost", "door_token", ""))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessWith(t, store.Options{}, withProviders(tc.fsys))
			wantSeries(t, h.scrape(), map[string]float64{`grelot_providers_invalid`: tc.want})
		})
	}
}

func TestMetrics_eachPushTryIsCountedByDoorAndOutcome(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	const door = `door="testhost/push"`
	for _, tc := range []struct {
		name    string
		outcome string
		reply   pushReply
		up      float64
	}{
		{name: "202", reply: pushReply{status: http.StatusAccepted}, outcome: "accepted", up: 1},
		{name: "503", reply: pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}}, outcome: "retry", up: 1},
		{name: "404 on the gone list", reply: pushReply{status: http.StatusNotFound}, outcome: "gone", up: 1},
		{name: "400", reply: pushReply{status: http.StatusBadRequest}, outcome: "gave_up", up: 1},
		{name: "401", reply: pushReply{status: http.StatusUnauthorized}, outcome: "gave_up", up: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rcv := newPushReceiver(t, tc.reply)
			synctest.Test(t, func(t *testing.T) {
				h := newPushHarness(t, rcv.kiroDoor()...)
				h.mustRegisterAlone(sessA, "reader")
				h.mustRegisterAlone(sessB, "writer")
				if got := h.scrape()[`grelot_push_door_up{`+door+`}`]; got != 1 {
					t.Fatalf("door up before any send = %v, want 1", got)
				}
				h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
				synctest.Wait()

				got := h.scrape()
				wantSeries(t, got, map[string]float64{
					`grelot_push_door_up{` + door + `}`:                                  tc.up,
					`grelot_push_sends_total{` + door + `,outcome="` + tc.outcome + `"}`: 1,
				})
				sends := 0.0
				for name, v := range got {
					if strings.HasPrefix(name, "grelot_push_sends_total{") {
						sends += v
					}
				}
				if sends != 1 {
					t.Errorf("push tries counted = %v, want the one try", sends)
				}
			})
		})
	}
}
