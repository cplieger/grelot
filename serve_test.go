//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/accounts"
	"github.com/cplieger/grelot/internal/auth/authtest"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/store"
)

// syncBuffer is a bytes.Buffer safe for serve's goroutine to write while the
// test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var listeningAddr = regexp.MustCompile(`"msg":"listening","addr":"([^"]+)","ops_addr":"([^"]+)"`)

// testConfig is a config over files of the test's own, its accounts file
// seeded with authtest.SeedKey.
func testConfig(t *testing.T) *server.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := &server.Config{
		PathRoot: protopath.DefaultRoot, PublicURL: &url.URL{Scheme: "http", Host: "localhost:9737"},
		StorePath: filepath.Join(dir, "grelot.db"), AccountsPath: filepath.Join(dir, "accounts.db"),
	}
	authtest.Seed(t, cfg.AccountsPath)
	return cfg
}

func TestServe(t *testing.T) {
	addrs, logs, cancel, done := startServe(t)
	addr := addrs.ops

	t.Run("healthz answers ok once the sweeper has passed", func(t *testing.T) { waitHealthy(t, addr) })
	t.Run("the API listener serves neither operations route", func(t *testing.T) {
		for _, path := range []string{"/healthz", "/metrics"} {
			resp, err := http.Get("http://" + addrs.api + path)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s on the API listener = %d, want 404", path, resp.StatusCode)
			}
		}
	})
	t.Run("metrics name the running version", func(t *testing.T) {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		page, _ := io.ReadAll(resp.Body)
		if want := `grelot_build_info{version="` + buildVersion() + `"} 1`; !strings.Contains(string(page), want) {
			t.Errorf("/metrics has no %s", want)
		}
	})
	t.Run("grelot health probes OPS_LISTEN_ADDR", func(t *testing.T) {
		t.Setenv("LISTEN_ADDR", addr)
		t.Setenv("OPS_LISTEN_ADDR", addrs.api)
		var stderr bytes.Buffer
		if code := run([]string{"health"}, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "404") {
			t.Errorf("OPS_LISTEN_ADDR naming the API: exit %d, stderr %q, want 1 naming its 404", code, stderr.String())
		}
		t.Setenv("LISTEN_ADDR", addrs.api)
		t.Setenv("OPS_LISTEN_ADDR", addr)
		stderr.Reset()
		if code := run([]string{"health"}, io.Discard, &stderr); code != 0 {
			t.Errorf("exit %d, want 0; stderr %q", code, stderr.String())
		}
	})

	t.Run("grelot health passes while serving", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := probeHealth(&stderr, addr, time.Second); code != 0 {
			t.Errorf("exit %d, want 0; stderr %q", code, stderr.String())
		}
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return within 5 s of cancel")
	}
	if !strings.Contains(logs.String(), `"msg":"stopped"`) {
		t.Errorf("no stopped line; logs: %s", logs.String())
	}
	if got := healthChanges(t, logs); len(got) != 2 || got[0]["level"] != "INFO" || got[0]["healthy"] != true ||
		got[1]["level"] != "WARN" || got[1]["healthy"] != false || got[1]["draining"] != true {
		t.Errorf("health state changed lines %v, want one INFO healthy=true then one WARN healthy=false draining=true", got)
	}
	var stderr bytes.Buffer
	if code := probeHealth(&stderr, addr, time.Second); code != 1 || !strings.Contains(stderr.String(), "unhealthy") {
		t.Errorf("grelot health after stop: exit %d, stderr %q, want 1 and unhealthy", code, stderr.String())
	}
}

// The server names a release stamp as it is, and a stamp no client could
// parse as the development version.
func TestServe_namesTheWireVersion(t *testing.T) {
	for stamped, want := range map[string]string{"v1.2.3-dev.4": "v1.2.3-dev.4", "v1.2.4-0.20261105101010-0123456789ab": "dev"} {
		t.Run(stamped, func(t *testing.T) {
			stamp(t, stamped)
			addrs, _, _, _ := startServe(t)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addrs.api+"/v1/version", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+authtest.SeedKey)
			req.Header.Set("Grelot-Version", "dev")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if got := resp.Header.Get("Grelot-Version"); resp.StatusCode != http.StatusOK || got != want {
				t.Errorf("status %d, Grelot-Version %q; want 200 and %q", resp.StatusCode, got, want)
			}
		})
	}
}

