//go:build !windows

package providers_test

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/store"
)

func newServer(t *testing.T) *testServer {
	t.Helper()
	ts := &testServer{clock: &clock{now: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := server.NewHub()
	st, err := store.Open(filepath.Join(t.TempDir(), "grelot.db"), &store.Options{
		Logger: logger, PathRoot: protopath.DefaultRoot, Woke: hub.Woke, Now: ts.clock.Now,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := server.New(&server.Options{
		Store: st, Logger: logger, Hub: hub, Version: "v1.1.0", Now: ts.clock.Now,
		Config: server.Config{Token: "test-token", PathRoot: protopath.DefaultRoot},
	}).Handler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts.mu.Lock()
		ts.reqs = append(ts.reqs, request{Header: r.Header.Clone(), Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body)})
		ts.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(hs.Close)
	ts.URL = hs.URL
	return ts
}

// newShell is a shell of the named shipped provider whose environment
// holds vars, with a client config naming the server and host label box.
func newShell(t *testing.T, srv *testServer, name string, vars map[string]string) *shell {
	t.Helper()
	s := &shell{t: t, srv: srv, provider: name, vars: vars, home: t.TempDir(), config: t.TempDir(), wd: t.TempDir(), version: "v1.1.0"}
	file := client.ConfigFile(s.config)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, fmt.Appendf(nil, "server: %s\ntoken: test-token\nhost: box\n", srv.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

// silentServer is a server that never answers; hits counts the requests
// it has received.
func silentServer(t *testing.T) (url string, hits func() int) {
	t.Helper()
	release := make(chan struct{})
	var mu sync.Mutex
	n := 0
	hs := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(hs.Close)
	t.Cleanup(func() { close(release) })
	return hs.URL, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}
