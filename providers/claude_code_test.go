package providers_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/transport"
	"github.com/cplieger/grelot/providers"
)

// The sessions of the recorded Claude Code payloads.
const (
	// claudeTUI is the Windows TUI session before /clear, claudeCleared the
	// one /clear started in the same TUI, which --continue resumed.
	claudeTUI     = "a9c96e81-c3c7-4658-9534-acfd0d061ae6"
	claudeCleared = "cf282512-ceef-4065-8ac9-000a6a72dee7"
	// claudeFork is the session the derived fork start names.
	claudeFork = "5d0e7c1a-2b4f-4e8a-9c3d-7f6b1a2e4c90"
	// claudePrint is the Windows -p run's.
	claudePrint = "63cd652a-8c48-452b-a178-11ba86bda9b3"
	// claudeMonitor is the Linux session a Monitor woke.
	claudeMonitor = "a554296a-f8e6-422e-9919-addc9ac2b0d0"
	// claudeEnded is the Linux -p run whose exit sent SessionEnd.
	claudeEnded = "e4338dc1-e8a9-4c73-868d-6d89bf3ccc56"
	// claudeParent is the Linux session whose subagent finished.
	claudeParent = "7bd3ea77-8a49-4b04-b409-d34362a2159f"
	// claudeFailed is the Linux -p run an API error ended.
	claudeFailed = "a566043a-0e68-47a3-b0c8-f88261a1f011"
)

// claudeStart is the background door's start line, the first watcher door.
const claudeStart = "run `grelot watch --door background` with run_in_background"

// claudeShell is a Claude Code shell whose CLAUDE_CODE_SESSION_ID is
// session.
func claudeShell(t *testing.T, srv *testServer, session string) *shell {
	t.Helper()
	return newShell(t, srv, "claude-code", map[string]string{"CLAUDE_CODE_SESSION_ID": session})
}

func TestClaudeCodeSessionStart_unregistered(t *testing.T) {
	for _, name := range []string{"session_start-startup-print", "session_start-startup-tui", "session_start-resume", "session_start-compact"} {
		t.Run(name, func(t *testing.T) {
			sh := claudeShell(t, newServer(t), sessionOf(t, "claude-code", name))
			got := sh.hook("session_start", sh.fixture(name))
			if got.Code != 0 || got.Stdout != registerLine+"\n" || got.Stderr != "" {
				t.Errorf("exit %d, stdout %q, stderr %q; want 0 and only the register line as context", got.Code, got.Stdout, got.Stderr)
			}
			if len(got.Requests) != 1 || got.Requests[0].Path != "/v1/me" || got.Requests[0].Query != "" {
				t.Errorf("requests %v, want one GET /v1/me with no proc", got.Requests)
			}
		})
	}
}

// Every start a live session reports, compaction included, gives it the
// card, the status line and the manual again.
func TestClaudeCodeSessionStart_live(t *testing.T) {
	for _, name := range []string{"session_start-startup-tui", "session_start-resume", "session_start-compact"} {
		t.Run(name, func(t *testing.T) {
			sh := claudeShell(t, newServer(t), sessionOf(t, "claude-code", name))
			sh.register("claude-dev")
			manual := sh.must("", "help")
			got := sh.hook("session_start", sh.fixture(name))
			lines := strings.Split(got.Stdout, "\n")
			if got.Code != 0 || !strings.HasPrefix(lines[0], "@claude-dev · box · ") || !strings.HasSuffix(lines[0], " · provider claude-code") {
				t.Errorf("exit %d, first line %q; want 0 and the card of @claude-dev, provider claude-code", got.Code, lines[0])
			}
			i := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "grelot: @claude-dev · 0 unread") })
			if i < 0 || !strings.Contains(lines[i], claudeStart) {
				t.Errorf("stdout %q, want a status line naming %q", got.Stdout, claudeStart)
			}
			if !strings.HasSuffix(got.Stdout, manual) {
				t.Errorf("stdout %q, want it to end with grelot help's text", got.Stdout)
			}
		})
	}
}