func TestProbeHealth(t *testing.T) {
	t.Parallel()
	t.Run("a live listener answering 503 fails", func(t *testing.T) {
		t.Parallel()
		st, err := store.Open(filepath.Join(t.TempDir(), "grelot.db"), &store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		srv := server.New(&server.Options{Store: st, Logger: slog.New(slog.DiscardHandler), Config: *testConfig(t), Auth: authtest.New(t, nil)})
		ts := httptest.NewServer(srv.OpsHandler())
		t.Cleanup(ts.Close)
		var stderr bytes.Buffer
		if code := probeHealth(&stderr, ts.Listener.Addr().String(), time.Second); code != 1 || !strings.Contains(stderr.String(), "503") {
			t.Errorf("before any sweep: exit %d, stderr %q, want 1 naming the 503", code, stderr.String())
		}
	})
	t.Run("a wildcard listen address probes loopback", func(t *testing.T) {
		t.Parallel()
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(ts.Close)
		_, port, _ := net.SplitHostPort(ts.Listener.Addr().String())
		var stderr bytes.Buffer
		if code := probeHealth(&stderr, ":"+port, time.Second); code != 0 {
			t.Errorf("exit %d, want 0; stderr %q", code, stderr.String())
		}
	})
	t.Run("a listener that accepts but never answers fails within the budget", func(t *testing.T) {
		t.Parallel()
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		start := time.Now()
		var stderr bytes.Buffer
		if code := probeHealth(&stderr, ln.Addr().String(), 200*time.Millisecond); code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("the probe took %v, want it cut at its 200 ms budget", took)
		}
	})
	t.Run("an address with no port fails", func(t *testing.T) {
		t.Parallel()
		var stderr bytes.Buffer
		if code := probeHealth(&stderr, "9737", time.Second); code != 1 || !strings.Contains(stderr.String(), `OPS_LISTEN_ADDR "9737"`) {
			t.Errorf("exit %d, stderr %q, want 1 naming OPS_LISTEN_ADDR and its value", code, stderr.String())
		}
	})
}

func TestListenAddr_theImageAndComposeExampleNameBothPorts(t *testing.T) {
	for file, wants := range map[string][]string{
		"Dockerfile":   {"\nEXPOSE 9737 9738\n"},
		"compose.yaml": {`- "9737:9737"`, `:9738:9738"`},
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range wants {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s does not contain %q", file, want)
			}
		}
	}
}

// brokenListener fails every Accept, as a listener whose socket died does.
type brokenListener struct{ net.Listener }

var errAcceptBroken = errors.New("accept: the socket is gone")

func (brokenListener) Accept() (net.Conn, error) { return nil, errAcceptBroken }

// The operations server failing after its bind stops the API too, and its
// error is the one returned.
func TestRunListeners_anOperationsServeFailureStopsTheAPI(t *testing.T) {
	t.Parallel()
	st, srv, err := build(testConfig(t), nil, authtest.New(t, nil), slog.New(slog.DiscardHandler), obs.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Release(context.WithoutCancel(t.Context())); _ = st.Close() })
	lc := &net.ListenConfig{}
	api, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ops, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runListeners(ctx, cancel, srv, api, brokenListener{ops}) }()
	select {
	case err := <-done:
		if !errors.Is(err, errAcceptBroken) {
			t.Errorf("runListeners = %v, want the operations accept error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runListeners still serving the API 10 s after the operations server failed")
	}
}

// breakableListener passes Accept through until broken, after which every
// Accept fails as brokenListener's does.
type breakableListener struct {
	net.Listener
	broken atomic.Bool
}

func (l *breakableListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil && l.broken.Load() {
		return nil, errAcceptBroken
	}
	return c, err
}

