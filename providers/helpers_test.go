package providers_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	// version is the client's release, the server's own unless a test
	// sets another.
	version string
	// fixtures is the provider whose recorded payloads fixture reads, the
	// shell's own when empty.
	fixtures string
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
		Version:     s.version,
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

// fixture reads a recorded hook input, testdata/golden/<provider>/<name>.json.
func (s *shell) fixture(name string) []byte {
	s.t.Helper()
	dir := s.fixtures
	if dir == "" {
		dir = s.provider
	}
	data, err := os.ReadFile(filepath.Join("testdata", "golden", dir, name+".json"))
	if err != nil {
		s.t.Fatal(err)
	}
	return data
}

// must runs one grelot command and fails the test unless it exits 0.
func (s *shell) must(stdin string, args ...string) string {
	s.t.Helper()
	got := s.run(stdin, args...)
	if got.Code != 0 {
		s.t.Fatalf("grelot %s exited %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), got.Code, got.Stdout, got.Stderr)
	}
	return got.Stdout
}

// register registers the shell's session as name, working in its own
// directory for two hours.
func (s *shell) register(name string) {
	s.t.Helper()
	s.must("", "register", name, "--paths", filepath.ToSlash(s.wd), "--desc", "testing the provider file", "--ttl", "2h")
}

// post sends one message to target from the shell's identity.
func (s *shell) post(target, body string) {
	s.t.Helper()
	s.must(body, "post", target, "Question: shape", "--name", "no change", "--paths", "no change", "--desc", "no change")
}

// wire is one command a hook file runs for a host event.
type wire struct {
	Event   string
	Command string
	Timeout int
}

// kiroHookFile is the hook file format of the Kiro agent server: a v1 file
// in ~/.kiro/hooks/.
type kiroHookFile struct {
	Version string `json:"version"`
	Hooks   []struct {
		Trigger string `json:"trigger"`
		Matcher string `json:"matcher"`
		Action  struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"action"`
		Timeout int `json:"timeout"`
	} `json:"hooks"`
}

// hookWiring reads a provider's hook file, <provider>/hooks.json, as the
// commands it runs per event.
func hookWiring(t *testing.T, name string) []wire {
	t.Helper()
	var f kiroHookFile
	if err := json.Unmarshal(mustRead(t, filepath.Join(name, "hooks.json")), &f); err != nil {
		t.Fatalf("%s/hooks.json: %v", name, err)
	}
	if f.Version != "v1" {
		t.Errorf("%s/hooks.json version %q, want v1", name, f.Version)
	}
	var got []wire
	for _, h := range f.Hooks {
		if h.Action.Type != "command" || h.Matcher != "" {
			t.Errorf("%s/hooks.json: %s hook is a %q action with matcher %q, want a command for every call", name, h.Trigger, h.Action.Type, h.Matcher)
		}
		got = append(got, wire{Event: h.Trigger, Command: h.Action.Command, Timeout: h.Timeout})
	}
	return got
}

// wantWiring is what a provider's hook file must run: grelot hook
// <provider> <role> under each role's event with a 10 s timeout, for every
// role the provider spells and no other.
func wantWiring(t *testing.T, name string) []wire {
	t.Helper()
	set, err := provider.LoadSet(providers.FS)
	if err != nil {
		t.Fatal(err)
	}
	p, err := set.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	var want []wire
	add := func(role, event string) {
		want = append(want, wire{Event: event, Command: "grelot hook " + name + " " + role, Timeout: 10})
	}
	hs := &p.Hooks
	if hs.SessionStart != nil {
		add("session_start", hs.SessionStart.Event)
	}
	if hs.Prompt != nil {
		add("prompt", hs.Prompt.Event)
	}
	if hs.Stop != nil {
		add("stop", hs.Stop.Event)
	}
	if hs.TurnEnd != nil {
		add("turn_end", hs.TurnEnd.Event)
	}
	if hs.SessionEnd != nil {
		add("session_end", hs.SessionEnd.Event)
	}
	if hs.SubagentStop != nil {
		add("subagent_stop", hs.SubagentStop.Event)
	}
	return want
}
