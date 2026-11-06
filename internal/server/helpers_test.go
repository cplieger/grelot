//go:build !windows

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/store"
)

const (
	testToken   = "test-token"
	testVersion = "v1.1.0-dev.4"
	sessA       = "kiro-cli:session-a"
	sessB       = "kiro-cli:session-b"
	sessC       = "kiro-cli:session-c"
	repoGrelot  = "/workspace/grelot"
)

// testNow is the fixed instant every server test starts from.
var testNow = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

// clock is a settable test clock, safe across goroutines.
type clock struct {
	t  time.Time
	mu sync.Mutex
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// logSink keeps every record a test's server and store wrote.
type logSink struct {
	mu      *sync.Mutex
	records *[]slog.Record
}

func newLogSink() logSink { return logSink{mu: &sync.Mutex{}, records: &[]slog.Record{}} }

func (logSink) Enabled(context.Context, slog.Level) bool { return true }

func (l logSink) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	*l.records = append(*l.records, r.Clone())
	return nil
}

func (l logSink) WithAttrs([]slog.Attr) slog.Handler { return l }

func (l logSink) WithGroup(string) slog.Handler { return l }

// text renders every record with its attributes, one per line.
func (l logSink) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	h := slog.NewTextHandler(&b, nil)
	for _, r := range *l.records {
		_ = h.Handle(context.Background(), r)
	}
	return b.String()
}

// lines returns the records at level with message msg.
func (l logSink) lines(level slog.Level, msg string) []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []slog.Record
	for _, r := range *l.records {
		if r.Level == level && r.Message == msg {
			out = append(out, r)
		}
	}
	return out
}

// atLeast returns the records at level or above.
func (l logSink) atLeast(level slog.Level) []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []slog.Record
	for _, r := range *l.records {
		if r.Level >= level {
			out = append(out, r)
		}
	}
	return out
}

// harness is one server over a fresh store on a test clock.
type harness struct {
	t     *testing.T
	store *store.Store
	srv   *Server
	http  *httptest.Server
	clk   *clock
	push  *fakePush
	// metrics is the page the store and server record into.
	metrics *obs.Metrics
	// open opens the store at dbPath and builds the server over it.
	open   func()
	logs   logSink
	dbPath string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, store.Options{})
}

// newHarnessWith is newHarness over a store opened with o, its logger,
// clock and path root set by the harness; o.Woke runs before the hub's.
func newHarnessWith(t *testing.T, o store.Options, edits ...func(*Options)) *harness {
	t.Helper()
	clk := &clock{t: testNow}
	h := buildHarness(t, o, clk.now, edits...)
	h.clk = clk
	h.http = httptest.NewServer(h.srv.Handler())
	t.Cleanup(h.http.Close)
	return h
}

// newBubbleHarness is a harness on the clock of the synctest bubble it is
// called in. It serves each request in process: a goroutine blocked on a
// socket is not durably blocked, so the bubble's clock would never move.
func newBubbleHarness(t *testing.T, edits ...func(*Options)) *harness {
	t.Helper()
	return buildHarness(t, store.Options{}, time.Now, edits...)
}

// buildHarness applies edits to the server's Options before New.
func buildHarness(t *testing.T, o store.Options, now func() time.Time, edits ...func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, logs: newLogSink(), dbPath: filepath.Join(t.TempDir(), "grelot.db")}
	h.open = func() {
		so := o
		logger := slog.New(h.logs)
		hub := NewHub()
		m := obs.New()
		so.Logger, so.Now, so.PathRoot, so.Woke, so.Metrics = logger, now, protopath.DefaultRoot, hub.Woke, m
		if before := o.Woke; before != nil {
			so.Woke = func(ids []uint64) {
				before(ids)
				hub.Woke(ids)
			}
		}
		st, err := store.Open(h.dbPath, &so)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		h.push = &fakePush{states: map[uint64]pushState{}}
		opts := &Options{
			Store: st, Logger: logger, Now: now, Version: testVersion, Push: h.push, Hub: hub, Metrics: m,
			Config: Config{Token: testToken, PathRoot: protopath.DefaultRoot},
		}
		for _, edit := range edits {
			edit(opts)
		}
		h.store, h.srv, h.metrics = st, New(opts), m
		t.Cleanup(h.srv.Release)
	}
	h.open()
	return h
}

// restart stops the server and closes its store, then opens a new store
// over the same file and a new server with the same options, keeping the
// log. Only a bubble harness restarts: a network one's listener serves the
// old server's handler.
func (h *harness) restart() {
	h.t.Helper()
	h.restartAfter(0)
}