// /clear starts a new session in the same TUI process, so its start names
// the identity that process held; a fork is a second session and never
// gets that hint.
func TestClaudeCodeSessionStart_takeOverAndFork(t *testing.T) {
	const hint = "grelot: you were claude-dev before this session started; to go on as it, run `grelot register claude-dev --take-over --ttl <d>`"
	for _, exe := range []string{"claude.exe", "claude"} {
		t.Run(exe, func(t *testing.T) {
			srv := newServer(t)
			sh := claudeShell(t, srv, claudeTUI)
			sh.procs = under(exe)
			sh.register("claude-dev")

			sh.vars["CLAUDE_CODE_SESSION_ID"] = claudeCleared
			got := sh.hook("session_start", sh.fixture("session_start-clear"))
			if got.Code != 0 || got.Stdout != registerLine+"\n"+hint+"\n" {
				t.Errorf("clear: exit %d, stdout %q; want the register line, then %q", got.Code, got.Stdout, hint)
			}
			if len(got.Requests) != 1 || got.Requests[0].Query != "proc=200%3A20" {
				t.Errorf("clear: requests %v, want one GET /v1/me naming the TUI process", got.Requests)
			}

			sh.vars["CLAUDE_CODE_SESSION_ID"] = claudeFork
			got = sh.hook("session_start", sh.fixture("session_start-fork"))
			if got.Code != 0 || got.Stdout != registerLine+"\n" {
				t.Errorf("fork: exit %d, stdout %q; want only the register line", got.Code, got.Stdout)
			}
			if len(got.Requests) != 1 || got.Requests[0].Query != "" {
				t.Errorf("fork: requests %v, want one GET /v1/me with no proc", got.Requests)
			}
		})
	}
}

func TestClaudeCodePrompt(t *testing.T) {
	srv := newServer(t)
	sh := claudeShell(t, srv, claudeTUI)
	t.Run("unregistered", func(t *testing.T) {
		got := sh.hook("prompt", sh.fixture("prompt-typed"))
		if got.Code != 0 || got.Stdout != registerLine+"\n" {
			t.Errorf("exit %d, stdout %q; want 0 and the register line as context", got.Code, got.Stdout)
		}
	})
	t.Run("registered", func(t *testing.T) {
		sh.register("claude-dev")
		srv.clock.advance(5 * time.Minute)
		got := sh.hook("prompt", sh.fixture("prompt-typed"))
		want := "grelot: @claude-dev · 0 unread · desc set 5 min ago · wake: down, start: " + claudeStart + " · grelot help\n"
		if got.Code != 0 || got.Stdout != want {
			t.Errorf("exit %d, stdout %q; want 0 and %q", got.Code, got.Stdout, want)
		}
		if len(got.Requests) != 1 || got.Requests[0].Query != "turn=start" {
			t.Errorf("requests %v, want one GET /v1/me?turn=start", got.Requests)
		}
	})
}

// A Monitor event carrying a signal line runs its turn while the signal's
// mail is unread, and is blocked once the session has read it. A task
// notification in another wrapper is a turn like any other.
func TestClaudeCodePrompt_monitorSignal(t *testing.T) {
	srv := newServer(t)
	sh := claudeShell(t, srv, claudeMonitor)
	sh.register("claude-dev")
	writer := claudeShell(t, srv, "writer-session")
	writer.register("writer-dev")
	writer.post("@claude-dev", "a question")

	got := sh.hook("prompt", sh.fixture("prompt-monitor-event"))
	if got.Code != 0 || !strings.HasPrefix(got.Stdout, "grelot: @claude-dev · 1 unread · ") {
		t.Errorf("unread: exit %d, stdout %q; want the turn line", got.Code, got.Stdout)
	}
	if len(got.Requests) != 1 || got.Requests[0].Query != "signal=1&turn=start" {
		t.Errorf("unread: requests %v, want one GET /v1/me?turn=start carrying signal 1", got.Requests)
	}

	sh.must("", "inbox")
	got = sh.hook("prompt", sh.fixture("prompt-monitor-event"))
	b := parseBlock(t, got.Stdout)
	if got.Code != 0 || b.Reason != "grelot (automated, not the user): signal 1 is stale; its mail has been read, so this turn ends" {
		t.Errorf("read: exit %d, reason %q; want 0 and the stale-signal block", got.Code, b.Reason)
	}

	got = sh.hook("prompt", sh.fixture("prompt-task-notification"))
	if got.Code != 0 || !strings.HasPrefix(got.Stdout, "grelot: @claude-dev · 0 unread · ") {
		t.Errorf("a stream's end: exit %d, stdout %q; want the turn line", got.Code, got.Stdout)
	}
	if len(got.Requests) != 1 || got.Requests[0].Query != "turn=start" {
		t.Errorf("a stream's end: requests %v, want no signal", got.Requests)
	}
}

