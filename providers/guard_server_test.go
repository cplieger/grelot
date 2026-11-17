//go:build !windows

package providers_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The guard's line for @holder-dev's grant on engine.go, which every guard
// scene reserves an hour from the server's 09:00 UTC start.
const (
	heldFile   = "/workspace/grelot/engine.go"
	holderLine = "grelot: @holder-dev holds " + heldFile + " until 2026-11-05 10:00 UTC (splitting the engine); you can still edit it. " +
		"Coordinate with grelot post '@holder-dev' or reserve it after them."
)

// guardScene is a registered writer shell beside @holder-dev, another
// identity on the same server, which holds clone/engine.go for an hour.
type guardScene struct {
	srv    *testServer
	writer *shell
	holder *shell
	clone  string
	file   string
}

// newGuardScene registers writer as name in a clone of cplieger/grelot and
// has @holder-dev reserve engine.go there.
func newGuardScene(t *testing.T, srv *testServer, writer *shell, name string) *guardScene {
	t.Helper()
	clone := repoClone(t, filepath.Join(t.TempDir(), "grelot"), grelotRemote)
	holder := newShell(t, srv, "claude-code", map[string]string{"CLAUDE_CODE_SESSION_ID": "holder-session"})
	holder.register("holder-dev")
	file := filepath.Join(clone, "engine.go")
	holder.must("", "reserve", file, "--ttl=1h", "--reason", "splitting the engine")
	writer.register(name)
	return &guardScene{srv: srv, writer: writer, holder: holder, clone: clone, file: file}
}

// claudeScene is a guard scene whose writer is a Claude Code session.
func claudeScene(t *testing.T) *guardScene {
	t.Helper()
	srv := newServer(t)
	return newGuardScene(t, srv, claudeShell(t, srv, sessionOf(t, "claude-code", "guard-write")), "claude-dev")
}

// write is the writer's Write of path, as Claude Code's PreToolUse sends it.
func (sc *guardScene) write(path string) []byte {
	sc.writer.t.Helper()
	return withFields(sc.writer.t, sc.writer.fixture("guard-write"), map[string]any{"tool_input.file_path": path})
}

