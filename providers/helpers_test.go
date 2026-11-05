package providers_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/cli"
	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/proctree"
	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/providers"
)

// testMachine is a machine id as the client reports one.
const testMachine = "dmi:0123456789abcdef0123456789abcdef"

// clock is the server's and the hooks' clock, which a test moves forward.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// request is one request the server received.
type request struct {
	Header http.Header
	Method string
	Path   string
	Query  string
	Body   string
}

// testServer is grelot's server over a fresh store, recording every request.
type testServer struct {
	clock *clock
	URL   string
	mu    sync.Mutex
	reqs  []request
}

func newServer(t *testing.T) *testServer {
	t.Helper()
	ts := &testServer{clock: &clock{now: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := server.NewHub()
	st, err := store.Open(filepath.Join(t.TempDir(), "grelot.db"), store.Options{
		Logger: logger, PathRoot: store.DefaultPathRoot, Woke: hub.Woke, Now: ts.clock.Now,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := server.New(&server.Options{
		Store: st, Logger: logger, Hub: hub, Version: "v1.1.0", Now: ts.clock.Now,
		Config: server.Config{Token: "test-token", PathRoot: store.DefaultPathRoot},
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

func (ts *testServer) count() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.reqs)
}

func (ts *testServer) since(n int) []request {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]request(nil), ts.reqs[n:]...)
}

// shell is one agent shell of a shipped provider: its environment, its own
// client config and the shipped provider set.
type shell struct {
	t        *testing.T
	srv      *testServer
	vars     map[string]string
	procs    proctree.Table
	provider string
	home     string
	config   string
	wd       string
}

// newShell is a shell of the named shipped provider whose environment
// holds vars, with a client config naming the server and host label box.
func newShell(t *testing.T, srv *testServer, name string, vars map[string]string) *shell {
	t.Helper()
	s := &shell{t: t, srv: srv, provider: name, vars: vars, home: t.TempDir(), config: t.TempDir(), wd: t.TempDir()}
	file := client.ConfigFile(s.config)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, fmt.Appendf(nil, "server: %s\ntoken: test-token\nhost: box\n", srv.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

// outcome is what one grelot command did: its exit code, its output, and
// the requests the server received while it ran.
type outcome struct {
	Stdout   string
	Stderr   string
	Requests []request
	Code     int
}

// run runs one grelot command in the shell with stdin as its input.
func (s *shell) run(stdin string, args ...string) outcome {
	s.t.Helper()
	set, err := provider.LoadSet(providers.FS)
	if err != nil {
		s.t.Fatalf("LoadSet: %v", err)
	}
	var stdout, stderr bytes.Buffer
	before := s.srv.count()
	code := cli.Run(s.t.Context(), &cli.Env{
		Stdin: strings.NewReader(stdin), Stdout: &stdout, Stderr: &stderr,
		Lookup: func(name string) (string, bool) {
			v, ok := s.vars[name]
			return v, ok
		},
		Hostname:    func() (string, error) { return "", os.ErrNotExist },
		Getwd:       func() (string, error) { return s.wd, nil },
		Home:        func() (string, error) { return s.home, nil },
		Now:         s.srv.clock.Now,
		Machine:     func(context.Context) string { return testMachine },
		InContainer: func() bool { return false },
		Providers:   set,
		Processes:   s.procs,
		ConfigDir:   s.config,
		Version:     "v1.1.0",
		PID:         300,
	}, args)
	return outcome{Code: code, Stdout: stdout.String(), Stderr: stderr.String(), Requests: s.srv.since(before)}
}

// hook runs grelot hook <provider> <role> with in as its stdin, as the
// provider's hook file runs it.
func (s *shell) hook(role string, in []byte) outcome {
	s.t.Helper()
	return s.run(string(in), "hook", s.provider, role)
}

// fixture reads a recorded hook input of the shell's provider,
// testdata/golden/<provider>/<name>.json.
func (s *shell) fixture(name string) []byte {
	s.t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", s.provider, name+".json"))
	if err != nil {
		s.t.Fatal(err)
	}
	return data
}
