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

	"github.com/cplieger/go/sse/v2"
	"github.com/cplieger/grelot/internal/auth"
	"github.com/cplieger/grelot/internal/auth/authtest"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/store"
)

// testKeepalive spaces the event stream's keepalives, so a test sees one
// soon after it opens a stream.
const testKeepalive = 200 * time.Millisecond

const (
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

// unlimited lifts every /v1 limit, so a test sends as many requests as it
// needs on the harness's frozen clock; a limit's own test sets Limits.
var unlimited = Limits{Rate: 1e9, Burst: 1e9, KeyWaits: 1e6, ServerWaits: 1e6}

// harness is one server over a fresh store on a test clock.
type harness struct {
	t     *testing.T
	store *store.Store
	srv   *Server
	http  *httptest.Server
	// ops serves the server's OpsHandler; nil for a bubble harness.
	ops  *httptest.Server
	clk  *clock
	push *fakePush
	// metrics is the page the store and server record into.
	metrics *obs.Metrics
	// open opens the store at dbPath with storeOpts and builds the server
	// over it.
	open      func()
	logs      logSink
	dbPath    string
	storeOpts store.Options
	// browser is the cookies the sign-in routes set, sent with every /api
	// request.
	browser *browser
	// auth is the account service every server of the harness runs on
	// when its edits name none; machine is the key its /v1 requests send,
	// minted on the first.
	auth      *auth.Auth
	machine   string
	machineMu sync.Mutex
}

// browser is one user agent's cookies for the server, kept by name: the
// harness serves over plain http, where a cookie jar would drop the
// session cookie's Secure flag along with the cookie.
type browser struct {
	cookies map[string]string
	mu      sync.Mutex
}

func (b *browser) add(r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, value := range b.cookies {
		r.AddCookie(&http.Cookie{Name: name, Value: value})
	}
}

func (b *browser) keep(resp *http.Response) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c.Value
		}
	}
}

// header is the browser's cookies as one Cookie header.
func (b *browser) header() string {
	r := &http.Request{Header: http.Header{}}
	b.add(r)
	return r.Header.Get("Cookie")
}

