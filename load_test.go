//go:build loadtest && !windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/go/slogx"
	"github.com/cplieger/grelot/internal/auth/authtest"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/store"
)

// go test -tags loadtest -run TestLoad -timeout 60m -v . starts the server
// as a child process, so the memory it reads is the server's alone, and
// drives it at the API's limits until the store refuses posts, four agents
// to each machine key. Optional:
// LOAD_IDENTITIES (100), LOAD_STORE_MAX_BYTES (the default ceiling),
// LOAD_GOMEMLIMIT (no limit), LOAD_TURN (20s between inbox reads),
// LOAD_DEADLINE (30m), and the paths LOAD_REPORT (JSON) and
// LOAD_CPUPROFILE (the server's profile).

const (
	loadServeEnv = "GRELOT_LOAD_SERVE"
	// The API's limits: 120 characters of subject, 64 KiB of body, 30
	// posts a minute per identity, 100 entries an inbox page, 55 s waits.
	loadSubjectLen = 120
	loadBodyLen    = 64 << 10
	loadPostEvery  = 2100 * time.Millisecond
	loadInboxLimit = 100
	loadWait       = "55s"
	loadRepos      = 10
	// loadAgentsPerKey is how many agents share one machine key. Each sends
	// about one request a second (a post every loadPostEvery, its inbox
	// pages, acks and re-held wait), so 16 stay under a key's 20 a second;
	// their 16 standing waits sit far under its 128.
	loadAgentsPerKey = 16
)

func TestMain(m *testing.M) {
	if os.Getenv(loadServeEnv) == "1" {
		os.Exit(serveForLoad())
	}
	os.Exit(m.Run())
}

// serveForLoad is run's serve on free loopback ports, so the parent reads
// the addresses from the listening line.
func serveForLoad() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	h, _ := slogx.NewHandler(slogx.Options{Output: os.Stderr, Format: slogx.JSON})
	logger := slog.New(h)
	cfg, err := server.LoadConfig()
	if err != nil {
		logger.Error("config invalid", "error", err)
		return 1
	}
	if p := os.Getenv("LOAD_CPUPROFILE"); p != "" {
		f, err := os.Create(p)
		if err != nil || pprof.StartCPUProfile(f) != nil {
			logger.Error("cpu profile", "error", err)
			return 1
		}
		defer pprof.StopCPUProfile()
	}
	if err := serve(ctx, &cfg, listeners{api: "127.0.0.1:0", ops: "127.0.0.1:0"}, logger); err != nil {
		logger.Error("server stopped", "error", err)
		return 1
	}
	return 0
}

