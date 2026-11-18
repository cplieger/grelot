//go:build !windows

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/client"
)

// guardFile is alpha with a guard role on its write tool, reading the
// written file from tool_input.path and a patch from tool_input.patch, at
// limit bytes of context (0 for none).
func guardFile(limit int) string {
	return fmt.Sprintf(`name: alpha
detect: {env: [ALPHA_ID]}
session: {env: ALPHA_ID, process: [alphacli], process_shared: [--serve]}
hooks:
  guard:
    event: Write
    when: {tool_name: [write]}
    budget: 1s
    context_limit: %d
    stdin: {agent_id: agent_id, paths: [tool_input.path], patch: tool_input.patch, cwd: cwd}
    outputs: {context: {text: "{{.Context}}"}}
`, limit)
}

// The lines the guard prints for @holder-dev's grants, reserved for an hour
// from the server's 09:00 UTC start.
const (
	guardInternal = "grelot: @holder-dev holds /workspace/toolbelt/internal until 2026-11-05 10:00 UTC (splitting internal); your write goes ahead. " +
		"Coordinate with grelot post '@holder-dev' or reserve it after them."
	guardReadme = "grelot: @holder-dev holds /workspace/toolbelt/README.md until 2026-11-05 10:00 UTC (rewriting the docs); your write goes ahead. " +
		"Coordinate with grelot post '@holder-dev' or reserve it after them."
)

// guardSetup is a writer of alpha session s-2 registered on a clone of
// toolbelt, beside @holder-dev, which holds the clone's internal folder and
// README.md for an hour.
type guardSetup struct {
	ts     *testServer
	writer *agent
	clone  string
}

func newGuardSetup(t *testing.T, limit int) *guardSetup {
	t.Helper()
	isolateGit(t)
	ts := newServer(t)
	clone := gitClone(t, filepath.Join(t.TempDir(), "src", "toolbelt"), toolbeltRemote)
	holder := newAgent(t, ts, "s-1", "")
	holder.must("register", "holder-dev", "--paths", clone, "--desc", "holding files", "--ttl", "2h")
	holder.must("reserve", filepath.Join(clone, "internal"), "--ttl=1h", "--reason", "splitting internal")
	holder.must("reserve", filepath.Join(clone, "README.md"), "--ttl=1h", "--reason", "rewriting the docs")
	writer := newAgent(t, ts, "s-2", "")
	writer.wd = clone
	writer.must("register", "writer-dev", "--paths", clone, "--desc", "writing files", "--ttl", "2h")
	writer.env.Providers = providerSet(t, "alpha", guardFile(limit), "beta", betaFile)
	return &guardSetup{ts: ts, writer: writer, clone: clone}
}

// guard runs the writer's guard on a write tool call with input as its
// tool_input and cwd, and answers the holders queries it sent.
func (g *guardSetup) guard(t *testing.T, input map[string]any, cwd string) (code int, stdout, stderr string, asked [][]string) {
	t.Helper()
	before := len(g.ts.requests())
	stdin, err := json.Marshal(map[string]any{"tool_name": "write", "tool_input": input, "cwd": cwd})
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = g.writer.hook("guard", string(stdin))
	for _, r := range g.ts.requests()[before:] {
		u, err := url.Parse(r.URL)
		if err != nil {
			t.Fatal(err)
		}
		if r.Method != http.MethodGet || u.Path != "/v1/holders" {
			t.Errorf("the guard sent %s %s, want only GET /v1/holders", r.Method, u.Path)
			continue
		}
		asked = append(asked, u.Query()["path"])
	}
	return code, stdout, stderr, asked
}

// wantNamed fails unless the guard names @holder-dev's grant on a write in
// the internal folder, so a silence the test checks next is the guard's
// choice, not a guard that never speaks.
func (g *guardSetup) wantNamed(t *testing.T) {
	t.Helper()
	if code, out, _, _ := g.guard(t, map[string]any{"path": filepath.Join(g.clone, "internal", "a.go")}, g.clone); code != 0 || out != guardInternal+"\n" {
		t.Fatalf("the write of a held file: exit %d, stdout %q; want %q", code, out, guardInternal+"\n")
	}
}

func patchOf(lines ...string) string {
	return "*** Begin Patch\n" + strings.Join(lines, "\n") + "\n*** End Patch\n"
}

