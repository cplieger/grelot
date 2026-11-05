package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// harness is one server over a fresh store on a test clock.
type harness struct {
	t     *testing.T
	store *store.Store
	srv   *Server
	http  *httptest.Server
	clk   *clock
	push  *fakePush
	logs  logSink
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, store.Options{})
}

// newHarnessWith is newHarness over a store opened with o, its logger,
// clock and path root set by the harness.
func newHarnessWith(t *testing.T, o store.Options) *harness {
	t.Helper()
	clk := &clock{t: testNow}
	logs := newLogSink()
	logger := slog.New(logs)
	o.Logger, o.Now, o.PathRoot = logger, clk.now, DefaultPathRoot
	st, err := store.Open(filepath.Join(t.TempDir(), "grelot.db"), o)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	push := &fakePush{states: map[uint64]pushState{}}
	srv := New(&Options{
		Store: st, Logger: logger, Now: clk.now, Version: testVersion, Push: push,
		Config: Config{Token: testToken, PathRoot: DefaultPathRoot},
	})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &harness{t: t, store: st, srv: srv, http: hs, clk: clk, logs: logs, push: push}
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
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(h.t.Context(), method, h.http.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set(headerVersion, testVersion)
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := h.http.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := &response{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
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
	if _, err := h.store.Post(h.t.Context(), &store.Posting{
		Caller: store.Caller{Session: session}, Subject: "hello", Body: "a body",
		To: store.Target{Agent: to}, Fields: nc,
	}); err != nil {
		h.t.Fatalf("Post to %s: %v", to, err)
	}
}