type loadReport struct {
	GOMEMLIMIT        string   `json:"gomemlimit"`
	Identities        int      `json:"identities"`
	MaxBytes          int64    `json:"store_max_bytes"`
	Seconds           float64  `json:"seconds_to_full"`
	Posts             int64    `json:"posts"`
	PostsByKind       [3]int64 `json:"posts_announce_path_direct"`
	Deliveries        int64    `json:"deliveries"`
	PostsPerSecond    float64  `json:"posts_per_second"`
	PostP50Millis     float64  `json:"post_p50_ms"`
	PostP99Millis     float64  `json:"post_p99_ms"`
	InUseAtFull       float64  `json:"in_use_bytes_at_full"`
	FileAtFull        float64  `json:"file_bytes_at_full"`
	InUseRatio        float64  `json:"in_use_ratio_at_full"`
	BytesPerPost      float64  `json:"in_use_bytes_per_post"`
	InboxPages        int64    `json:"inbox_pages"`
	Acks              int64    `json:"acks"`
	AcksAfterFull     int64    `json:"acks_after_full"`
	Nudges            int64    `json:"nudges"`
	PeakHeapInuse     float64  `json:"peak_heap_inuse_bytes"`
	PeakGoroutines    float64  `json:"peak_goroutines"`
	PeakRSS           int64    `json:"peak_rss_bytes"`
	PeakRSSAnon       int64    `json:"peak_rss_anon_bytes"`
	PeakRSSFile       int64    `json:"peak_rss_file_bytes"`
	HealthzFailures   int64    `json:"healthz_failures"`
	RateLimited       int64    `json:"rate_limited"`
	ErrorLines        int64    `json:"error_lines"`
	UnexpectedReplies int64    `json:"unexpected_replies"`
	// Unexpected counts each unexpected reply by request and status, 0
	// for a transport error.
	Unexpected       map[string]int64 `json:"unexpected"`
	CPUSecondsAtFull float64          `json:"cpu_seconds_at_full"`
	// HostLoad is the host's one-minute load at the start and the end: the
	// client shares the server's CPUs, so a busy host slows both.
	HostLoad [2]string `json:"host_load_start_end"`
	// Timeouts counts 503 timeout answers, and Resends the writes sent
	// again with their key after one.
	Timeouts int64 `json:"timeouts"`
	Resends  int64 `json:"resends"`
	// ReplaysAfterTimeout is the server's replay count: the client sends a
	// write again only after a timeout, so each replay is a write that
	// committed and answered 503.
	ReplaysAfterTimeout float64 `json:"replays_after_timeout"`
	// StoredUnanswered is the posts the server stored less those it
	// answered 201.
	StoredUnanswered float64 `json:"stored_unanswered"`
}

