package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

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
		Token: "test-token", PathRoot: server.DefaultPathRoot,
		StorePath: filepath.Join(t.TempDir(), "grelot.db"),
	}
}

func TestServe(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	marker := filepath.Join(t.TempDir(), ".healthy")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, testConfig(t), "127.0.0.1:0", marker, slog.New(slog.NewJSONHandler(&logs, nil)))
	}()

	var addr string
	for deadline := time.Now().Add(5 * time.Second); addr == ""; time.Sleep(10 * time.Millisecond) {
		if m := listeningAddr.FindStringSubmatch(logs.String()); m != nil {
			addr = m[1]
		} else if time.Now().After(deadline) {
			t.Fatalf("no listening line within 5 s; logs: %s", logs.String())
		}
	}

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

func TestServe_listenError(t *testing.T) {
	t.Parallel()
	err := serve(context.Background(), testConfig(t), "256.0.0.1:0", filepath.Join(t.TempDir(), ".healthy"), slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("serve on an invalid address returned nil")
	}
}

func TestRun_unknownCommand(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if code := run([]string{"bogus"}, &stderr); code != 2 {
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
	if code := run([]string{"serve"}, &stderr); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "GRELOT_TOKEN") {
		t.Errorf("stderr %q does not name GRELOT_TOKEN", stderr.String())
	}
}
