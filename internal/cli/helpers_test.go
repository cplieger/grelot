package cli

import (
	"bytes"
	"context"
	"fmt"
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
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/proctree"
	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/store"
)

// The test providers: alpha keeps its session in ALPHA_ID and names its CLI
// process; beta keeps it in BETA_ID.
const (
	alphaFile = "name: alpha\ndetect: {env: [ALPHA_ID]}\nsession: {env: ALPHA_ID, process: [alphacli], process_shared: [--serve]}\n"
	betaFile  = "name: beta\ndetect: {env: [BETA_ID]}\nsession: {env: BETA_ID}\n"
)

// testMachine is a machine id as where.Machine answers one.
const testMachine = "dmi:0123456789abcdef0123456789abcdef"

// clock is the server's clock, which a test moves forward. Once live, it
// also runs with the wall clock, so a held wait can lapse.
type clock struct {
	mu   sync.Mutex
	now  time.Time
	live time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.live.IsZero() {
		return c.now.Add(time.Since(c.live))
	}
	return c.now
}

func (c *clock) run() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live = time.Now()
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// sent is one request the server received.
type sent struct {
	Header http.Header
	Method string
	URL    string
	Body   string
}

// testServer is grelot's server over a fresh store, recording every
// request it answers.
type testServer struct {
	clock *clock
	URL   string
	mu    sync.Mutex
	reqs  []sent
	// answer is the body of the last answer.
	answer string
}

func newServer(t *testing.T) *testServer {
	t.Helper()
	ts := &testServer{clock: &clock{now: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := server.NewHub()
	st, err := store.Open(filepath.Join(t.TempDir(), "grelot.db"), &store.Options{
		Logger: logger, PathRoot: store.DefaultPathRoot, Woke: hub.Woke, Now: ts.clock.Now,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(&server.Options{
		Store: st, Logger: logger, Hub: hub, Version: "v1.1.0-dev.4", Now: ts.clock.Now,
		Config: server.Config{Token: "test-token", PathRoot: store.DefaultPathRoot},
	})
	h := srv.Handler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts.mu.Lock()
		ts.reqs = append(ts.reqs, sent{Header: r.Header.Clone(), Method: r.Method, URL: r.URL.String(), Body: string(body)})
		ts.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := &teeWriter{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		ts.mu.Lock()
		ts.answer = rec.body.String()
		ts.mu.Unlock()
	}))
	t.Cleanup(hs.Close)
	ts.URL = hs.URL
	return ts
}

// teeWriter keeps a copy of the body it writes.
type teeWriter struct {
	http.ResponseWriter
	body bytes.Buffer
}

func (w *teeWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
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

// processes is a fake process table.
type processes map[int]proctree.Process

func (p processes) Lookup(pid int) (proctree.Process, error) {
	if v, ok := p[pid]; ok {
		return v, nil
	}
	return proctree.Process{}, os.ErrNotExist
}

// agentTable puts the client at pid 300 under the alpha CLI at pid 200.
var agentTable = processes{
	300: {Name: "grelot", Args: []string{"grelot"}, PID: 300, PPID: 250, Start: 30},
	250: {Name: "sh", Args: []string{"sh"}, PID: 250, PPID: 200, Start: 25},
	200: {Name: "alphacli", Args: []string{"alphacli"}, PID: 200, PPID: 1, Start: 20},
	1:   {Name: "init", Args: []string{"init"}, PID: 1, Start: 1},
}

// agent is one shell a test runs grelot commands in.
type agent struct {
	t      *testing.T
	env    *Env
	vars   map[string]string
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	wd     string
	// thread is the thread id of the agent's last post.
	thread string
}

// newAgent is a shell of provider alpha in session, with its own config
// dir whose client config adds extra to the server, token and host keys.
func newAgent(t *testing.T, ts *testServer, session, extra string) *agent {
	t.Helper()
	dir := t.TempDir()
	writeClientConfig(t, dir, fmt.Sprintf("server: %s\ntoken: test-token\nhost: box\n%s", ts.URL, extra))
	a := &agent{t: t, vars: map[string]string{"ALPHA_ID": session}, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, wd: dir}
	a.env = &Env{
		Stdin: strings.NewReader(""), Stdout: a.stdout, Stderr: a.stderr,
		Lookup: func(name string) (string, bool) {
			v, ok := a.vars[name]
			return v, ok
		},
		Hostname:    func() (string, error) { return "", os.ErrNotExist },
		Getwd:       func() (string, error) { return a.wd, nil },
		Machine:     func(context.Context) string { return testMachine },
		InContainer: func() bool { return false },
		Providers:   providerSet(t, "alpha", alphaFile, "beta", betaFile),
		Processes:   agentTable,
		ConfigDir:   dir,
		Version:     "v1.1.0",
		PID:         300,
	}
	if ts.clock != nil {
		a.env.Now = ts.clock.Now
	}
	return a
}

func writeClientConfig(t *testing.T, dir, content string) {
	t.Helper()
	file := client.ConfigFile(dir)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// run runs one grelot command and answers its exit code, stdout and
// stderr.
func (a *agent) run(args ...string) (code int, stdout, stderr string) {
	a.stdout.Reset()
	a.stderr.Reset()
	code = Run(a.t.Context(), a.env, args)
	return code, a.stdout.String(), a.stderr.String()
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

func providerSet(t *testing.T, kv ...string) *provider.Set {
	t.Helper()
	fsys := fstest.MapFS{}
	for i := 0; i+1 < len(kv); i += 2 {
		fsys[kv[i]+".yaml"] = &fstest.MapFile{Data: []byte(kv[i+1])}
	}
	s, err := provider.LoadSet(fsys)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	return s
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