func TestLoad(t *testing.T) {
	n := loadInt(t, "LOAD_IDENTITIES", 100)
	if n < 2 {
		t.Fatal("LOAD_IDENTITIES must be at least 2, so a direct post has another agent to go to")
	}
	maxBytes := int64(loadInt(t, "LOAD_STORE_MAX_BYTES", store.DefaultMaxBytes))
	turn := loadDuration(t, "LOAD_TURN", 20*time.Second)
	deadline := loadDuration(t, "LOAD_DEADLINE", 30*time.Minute)
	rep := &loadReport{Identities: n, MaxBytes: maxBytes, GOMEMLIMIT: os.Getenv("LOAD_GOMEMLIMIT")}
	rep.HostLoad[0] = hostLoad()

	child := startLoadServer(t, maxBytes, rep.GOMEMLIMIT, (n+loadAgentsPerKey-1)/loadAgentsPerKey)
	c := &loadClient{base: "http://" + child.addrs.api, ops: "http://" + child.addrs.ops, keys: map[string]string{}, http: &http.Client{
		Timeout:   90 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: 4 * n, DisableCompression: true},
	}}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var bg sync.WaitGroup
	bg.Go(func() { child.sample(ctx, c, rep) })

	var cnt loadCounts
	agents := make([]*loadAgent, n)
	for i := range agents {
		agents[i] = &loadAgent{i: i, name: fmt.Sprintf("agent-%03d", i), session: "load:session-" + strconv.Itoa(i)}
		c.keys[agents[i].session] = child.keys[i/loadAgentsPerKey]
		agents[i].register(t, c, &cnt)
	}

	var (
		full      atomic.Bool
		fullAt    time.Time
		stopOnce  sync.Once
		latencies = make(chan time.Duration, 1<<16)
	)
	body := strings.Repeat("0123456789abcdef", loadBodyLen/16)
	subject := strings.Repeat("s", loadSubjectLen)
	checkPastTheLimits(t, c, &cnt, agents[0], subject, body)
	start := time.Now()
	stopPosting := make(chan struct{})
	stop := func(atFull bool) {
		stopOnce.Do(func() {
			if atFull {
				fullAt = time.Now()
				full.Store(true)
			}
			close(stopPosting)
		})
	}
	var posters, readers sync.WaitGroup
	for _, a := range agents {
		readers.Go(func() { a.watch(ctx, c, &cnt) })
		readers.Go(func() { a.read(ctx, c, turn, time.Duration(a.i)*turn/time.Duration(n), &full, &cnt) })
		posters.Go(func() {
			offset := time.Duration(a.i) * loadPostEvery / time.Duration(n)
			a.post(ctx, c, stopPosting, offset, n, subject, body, &cnt, latencies, func() { stop(true) })
		})
	}

	select {
	case <-stopPosting:
	case <-time.After(deadline):
		stop(false)
		t.Errorf("no 507 within %s: %d posts", deadline, cnt.posts.Load())
	}
	posters.Wait()
	page := c.scrape(t.Context())
	if full.Load() {
		rep.Seconds = fullAt.Sub(start).Seconds()
	}

	// Writes that free space or keep state honest still go through.
	if code := agents[0].renew(t.Context(), c, &cnt); code != http.StatusOK {
		t.Errorf("a renewal past the ceiling answered %d, want 200", code)
	}
	if got, _ := agents[1].postOnce(t.Context(), c, &cnt, n, 0, subject, body); full.Load() && got.status != http.StatusInsufficientStorage {
		t.Errorf("a post past the ceiling answered %d, want 507", got.status)
	}
	for end := time.Now().Add(turn + 10*time.Second); cnt.acksAfterFull.Load() == 0 && time.Now().Before(end); {
		time.Sleep(500 * time.Millisecond)
	}
	cancel()
	readers.Wait()
	bg.Wait()
	end := c.scrape(t.Context())
	rep.ReplaysAfterTimeout = end["grelot_writes_replayed_total"]
	rep.StoredUnanswered = -float64(cnt.posts.Load())
	for _, kind := range []string{"announce", "path", "direct"} {
		rep.StoredUnanswered += end[`grelot_posts_total{thread_kind="`+kind+`"}`]
	}
	rep.HostLoad[1] = hostLoad()
	child.stop(t)

	close(latencies)
	var lat []time.Duration
	for d := range latencies {
		lat = append(lat, d)
	}
	slices.Sort(lat)
	rep.Posts, rep.Deliveries = cnt.posts.Load(), cnt.deliveries.Load()
	for k := range rep.PostsByKind {
		rep.PostsByKind[k] = cnt.byKind[k].Load()
	}
	rep.InboxPages, rep.Acks, rep.AcksAfterFull, rep.Nudges = cnt.pages.Load(), cnt.acks.Load(), cnt.acksAfterFull.Load(), cnt.nudges.Load()
	rep.RateLimited, rep.UnexpectedReplies, rep.Unexpected = cnt.rateLimited.Load(), cnt.unexpected.Load(), cnt.unexpectedBy
	rep.Timeouts, rep.Resends = cnt.timeouts.Load(), cnt.resends.Load()
	rep.ErrorLines = int64(len(child.errorLines))
	for _, line := range child.errorLines[:min(5, len(child.errorLines))] {
		t.Logf("server: %s", line)
	}
	if rep.Seconds > 0 {
		rep.PostsPerSecond = float64(rep.Posts) / rep.Seconds
	}
	if len(lat) > 0 {
		rep.PostP50Millis = float64(lat[len(lat)/2].Microseconds()) / 1000
		rep.PostP99Millis = float64(lat[len(lat)*99/100].Microseconds()) / 1000
	}
	rep.InUseAtFull, rep.FileAtFull = page["grelot_store_in_use_bytes"], page["grelot_store_file_bytes"]
	rep.CPUSecondsAtFull = page["process_cpu_seconds_total"]
	rep.InUseRatio = rep.InUseAtFull / float64(maxBytes)
	if rep.Posts > 0 {
		rep.BytesPerPost = rep.InUseAtFull / float64(rep.Posts)
	}

	out, _ := json.MarshalIndent(rep, "", "  ")
	t.Logf("load report:\n%s", out)
	if p := os.Getenv("LOAD_REPORT"); p != "" {
		if err := os.WriteFile(p, out, 0o600); err != nil {
			t.Error(err)
		}
	}

	// Each post measures the store under the write lock, so the refusal
	// starts past 90 % of the ceiling with at most one request over it.
	if limit := 0.9*float64(maxBytes) + 128<<10; rep.InUseAtFull*10 <= float64(maxBytes)*9 || rep.InUseAtFull > limit {
		t.Errorf("bytes in use at the first 507 are %.0f, want above 90 %% of %d and at most %.0f", rep.InUseAtFull, maxBytes, limit)
	}
	if rep.RateLimited != 0 {
		t.Errorf("%d posts rate limited, want none at %s apart", rep.RateLimited, loadPostEvery)
	}
	if rep.UnexpectedReplies != 0 {
		t.Errorf("%d unexpected replies", rep.UnexpectedReplies)
	}
	if rep.ReplaysAfterTimeout != 0 {
		t.Errorf("%.0f writes answered 503 timeout had committed: their re-send replayed the first result", rep.ReplaysAfterTimeout)
	}
	if rep.StoredUnanswered != 0 {
		t.Errorf("%.0f posts stored that were never answered 201", rep.StoredUnanswered)
	}
	if rep.HealthzFailures != 0 {
		t.Errorf("/healthz failed %d times under load", rep.HealthzFailures)
	}
	if rep.ErrorLines != 0 {
		t.Errorf("%d error lines under load", rep.ErrorLines)
	}
	if rep.AcksAfterFull == 0 {
		t.Error("no ack went through past the ceiling")
	}
}

