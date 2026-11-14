package cli

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/proctree"
	"github.com/cplieger/grelot/internal/provider"
)

// unreachableURL is a server that closes every connection before it
// answers. The test keeps its port: a port freed by closing the listener
// can be taken by any other listener, which then answers.
func unreachableURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return "http://" + l.Addr().String()
}

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
	// Key is the machine key the server admits.
	Key  string
	mu   sync.Mutex
	reqs []sent
	// answer is the body of the last answer.
	answer string
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

// processes is a fake process table.
type processes map[int]proctree.Process

// agentTable puts the client at pid 300 under the alpha CLI at pid 200.
var agentTable = processes{
	300: {Name: "grelot", Args: []string{"grelot"}, PID: 300, PPID: 250, Start: 30},
	250: {Name: "sh", Args: []string{"sh"}, PID: 250, PPID: 200, Start: 25},
	200: {Name: "alphacli", Args: []string{"alphacli"}, PID: 200, PPID: 1, Start: 20},
	1:   {Name: "init", Args: []string{"init"}, PID: 1, Start: 1},
}

// agent is one shell a test runs grelot commands in.
type agent struct {
	t *testing.T
	// key is the machine key of the server newAgent made it for.
	key    string
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
	key := cmp.Or(ts.Key, "test-key") // a fake server reads no key
	writeClientConfig(t, dir, fmt.Sprintf("server: %s\ntoken: %s\nhost: box\n%s", ts.URL, key, extra))
	a := &agent{t: t, key: key, vars: map[string]string{"ALPHA_ID": session}, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, wd: dir}
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

func (p processes) Lookup(pid int) (proctree.Process, error) {
	if v, ok := p[pid]; ok {
		return v, nil
	}
	return proctree.Process{}, os.ErrNotExist
}