func (l *breakableListener) breakNow() {
	l.broken.Store(true)
	_ = l.Close()
}

// The API server failing on its own stops host push before runListeners
// returns, cancelling a send in flight, so serve never closes the store
// under the sender.
func TestRunListeners_anAPIServeFailureStopsHostPush(t *testing.T) {
	t.Setenv("SECRET_DOOR_TOKEN", "door-secret-value")
	entered, cancelled, hung := make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{})
	rcv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// net/http notices a closed connection only once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
			cancelled <- struct{}{}
		case <-hung:
		}
	}))
	t.Cleanup(rcv.Close)
	t.Cleanup(func() { close(hung) })
	cfg := testConfig(t)
	cfg.ProvidersDir = t.TempDir()
	door := "name: testhost\ndoors:\n  - name: push\n    layer: 1\n    transport: http\n" +
		"    url: '" + rcv.URL + "/notify/{{path .Session}}'\n" +
		"    body: '{\"line\": {{json .Line}}, \"key\": {{json .DedupeKey}}}'\n" +
		"    headers: {Authorization: 'Bearer {{secret \"door_token\"}}'}\n" +
		"    dedupe_window: 10m\n    gone: [404, 410]\n    match: {provider: kiro-cli}\n"
	if err := os.WriteFile(filepath.Join(cfg.ProvidersDir, "testhost.yaml"), []byte(door), 0o600); err != nil {
		t.Fatal(err)
	}
	st, srv, err := build(cfg, nil, authtest.New(t, nil), slog.New(slog.DiscardHandler), obs.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Release(context.WithoutCancel(t.Context())); _ = st.Close() })
	lc := &net.ListenConfig{}
	inner, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	api := &breakableListener{Listener: inner}
	ops, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runListeners(ctx, cancel, srv, api, ops) }()

	addr := inner.Addr().String()
	where := `"where":{"machine":"dmi:` + strings.Repeat("1", 32) + `","environment":"` + strings.Repeat("a", 32) + `","checkouts":[]}`
	for _, name := range []string{"reader", "writer"} {
		register := `{"name":"` + name + `","paths":["/workspace/` + name + `"],"description":"testing push","host":"host-a/app",` +
			`"os":"linux","arch":"amd64","ttl":"1h",` + where + `}`
		if status, code := call(t, addr, http.MethodPost, "/v1/register", "kiro-cli:session-"+name, `"register-key-`+name+`"`, register); status != http.StatusCreated {
			t.Fatalf("register %s: %d %s", name, status, code)
		}
	}
	post := `{"to":{"agent":"reader"},"subject":"hello","body":"a body",` +
		`"identity":{"name":"no change","description":"no change","paths":["no change"]}}`
	if status, code := call(t, addr, http.MethodPost, "/v1/posts", "kiro-cli:session-writer", `"post-key-00000001"`, post); status != http.StatusCreated {
		t.Fatalf("post: %d %s", status, code)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no push reached the receiver")
	}

	api.breakNow()
	select {
	case err := <-done:
		if !errors.Is(err, errAcceptBroken) {
			t.Errorf("runListeners = %v, want the API accept error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runListeners did not return 10 s after the API server failed")
	}
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Error("the send in flight was not cancelled: host push outlived runListeners")
	}
}

// serve refuses an operations address on the API's port before opening
// anything.
func TestRun_serveRefusesTheOperationsAddressOnTheAPIPort(t *testing.T) {
	dir := t.TempDir()
	cfg := &server.Config{StorePath: filepath.Join(dir, "grelot.db"), AccountsPath: filepath.Join(dir, "accounts.db")}
	t.Setenv("PUBLIC_URL", "http://localhost:9737")
	t.Setenv("STORE_PATH", cfg.StorePath)
	t.Setenv("ACCOUNTS_PATH", cfg.AccountsPath)
	t.Setenv("LISTEN_ADDR", "127.0.0.1:9737")
	t.Setenv("OPS_LISTEN_ADDR", ":9737")
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var stderr bytes.Buffer
	if code := run([]string{"serve"}, io.Discard, &stderr); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "OPS_LISTEN_ADDR must name another port than LISTEN_ADDR (9737)") {
		t.Errorf("stderr %q does not name the shared port", stderr.String())
	}
	if _, err := os.Stat(cfg.AccountsPath); !os.IsNotExist(err) {
		t.Errorf("the accounts file was created (%v), want nothing opened", err)
	}
}