type loadCounts struct {
	unexpectedBy                                                                   map[string]int64
	posts, deliveries, pages, acks, acksAfterFull, nudges, rateLimited, unexpected atomic.Int64
	timeouts, resends                                                              atomic.Int64
	byKind                                                                         [3]atomic.Int64
	mu                                                                             sync.Mutex
}

func (c *loadCounts) miss(request string, status int) {
	c.unexpected.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.unexpectedBy == nil {
		c.unexpectedBy = map[string]int64{}
	}
	c.unexpectedBy[request+" "+strconv.Itoa(status)]++
}

type loadClient struct {
	http *http.Client
	// keys is each agent session's machine key, written before the first
	// request.
	keys map[string]string
	base string
	// ops is the operations listener's URL, for /metrics and /healthz.
	ops string
}

// do sends one request as session, or with no session when it is empty,
// and decodes a JSON reply into out when out is not nil.
func (c *loadClient) do(ctx context.Context, method, path, session string, in, out any) (int, error) {
	code, _, err := c.send(ctx, method, path, session, "", in, out)
	return code, err
}

// write sends a keyed write with a new key, and once more with the same
// key a second after a 503 timeout, as grelot's client does.
func (c *loadClient) write(ctx context.Context, cnt *loadCounts, method, path, session string, in, out any) (int, error) {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	key := `"` + base64.RawURLEncoding.EncodeToString(raw) + `"`
	for sent := 0; ; sent++ {
		code, why, err := c.send(ctx, method, path, session, key, in, out)
		if code != http.StatusServiceUnavailable || why != "timeout" {
			return code, err
		}
		cnt.timeouts.Add(1)
		if sent == 1 {
			return code, err
		}
		cnt.resends.Add(1)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// send is do with key as the Idempotency-Key when it is not empty, and
// returns an error answer's code.
func (c *loadClient) send(ctx context.Context, method, path, session, key string, in, out any) (int, string, error) {
	var body io.Reader = http.NoBody
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, "", err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.keys[session])
	req.Header.Set("Grelot-Version", "v1.1.0")
	if session != "" {
		req.Header.Set("Grelot-Session", session)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	var refusal struct {
		Code string `json:"code"`
	}
	switch {
	case resp.StatusCode >= 400:
		_ = json.NewDecoder(resp.Body).Decode(&refusal)
	case out != nil && resp.StatusCode/100 == 2 && resp.StatusCode != http.StatusNoContent:
		err = json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, refusal.Code, err
}

// healthz sends a bare GET /healthz to the operations listener.
func (c *loadClient) healthz(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ops+"/healthz", http.NoBody)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// scrape reads every series of /metrics, its labels included as the page
// prints them.
func (c *loadClient) scrape(ctx context.Context) map[string]float64 {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ops+"/metrics", http.NoBody)
	if err != nil {
		return nil
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	out := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		i := strings.LastIndexByte(line, ' ')
		if i < 0 || strings.HasPrefix(line, "#") {
			continue
		}
		if v, err := strconv.ParseFloat(line[i+1:], 64); err == nil {
			out[line[:i]] = v
		}
	}
	return out
}

type loadAgent struct {
	name, session string
	i             int
}

func (a *loadAgent) repo() string { return "/workspace/repo-" + strconv.Itoa(a.i%loadRepos) }

func (a *loadAgent) where() map[string]any {
	return map[string]any{
		"machine": fmt.Sprintf("machine-id:%032x", a.i), "environment": fmt.Sprintf("%032x", a.i+1<<20), "checkouts": []any{},
	}
}

func (a *loadAgent) register(t *testing.T, c *loadClient, cnt *loadCounts) {
	t.Helper()
	code, err := c.write(t.Context(), cnt, http.MethodPost, "/v1/register", a.session, map[string]any{
		"name": a.name, "paths": []string{a.repo()}, "description": "load", "host": "load", "os": "linux",
		"arch": "amd64", "ttl": "6h", "where": a.where(),
	}, nil)
	if err != nil || code != http.StatusCreated {
		t.Fatalf("register %s: %d %v", a.name, code, err)
	}
}

func (a *loadAgent) renew(ctx context.Context, c *loadClient, cnt *loadCounts) int {
	code, _ := c.write(ctx, cnt, http.MethodPatch, "/v1/me", a.session, map[string]any{"ttl": "6h", "where": a.where()}, nil)
	return code
}

// watch holds the agent's standing wait open for the whole run, as its
// watcher does.
func (a *loadAgent) watch(ctx context.Context, c *loadClient, cnt *loadCounts) {
	for ctx.Err() == nil {
		code, err := c.do(ctx, http.MethodPost, "/v1/events/wait", a.session, map[string]string{"wait": loadWait}, nil)
		switch {
		case ctx.Err() != nil:
		case err != nil || (code != http.StatusOK && code != http.StatusNoContent):
			cnt.miss("wait", code)
			// A refused wait answers at once; looping on it would only
			// measure the loop.
			time.Sleep(time.Second)
		case code == http.StatusOK:
			cnt.nudges.Add(1)
		}
	}
}

// read drains the agent's inbox once a turn in full pages and acks each.
func (a *loadAgent) read(ctx context.Context, c *loadClient, turn, offset time.Duration, full *atomic.Bool, cnt *loadCounts) {
	timer := time.NewTimer(offset)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		for ctx.Err() == nil {
			var page struct {
				Entries []struct {
					Seq uint64 `json:"seq"`
				} `json:"entries"`
			}
			code, err := c.do(ctx, http.MethodGet, "/v1/inbox?limit="+strconv.Itoa(loadInboxLimit), a.session, nil, &page)
			if ctx.Err() != nil {
				return
			}
			if err != nil || code != http.StatusOK {
				cnt.miss("inbox", code)
				break
			}
			cnt.pages.Add(1)
			if len(page.Entries) == 0 {
				break
			}
			code, err = c.write(ctx, cnt, http.MethodPost, "/v1/inbox/ack", a.session, map[string]uint64{"upto": page.Entries[len(page.Entries)-1].Seq}, nil)
			if ctx.Err() != nil {
				return
			}
			if err != nil || code != http.StatusOK {
				cnt.miss("ack", code)
				break
			}
			cnt.acks.Add(1)
			if full.Load() {
				cnt.acksAfterFull.Add(1)
			}
			if len(page.Entries) < loadInboxLimit {
				break
			}
		}
		timer.Reset(turn)
	}
}

// post sends a maximal post every loadPostEvery until stop closes or the
// store refuses one, which calls onFull.
func (a *loadAgent) post(ctx context.Context, c *loadClient, stop <-chan struct{}, offset time.Duration, n int,
	subject, body string, cnt *loadCounts, latencies chan<- time.Duration, onFull func(),
) {
	timer := time.NewTimer(offset)
	defer timer.Stop()
	for k := 0; ; k++ {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-timer.C:
		}
		timer.Reset(loadPostEvery)
		began := time.Now()
		got, kind := a.postOnce(ctx, c, cnt, n, k, subject, body)
		switch got.status {
		case http.StatusCreated:
			select {
			case latencies <- time.Since(began):
			default:
			}
			cnt.posts.Add(1)
			cnt.byKind[kind].Add(1)
			cnt.deliveries.Add(int64(got.recipients))
		case http.StatusInsufficientStorage:
			onFull()
			return
		case http.StatusTooManyRequests:
			cnt.rateLimited.Add(1)
		default:
			if ctx.Err() == nil {
				cnt.miss("post", got.status)
			}
		}
	}
}