func TestClaudeCodeStop(t *testing.T) {
	t.Run("unregistered", func(t *testing.T) {
		sh := claudeShell(t, newServer(t), claudePrint)
		got := sh.hook("stop", sh.fixture("stop-first"))
		if b := parseBlock(t, got.Stdout); got.Code != 0 || b.Reason != registerLine {
			t.Errorf("exit %d, reason %q; want 0 and the register line", got.Code, b.Reason)
		}
		if body := holdBody(t, got.Requests); body["for"] != "stop" || body["last"] != false {
			t.Errorf("hold body %v, want for stop and last false: Claude Code runs Stop again after a block", body)
		}
	})
	t.Run("unread mail, then the stop after the block", func(t *testing.T) {
		srv := newServer(t)
		sh := claudeShell(t, srv, claudePrint)
		sh.register("claude-dev")
		writer := claudeShell(t, srv, "writer-session")
		writer.register("writer-dev")
		writer.post("@claude-dev", "a question")
		srv.clock.advance(time.Second)

		got := sh.hook("stop", sh.fixture("stop-first"))
		b := parseBlock(t, got.Stdout)
		if got.Code != 0 || !strings.HasPrefix(b.Reason, "grelot (automated, not the user): unread ") || !strings.Contains(b.Reason, "1 direct") ||
			!strings.Contains(b.Reason, "run grelot inbox before you stop") || strings.Contains(got.Stdout, "a question") {
			t.Errorf("first stop: exit %d, stdout %q; want 0 and the unread hold line with no body", got.Code, got.Stdout)
		}
		got = sh.hook("stop", sh.fixture("stop-active"))
		if got.Code != 0 || got.Stdout != "" {
			t.Errorf("second stop: exit %d, stdout %q; want 0 and nothing", got.Code, got.Stdout)
		}
		if len(got.Requests) != 1 || got.Requests[0].Path != "/v1/lifecycle" || got.Requests[0].Body != `{"event":"turn_end"}` {
			t.Errorf("second stop: requests %v, want one turn_end report", got.Requests)
		}
	})
	t.Run("a Monitor running and no mail", func(t *testing.T) {
		sh := claudeShell(t, newServer(t), claudeCleared)
		sh.register("claude-dev")
		got := sh.hook("stop", sh.fixture("stop-background-task"))
		if got.Code != 0 || got.Stdout != "" || got.Stderr != "" {
			t.Errorf("exit %d, stdout %q, stderr %q; want 0 and nothing", got.Code, got.Stdout, got.Stderr)
		}
	})
	t.Run("a 426 goes to the user as a systemMessage", func(t *testing.T) {
		sh := claudeShell(t, newServer(t), claudePrint)
		sh.version = "v0.0.1"
		got := sh.hook("stop", sh.fixture("stop-first"))
		var keys map[string]string
		if err := json.Unmarshal([]byte(got.Stdout), &keys); err != nil {
			t.Fatalf("stdout %q is not one JSON object: %v", got.Stdout, err)
		}
		if got.Code != 0 || len(keys) != 1 || !strings.HasPrefix(keys["systemMessage"], "grelot: this grelot server") {
			t.Errorf("exit %d, stdout %q; want 0 and only a systemMessage with the 426's line", got.Code, got.Stdout)
		}
	})
}

// Copilot CLI reads a repository's .claude/settings.json and Cursor loads
// Claude Code hook files, so the claude-code hook lines can run with a
// stdin session_id and no CLAUDE_CODE_SESSION_ID. That shell cannot
// register, so the hooks print and ask nothing rather than demand it.
func TestClaudeCode_anotherHostRunningItsHooks(t *testing.T) {
	for _, c := range []struct{ role, fixture string }{
		{"session_start", "session_start-startup-tui"},
		{"prompt", "prompt-typed"},
		{"stop", "stop-first"},
	} {
		t.Run(c.role, func(t *testing.T) {
			sh := newShell(t, newServer(t), "claude-code", map[string]string{})
			in := sh.fixture(c.fixture)
			if !bytes.Contains(in, []byte(`"session_id"`)) {
				t.Fatalf("fixture %s has no session_id", c.fixture)
			}
			got := sh.hook(c.role, in)
			if got.Code != 0 || got.Stdout != "" || got.Stderr != "" || len(got.Requests) != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q, requests %v; want 0, nothing and none", got.Code, got.Stdout, got.Stderr, got.Requests)
			}
		})
	}
}

// Claude Code's StopFailure and SubagentStop reach no shipped role: the
// hook file wires neither, and the hook prints and asks nothing.
func TestClaudeCodeUnwiredEvents(t *testing.T) {
	cases := []struct{ role, fixture, session string }{
		{"turn_end", "turn_end-stop-failure", claudeFailed},
		{"subagent_stop", "subagent_stop", claudeParent},
	}
	for _, c := range cases {
		t.Run(c.role, func(t *testing.T) {
			sh := claudeShell(t, newServer(t), c.session)
			sh.register("claude-dev")
			got := sh.hook(c.role, sh.fixture(c.fixture))
			if got.Code != 0 || got.Stdout != "" || got.Stderr != "" || len(got.Requests) != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q, requests %v; want 0, nothing and none", got.Code, got.Stdout, got.Stderr, got.Requests)
			}
		})
	}
}

