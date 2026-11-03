package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestHealthz(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newMux())
	t.Cleanup(srv.Close)

	t.Run("GET answers 200 with a JSON status", func(t *testing.T) {
		t.Parallel()
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || string(body) != `{"status":"ok"}` {
			t.Errorf("got %d %q, want 200 {\"status\":\"ok\"}", resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type %q, want application/json", ct)
		}
	})

	t.Run("POST is refused", func(t *testing.T) {
		t.Parallel()
		resp, err := http.Post(srv.URL+"/healthz", "text/plain", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("got %d, want 405", resp.StatusCode)
		}
	})

	t.Run("an unknown path is 404", func(t *testing.T) {
		t.Parallel()
		resp, err := http.Get(srv.URL + "/nope")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("got %d, want 404", resp.StatusCode)
		}
	})
}

func TestProbe(t *testing.T) {
	t.Parallel()

	t.Run("a healthy server passes", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(newMux())
		t.Cleanup(srv.Close)
		if err := probe(srv.URL+"/healthz", time.Second); err != nil {
			t.Errorf("probe: %v", err)
		}
	})

	t.Run("a non-200 answer fails", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)
		err := probe(srv.URL+"/healthz", time.Second)
		if err == nil || !strings.Contains(err.Error(), "503") {
			t.Errorf("probe error %v, want one naming status 503", err)
		}
	})

	t.Run("an unreachable server fails", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(newMux())
		url := srv.URL + "/healthz"
		srv.Close()
		if err := probe(url, time.Second); err == nil {
			t.Error("probe of a closed server returned nil")
		}
	})
}

func TestServe(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, "127.0.0.1:0", slog.New(slog.NewJSONHandler(&logs, nil))) }()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), `"msg":"listening"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no listening line within 5 s; logs: %s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
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
}

func TestServe_listen_error(t *testing.T) {
	t.Parallel()
	err := serve(context.Background(), "256.0.0.1:0", slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("serve on an invalid address returned nil")
	}
}

func TestRun_unknown_command(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if code := run([]string{"bogus"}, &stderr); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "bogus"`) {
		t.Errorf("stderr %q does not name the command", stderr.String())
	}
}