// checkPastTheLimits posts one character of subject, one byte of message
// body and one byte of request body past the API's limits, and requires
// each refused. A refused post gives its rate-limit slot back.
func checkPastTheLimits(t *testing.T, c *loadClient, cnt *loadCounts, a *loadAgent, subject, body string) {
	t.Helper()
	for _, tc := range []struct {
		name string
		post map[string]any
		want int
	}{
		{"a subject of 121 characters", map[string]any{"subject": subject + "s", "body": body}, http.StatusBadRequest},
		{"a message body of 64 KiB + 1", map[string]any{"subject": subject, "body": body + "x"}, http.StatusBadRequest},
		{"a request body over 128 KiB", map[string]any{"subject": subject, "body": body + body}, http.StatusRequestEntityTooLarge},
	} {
		// Everything else is what every accepted post below sends.
		tc.post["to"] = "announce"
		tc.post["identity"] = map[string]any{"name": "no change", "description": "no change", "paths": []string{"no change"}}
		code, err := c.write(t.Context(), cnt, http.MethodPost, "/v1/posts", a.session, tc.post, nil)
		if err != nil || code != tc.want {
			t.Errorf("%s answered %d (%v), want %d", tc.name, code, err, tc.want)
		}
	}
}

type postReply struct {
	status, recipients int
}

