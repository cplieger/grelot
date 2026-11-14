package providers_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/cli"
	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/proctree"
	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/providers"
)

// testMachine is a machine id as the client reports one.
const testMachine = "dmi:0123456789abcdef0123456789abcdef"

// A session with no identity gets registerStart at session start,
// registerPrompt at each turn start and registerHold at its first stop.
const (
	registerStart  = "grelot: not registered. Register so other agents can find and message you: run `grelot help`, then `grelot register` as it says. If registering is refused, blocked or against the user's instructions (a sandbox, an approval reviewer, an instruction not to run commands), finish the user's task and tell the user; grelot holds one turn end for it, never more."
	registerPrompt = "grelot: not registered. Run `grelot help`, then `grelot register`. If refused, blocked or against the user's instructions, finish the task and tell the user."
	registerHold   = "grelot: not registered. Register now if you can: run `grelot help`, then `grelot register` as it says. If registering is refused, blocked or against the user's instructions (a sandbox, an approval reviewer, an instruction not to run commands), finish the user's task and tell the user; grelot will not hold this session for it again."
)

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
	// Key is the machine key the server admits.
	Key  string
	mu   sync.Mutex
	reqs []request
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
	// hookOmits are the variables the host leaves out of its hook
	// processes, though its agent's shell commands carry them.
	hookOmits []string
	inHook    bool
}

// useServer points the shell's client config at url instead of its test
// server.
func (s *shell) useServer(url string) {
	s.t.Helper()
	if err := os.WriteFile(client.ConfigFile(s.config), fmt.Appendf(nil, "server: %s\ntoken: test-token\nhost: box\n", url), 0o600); err != nil {
		s.t.Fatal(err)
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

// under is a process table putting the client, pid 300, under a shell under
// the host process name at pid 200, started at 20.
func under(name string, args ...string) processes {
	return processes{
		300: {Name: "grelot", Args: []string{"grelot"}, PID: 300, PPID: 250, Start: 30},
		250: {Name: "bash", Args: []string{"bash"}, PID: 250, PPID: 200, Start: 25},
		200: {Name: name, Args: append([]string{name}, args...), PID: 200, PPID: 1, Start: 20},
		1:   {Name: "init", Args: []string{"init"}, PID: 1, Start: 1},
	}
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
			if s.inHook && slices.Contains(s.hookOmits, name) {
				return "", false
			}
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
	s.inHook = true
	defer func() { s.inHook = false }()
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

// settingsHookFile is the hook file format Claude Code's settings and
// plugins share: command groups per event name.
type settingsHookFile struct {
	Hooks map[string][]struct {
		Matcher *string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// flatHook is one command entry of a hook file that lists entries straight
// under each event: Copilot CLI's and Cursor's (a version 1 file), and
// Antigravity's named groups. timeout, in seconds, is the key all three
// read (Copilot CLI's alias of timeoutSec).
type flatHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// hookWiring reads a provider's hook file, <provider>/hooks.json, as the
// commands it runs per event, in event order then file order.
func hookWiring(t *testing.T, name string) []wire {
	t.Helper()
	return parseWiring(t, name, mustRead(t, filepath.Join(name, "hooks.json")))
}

// parseWiring reads data, the named provider's hook file, as hookWiring
// does.
func parseWiring(t *testing.T, name string, data []byte) []wire {
	t.Helper()
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data, &head); err != nil {
		t.Fatalf("%s/hooks.json: %v", name, err)
	}
	switch v := string(head["version"]); {
	case strings.HasPrefix(v, `"`):
		return kiroWiring(t, name, data)
	case v == "1":
		var f struct {
			Hooks   map[string]json.RawMessage `json:"hooks"`
			Version int                        `json:"version"`
		}
		strictJSON(t, name, data, &f)
		return flatWiring(t, name, f.Hooks)
	case v != "":
		t.Fatalf("%s/hooks.json: version %s, want v1 or 1", name, v)
	case head["hooks"] == nil:
		// Antigravity's named groups. Its schema example lists SessionStart
		// handlers flat and nests only tool events in matcher groups; the
		// matcher is ignored for SessionStart and Stop
		// (https://antigravity.google/docs/hooks, "Schema and file format").
		var groups map[string]map[string]json.RawMessage
		strictJSON(t, name, data, &groups)
		var got []wire
		for _, group := range slices.Sorted(maps.Keys(groups)) {
			got = append(got, flatWiring(t, name, groups[group])...)
		}
		return got
	}
	var f settingsHookFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("%s/hooks.json: %v", name, err)
	}
	var got []wire
	for _, event := range slices.Sorted(maps.Keys(f.Hooks)) {
		for _, g := range f.Hooks[event] {
			if g.Matcher != nil {
				t.Errorf("%s/hooks.json: a %s group has matcher %q, want none, so every %s runs it", name, event, *g.Matcher, event)
			}
			for _, h := range g.Hooks {
				if h.Type != "command" {
					t.Errorf("%s/hooks.json: a %s hook has type %q, want command", name, event, h.Type)
				}
				got = append(got, wire{Event: event, Command: h.Command, Timeout: h.Timeout})
			}
		}
	}
	return got
}

// flatWiring reads events, each a list of flatHook entries. A key a host
// does not read, such as a matcher or a misspelt timeout, fails the test.
func flatWiring(t *testing.T, name string, events map[string]json.RawMessage) []wire {
	t.Helper()
	var got []wire
	for _, event := range slices.Sorted(maps.Keys(events)) {
		var hooks []flatHook
		strictJSON(t, name, events[event], &hooks)
		for _, h := range hooks {
			if h.Type != "command" {
				t.Errorf("%s/hooks.json: a %s hook has type %q, want command", name, event, h.Type)
			}
			got = append(got, wire{Event: event, Command: h.Command, Timeout: h.Timeout})
		}
	}
	return got
}

// strictJSON decodes data into v, failing the test on a key v has no field
// for.
func strictJSON(t *testing.T, name string, data []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s/hooks.json: %v", name, err)
	}
}

func kiroWiring(t *testing.T, name string, data []byte) []wire {
	t.Helper()
	var f kiroHookFile
	if err := json.Unmarshal(data, &f); err != nil {
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

// sameWiring reports whether two wirings run the same commands for the same
// events, whatever their order.
func sameWiring(got, want []wire) bool {
	order := func(a, b wire) int {
		return cmp.Or(strings.Compare(a.Event, b.Event), strings.Compare(a.Command, b.Command), a.Timeout-b.Timeout)
	}
	return slices.Equal(slices.SortedFunc(slices.Values(got), order), slices.SortedFunc(slices.Values(want), order))
}

// wantWiring is what a provider's hook file must run: grelot hook
// <provider> <role> under each role's event, for every role the provider
// spells and no other, with the timeout timeouts names for the event, or
// 30 s.
func wantWiring(t *testing.T, name string, timeouts map[string]int) []wire {
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
		want = append(want, wire{Event: event, Command: "grelot hook " + name + " " + role, Timeout: cmp.Or(timeouts[event], 30)})
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
