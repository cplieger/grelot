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

	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/store"
)

// page reads /metrics with no headers.
func (h *harness) page() string {
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
	return string(raw)
}

// scrape reads /metrics into series name, labels included as the page
// prints them (sorted by name), to value.
func (h *harness) scrape() map[string]float64 {
	h.t.Helper()
	out := map[string]float64{}
	for line := range strings.SplitSeq(h.page(), "\n") {
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
	for _, name := range []string{
		`grelot_identities{state="live"}`, `grelot_sweep_failures_total`,
		`go_goroutines`, `go_memstats_heap_inuse_bytes`, `go_gc_duration_seconds_count`, `process_resident_memory_bytes`,
	} {
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
				// Every outcome is on the page at 0 before the first try,
				// so increase() over the alert window sees that try.
				before := map[string]float64{`grelot_push_door_up{` + door + `}`: 1}
				for _, o := range []string{"accepted", "retry", "gone", "gave_up"} {
					before[`grelot_push_sends_total{`+door+`,outcome="`+o+`"}`] = 0
				}
				wantSeries(t, h.scrape(), before)
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

func TestMetrics_noLabelValueNamesAnAgentPathSubjectOrSession(t *testing.T) {
	h := newHarness(t)
	const (
		sessQ   = "kiro-cli:session-quillfeather"
		sessM   = "kiro-cli:session-marrowbone"
		repo    = "/workspace/zephyrine"
		subject = "ochre lantern"
	)
	for sess, name := range map[string]string{sessQ: "quillfeather", sessM: "marrowbone"} {
		if r := h.as(sess, http.MethodPost, "/v1/register", registerBody(name, "6h", repo)); r.status != http.StatusCreated {
			t.Fatalf("register %s: %d %s", name, r.status, r.raw)
		}
	}
	h.mustPost(sessQ, map[string]any{"agent": "marrowbone"}, subject)
	h.mustPost(sessQ, map[string]any{"path": repo}, subject)
	h.mustPost(sessQ, announce, subject)
	if r := h.as(sessQ, http.MethodPost, "/v1/reservations", map[string]any{"path": repo + "/internal", "reason": "editing", "ttl": "1h"}); r.status != http.StatusCreated {
		t.Fatalf("reserve: %d %s", r.status, r.raw)
	}
	handle := h.grantDelegate(sessQ, "tanglewood")
	h.request(http.MethodPost, "/v1/posts", postBody(map[string]any{"path": "/workspace/elsewhere"}, subject, "a body"),
		map[string]string{headerSession: sessQ, headerAs: handle})
	h.as(sessM, http.MethodGet, "/v1/inbox", nil)
	h.as(sessM, http.MethodGet, "/v1/history?path="+repo, nil)
	h.as(sessM, http.MethodGet, "/v1/identities/quillfeather", nil)

	page := h.page()
	for _, s := range []string{"quillfeather", "marrowbone", "zephyrine", "elsewhere", "ochre", "tanglewood", handle, "session-"} {
		if strings.Contains(page, s) {
			t.Errorf("the page carries %q", s)
		}
	}
}

// seriesBudget caps the page at its worst case: the strict product of
// every route and every code, with each latency bucket a series, is about
// 730, so a new label that multiplies it trips this.
const seriesBudget = 1000

func TestMetrics_seriesStayBoundedAtTheWorstCase(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	// Eight doors stands for the largest folder an operator runs: two per
	// shipped coding tool.
	const doors = 8
	var files []string
	for i := range doors {
		name := "tool" + strconv.Itoa(i)
		files = append(files, name, doorFile(name, "door_token", "    match: {host: host-"+strconv.Itoa(i)+"/app}\n"))
	}
	h := newHarnessWith(t, store.Options{}, pushFrom(files...))

	// One request per route the API serves, no session sent, so each
	// answers at once.
	for _, r := range [][2]string{
		{"POST", "/v1/register"},
		{"POST", "/v1/revive"},
		{"GET", "/v1/me"},
		{"PATCH", "/v1/me"},
		{"DELETE", "/v1/me"},
		{"GET", "/v1/identities"},
		{"POST", "/v1/posts"},
		{"GET", "/v1/threads"},
		{"GET", "/v1/messages/1/receipts"},
		{"GET", "/v1/history"},
		{"PUT", "/v1/follows"},
		{"DELETE", "/v1/follows"},
		{"POST", "/v1/inbox/ack"},
		{"POST", "/v1/reservations"},
		{"GET", "/v1/reservations"},
		{"PATCH", "/v1/reservations/1"},
		{"DELETE", "/v1/reservations/1"},
		{"POST", "/v1/delegates"},
		{"DELETE", "/v1/delegates/review"},
		{"POST", "/v1/lifecycle"},
		{"POST", "/v1/events/hold"},
		{"POST", "/v1/events/wait"},
		{"GET", "/v1/inbox"},
		{"GET", "/v1/no-such-route"},
		{"GET", "/healthz"},
		{"GET", "/no-such-page"},
	} {
		h.request(r[0], r[1], nil, nil)
	}
	h.page()
	var routes []string
	for name := range h.scrape() {
		if rest, ok := strings.CutPrefix(name, `grelot_http_request_duration_seconds_count{route="`); ok {
			routes = append(routes, strings.TrimSuffix(rest, `"}`))
		}
	}
	// The label drops the method: 18 /v1 paths, the /v1/ catch-all,
	// /healthz, /metrics and unmatched.
	if len(routes) != 22 {
		t.Fatalf("%d route labels %v, want 22", len(routes), routes)
	}

	for _, route := range routes {
		for _, code := range []int{200, 201, 202, 204, 400, 401, 403, 404, 405, 409, 410, 413, 426, 429, 500, 503, 507} {
			h.metrics.RecordHTTP(httpserver.RequestMetric{Path: route, Status: code, Latency: 30 * time.Second})
		}
	}
	for i := range doors {
		for _, outcome := range []string{obs.SendAccepted, obs.SendRetry, obs.SendGaveUp, obs.SendGone} {
			h.metrics.PushSends.Inc("tool"+strconv.Itoa(i)+"/push", outcome)
		}
	}
	for _, kind := range []string{"announce", "path", "direct", "cards"} {
		h.metrics.Posts.Inc(kind)
	}
	for _, via := range []string{"push", "watcher", "hook"} {
		for _, attempt := range []string{obs.AttemptFirst, obs.AttemptRepeat} {
			h.metrics.Nudges.Inc(via, attempt)
		}
		h.metrics.NudgeDelay.Observe(1, via)
	}
	worst := h.scrape()
	if n := len(worst); n > seriesBudget {
		t.Errorf("%d series at the worst case, want at most %d", n, seriesBudget)
	}
	t.Logf("%d series at the worst case", len(worst))

	t.Run("activity adds none", func(t *testing.T) {
		for i := range 40 {
			sess := "kiro-cli:session-" + strconv.Itoa(i)
			h.mustRegister(sess, "agent-"+strconv.Itoa(i))
			h.mustPost(sess, announce, "subject "+strconv.Itoa(i))
			h.as(sess, http.MethodGet, "/v1/history?path=/workspace/repo-"+strconv.Itoa(i), nil)
		}
		if n := len(h.scrape()); n != len(worst) {
			t.Errorf("%d series after 40 agents posted, want the %d of the worst case", n, len(worst))
		}
	})
}
