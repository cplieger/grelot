//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/server"
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
	t.Parallel()
	marker := filepath.Join(t.TempDir(), ".healthy")
	addr, logs, cancel, done := startServe(t, marker)

	t.Run("healthz answers up once the sweeper has passed", func(t *testing.T) {
		var status string
		for deadline := time.Now().Add(5 * time.Second); status != "up"; time.Sleep(10 * time.Millisecond) {
			resp, err := http.Get("http://" + addr + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			var body struct{ Status string }
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			status = body.Status
			if time.Now().After(deadline) {
				t.Fatalf("healthz status %q after 5 s, want up", status)
			}
		}
	})

	t.Run("the health marker exists while serving", func(t *testing.T) {
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("marker: %v", err)
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
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker after stop: %v, want it removed", err)
	}
}

// startServe runs serve on a free port, its health marker at marker, until
// the returned cancel or the test's end, and returns its address, its logs
// and serve's result.
func startServe(t *testing.T, marker string) (addr string, logs *syncBuffer, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	logs = &syncBuffer{}
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		out <- serve(ctx, cfg, "127.0.0.1:0", marker, slog.New(slog.NewJSONHandler(logs, nil)))
		close(stopped)
	}()
	// Registered after the TempDir calls, so it runs before their removal:
	// Windows cannot delete a store file serve still holds open.
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
	addr, logs, cancel, done := startServe(t, filepath.Join(t.TempDir(), ".healthy"))
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
	err := serve(context.Background(), testConfig(t), "256.0.0.1:0", filepath.Join(t.TempDir(), ".healthy"), slog.New(slog.DiscardHandler))
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