// postOnce sends the agent's kth post: an announcement every tenth, else a
// path post to one of the repos or a direct post to another agent in turn.
func (a *loadAgent) postOnce(ctx context.Context, c *loadClient, cnt *loadCounts, n, k int, subject, body string) (postReply, int) {
	var to any
	kind := 0
	switch {
	case k%10 == 0:
		to = "announce"
	case k%2 == 1:
		to, kind = map[string]string{"path": "/workspace/repo-" + strconv.Itoa((a.i+k)%loadRepos)}, 1
	default:
		other := (a.i + k) % n
		if other == a.i {
			other = (a.i + 1) % n
		}
		to, kind = map[string]string{"agent": fmt.Sprintf("agent-%03d", other)}, 2
	}
	var out struct {
		Recipients struct {
			Total int `json:"total"`
		} `json:"recipients"`
	}
	code, err := c.write(ctx, cnt, http.MethodPost, "/v1/posts", a.session, map[string]any{
		"to": to, "subject": subject, "body": body,
		"identity": map[string]any{"name": "no change", "description": "no change", "paths": []string{"no change"}},
	}, &out)
	if err != nil {
		code = 0
	}
	return postReply{status: code, recipients: out.Recipients.Total}, kind
}

type loadServer struct {
	cmd    *exec.Cmd
	exited chan error
	addrs  listeners
	// keys are the machine keys the server admits.
	keys []string
	// errorLines is the server's error-level lines, complete once exited
	// has sent.
	errorLines []string
}

