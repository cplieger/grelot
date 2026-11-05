package providers_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/transport"
)

// The threads of the recorded Codex payloads.
const (
	// codexTUI is the Windows TUI thread, codexAfterNew the one /new started
	// in that TUI.
	codexTUI      = "01a19ef0-6415-7941-87f6-9ca8f875c78c"
	codexAfterNew = "01a19ef9-83eb-7c22-9080-cf19bea068a2"
	// codexExec is the Windows codex exec run's.
	codexExec = "01a19def-c194-7ff3-9007-5062de12f462"
	// codexLinux is the Linux TUI thread before /clear, which an Esc
	// interrupted; codexCleared the one /clear started in that TUI.
	codexLinux   = "01a1a4bc-787d-7423-a284-cc692ac519a0"
	codexCleared = "01a1a4be-ae91-7ef2-a7a5-01551eaaa3b3"
	// codexSub is the subagent thread of the derived subagent payloads.
	codexSub = "01a19ef2-1c3d-7a52-8e4f-3b6d9c0a7e15"
)

// codexStart is the queue door's start line.
const codexStart = "run `grelot watch --door queue` as a background terminal"

// codexShell is a Codex shell whose CODEX_THREAD_ID is thread. Codex sets
// that variable in shell commands and leaves it out of hook processes.
func codexShell(t *testing.T, srv *testServer, thread string) *shell {
	t.Helper()
	sh := newShell(t, srv, "codex", map[string]string{"CODEX_THREAD_ID": thread})
	sh.hookOmits = []string{"CODEX_THREAD_ID"}
	return sh
}

func TestCodexSessionStart_unregistered(t *testing.T) {
	for _, name := range []string{"session_start-startup-exec", "session_start-startup-tui", "session_start-startup-after-new"} {
		t.Run(name, func(t *testing.T) {
			sh := codexShell(t, newServer(t), sessionOf(t, "codex", name))
			sh.procs = under("codex.exe")
			got := sh.hook("session_start", sh.fixture(name))
			if got.Code != 0 || got.Stdout != registerLine+"\n" || got.Stderr != "" {
				t.Errorf("exit %d, stdout %q, stderr %q; want 0 and only the register line as context", got.Code, got.Stdout, got.Stderr)
			}
			if len(got.Requests) != 1 || got.Requests[0].Path != "/v1/me" || got.Requests[0].Query != "" ||
				got.Requests[0].Header.Get("Grelot-Session") != "codex:"+sessionOf(t, "codex", name) {
				t.Errorf("requests %v, want one GET /v1/me as the stdin's thread with no proc", got.Requests)
			}
		})
	}
}

func TestCodexSessionStart_live(t *testing.T) {
	for _, name := range []string{"session_start-startup-exec", "session_start-startup-tui"} {
		t.Run(name, func(t *testing.T) {
			sh := codexShell(t, newServer(t), sessionOf(t, "codex", name))
			sh.register("codex-dev")
			manual := sh.must("", "help")
			got := sh.hook("session_start", sh.fixture(name))
			lines := strings.Split(got.Stdout, "\n")
			if got.Code != 0 || !strings.HasPrefix(lines[0], "@codex-dev · box · ") || !strings.HasSuffix(lines[0], " · provider codex") {
				t.Errorf("exit %d, first line %q; want 0 and the card of @codex-dev, provider codex", got.Code, lines[0])
			}
			i := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "grelot: @codex-dev · 0 unread") })
			if i < 0 || !strings.Contains(lines[i], codexStart) {
				t.Errorf("stdout %q, want a status line naming %q", got.Stdout, codexStart)
			}
			if !strings.HasSuffix(got.Stdout, manual) {
				t.Errorf("stdout %q, want it to end with grelot help's text", got.Stdout)
			}
		})
	}
}