// withFields is the hook input data with each dotted path of fields set.
func withFields(t *testing.T, data []byte, fields map[string]any) []byte {
	t.Helper()
	var in map[string]any
	if err := json.Unmarshal(data, &in); err != nil {
		t.Fatal(err)
	}
	for path, v := range fields {
		m := in
		parts := strings.Split(path, ".")
		for _, p := range parts[:len(parts)-1] {
			next, ok := m[p].(map[string]any)
			if !ok {
				t.Fatalf("the input has no object at %s", p)
			}
			m = next
		}
		m[parts[len(parts)-1]] = v
	}
	out, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// wantContext is the one stdout line that gives a PreToolUse hook's text to
// the model as additional context with no decision, on both Claude Code
// (https://code.claude.com/docs/en/hooks, "PreToolUse decision control")
// and Codex (parse_pre_tool_use, codex-rs/hooks/src/engine/output_parser.rs
// at rust-v0.169.0).
func wantContext(t *testing.T, lines ...string) string {
	t.Helper()
	text, err := json.Marshal(strings.Join(lines, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return `{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":` + string(text) + "}}\n"
}

// holdersAsked is the path list of each GET /v1/holders in reqs, and fails
// the test on any other request.
func holdersAsked(t *testing.T, reqs []request) [][]string {
	t.Helper()
	var out [][]string
	for _, r := range reqs {
		if r.Method != http.MethodGet || r.Path != "/v1/holders" {
			t.Errorf("the guard sent %s %s, want only GET /v1/holders", r.Method, r.Path)
			continue
		}
		q, err := url.ParseQuery(r.Query)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, q["path"])
	}
	return out
}

// wantSilent fails unless the guard exited 0 with no output.
func wantSilent(t *testing.T, got outcome) {
	t.Helper()
	if got.Code != 0 || got.Stdout != "" || got.Stderr != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want 0 and no output", got.Code, got.Stdout, got.Stderr)
	}
}

// wantHolder fails unless the guard named @holder-dev for engine.go, so a
// silence the test checks next is the guard's choice, not a guard that
// never speaks.
func (sc *guardScene) wantHolder(t *testing.T) {
	t.Helper()
	if got := sc.writer.hook("guard", sc.write(sc.file)); got.Code != 0 || got.Stdout != wantContext(t, holderLine) {
		t.Fatalf("the write of a held file: exit %d, stdout %q; want %q", got.Code, got.Stdout, wantContext(t, holderLine))
	}
}

func TestGuard_silentWhenFree(t *testing.T) {
	sc := claudeScene(t)
	got := sc.writer.hook("guard", sc.write(filepath.Join(sc.clone, "free.go")))
	wantSilent(t, got)
	if asked := holdersAsked(t, got.Requests); !slices.EqualFunc(asked, [][]string{{"/workspace/grelot/free.go"}}, slices.Equal) {
		t.Errorf("asked %v, want one GET /v1/holders for the written file", asked)
	}
}

func TestGuard_silentForOwnReservation(t *testing.T) {
	sc := claudeScene(t)
	mine := filepath.Join(sc.clone, "internal")
	sc.writer.must("", "reserve", mine, "--ttl=1h", "--reason", "my own")
	got := sc.writer.hook("guard", sc.write(filepath.Join(mine, "x.go")))
	wantSilent(t, got)
	if asked := holdersAsked(t, got.Requests); len(asked) != 1 {
		t.Errorf("asked %v, want one GET /v1/holders", asked)
	}
}

func TestGuard_silentWhenTheServerIsUnreachable(t *testing.T) {
	sc := claudeScene(t)
	sc.wantHolder(t)
	sc.writer.useServer(closedServer(t))
	wantSilent(t, sc.writer.hook("guard", sc.write(sc.file)))
}

// guardBudget is the shipped guard's budget, the most it waits on the
// server.
const guardBudget = time.Second

func TestGuard_silentPastItsBudget(t *testing.T) {
	sc := claudeScene(t)
	sc.wantHolder(t)
	silent := newSilentServer(t)
	sc.writer.useServer(silent.URL)
	start := time.Now()
	got := sc.writer.hook("guard", sc.write(sc.file))
	took := time.Since(start)
	wantSilent(t, got)
	if silent.hits() != 1 {
		t.Errorf("the silent server got %d requests, want 1", silent.hits())
	}
	if took > guardBudget+time.Second {
		t.Errorf("the guard returned after %s, want within its %s budget and a second", took, guardBudget)
	}
	if waited := silent.waited(t); waited > guardBudget+500*time.Millisecond {
		t.Errorf("the guard held its request %s, want about its %s budget", waited, guardBudget)
	}
}

func TestGuard_silentOnAnErrorAnswer(t *testing.T) {
	sc := claudeScene(t)
	sc.wantHolder(t)
	for _, c := range []struct {
		name, code, msg string
		status          int
	}{
		{"released_server_404", "not_found", "no such route: GET /v1/holders", http.StatusNotFound},
		{"no_identity_410", "none", "this session has no identity", http.StatusGone},
		{"server_error_500", "internal", "internal error", http.StatusInternalServerError},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newAnswering(t, c.status, c.code, c.msg)
			sc.writer.useServer(a.URL)
			wantSilent(t, sc.writer.hook("guard", sc.write(sc.file)))
			if got := a.got(); !slices.Equal(got, []string{"/v1/holders"}) {
				t.Errorf("the server got %v, want one /v1/holders", got)
			}
		})
	}
	t.Run("no_identity_on_this_server", func(t *testing.T) {
		sc.writer.useServer(sc.srv.URL)
		sc.writer.must("", "bye")
		got := sc.writer.hook("guard", sc.write(sc.file))
		wantSilent(t, got)
		if asked := holdersAsked(t, got.Requests); len(asked) != 1 {
			t.Errorf("asked %v, want one GET /v1/holders", asked)
		}
	})
}

func TestGuard_silentInAOneShotRun(t *testing.T) {
	sc := claudeScene(t)
	sc.wantHolder(t)
	sc.writer.vars["CLAUDE_CODE_ENTRYPOINT"] = "sdk-cli"
	got := sc.writer.hook("guard", sc.write(sc.file))
	wantSilent(t, got)
	if len(got.Requests) != 0 {
		t.Errorf("requests %v, want none", got.Requests)
	}
}

func TestGuard_silentInASubagent(t *testing.T) {
	sc := claudeScene(t)
	sc.wantHolder(t)
	in := withFields(t, sc.writer.fixture("guard-subagent"), map[string]any{"tool_input.file_path": sc.file})
	got := sc.writer.hook("guard", in)
	wantSilent(t, got)
	if len(got.Requests) != 0 {
		t.Errorf("requests %v, want none", got.Requests)
	}
}

func TestGuard_silentWithNoPath(t *testing.T) {
	sc := claudeScene(t)
	sc.wantHolder(t)
	in := withFields(t, sc.writer.fixture("guard-write"), map[string]any{"tool_input": map[string]any{"content": "x"}})
	got := sc.writer.hook("guard", in)
	wantSilent(t, got)
	if len(got.Requests) != 0 {
		t.Errorf("requests %v, want none", got.Requests)
	}
}