// Two written files under one reserved folder get one line for the grant.
func TestHookGuard_oneLinePerGrant(t *testing.T) {
	g := newGuardSetup(t, 0)
	patch := patchOf("*** Update File: internal/a.go", "@@", "-a", "+b", "*** Add File: internal/b.go", "+b")
	code, out, errOut, asked := g.guard(t, map[string]any{"patch": patch}, g.clone)
	if code != 0 || out != guardInternal+"\n" || errOut != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want 0 and %q", code, out, errOut, guardInternal+"\n")
	}
	want := [][]string{{"/workspace/toolbelt/internal/a.go", "/workspace/toolbelt/internal/b.go"}}
	if !slices.EqualFunc(asked, want, slices.Equal) {
		t.Errorf("asked %v, want %v", asked, want)
	}
}

// A queue entry on any written file under the grant is named, whichever
// file the write lists first.
func TestHookGuard_namesAQueueOnALaterFileOfTheGrant(t *testing.T) {
	g := newGuardSetup(t, 0)
	out := g.writer.must("reserve", filepath.Join(g.clone, "internal", "b.go"), "--ttl=30m", "--reason", "next", "--queue")
	m := regexp.MustCompile(`reservation (\d+) queued`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("reserve --queue printed no queued id:\n%s", out)
	}
	want := "grelot: @holder-dev holds /workspace/toolbelt/internal until 2026-11-05 10:00 UTC (splitting internal); your write goes ahead. " +
		"You are queued for /workspace/toolbelt/internal/b.go (reservation " + m[1] + "): coordinate with grelot post '@holder-dev' or wait for the grant, which reaches you as mail.\n"
	patch := patchOf("*** Update File: internal/a.go", "@@", "-a", "+b", "*** Update File: internal/b.go", "@@", "-a", "+b")
	if code, got, _, _ := g.guard(t, map[string]any{"patch": patch}, g.clone); code != 0 || got != want {
		t.Errorf("exit %d, stdout %q; want %q", code, got, want)
	}
}

// A relative stdin path is under the hook's working directory.
func TestHookGuard_aRelativeStdinPathIsUnderTheWorkingDirectory(t *testing.T) {
	g := newGuardSetup(t, 0)
	t.Chdir(t.TempDir())
	code, out, _, asked := g.guard(t, map[string]any{"path": filepath.Join("internal", "a.go")}, "")
	if code != 0 || out != guardInternal+"\n" {
		t.Errorf("exit %d, stdout %q; want %q", code, out, guardInternal+"\n")
	}
	if want := [][]string{{"/workspace/toolbelt/internal/a.go"}}; !slices.EqualFunc(asked, want, slices.Equal) {
		t.Errorf("asked %v, want %v", asked, want)
	}
}

// A relative patch path is under the hook's cwd, never the hook process's
// working directory: with no cwd it is not checked.
func TestHookGuard_aRelativePatchPathNeedsACwd(t *testing.T) {
	g := newGuardSetup(t, 0)
	g.wantNamed(t)
	t.Chdir(g.clone)
	code, out, errOut, asked := g.guard(t, map[string]any{"patch": patchOf("*** Delete File: internal/a.go")}, "")
	if code != 0 || out != "" || errOut != "" || len(asked) != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q, asked %v; want 0, no output and no request", code, out, errOut, asked)
	}
	abs := filepath.Join(g.clone, "internal", "a.go")
	if code, out, _, _ = g.guard(t, map[string]any{"patch": patchOf("*** Delete File: " + abs)}, ""); code != 0 || out != guardInternal+"\n" {
		t.Errorf("an absolute patch path with no cwd: exit %d, stdout %q; want %q", code, out, guardInternal+"\n")
	}
}

// The guard keeps the most lines whose rendered text fits the limit, then
// a line counting the grants it left out; under even one line and that
// count, a line counting them all.
func TestHookGuard_contextLimit(t *testing.T) {
	both := guardInternal + "\n" + guardReadme + "\n"
	first := guardInternal + "\n" + "grelot: other agents hold more reservations over files in this write, 1 not shown; grelot reservations lists them.\n"
	none := "grelot: other agents hold reservations over files in this write, 2 not shown; grelot reservations lists them.\n"
	for _, c := range []struct {
		name, want string
		limit      int
	}{
		{"no_limit", both, 0},
		{"both_at_their_size", both, len(both)},
		{"a_byte_under_both", first, len(both) - 1},
		{"one_line_at_its_size", first, len(first)},
		{"under_one_line", none, len(first) - 1},
		{"under_the_count", none, 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newGuardSetup(t, c.limit)
			patch := patchOf("*** Update File: internal/a.go", "@@", "-a", "+b", "*** Update File: README.md", "@@", "-a", "+b")
			if code, out, _, _ := g.guard(t, map[string]any{"patch": patch}, g.clone); code != 0 || out != c.want {
				t.Errorf("limit %d: exit %d, stdout %q; want %q", c.limit, code, out, c.want)
			}
		})
	}
}

