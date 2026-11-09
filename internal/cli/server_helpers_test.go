//go:build !windows

package cli

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/store"
)

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newServer(t *testing.T) *testServer {
	t.Helper()
	return newServerAt(t, "v1.1.0-dev.4", nil)
}

// newServerAt is newServer at version, answering any request refuse
// matches with the 400 a server newer than the key rule gave a client
// that sent no Idempotency-Key.
func newServerAt(t *testing.T, version string, refuse func(*http.Request) bool) *testServer {
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
	srv := server.New(&server.Options{
		Store: st, Logger: logger, Hub: hub, Version: version, Now: ts.clock.Now,
		Config: server.Config{Token: "test-token", PathRoot: protopath.DefaultRoot},
	})
	h := srv.Handler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts.mu.Lock()
		ts.reqs = append(ts.reqs, sent{Header: r.Header.Clone(), Method: r.Method, URL: r.URL.String(), Body: string(body)})
		ts.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := &teeWriter{ResponseWriter: w}
		if refuse != nil && refuse(r) {
			w.Header().Set("Grelot-Version", version)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(rec, `{"error":"this route requires an Idempotency-Key: send a new quoted random key with each request","code":"invalid"}`)
		} else {
			h.ServeHTTP(rec, r)
		}
		ts.mu.Lock()
		ts.answer = rec.body.String()
		ts.mu.Unlock()
	}))
	t.Cleanup(hs.Close)
	ts.URL = hs.URL
	return ts
}

// lastAnswer is the body of the last answer the server sent.
func (ts *testServer) lastAnswer(t *testing.T) string {
	t.Helper()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.answer
}

func goosArch() string { return runtime.GOOS + "/" + runtime.GOARCH }

// requests answers the requests received so far.
func (ts *testServer) requests() []sent {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]sent(nil), ts.reqs...)
}

// last answers the last request to path, by method and route.
func (ts *testServer) last(t *testing.T, method, route string) sent {
	t.Helper()
	reqs := ts.requests()
	for _, req := range slices.Backward(reqs) {
		if req.Method == method && strings.SplitN(req.URL, "?", 2)[0] == route {
			return req
		}
	}
	t.Fatalf("no %s %s request was sent", method, route)
	return sent{}
}

// call sends one request as session, with Grelot-Self when self, for a
// step the test takes outside the verbs under test.
func (ts *testServer) call(t *testing.T, session string, self bool, req *client.Request, out any) {
	t.Helper()
	a, err := client.NewAPI(&client.Config{Server: ts.URL, Token: "test-token"}, "v1.1.0", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.Session, a.Self = session, self
	if _, err := a.Do(t.Context(), req, out); err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.Path, err)
	}
}

// must runs args and fails the test unless it exits 0.
func (a *agent) must(args ...string) string {
	a.t.Helper()
	code, out, errOut := a.run(args...)
	if code != 0 {
		a.t.Fatalf("grelot %s exited %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

// isolateGit keeps the developer's git config out of the tests.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// gitClone creates a repository at dir whose origin is remote, none when
// remote is "", and answers dir with its symlinks resolved, as git prints
// toplevels.
func gitClone(t *testing.T, dir, remote string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	cmds := [][]string{{"init", "-q"}}
	if remote != "" {
		cmds = append(cmds, []string{"remote", "add", "origin", remote})
	}
	for _, args := range cmds {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func writeFile(t *testing.T, file string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