// /clear starts a new thread in the same process, so its start names the
// identity that process held, unless the process is the daemon's shared
// app-server. /new sends startup and never gets the hint.
func TestCodexSessionStart_takeOver(t *testing.T) {
	const hint = "grelot: you were codex-dev before this session started; to go on as it, run `grelot register codex-dev --take-over --ttl <d>`"
	for _, exe := range []string{"codex", "codex.exe"} {
		t.Run(exe, func(t *testing.T) {
			sh := codexShell(t, newServer(t), codexLinux)
			sh.procs = under(exe, "--no-daemon")
			sh.register("codex-dev")

			sh.vars["CODEX_THREAD_ID"] = codexCleared
			got := sh.hook("session_start", sh.fixture("session_start-clear"))
			if got.Code != 0 || got.Stdout != registerLine+"\n"+hint+"\n" {
				t.Errorf("clear: exit %d, stdout %q; want the register line, then %q", got.Code, got.Stdout, hint)
			}
			if len(got.Requests) != 1 || got.Requests[0].Query != "proc=200%3A20" {
				t.Errorf("clear: requests %v, want one GET /v1/me naming the TUI process", got.Requests)
			}

			sh.vars["CODEX_THREAD_ID"] = codexAfterNew
			got = sh.hook("session_start", sh.fixture("session_start-startup-after-new"))
			if got.Code != 0 || got.Stdout != registerLine+"\n" {
				t.Errorf("/new: exit %d, stdout %q; want only the register line", got.Code, got.Stdout)
			}
			if len(got.Requests) != 1 || got.Requests[0].Query != "" {
				t.Errorf("/new: requests %v, want one GET /v1/me with no proc", got.Requests)
			}
		})
	}
	t.Run("under the daemon's app-server", func(t *testing.T) {
		sh := codexShell(t, newServer(t), codexLinux)
		sh.procs = under("codex", "app-server", "--listen", "unix://", "--managed-daemon")
		sh.register("codex-dev")
		sh.vars["CODEX_THREAD_ID"] = codexCleared
		got := sh.hook("session_start", sh.fixture("session_start-clear"))
		if got.Code != 0 || got.Stdout != registerLine+"\n" {
			t.Errorf("exit %d, stdout %q; want only the register line", got.Code, got.Stdout)
		}
		if len(got.Requests) != 1 || got.Requests[0].Query != "" {
			t.Errorf("requests %v, want one GET /v1/me with no proc", got.Requests)
		}
	})
}

func TestCodexPrompt(t *testing.T) {
	srv := newServer(t)
	sh := codexShell(t, srv, codexTUI)
	t.Run("unregistered", func(t *testing.T) {
		got := sh.hook("prompt", sh.fixture("prompt-typed"))
		if got.Code != 0 || got.Stdout != registerLine+"\n" {
			t.Errorf("exit %d, stdout %q; want 0 and the register line as context", got.Code, got.Stdout)
		}
	})
	sh.register("codex-dev")
	srv.clock.advance(5 * time.Minute)
	want := "grelot: @codex-dev · 0 unread · desc set 5 min ago · wake: down, start: " + codexStart + " · grelot help\n"
	for _, name := range []string{"prompt-typed", "prompt-queued"} {
		t.Run(name, func(t *testing.T) {
			got := sh.hook("prompt", sh.fixture(name))
			if got.Code != 0 || got.Stdout != want {
				t.Errorf("exit %d, stdout %q; want 0 and %q", got.Code, got.Stdout, want)
			}
			if len(got.Requests) != 1 || got.Requests[0].Query != "turn=start" || got.Requests[0].Header.Get("Grelot-Session") != "codex:"+codexTUI {
				t.Errorf("requests %v, want one GET /v1/me?turn=start as the TUI's thread", got.Requests)
			}
		})
	}
	t.Run("a subagent's prompt", func(t *testing.T) {
		got := sh.hook("prompt", sh.fixture("prompt-subagent"))
		if got.Code != 0 || got.Stdout != "" || len(got.Requests) != 0 {
			t.Errorf("exit %d, stdout %q, requests %v; want nothing and none", got.Code, got.Stdout, got.Requests)
		}
	})
}

// After an Esc, Codex holds every queued nudge until the user types, so a
// pile of stale signal lines arrives in a row, and each ends its turn
// before the model runs.
func TestCodexPrompt_staleSignals(t *testing.T) {
	srv := newServer(t)
	sh := codexShell(t, srv, codexTUI)
	sh.register("codex-dev")
	writer := codexShell(t, srv, codexExec)
	writer.register("writer-dev")
	for range 7 {
		writer.post("@codex-dev", "a question")
	}

	got := sh.hook("prompt", sh.fixture("prompt-signal-line"))
	if got.Code != 0 || !strings.HasPrefix(got.Stdout, "grelot: @codex-dev · 7 unread · ") {
		t.Errorf("unread: exit %d, stdout %q; want the turn line", got.Code, got.Stdout)
	}
	if len(got.Requests) != 1 || got.Requests[0].Query != "signal=7&turn=start" {
		t.Errorf("unread: requests %v, want one GET /v1/me?turn=start carrying signal 7", got.Requests)
	}

	sh.must("", "inbox")
	for _, c := range []struct {
		fixture string
		seq     string
	}{{"prompt-signal-line-earlier", "6"}, {"prompt-signal-line", "7"}} {
		got := sh.hook("prompt", sh.fixture(c.fixture))
		b := parseBlock(t, got.Stdout)
		if want := "grelot (automated, not the user): signal " + c.seq + " is stale; its mail has been read, so this turn ends"; got.Code != 0 || b.Reason != want {
			t.Errorf("%s: exit %d, reason %q; want 0 and %q", c.fixture, got.Code, b.Reason, want)
		}
	}
	got = sh.hook("prompt", sh.fixture("prompt-typed"))
	if got.Code != 0 || !strings.HasPrefix(got.Stdout, "grelot: @codex-dev · 0 unread · ") {
		t.Errorf("the user's next prompt: exit %d, stdout %q; want the turn line", got.Code, got.Stdout)
	}
}