func TestClaudeCodeSessionEnd(t *testing.T) {
	t.Run("it reports the end and prints nothing", func(t *testing.T) {
		sh := claudeShell(t, newServer(t), claudeEnded)
		sh.register("claude-dev")
		got := sh.hook("session_end", sh.fixture("session_end-other"))
		if got.Code != 0 || got.Stdout != "" || got.Stderr != "" {
			t.Errorf("exit %d, stdout %q, stderr %q; want 0 and nothing", got.Code, got.Stdout, got.Stderr)
		}
		if len(got.Requests) != 1 || got.Requests[0].Path != "/v1/lifecycle" || got.Requests[0].Body != `{"event":"session_end"}` ||
			got.Requests[0].Header.Get("Grelot-Session") != "claude-code:"+claudeEnded {
			t.Errorf("requests %v, want one session_end report as the ended session", got.Requests)
		}
	})
	// Claude Code gives SessionEnd hooks 1.5 s between them
	// (https://code.claude.com/docs/en/hooks, the timeout field).
	t.Run("it gives up inside Claude Code's 1.5 s", func(t *testing.T) {
		sh := claudeShell(t, newServer(t), claudeEnded)
		url, hits := silentServer(t)
		sh.useServer(url)
		start := time.Now()
		got := sh.hook("session_end", sh.fixture("session_end-other"))
		took := time.Since(start)
		if got.Code != 0 || got.Stdout != "" || got.Stderr != "" || hits() != 1 {
			t.Errorf("exit %d, stdout %q, stderr %q, %d requests; want 0, nothing and the one report", got.Code, got.Stdout, got.Stderr, hits())
		}
		if took >= 1500*time.Millisecond {
			t.Errorf("the hook took %s against a server that never answers, want under 1.5s", took)
		}
	})
}

func TestClaudeCodeDoors(t *testing.T) {
	p := shipped(t, "claude-code")
	var names []string
	for _, d := range p.Doors {
		names = append(names, d.Name)
	}
	if !slices.Equal(names, []string{"background", "monitor"}) {
		t.Fatalf("doors %v, want background, then monitor", names)
	}
	signal := transport.SignalLine(7, 2)
	end := transport.EndLine(transport.EndEnded)
	cases := []struct {
		door      *provider.Door
		start     string
		transport provider.Transport
		nudgeExit bool
	}{
		{door: &p.Doors[0], start: claudeStart, transport: provider.TransportExit, nudgeExit: true},
		{door: &p.Doors[1], start: "`grelot watch --door monitor` under Monitor, timeout_ms 1800000", transport: provider.TransportStdout},
	}
	for _, c := range cases {
		t.Run(c.door.Name, func(t *testing.T) {
			if c.door.Transport != c.transport || c.door.Layer != 2 || c.door.Start != c.start || c.door.Hold != 0 {
				t.Errorf("door %+v, want a layer 2 %s door with no hold, start %q", c.door, c.transport, c.start)
			}
			var out bytes.Buffer
			w, err := transport.NewWatch(c.door, &out, nil)
			if err != nil {
				t.Fatal(err)
			}
			step, err := w.Nudge(t.Context(), &provider.Data{Line: signal})
			if err != nil || step.Exit != c.nudgeExit || step.Code != 0 || out.String() != signal+"\n" {
				t.Errorf("nudge: step %+v, %v, stdout %q; want the signal line alone, exit %v with 0", step, err, out.String(), c.nudgeExit)
			}
			out.Reset()
			step, err = w.End(t.Context(), &provider.Data{Line: end})
			if err != nil || !step.Exit || step.Code != 1 || out.String() != end+"\n" {
				t.Errorf("end: step %+v, %v, stdout %q; want the end line alone and exit 1, the default end code", step, err, out.String())
			}
		})
	}
}

func TestClaudeCodeHookEvents(t *testing.T) {
	want := wantWiring(t, "claude-code", nil)
	var events []string
	for _, w := range want {
		events = append(events, w.Event)
	}
	if !slices.Equal(events, []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"}) {
		t.Errorf("the provider spells events %v, want SessionStart, UserPromptSubmit, Stop and SessionEnd", events)
	}
}

// shipped is the named provider as the embedded set loads it.
func shipped(t *testing.T, name string) *provider.Provider {
	t.Helper()
	set, err := provider.LoadSet(providers.FS)
	if err != nil {
		t.Fatal(err)
	}
	p, err := set.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// sessionOf is the session_id of a recorded payload.
func sessionOf(t *testing.T, dir, name string) string {
	t.Helper()
	var in struct {
		Session string `json:"session_id"`
	}
	if err := json.Unmarshal(mustRead(t, "testdata/golden/"+dir+"/"+name+".json"), &in); err != nil {
		t.Fatal(err)
	}
	return in.Session
}