// A failing operations listener stops serve, API and admin socket all,
// with its error.
func TestServe_anOperationsListenErrorStopsServe(t *testing.T) {
	t.Parallel()
	taken, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	addrs := testListeners(t)
	addrs.ops = taken.Addr().String()
	err = serve(context.Background(), testConfig(t), addrs, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("serve with the operations port taken = %v, want address already in use", err)
	}
	if _, err := os.Lstat(addrs.admin); !os.IsNotExist(err) {
		t.Errorf("Lstat of the admin socket after serve = %v, want it removed", err)
	}
}

// testListeners are free ports and an admin socket in a folder of its own,
// whose path fits a socket address wherever the temporary folder is.
func testListeners(t *testing.T) listeners {
	t.Helper()
	dir, err := os.MkdirTemp("", "grelot")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return listeners{api: "127.0.0.1:0", ops: "127.0.0.1:0", admin: filepath.Join(dir, "admin.sock")}
}

// startServe runs serve on a free port until the returned cancel or the
// test's end, and returns its address, its logs and serve's result.
func startServe(t *testing.T) (addrs listeners, logs *syncBuffer, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	return startServeWith(t, func(h slog.Handler) slog.Handler { return h })
}

// startServeWith is startServe logging through wrap's handler around the
// JSON one.
func startServeWith(t *testing.T, wrap func(slog.Handler) slog.Handler) (addrs listeners, logs *syncBuffer, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	return startServeOn(t, testConfig(t), wrap)
}

// startServeOn is startServeWith on cfg.
func startServeOn(t *testing.T, cfg *server.Config, wrap func(slog.Handler) slog.Handler) (addrs listeners, logs *syncBuffer, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	logs = &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan error, 1)
	stopped := make(chan struct{})
	want := testListeners(t)
	go func() {
		out <- serve(ctx, cfg, want, slog.New(wrap(slog.NewJSONHandler(logs, nil))))
		close(stopped)
	}()
	// Registered after the TempDir calls, so serve has returned and closed
	// the store before they are removed.
	t.Cleanup(func() { cancel(); <-stopped })
	for deadline := time.Now().Add(5 * time.Second); addrs.api == ""; time.Sleep(10 * time.Millisecond) {
		if m := listeningAddr.FindStringSubmatch(logs.String()); m != nil {
			addrs = listeners{api: m[1], ops: m[2], admin: want.admin}
		} else if time.Now().After(deadline) {
			t.Fatalf("no listening line within 5 s; logs: %s", logs.String())
		}
	}
	return addrs, logs, cancel, out
}

// waitHealthy polls addr's /healthz until it answers ok, failing after 5 s.
func waitHealthy(t *testing.T, addr string) {
	t.Helper()
	var status string
	for deadline := time.Now().Add(5 * time.Second); status != "ok"; time.Sleep(10 * time.Millisecond) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		var body struct{ Status string }
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		status = body.Status
		if time.Now().After(deadline) {
			t.Fatalf("healthz status %q after 5 s, want ok", status)
		}
	}
}

// healthzStatus is the status of a GET of addr's /healthz, 0 when the
// request fails.
func healthzStatus(addr string) int {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// healthChanges returns serve's health state changed lines, in order.
func healthChanges(t *testing.T, logs *syncBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(logs.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] == "health state changed" {
			out = append(out, rec)
		}
	}
	return out
}

// call sends one /v1 request from session, with key as its
// Idempotency-Key when not empty, and returns its status and code.
func call(t *testing.T, addr, method, path, session, key, body string) (status int, code string) {
	t.Helper()
	status, code, err := callErr(t.Context(), addr, method, path, session, key, body)
	if err != nil {
		t.Fatal(err)
	}
	return status, code
}

