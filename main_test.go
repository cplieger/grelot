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
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
		Token: "test-token", PathRoot: server.DefaultPathRoot,
		StorePath: filepath.Join(t.TempDir(), "grelot.db"),
	}
}

func TestServe(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, testConfig(t), "127.0.0.1:0", slog.New(slog.NewJSONHandler(&logs, nil)))
	}()

	var addr string
	for deadline := time.Now().Add(5 * time.Second); addr == ""; time.Sleep(10 * time.Millisecond) {
		if m := listeningAddr.FindStringSubmatch(logs.String()); m != nil {
			addr = m[1]
		} else if time.Now().After(deadline) {
			t.Fatalf("no listening line within 5 s; logs: %s", logs.String())
		}
	}

	t.Run("healthz answers ok once the sweeper has passed", func(t *testing.T) {
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
	var stderr bytes.Buffer
	if code := probeHealth(&stderr, addr, time.Second); code != 1 || !strings.Contains(stderr.String(), "unhealthy") {
		t.Errorf("grelot health after stop: exit %d, stderr %q, want 1 and unhealthy", code, stderr.String())
	}
}

func TestProbeHealth(t *testing.T) {
	t.Parallel()
	t.Run("a stale sweep with a live listener fails", func(t *testing.T) {
		t.Parallel()
		var at atomic.Int64
		at.Store(time.Now().UnixNano())
		now := func() time.Time { return time.Unix(0, at.Load()) }
		st, err := store.Open(filepath.Join(t.TempDir(), "grelot.db"), &store.Options{Now: now})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		srv := server.New(&server.Options{
			Store: st, Logger: slog.New(slog.DiscardHandler), Now: now, Config: *testConfig(t),
		})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		addr := ts.Listener.Addr().String()
		srv.Beat(t.Context())
		var stderr bytes.Buffer
		if code := probeHealth(&stderr, addr, time.Second); code != 0 {
			t.Fatalf("fresh sweep: exit %d, want 0; stderr %q", code, stderr.String())
		}
		at.Add(int64(4*store.SweepEvery + time.Second))
		if code := probeHealth(&stderr, addr, time.Second); code != 1 || !strings.Contains(stderr.String(), "503") {
			t.Errorf("stale sweep: exit %d, stderr %q, want 1 naming the 503", code, stderr.String())
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
		if code := probeHealth(&stderr, "8080", time.Second); code != 1 || !strings.Contains(stderr.String(), "8080") {
			t.Errorf("exit %d, stderr %q, want 1 naming the address", code, stderr.String())
		}
	})
}

// startServe runs serve on a free port until the returned cancel, and
// returns its address, its logs and serve's result.
func startServe(t *testing.T) (addr string, logs *syncBuffer, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	logs = &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out := make(chan error, 1)
	go func() {
		out <- serve(ctx, testConfig(t), "127.0.0.1:0", slog.New(slog.NewJSONHandler(logs, nil)))
	}()
	for deadline := time.Now().Add(5 * time.Second); addr == ""; time.Sleep(10 * time.Millisecond) {
		if m := listeningAddr.FindStringSubmatch(logs.String()); m != nil {
			addr = m[1]
		} else if time.Now().After(deadline) {
			t.Fatalf("no listening line within 5 s; logs: %s", logs.String())
		}
	}
	return addr, logs, cancel, out
}

// call sends one /v1 request from session and returns its status and code.
func call(t *testing.T, addr, method, path, session, body string) (status int, code string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Grelot-Version", "dev")
	req.Header.Set("Grelot-Session", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out struct{ Code string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Code
}

func TestServe_aStopAnswersAnOpenWaitAndDrains(t *testing.T) {
	t.Parallel()
	addr, logs, cancel, done := startServe(t)
	const session = "test:session-a"
	register := `{"name":"reader","paths":["/workspace/grelot"],"description":"reading","host":"host-a/app",` +
		`"os":"linux","arch":"amd64","ttl":"1h","where":{"machine":"dmi:` + strings.Repeat("1", 32) +
		`","environment":"` + strings.Repeat("a", 32) + `","checkouts":[]}}`
	if status, code := call(t, addr, http.MethodPost, "/v1/register", session, register); status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, code)
	}
	type answer struct {
		code   string
		status int
	}
	waited := make(chan answer, 1)
	go func() {
		status, code := call(t, addr, http.MethodPost, "/v1/events/wait", session, `{"wait":"55s"}`)
		waited <- answer{code: code, status: status}
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case a := <-waited:
		if a.status != http.StatusServiceUnavailable || a.code != "shutting_down" {
			t.Errorf("the open wait = %d %s, want 503 shutting_down", a.status, a.code)
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

func TestRun_unknownCommand(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if code := run([]string{"bogus"}, io.Discard, &stderr); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "bogus"`) {
		t.Errorf("stderr %q does not name the command", stderr.String())
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

func TestRun_clientLoadsTheShippedProvidersUnderTheUsersOwn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("KIRO_SESSION_ID", "")
	t.Setenv("GRELOT_TOKEN_FILE", "")
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(configDir, "grelot")
	if err := os.MkdirAll(filepath.Join(dir, "providers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("server: http://127.0.0.1:1\ntoken: test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	me := func(t *testing.T) string {
		t.Helper()
		var stderr bytes.Buffer
		if code := run([]string{"--provider", "kiro-cli", "me"}, io.Discard, &stderr); code != 1 {
			t.Errorf("exit %d, want 1; stderr %q", code, stderr.String())
		}
		return stderr.String()
	}
	t.Run("a shipped file with no file on disk", func(t *testing.T) {
		if got := me(t); !strings.Contains(got, "KIRO_SESSION_ID") {
			t.Errorf("stderr %q, want the shipped kiro-cli file's session variable", got)
		}
	})
	t.Run("a user file replaces the shipped one", func(t *testing.T) {
		file := filepath.Join(dir, "providers", "kiro-cli.yaml")
		if err := os.WriteFile(file, []byte("name: kiro-cli\nsession: {env: USER_FILE_SESSION}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := me(t); !strings.Contains(got, "USER_FILE_SESSION") || strings.Contains(got, "KIRO_SESSION_ID") {
			t.Errorf("stderr %q, want the user file's session variable only", got)
		}
	})
}

func TestRun_clientVerbs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cases := []struct {
		name     string
		args     []string
		wantCode int
		stdout   string
		stderr   string
	}{
		{name: "help prints the manual", args: []string{"help"}, stdout: "### grelot register"},
		{name: "a verb's help prints its section", args: []string{"register", "--help"}, stdout: "### grelot register"},
		{name: "self before the verb", args: []string{"--self", "help"}, stdout: "## Commands"},
		{name: "provider before the verb", args: []string{"--provider", "any", "help"}, stdout: "## Commands"},
		{name: "provider with no name", args: []string{"--provider"}, wantCode: 2, stderr: "--provider needs a provider name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(c.args, &stdout, &stderr); code != c.wantCode {
				t.Errorf("exit %d, want %d; stderr %q", code, c.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), c.stdout) {
				t.Errorf("stdout does not hold %q:\n%s", c.stdout, stdout.String())
			}
			if !strings.Contains(stderr.String(), c.stderr) {
				t.Errorf("stderr %q does not hold %q", stderr.String(), c.stderr)
			}
		})
	}
}