func TestCodexStop(t *testing.T) {
	t.Run("unregistered", func(t *testing.T) {
		sh := codexShell(t, newServer(t), codexExec)
		got := sh.hook("stop", sh.fixture("stop-first"))
		if b := parseBlock(t, got.Stdout); got.Code != 0 || b.Reason != registerLine {
			t.Errorf("exit %d, reason %q; want 0 and the register line", got.Code, b.Reason)
		}
		if body := holdBody(t, got.Requests); body["for"] != "stop" || body["last"] != false {
			t.Errorf("hold body %v, want for stop and last false: Codex runs Stop again after a block", body)
		}
	})
	t.Run("unread mail, then the stop after the block", func(t *testing.T) {
		srv := newServer(t)
		sh := codexShell(t, srv, codexExec)
		sh.register("codex-dev")
		writer := codexShell(t, srv, codexTUI)
		writer.register("writer-dev")
		writer.post("@codex-dev", "a question")
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
	t.Run("a 426 goes to the user as a systemMessage", func(t *testing.T) {
		sh := codexShell(t, newServer(t), codexExec)
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

// Codex's Interrupt reaches no shipped role: the hook file does not wire
// it, and the hook prints and asks nothing.
func TestCodexInterrupt(t *testing.T) {
	sh := codexShell(t, newServer(t), codexLinux)
	sh.register("codex-dev")
	got := sh.hook("turn_end", sh.fixture("turn_end-interrupt"))
	if got.Code != 0 || got.Stdout != "" || got.Stderr != "" || len(got.Requests) != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q, requests %v; want 0, nothing and none", got.Code, got.Stdout, got.Stderr, got.Requests)
	}
}

// Quitting a TUI after /clear reports both threads' ends. The identity
// moved to the new thread with the take-over, so only that report ends it.
func TestCodexSessionEnd(t *testing.T) {
	srv := newServer(t)
	sh := codexShell(t, srv, codexLinux)
	sh.procs = under("codex", "--no-daemon")
	sh.register("codex-dev")
	sh.vars["CODEX_THREAD_ID"] = codexCleared
	sh.hook("session_start", sh.fixture("session_start-clear"))
	sh.must("", "register", "codex-dev", "--take-over", "--paths", filepath.ToSlash(sh.wd), "--desc", "testing the provider file", "--ttl", "2h")

	got := sh.hook("session_end", sh.fixture("session_end-replaced"))
	if got.Code != 0 || got.Stdout != "" || got.Stderr != "" {
		t.Errorf("the replaced thread: exit %d, stdout %q, stderr %q; want 0 and nothing", got.Code, got.Stdout, got.Stderr)
	}
	// The take-over leased 2 h from 09:00; a session's end caps the lease at
	// an hour from then.
	if me := sh.must("", "me"); !strings.Contains(me, "until 2026-11-05 11:00 UTC") {
		t.Errorf("after the replaced thread's end, me says %q; want the 2 h lease unchanged", me)
	}

	got = sh.hook("session_end", sh.fixture("session_end-other"))
	if got.Code != 0 || got.Stdout != "" || got.Stderr != "" {
		t.Errorf("the current thread: exit %d, stdout %q, stderr %q; want 0 and nothing", got.Code, got.Stdout, got.Stderr)
	}
	if len(got.Requests) != 1 || got.Requests[0].Path != "/v1/lifecycle" || got.Requests[0].Body != `{"event":"session_end"}` ||
		got.Requests[0].Header.Get("Grelot-Session") != "codex:"+codexCleared {
		t.Errorf("the current thread: requests %v, want one session_end report as that thread", got.Requests)
	}
	if me := sh.must("", "me"); !strings.Contains(me, "until 2026-11-05 10:00 UTC") {
		t.Errorf("after the current thread's end, me says %q; want the lease capped at an hour", me)
	}
}

// Codex gives a SessionEnd hook at most 3 seconds
// (SESSION_END_MAX_TIMEOUT_SEC in
// https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/hooks/src/events/session_end.rs).
func TestCodexSessionEnd_givesUpInsideCodexsLimit(t *testing.T) {
	sh := codexShell(t, newServer(t), codexCleared)
	url, hits := silentServer(t)
	sh.useServer(url)
	start := time.Now()
	got := sh.hook("session_end", sh.fixture("session_end-other"))
	took := time.Since(start)
	if got.Code != 0 || got.Stdout != "" || got.Stderr != "" || hits() != 1 {
		t.Errorf("exit %d, stdout %q, stderr %q, %d requests; want 0, nothing and the one report", got.Code, got.Stdout, got.Stderr, hits())
	}
	if took >= 3*time.Second {
		t.Errorf("the hook took %s against a server that never answers, want under 3s", took)
	}
}

// A subagent's SubagentStop carries the root thread's session_id, and ends
// the delegate the subagent registered from its own thread.
func TestCodexSubagentStop(t *testing.T) {
	srv := newServer(t)
	parent := codexShell(t, srv, codexExec)
	parent.register("codex-dev")
	out := parent.must("", "delegate", "review", "--ttl", "1h")
	i := strings.Index(out, "grelot-link:")
	if i < 0 {
		t.Fatalf("grelot delegate printed %q, no link", out)
	}
	grant := strings.Fields(out[i:])[0]
	sub := codexShell(t, srv, codexSub)
	sub.must("", "register", "--delegate", grant, "--desc", "reviewing the diff")
	if me := sub.must("", "me"); !strings.Contains(me, "@codex-dev/review") {
		t.Fatalf("the delegate's me before the stop: %q", me)
	}

	got := parent.hook("subagent_stop", parent.fixture("subagent_stop"))
	if got.Code != 0 || got.Stdout != "" || got.Stderr != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want 0 and nothing", got.Code, got.Stdout, got.Stderr)
	}
	if len(got.Requests) != 1 || got.Requests[0].Path != "/v1/lifecycle" || got.Requests[0].Header.Get("Grelot-Session") != "codex:"+codexSub {
		t.Errorf("requests %v, want one report as the subagent's thread", got.Requests)
	}
	if me := sub.run("", "me"); !strings.Contains(me.Stdout+me.Stderr, "this session has no identity") {
		t.Errorf("the delegate's me after the stop: %q, want no identity", me.Stdout+me.Stderr)
	}
	if me := parent.must("", "me"); !strings.Contains(me, "@codex-dev") {
		t.Errorf("the parent's me after the stop: %q, want it unchanged", me)
	}
}

func TestCodexQueueDoor(t *testing.T) {
	p := shipped(t, "codex")
	if len(p.Doors) != 1 {
		t.Fatalf("doors %v, want the queue door alone", p.Doors)
	}
	d := &p.Doors[0]
	if d.Name != "queue" || d.Transport != provider.TransportExec || d.Layer != 2 || d.Start != codexStart || d.Asset != "codex/grelot.rules" {
		t.Errorf("door %+v, want the layer 2 exec door queue, start %q, asset codex/grelot.rules", d, codexStart)
	}
	signal := transport.SignalLine(7, 2)
	argv, err := d.RenderArgv(&provider.Data{Session: codexTUI, Line: signal})
	if want := []string{"codex", "queue", "--thread", codexTUI, "--message", signal}; err != nil || !slices.Equal(argv, want) {
		t.Errorf("argv %q, %v; want %q", argv, err, want)
	}
}

func TestCodexHookFile(t *testing.T) {
	want := wantWiring(t, "codex", map[string]int{"SessionEnd": 3})
	var events []string
	for _, w := range want {
		events = append(events, w.Event)
	}
	if !slices.Equal(events, []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd", "SubagentStop"}) {
		t.Errorf("the provider spells events %v, want SessionStart, UserPromptSubmit, Stop, SessionEnd and SubagentStop", events)
	}
	if got := hookWiring(t, "codex"); !sameWiring(got, want) {
		t.Errorf("hooks.json wires %v, want %v", got, want)
	}
}

// The rule file allows grelot and nothing that only starts with its name,
// so Codex runs it outside the sandbox with no approval.
func TestCodexRules(t *testing.T) {
	const want = `prefix_rule(
    pattern = ["grelot"],
    decision = "allow",
    justification = "grelot messaging client",
    match = ["grelot inbox", "grelot watch --door queue"],
    not_match = ["grelotx inbox"],
)
`
	got, err := os.ReadFile(filepath.Join("codex", "grelot.rules"))
	if err != nil || string(got) != want {
		t.Errorf("codex/grelot.rules is %q, %v; want exactly %q", got, err, want)
	}
}