// callErr is call returning its error instead of failing the test, so a
// goroutine other than the test's may call it: t.Fatal must run on the
// test's goroutine.
func callErr(ctx context.Context, addr, method, path, session, key, body string) (status int, code string, err error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+authtest.SeedKey)
	req.Header.Set("Grelot-Version", "dev")
	req.Header.Set("Grelot-Session", session)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	var out struct{ Code string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Code, nil
}

// onDrainLine runs onDrain when the health drain line is logged, before
// passing it on.
type onDrainLine struct {
	slog.Handler
	onDrain func()
}

func (h onDrainLine) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "health state changed" && r.Level == slog.LevelWarn {
		h.onDrain()
	}
	return h.Handler.Handle(ctx, r)
}

func TestServe_aStopAnswersAnOpenWaitAndDrains(t *testing.T) {
	t.Parallel()
	// The sleep holds the drain line, so a wait released before BeginDrain
	// is answered while drained is still false. The line is logged once
	// the verdict is unhealthy, so the operations listener answers 503 then.
	var drained atomic.Bool
	var opsAddr atomic.Pointer[string]
	opsAtDrain := make(chan int, 1)
	addrs, logs, cancel, done := startServeWith(t, func(h slog.Handler) slog.Handler {
		return onDrainLine{Handler: h, onDrain: func() {
			opsAtDrain <- healthzStatus(*opsAddr.Load())
			time.Sleep(300 * time.Millisecond)
			drained.Store(true)
		}}
	})
	opsAddr.Store(&addrs.ops)
	const session = "test:session-a"
	register := `{"name":"reader","paths":["/workspace/grelot"],"description":"reading","host":"host-a/app",` +
		`"os":"linux","arch":"amd64","ttl":"1h","where":{"machine":"dmi:` + strings.Repeat("1", 32) +
		`","environment":"` + strings.Repeat("a", 32) + `","checkouts":[]}}`
	if status, code := call(t, addrs.api, http.MethodPost, "/v1/register", session, `"register-key-0001"`, register); status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, code)
	}
	waitHealthy(t, addrs.ops)
	type answer struct {
		err    error
		code   string
		status int
	}
	waited := make(chan answer, 1)
	ctx := t.Context()
	go func() {
		status, code, err := callErr(ctx, addrs.api, http.MethodPost, "/v1/events/wait", session, "", `{"wait":"55s"}`)
		waited <- answer{err: err, code: code, status: status}
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case a := <-waited:
		if a.err != nil {
			t.Fatalf("the open wait: %v", a.err)
		}
		if a.status != http.StatusServiceUnavailable || a.code != "shutting_down" {
			t.Errorf("the open wait = %d %s, want 503 shutting_down", a.status, a.code)
		}
		if !drained.Load() {
			t.Error("the open wait was answered before /healthz turned unhealthy")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the open wait got no answer within 5 s of the stop")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return within 5 s of the stop")
	}
	if got := <-opsAtDrain; got != http.StatusServiceUnavailable {
		t.Errorf("the operations listener's /healthz at the drain = %d, want 503", got)
	}
	if !strings.Contains(logs.String(), `"msg":"stopped"`) {
		t.Errorf("no stopped line; logs: %s", logs.String())
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("an ERROR line at a stop with a wait open; logs: %s", logs.String())
	}
}