// forget drops every cookie, as a new browser would have none.
func (b *browser) forget() {
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(b.cookies)
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
	h.ops = httptest.NewServer(h.srv.OpsHandler())
	t.Cleanup(h.ops.Close)
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
	h := &harness{
		t: t, logs: newLogSink(), dbPath: filepath.Join(t.TempDir(), "grelot.db"), storeOpts: o,
		browser: &browser{cookies: map[string]string{}},
	}
	h.open = func() {
		so := h.storeOpts
		logger := slog.New(h.logs)
		hub, events := NewHub(), newEvents(logger, sse.WithKeepalive(testKeepalive), sse.WithWriteTimeout(10*time.Second))
		m := obs.New()
		so.Logger, so.Now, so.PathRoot, so.Woke, so.Metrics = logger, now, protopath.DefaultRoot, hub.Woke, m
		so.Changed = events.Changed
		if so.KeyLive == nil {
			so.KeyLive = func(id int64) (bool, error) { return h.srv.auth.KeyLive(id) }
		}
		if before := h.storeOpts.Woke; before != nil {
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
			Store: st, Logger: logger, Now: now, Version: testVersion, Push: h.push, Hub: hub, Events: events, Metrics: m,
			Config: Config{PathRoot: protopath.DefaultRoot}, Limits: unlimited,
		}
		for _, edit := range edits {
			edit(opts)
		}
		if opts.Auth == nil {
			if h.auth == nil {
				h.auth = authtest.New(t, AuthErrors(logger, m))
			}
			opts.Auth = h.auth
		}
		h.store, h.srv, h.metrics = st, New(opts), m
		t.Cleanup(func() { h.srv.Release(context.WithoutCancel(t.Context())) })
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
	h.srv.Release(h.t.Context())
	if err := h.store.Close(); err != nil {
		h.t.Fatalf("store.Close: %v", err)
	}
	time.Sleep(down)
	h.open()
}

// fill reopens the store over the same file with a ceiling it is already
// past, and serves a new server over it, so every write that adds rows
// answers 507 from then on.
func (h *harness) fill() {
	h.t.Helper()
	h.srv.Release(h.t.Context())
	if err := h.store.Close(); err != nil {
		h.t.Fatalf("store.Close: %v", err)
	}
	h.storeOpts.MaxBytes = 1
	h.open()
	if h.http != nil {
		h.http.Close()
		h.http = httptest.NewServer(h.srv.Handler())
		h.t.Cleanup(h.http.Close)
		h.ops.Close()
		h.ops = httptest.NewServer(h.srv.OpsHandler())
		h.t.Cleanup(h.ops.Close)
	}
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
	st, err := h.store.Status(h.t.Context(), store.Caller{Key: h.keyID(), Session: session})
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

const harnessKeyLabel = "harness"

// machineKey is the key the harness's /v1 requests send, minted once from
// the server's account service, whose first admin owns it.
func (h *harness) machineKey() (string, error) {
	h.machineMu.Lock()
	defer h.machineMu.Unlock()
	if h.machine == "" {
		key, err := h.srv.auth.CreateKey(context.Background(), harnessKeyLabel)
		if err != nil {
			return "", fmt.Errorf("mint the harness's machine key: %w", err)
		}
		h.machine = key
	}
	return h.machine, nil
}

// keyID is the ID of machineKey, which a store call acting for an identity
// the harness registered names.
func (h *harness) keyID() int64 {
	h.t.Helper()
	key, err := h.machineKey()
	if err != nil {
		h.t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(h.t.Context(), http.MethodGet, "/v1/me", http.NoBody)
	r.Header.Set("Authorization", "Bearer "+key)
	p, err := h.srv.auth.Authenticate(r)
	if err != nil || !p.IsAPIKey() {
		h.t.Fatalf("the harness's machine key does not authenticate: %v", err)
	}
	return p.KeyID
}

// request sends method path with the machine key, the version and headers,
// body encoded as JSON when it is not nil.
func (h *harness) request(method, path string, body any, headers map[string]string) *response {
	h.t.Helper()
	return h.requestCtx(h.t.Context(), method, path, body, headers)
}

// requestCtx is request under ctx, whose deadline the route's own sits
// under only when the harness serves in process.
func (h *harness) requestCtx(ctx context.Context, method, path string, body any, headers map[string]string) *response {
	h.t.Helper()
	r, err := h.send(ctx, method, path, body, headers)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// send is requestCtx returning its error instead of failing the test, so
// a goroutine other than the test's may call it: t.Fatal must run on the
// test's goroutine.
func (h *harness) send(ctx context.Context, method, path string, body any, headers map[string]string) (*response, error) {
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	base := ""
	if h.http != nil {
		base = h.http.URL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(path, "/v1/") {
		key, kerr := h.machineKey()
		if kerr != nil {
			return nil, kerr
		}
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set(headerVersion, testVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if route, _, _ := strings.Cut(path, "?"); method != http.MethodGet && !sentOnce[route] {
		req.Header.Set(headerKey, newTestKey())
	}
	if strings.HasPrefix(path, "/api/") {
		h.browser.add(req)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := h.roundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	h.browser.keep(resp)
	raw, _ := io.ReadAll(resp.Body)
	out := &response{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out, nil
}

// pending is a request sent from its own goroutine.
type pending struct {
	h       *harness
	answers chan sendResult
}

type sendResult struct {
	r   *response
	err error
}

// async sends the request from its own goroutine; answer or now collects
// its response on the test's goroutine.
func (h *harness) async(method, path string, body any, headers map[string]string) *pending {
	p := &pending{h: h, answers: make(chan sendResult, 1)}
	ctx := h.t.Context()
	go func() {
		r, err := h.send(ctx, method, path, body, headers)
		p.answers <- sendResult{r: r, err: err}
	}()
	return p
}

// answer waits for p's response.
func (p *pending) answer() *response {
	p.h.t.Helper()
	return p.got(<-p.answers)
}

// now is p's response if it has answered, or nil while it still holds.
func (p *pending) now() *response {
	p.h.t.Helper()
	select {
	case res := <-p.answers:
		return p.got(res)
	default:
		return nil
	}
}

// got is res's response, failing the test on its send error.
func (p *pending) got(res sendResult) *response {
	p.h.t.Helper()
	if res.err != nil {
		p.h.t.Fatal(res.err)
	}
	return res.r
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
	resp, err := h.roundTrip(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// roundTrip is do returning its error instead of failing the test.
func (h *harness) roundTrip(req *http.Request) (*http.Response, error) {
	if h.http == nil {
		rec := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(rec, req)
		return rec.Result(), nil
	}
	client := *h.http.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client.Do(req)
}

// opsCall sends a bare method path, with no header, to the operations
// listener, over the network or in process for a bubble harness.
func (h *harness) opsCall(method, path string) *http.Response {
	h.t.Helper()
	base := ""
	if h.ops != nil {
		base = h.ops.URL
	}
	req, err := http.NewRequestWithContext(h.t.Context(), method, base+path, http.NoBody)
	if err != nil {
		h.t.Fatal(err)
	}
	if h.ops == nil {
		rec := httptest.NewRecorder()
		h.srv.OpsHandler().ServeHTTP(rec, req)
		return rec.Result()
	}
	resp, err := h.ops.Client().Do(req)
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
		Caller: store.Caller{Key: h.keyID(), Session: session}, Subject: "hello", Body: "a body",
		To: store.Target{Agent: to}, Fields: nc,
	}); err != nil {
		h.t.Fatalf("Post to %s: %v", to, err)
	}
}