// restartAfter is restart with the server down for down, on the bubble's
// clock.
func (h *harness) restartAfter(down time.Duration) {
	h.t.Helper()
	if h.http != nil {
		h.t.Fatal("restart needs a bubble harness")
	}
	h.srv.Release()
	if err := h.store.Close(); err != nil {
		h.t.Fatalf("store.Close: %v", err)
	}
	time.Sleep(down)
	h.open()
}

// fakePush serves the identities a test gives a push state.
type fakePush struct {
	states map[uint64]pushState
	mu     sync.Mutex
}

var _ Pusher = (*fakePush)(nil)

func (f *fakePush) State(id uint64) pushState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.states[id]; ok {
		return st
	}
	return pushNone
}

func (*fakePush) registered(uint64, string, string) {}

func (*fakePush) beat(context.Context) {}

func (*fakePush) stop() {}

func (f *fakePush) set(id uint64, st pushState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[id] = st
}

// idOf is the store id of the identity session acts as.
func (h *harness) idOf(session string) uint64 {
	h.t.Helper()
	st, err := h.store.Status(h.t.Context(), store.Caller{Session: session})
	if err != nil {
		h.t.Fatalf("Status(%s): %v", session, err)
	}
	return st.Identity.ID
}

// response is one answer, its body kept raw and decoded.
type response struct {
	header http.Header
	body   map[string]any
	raw    string
	status int
}

func (r *response) line() string { return r.header.Get(headerStatus) }

func (r *response) str(key string) string {
	v, _ := r.body[key].(string)
	return v
}

// request sends method path with the bearer, the version and headers,
// body encoded as JSON when it is not nil.
func (h *harness) request(method, path string, body any, headers map[string]string) *response {
	h.t.Helper()
	return h.requestCtx(h.t.Context(), method, path, body, headers)
}

// requestCtx is request under ctx, whose deadline the route's own sits
// under only when the harness serves in process.
func (h *harness) requestCtx(ctx context.Context, method, path string, body any, headers map[string]string) *response {
	h.t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	base := ""
	if h.http != nil {
		base = h.http.URL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set(headerVersion, testVersion)
	if route, _, _ := strings.Cut(path, "?"); method != http.MethodGet && !sentOnce[route] {
		req.Header.Set(headerKey, newTestKey())
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp := h.do(req)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := &response{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// sentOnce are the write routes that take no Idempotency-Key; request
// keys every other write with a new key unless headers name one.
var sentOnce = map[string]bool{"/v1/lifecycle": true, "/v1/events/hold": true, "/v1/events/wait": true}

var testKeys atomic.Uint64

// newTestKey is a key no other request of the test binary sends.
func newTestKey() string { return fmt.Sprintf(`"test-key-%016d"`, testKeys.Add(1)) }

// do sends req over the network, or in process for a bubble harness.
func (h *harness) do(req *http.Request) *http.Response {
	h.t.Helper()
	if h.http == nil {
		rec := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(rec, req)
		return rec.Result()
	}
	resp, err := h.http.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// as sends a request from session.
func (h *harness) as(session, method, path string, body any) *response {
	h.t.Helper()
	return h.request(method, path, body, map[string]string{headerSession: session})
}

// where is a valid where report: one machine, one environment, no
// checkout.
func where(env string) map[string]any {
	return map[string]any{
		"machine":     "dmi:" + strings.Repeat("1", 32),
		"environment": strings.Repeat(env, 32),
		"checkouts":   []any{},
	}
}

func registerBody(name, ttl string, paths ...string) map[string]any {
	if len(paths) == 0 {
		paths = []string{repoGrelot}
	}
	return map[string]any{
		"name": name, "paths": paths, "description": "building the HTTP API",
		"host": "host-a/app", "os": "linux", "arch": "amd64",
		"where": where("a"), "ttl": ttl,
	}
}

// mustRegister registers name from session for 6 hours.
func (h *harness) mustRegister(session, name string) {
	h.t.Helper()
	if r := h.as(session, http.MethodPost, "/v1/register", registerBody(name, "6h")); r.status != http.StatusCreated {
		h.t.Fatalf("register %s: %d %s", name, r.status, r.raw)
	}
}

// directPost posts a direct message from session's identity to @to,
// straight through the store.
func (h *harness) directPost(session, to string) {
	h.t.Helper()
	nc := store.Fields{Name: store.NoChange, Description: store.NoChange, Paths: []string{store.NoChange}}
	if _, _, err := h.store.Post(h.t.Context(), store.Keyed{}, &store.Posting{
		Caller: store.Caller{Session: session}, Subject: "hello", Body: "a body",
		To: store.Target{Agent: to}, Fields: nc,
	}); err != nil {
		h.t.Fatalf("Post to %s: %v", to, err)
	}
}