func TestServe_postsLiveTheRetentionTheAccountsFileHolds(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	acc, err := accounts.Open(cfg.AccountsPath)
	if err != nil {
		t.Fatal(err)
	}
	set := accounts.DefaultSettings()
	set.Retention = 2 * time.Hour
	if err := acc.Put(t.Context(), set); err != nil {
		t.Fatal(err)
	}
	if err := acc.Close(); err != nil {
		t.Fatal(err)
	}
	addrs, _, _, _ := startServeOn(t, cfg, func(h slog.Handler) slog.Handler { return h })
	addr := addrs.api
	const session = "test:session-a"
	register := `{"name":"poster","paths":["/workspace/grelot"],"description":"posting","host":"host-a/app",` +
		`"os":"linux","arch":"amd64","ttl":"1h","where":{"machine":"dmi:` + strings.Repeat("1", 32) +
		`","environment":"` + strings.Repeat("a", 32) + `","checkouts":[]}}`
	if status, code := call(t, addr, http.MethodPost, "/v1/register", session, `"register-key-0001"`, register); status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, code)
	}
	before := time.Now()
	post := `{"to":{"path":"/workspace/grelot"},"subject":"hello","body":"a body",` +
		`"identity":{"name":"no change","description":"no change","paths":["no change"]}}`
	if status, code := call(t, addr, http.MethodPost, "/v1/posts", session, `"post-key-00000001"`, post); status != http.StatusCreated {
		t.Fatalf("post: %d %s", status, code)
	}
	after := time.Now()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/v1/threads", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+authtest.SeedKey)
	req.Header.Set("Grelot-Version", "dev")
	req.Header.Set("Grelot-Session", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Threads []struct {
			Expires time.Time `json:"expires_at"`
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Threads) != 1 {
		t.Fatalf("threads %+v, want the one path thread", got.Threads)
	}
	exp := got.Threads[0].Expires
	if lo, hi := before.Add(2*time.Hour), after.Add(2*time.Hour); exp.Before(lo) || exp.After(hi) {
		t.Errorf("the thread expires %v, want 2 h after the post, between %v and %v", exp, lo, hi)
	}
}

func TestServe_warnsThatTHREAD_TTLIsNoLongerRead(t *testing.T) {
	t.Setenv("THREAD_TTL", "24h")
	_, logs, _, _ := startServe(t)
	var warned int
	for line := range strings.Lines(logs.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["level"] == "WARN" && rec["msg"] == "THREAD_TTL is no longer read: set the history retention in Settings" {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("%d THREAD_TTL WARN lines, want 1; logs: %s", warned, logs.String())
	}
}

func TestServe_listenError(t *testing.T) {
	t.Parallel()
	addrs := testListeners(t)
	addrs.api = "256.0.0.1:0"
	err := serve(context.Background(), testConfig(t), addrs, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("serve on an invalid address returned nil")
	}
}

func TestRun_serveRefusesAMissingPublicURL(t *testing.T) {
	t.Setenv("PUBLIC_URL", "")
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var stderr bytes.Buffer
	if code := run([]string{"serve"}, io.Discard, &stderr); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "PUBLIC_URL") {
		t.Errorf("stderr %q does not name PUBLIC_URL", stderr.String())
	}
}

// The server's store asks the accounts file whether a key still
// authenticates, so a machine whose key was replaced keeps its sessions.
func TestBuild_aSessionKeepsItsIdentityUnderItsMachinesNewKey(t *testing.T) {
	au := authtest.New(t, nil)
	st, srv, err := build(testConfig(t), nil, au, slog.New(slog.DiscardHandler), obs.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Release(context.WithoutCancel(t.Context())); _ = st.Close() })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	register := func(key, idem string) int {
		body := `{"name":"alpha","paths":["/workspace/grelot"],"description":"testing keys","host":"host-a/app",` +
			`"os":"linux","arch":"amd64","ttl":"1h","where":{"machine":"dmi:` + strings.Repeat("1", 32) +
			`","environment":"` + strings.Repeat("a", 32) + `","checkouts":[]}}`
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/v1/register", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Grelot-Version", "dev")
		req.Header.Set("Grelot-Session", "kiro-cli:session-a")
		req.Header.Set("Idempotency-Key", idem)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := register(authtest.SeedKey, `"register-key-0001"`); got != http.StatusCreated {
		t.Fatalf("register under the first key: %d, want 201", got)
	}
	if err := au.RevokeKey(t.Context(), "seed"); err != nil {
		t.Fatal(err)
	}

	got := register(authtest.Key(t, au, "seed"), `"register-key-0002"`)

	if got != http.StatusOK {
		t.Errorf("the same session's register under the new key: %d, want 200 with its identity", got)
	}
}