func startLoadServer(t *testing.T, maxBytes int64, gomemlimit string, keys int) *loadServer {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "providers"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		for _, p := range []string{"GOMEMLIMIT=", "GRELOT_", "STORE_", "ACCOUNTS_PATH=", "PROVIDERS_DIR=", "LISTEN_ADDR=", "OPS_LISTEN_ADDR=", "THREAD_TTL=", "DORMANT_TTL=", "RESERVATION_MAX_TTL=", "PUBLIC_URL=", "TRUSTED_PROXIES=", "OIDC_", "ADMIN_SOCKET="} {
			if strings.HasPrefix(kv, p) {
				return true
			}
		}
		return false
	})
	seeded := authtest.SeedKeys(t, filepath.Join(dir, "accounts.db"), keys)
	env = append(env, loadServeEnv+"=1", "PUBLIC_URL=http://localhost:9737", "STORE_PATH="+filepath.Join(dir, "grelot.db"),
		"ACCOUNTS_PATH="+filepath.Join(dir, "accounts.db"), "ADMIN_SOCKET="+filepath.Join(dir, "admin.sock"),
		"STORE_MAX_BYTES="+strconv.FormatInt(maxBytes, 10), "PROVIDERS_DIR="+filepath.Join(dir, "providers"))
	if gomemlimit != "" {
		env = append(env, "GOMEMLIMIT="+gomemlimit)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = env
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &loadServer{cmd: cmd, exited: make(chan error, 1), keys: seeded}
	addrs := make(chan listeners, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if m := listeningAddr.FindStringSubmatch(line); m != nil {
				addrs <- listeners{api: m[1], ops: m[2]}
			}
			if strings.Contains(line, `"level":"ERROR"`) {
				s.errorLines = append(s.errorLines, line)
			}
		}
		s.exited <- cmd.Wait()
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	select {
	case s.addrs = <-addrs:
	case err := <-s.exited:
		t.Fatalf("server exited before listening: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("server not listening after 30s")
	}
	return s
}

// sample reads the server's memory from /proc five times a second and its
// heap, goroutines and health once a second, keeping the peaks.
func (s *loadServer) sample(ctx context.Context, c *loadClient, rep *loadReport) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		st := procStatus(s.cmd.Process.Pid)
		rep.PeakRSS = max(rep.PeakRSS, st["VmHWM"])
		rep.PeakRSSAnon = max(rep.PeakRSSAnon, st["RssAnon"])
		rep.PeakRSSFile = max(rep.PeakRSSFile, st["RssFile"])
		if i%5 != 0 {
			continue
		}
		page := c.scrape(ctx)
		rep.PeakHeapInuse = math.Max(rep.PeakHeapInuse, page["go_memstats_heap_inuse_bytes"])
		rep.PeakGoroutines = math.Max(rep.PeakGoroutines, page["go_goroutines"])
		if code, err := c.healthz(ctx); ctx.Err() == nil && (err != nil || code != http.StatusOK) {
			rep.HealthzFailures++
		}
	}
}

// stop ends the server as Docker does and requires a clean exit.
func (s *loadServer) stop(t *testing.T) {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-s.exited:
		if err != nil {
			t.Errorf("server exit after SIGTERM: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Error("server still running 30s after SIGTERM")
	}
}

// procStatus reads the kB fields of /proc/<pid>/status in bytes.
func procStatus(pid int) map[string]int64 {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return nil
	}
	out := map[string]int64{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		fields := strings.Fields(rest)
		if !ok || len(fields) != 2 || fields[1] != "kB" {
			continue
		}
		if v, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
			out[key] = v << 10
		}
	}
	return out
}

func hostLoad() string {
	raw, _ := os.ReadFile("/proc/loadavg")
	load, _, _ := strings.Cut(string(raw), " ")
	return load
}

func loadInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q: want a positive integer", key, v)
	}
	return n
}

func loadDuration(t *testing.T, key string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		t.Fatalf("%s=%q: want a positive duration", key, v)
	}
	return d
}
