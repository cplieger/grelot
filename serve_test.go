//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

var listeningAddr = regexp.MustCompile(`"msg":"listening","addr":"([^"]+)"`)

func testConfig(t *testing.T) *server.Config {
	t.Helper()
	return &server.Config{
		Token: "test-token", PathRoot: protopath.DefaultRoot,
		StorePath: filepath.Join(t.TempDir(), "grelot.db"),
	}
}

func TestServe(t *testing.T) {
	addr, logs, cancel, done := startServe(t)

	t.Run("healthz answers ok once the sweeper has passed", func(t *testing.T) { waitHealthy(t, addr) })
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
	t.Run("grelot health probes LISTEN_ADDR", func(t *testing.T) {
		t.Setenv("LISTEN_ADDR", addr)
		var stderr bytes.Buffer
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
			addr, _, _, _ := startServe(t)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/v1/version", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer test-token")
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
		srv := server.New(&server.Options{Store: st, Logger: slog.New(slog.DiscardHandler), Config: *testConfig(t)})
		ts := httptest.NewServer(srv.Handler())
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
		if code := probeHealth(&stderr, "9737", time.Second); code != 1 || !strings.Contains(stderr.String(), `LISTEN_ADDR "9737"`) {
			t.Errorf("exit %d, stderr %q, want 1 naming LISTEN_ADDR and its value", code, stderr.String())
		}
	})
}

func TestListenAddr_theImageAndComposeExampleNameItsPort(t *testing.T) {
	for file, want := range map[string]string{"Dockerfile": "\nEXPOSE 9737\n", "compose.yaml": `- "9737:9737"`} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s does not contain %q", file, want)
		}
	}
}

// startServe runs serve on a free port until the returned cancel or the
// test's end, and returns its address, its logs and serve's result.
func startServe(t *testing.T) (addr string, logs *syncBuffer, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	return startServeWith(t, func(h slog.Handler) slog.Handler { return h })
}

// startServeWith is startServe logging through wrap's handler around the
// JSON one.
func startServeWith(t *testing.T, wrap func(slog.Handler) slog.Handler) (addr string, logs *syncBuffer, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	logs = &syncBuffer{}
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		out <- serve(ctx, cfg, "127.0.0.1:0", slog.New(wrap(slog.NewJSONHandler(logs, nil))))
		close(stopped)
	}()
	// Registered after the TempDir calls, so serve has returned and closed
	// the store before they are removed.
	t.Cleanup(func() { cancel(); <-stopped })
	for deadline := time.Now().Add(5 * time.Second); addr == ""; time.Sleep(10 * time.Millisecond) {
		if m := listeningAddr.FindStringSubmatch(logs.String()); m != nil {
			addr = m[1]
		} else if time.Now().After(deadline) {
			t.Fatalf("no listening line within 5 s; logs: %s", logs.String())
		}
	}
	return addr, logs, cancel, out
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
	req.Header.Set("Authorization", "Bearer test-token")
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
	// is answered while drained is still false.
	var drained atomic.Bool
	addr, logs, cancel, done := startServeWith(t, func(h slog.Handler) slog.Handler {
		return onDrainLine{Handler: h, onDrain: func() {
			time.Sleep(300 * time.Millisecond)
			drained.Store(true)
		}}
	})
	const session = "test:session-a"
	register := `{"name":"reader","paths":["/workspace/grelot"],"description":"reading","host":"host-a/app",` +
		`"os":"linux","arch":"amd64","ttl":"1h","where":{"machine":"dmi:` + strings.Repeat("1", 32) +
		`","environment":"` + strings.Repeat("a", 32) + `","checkouts":[]}}`
	if status, code := call(t, addr, http.MethodPost, "/v1/register", session, `"register-key-0001"`, register); status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, code)
	}
	waitHealthy(t, addr)
	type answer struct {
		err    error
		code   string
		status int
	}
	waited := make(chan answer, 1)
	ctx := t.Context()
	go func() {
		status, code, err := callErr(ctx, addr, http.MethodPost, "/v1/events/wait", session, "", `{"wait":"55s"}`)
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
	if !strings.Contains(logs.String(), `"msg":"stopped"`) {
		t.Errorf("no stopped line; logs: %s", logs.String())
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("an ERROR line at a stop with a wait open; logs: %s", logs.String())
	}
}

func TestServe_listenError(t *testing.T) {
	t.Parallel()
	err := serve(context.Background(), testConfig(t), "256.0.0.1:0", slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("serve on an invalid address returned nil")
	}
}

func TestRun_serveRefusesAMissingToken(t *testing.T) {
	t.Setenv("GRELOT_TOKEN", "")
	t.Setenv("GRELOT_TOKEN_FILE", "")
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var stderr bytes.Buffer
	if code := run([]string{"serve"}, io.Discard, &stderr); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "GRELOT_TOKEN") {
		t.Errorf("stderr %q does not name GRELOT_TOKEN", stderr.String())
	}
}