// One request names at most the server's 256 paths: the first ones the
// call writes.
func TestHookGuard_asksAboutTheFirstPathsUpToTheServersCap(t *testing.T) {
	g := newGuardSetup(t, 0)
	var lines, want []string
	for i := range 300 {
		lines = append(lines, fmt.Sprintf("*** Add File: f%03d.go", i), "+x")
		if i < 256 {
			want = append(want, fmt.Sprintf("/workspace/toolbelt/f%03d.go", i))
		}
	}
	code, out, _, asked := g.guard(t, map[string]any{"patch": patchOf(lines...)}, g.clone)
	if code != 0 || out != "" || len(asked) != 1 || !slices.Equal(asked[0], want) {
		t.Errorf("exit %d, stdout %q, asked %d requests; want 0, no output and one request for the first 256 files", code, out, len(asked))
	}
}

// The budget bounds the guard's whole wait, git's lookup of the written
// path included: a git that hangs ends at the budget, and nothing is asked.
func TestHookGuard_silentWhenGitPassesTheBudget(t *testing.T) {
	g := newGuardSetup(t, 0)
	g.wantNamed(t)
	bin := t.TempDir()
	syscall.ForkLock.RLock()
	err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec sleep 10\n"), 0o755)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	start := time.Now()
	code, out, errOut, asked := g.guard(t, map[string]any{"path": filepath.Join(g.clone, "internal", "a.go")}, g.clone)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the guard returned after %s, want within its 1s budget and a second", took)
	}
	if code != 0 || out != "" || errOut != "" || len(asked) != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q, asked %v; want 0, no output and no request", code, out, errOut, asked)
	}
}

// Where the other roles print the could-not-check line, the guard says
// nothing and asks nothing.
func TestHookGuard_silentWithNoClientConfig(t *testing.T) {
	g := newGuardSetup(t, 0)
	g.wantNamed(t)
	if err := os.Remove(client.ConfigFile(g.writer.env.ConfigDir)); err != nil {
		t.Fatal(err)
	}
	code, out, errOut, asked := g.guard(t, map[string]any{"path": filepath.Join(g.clone, "internal", "a.go")}, g.clone)
	if code != 0 || out != "" || errOut != "" || len(asked) != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q, asked %v; want 0, no output and no request", code, out, errOut, asked)
	}
}

// What the server answers is data: a holder, path or reason carrying
// terminal escapes, bidi controls or line breaks stays on its one line.
func TestHookGuard_answerIsPrintedOnOneSafeLine(t *testing.T) {
	isolateGit(t)
	clone := gitClone(t, filepath.Join(t.TempDir(), "src", "toolbelt"), toolbeltRemote)
	var mu sync.Mutex
	var got []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"holders":[{"held":{"id":1,"holder":"evil\u001b[2Jdev",`+
			`"path":"/workspace/toolbelt/a.go\r\nfake: line","state":"held","reason":"red \u001b[31mtext\u202e here\nnext",`+
			`"expires_at":"2026-11-05T10:00:00Z"}}]}`)
	}))
	t.Cleanup(fake.Close)
	writer := newAgent(t, &testServer{URL: fake.URL}, "s-2", "")
	writer.env.Providers = providerSet(t, "alpha", guardFile(0), "beta", betaFile)
	code, out, _ := writer.hook("guard", `{"tool_name":"write","tool_input":{"path":"`+filepath.Join(clone, "a.go")+`"}}`)
	if code != 0 || strings.ContainsAny(out, "\x1b\u202e\r") || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, "grelot: @evil") {
		t.Errorf("exit %d, stdout %q; want 0 and one line with no escape, bidi control or line break", code, out)
	}
	if !slices.Equal(got, []string{"/v1/holders"}) {
		t.Errorf("the server got %v, want one /v1/holders", got)
	}
}