// A path in no clone, or in a clone with no remote, is the path itself in
// grelot's form, which names no repository: the guard does not ask about
// it, even where another agent reserved it.
func TestGuard_skipsAPathOutsideAnyRepository(t *testing.T) {
	sc := claudeScene(t)
	sc.wantHolder(t)
	for name, dir := range map[string]string{
		"no_clone":            t.TempDir(),
		"a_clone_with_no_url": repoClone(t, filepath.Join(t.TempDir(), "scratch"), ""),
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(dir, "notes.txt")
			sc.holder.must("", "reserve", file, "--ttl=1h", "--reason", "local notes")
			got := sc.writer.hook("guard", sc.write(file))
			wantSilent(t, got)
			if len(got.Requests) != 0 {
				t.Errorf("requests %v, want none", got.Requests)
			}
		})
	}
}

func TestGuard_doesNotRunForARead(t *testing.T) {
	sc := claudeScene(t)
	sc.wantHolder(t)
	in := withFields(t, sc.writer.fixture("guard-read"), map[string]any{"tool_input.file_path": sc.file})
	got := sc.writer.hook("guard", in)
	wantSilent(t, got)
	if len(got.Requests) != 0 {
		t.Errorf("requests %v, want none", got.Requests)
	}
}

var queuedID = regexp.MustCompile(`reservation (\d+) queued`)

func TestGuard_namesTheCallersQueuedWait(t *testing.T) {
	sc := claudeScene(t)
	out := sc.writer.must("", "reserve", sc.file, "--ttl=30m", "--reason", "next", "--queue")
	m := queuedID.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("reserve --queue printed no queued id:\n%s", out)
	}
	want := "grelot: @holder-dev holds " + heldFile + " until 2026-11-05 10:00 UTC (splitting the engine); you can still edit it. " +
		"You are queued for " + heldFile + " (reservation " + m[1] + "): coordinate with grelot post '@holder-dev' or wait for the grant, which reaches you as mail."
	got := sc.writer.hook("guard", sc.write(sc.file))
	if got.Code != 0 || got.Stdout != wantContext(t, want) {
		t.Errorf("exit %d, stdout %q; want %q", got.Code, got.Stdout, wantContext(t, want))
	}
}

// codexScene is a guard scene whose writer is a Codex thread.
func codexScene(t *testing.T) *guardScene {
	t.Helper()
	srv := newServer(t)
	return newGuardScene(t, srv, codexShell(t, srv, sessionOf(t, "codex", "guard-patch")), "codex-dev")
}

// patch is the writer's apply_patch of text in cwd, as Codex's PreToolUse
// sends it.
func (sc *guardScene) patch(text, cwd string) []byte {
	sc.writer.t.Helper()
	return withFields(sc.writer.t, sc.writer.fixture("guard-patch"), map[string]any{"tool_input.command": text, "cwd": cwd})
}

// overflowLine is the guard's last line when the lines naming holders pass
// the host's context limit.
var overflowLine = regexp.MustCompile(`^grelot: (\d+) more paths in this write are held by other agents; grelot reservations lists them\.$`)

// Each held grant gets one line in the order the patch names its files,
// until the next would pass Codex's 10,000 bytes; the last line counts the
// rest.
func TestGuard_staysUnderTheContextLimit(t *testing.T) {
	sc := codexScene(t)
	reason := strings.Repeat("r", 280)
	var patch strings.Builder
	patch.WriteString("*** Begin Patch\n")
	const files = 60
	for i := range files {
		name := "f" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + ".go"
		sc.holder.must("", "reserve", filepath.Join(sc.clone, name), "--ttl=1h", "--reason", reason)
		patch.WriteString("*** Update File: " + name + "\n@@\n-a\n+b\n")
	}
	patch.WriteString("*** End Patch\n")
	got := sc.writer.hook("guard", sc.patch(patch.String(), sc.clone))
	if got.Code != 0 || len(got.Stdout) > codexContextLimit {
		t.Fatalf("exit %d, %d bytes; want 0 and at most %d", got.Code, len(got.Stdout), codexContextLimit)
	}
	var out struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(got.Stdout), &out); err != nil {
		t.Fatalf("stdout %q: %v", got.Stdout, err)
	}
	lines := strings.Split(out.HookSpecificOutput.AdditionalContext, "\n")
	m := overflowLine.FindStringSubmatch(lines[len(lines)-1])
	if m == nil {
		t.Fatalf("last line %q, want the line counting the rest", lines[len(lines)-1])
	}
	kept := lines[:len(lines)-1]
	for i, l := range kept {
		name := "f" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + ".go"
		if !strings.HasPrefix(l, "grelot: @holder-dev holds /workspace/grelot/"+name+" until ") {
			t.Errorf("line %d %q, want the holder of %s", i, l, name)
		}
	}
	if n := len(kept) + mustAtoi(t, m[1]); n != files || len(kept) == 0 {
		t.Errorf("%d lines and %s more, want %d in all with at least one line", len(kept), m[1], files)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
