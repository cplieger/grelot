//go:build !windows

package providers_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/providers"
)

// The sessions of the recorded kiro-cli payloads.
const (
	// kiroSession is the headless run's.
	kiroSession = "sess_fcd984d7-fd4e-4051-bbea-b41fd81e9391"
	// tangentParent is the chat a tangent forked from, tangentSession the
	// tangent's own.
	tangentParent  = "sess_ef71553f-8632-4ea2-a64f-846c852e8478"
	tangentSession = "sess_5be813c2-3090-4b45-8c90-9c592f946395"
	// rewoundRoot is the TUI chat a /rewind replaced, rewindSession the
	// rewind's own.
	rewoundRoot   = "sess_dd97485d-a96a-44bc-ba17-eb8230525c06"
	rewindSession = "sess_f126e366-693d-43e1-81f8-7b5090d01b35"
	// kiroTUI is a TUI's first chat, kiroCleared the one /clear started in
	// the same kiro-cli chat process.
	kiroTUI     = "sess_882c3937-7013-4fe3-ae78-0a4cbf25c973"
	kiroCleared = "sess_863c88c8-0ef4-410e-810b-fab32cfa581f"
)

// registerLine is the turn-start text of a session with no identity.
const registerLine = "grelot: not registered. Register before you start work: run `grelot help`, then `grelot register` as it says."

// kiroShell is a shell of the named provider, kiro-cli or kiro-ide, whose
// KIRO_SESSION_ID is session, with its own KIRO_HOME. A kiro-ide shell's
// client config names its provider, as the host page has the user do,
// because detection alone picks kiro-cli.
func kiroShell(t *testing.T, srv *testServer, name, session string) *shell {
	t.Helper()
	sh := newShell(t, srv, name, map[string]string{"KIRO_SESSION_ID": session, "KIRO_HOME": t.TempDir()})
	sh.fixtures = "kiro-cli"
	if name == "kiro-ide" {
		f, err := os.OpenFile(client.ConfigFile(sh.config), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("provider: kiro-ide\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return sh
}

// placeRecord puts a recorded session.json where the agent server keeps
// it, $KIRO_HOME/sessions/<workspace hash>/<session>/session.json.
func placeRecord(t *testing.T, sh *shell, session string) {
	t.Helper()
	dir := filepath.Join(sh.vars["KIRO_HOME"], "sessions", "c52ddf65", session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := mustRead(t, filepath.Join("testdata", "golden", "kiro-cli", "records", session+".json"))
	if err := os.WriteFile(filepath.Join(dir, "session.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// stopBlock is a Stop output the agent server reads: a JSON object with
// only decision and reason.
type stopBlock struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func parseBlock(t *testing.T, out string) stopBlock {
	t.Helper()
	var keys map[string]any
	if err := json.Unmarshal([]byte(out), &keys); err != nil {
		t.Fatalf("stdout %q is not one JSON object: %v", out, err)
	}
	if len(keys) != 2 {
		t.Errorf("stdout %q holds keys %v, want decision and reason only", out, keys)
	}
	var b stopBlock
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatal(err)
	}
	if b.Decision != "block" {
		t.Errorf("decision %q, want block", b.Decision)
	}
	return b
}

// holdBody is the body of the one hold request among reqs.
func holdBody(t *testing.T, reqs []request) map[string]any {
	t.Helper()
	for _, r := range reqs {
		if r.Method == http.MethodPost && r.Path == "/v1/events/hold" {
			var body map[string]any
			if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
				t.Fatal(err)
			}
			return body
		}
	}
	t.Fatalf("requests %v hold no POST /v1/events/hold", reqs)
	return nil
}

func TestKiroCLISessionStart_unregistered(t *testing.T) {
	sh := kiroShell(t, newServer(t), "kiro-cli", kiroSession)
	got := sh.hook("session_start", sh.fixture("session_start"))
	if got.Code != 0 || got.Stdout != registerLine+"\n" || got.Stderr != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want 0 and only the register line as context", got.Code, got.Stdout, got.Stderr)
	}
	if len(got.Requests) != 1 {
		t.Fatalf("%d requests, want one GET /v1/me", len(got.Requests))
	}
	req := got.Requests[0]
	if req.Method != http.MethodGet || req.Path != "/v1/me" || req.Header.Get("Grelot-Session") != "kiro-cli:"+kiroSession {
		t.Errorf("request %s %s as %q, want GET /v1/me as kiro-cli:%s", req.Method, req.Path, req.Header.Get("Grelot-Session"), kiroSession)
	}
}

func TestKiroCLISessionStart_live(t *testing.T) {
	sh := kiroShell(t, newServer(t), "kiro-cli", kiroSession)
	sh.register("kiro-dev")
	manual := sh.must("", "help")
	got := sh.hook("session_start", sh.fixture("session_start"))
	if got.Code != 0 || got.Stderr != "" {
		t.Errorf("exit %d, stderr %q; want 0 and nothing on stderr", got.Code, got.Stderr)
	}
	lines := strings.Split(got.Stdout, "\n")
	t.Run("the card first", func(t *testing.T) {
		if !strings.HasPrefix(lines[0], "@kiro-dev · box · ") || !strings.HasSuffix(lines[0], " · provider kiro-cli") {
			t.Errorf("first line %q, want the card of @kiro-dev on host box, provider kiro-cli", lines[0])
		}
	})
	t.Run("the status line names the workflow's start line", func(t *testing.T) {
		i := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "grelot: @kiro-dev · 0 unread") })
		if i < 0 || !strings.Contains(lines[i], kiroStart) {
			t.Errorf("stdout %q, want a status line naming %q", got.Stdout, kiroStart)
		}
	})
	t.Run("the manual last", func(t *testing.T) {
		if !strings.HasSuffix(got.Stdout, manual) {
			t.Errorf("stdout %q, want it to end with grelot help's text", got.Stdout)
		}
	})
}

// A workflow step's SessionStart carries the step's own session on stdin
// and the root's in its shell; it prints nothing and asks nothing.
func TestKiroCLISessionStart_aStepPrintsNothing(t *testing.T) {
	sh := kiroShell(t, newServer(t), "kiro-cli", rewoundRoot)
	sh.register("kiro-dev")
	got := sh.hook("session_start", sh.fixture("session_start-step"))
	if got.Code != 0 || got.Stdout != "" || len(got.Requests) != 0 {
		t.Errorf("exit %d, stdout %q, %d requests; want 0, nothing and none", got.Code, got.Stdout, len(got.Requests))
	}
}

// kiro-cli fires SessionStart only for a chat with no history, which in a
// TUI's own `kiro-cli chat` process is its first chat or one /clear started.
// So every start there names the identity that process held, while a server
// process (acp, serve) hosts many chats and never gets that hint.
func TestKiroCLISessionStart_takeOverAfterClear(t *testing.T) {
	const hint = "grelot: you were kiro-dev before this session started; to go on as it, run `grelot register kiro-dev --take-over --ttl=<d>`"
	for _, c := range []struct {
		name  string
		procs processes
	}{
		{"kiro-cli chat", under("kiro-cli", "chat", "--trust-workspace")},
		{"kiro-cli with no subcommand", under("kiro-cli")},
		{"kiro-cli.exe chat", under("kiro-cli.exe", "chat")},
	} {
		t.Run(c.name, func(t *testing.T) {
			sh := kiroShell(t, newServer(t), "kiro-cli", kiroTUI)
			sh.procs = c.procs
			sh.register("kiro-dev")

			sh.vars["KIRO_SESSION_ID"] = kiroCleared
			got := sh.hook("session_start", sh.fixture("session_start-clear"))
			if got.Code != 0 || got.Stdout != registerLine+"\n"+hint+"\n" {
				t.Errorf("exit %d, stdout %q; want the register line, then %q", got.Code, got.Stdout, hint)
			}
			if len(got.Requests) != 1 || got.Requests[0].Query != "proc=200%3A20" {
				t.Errorf("requests %v, want one GET /v1/me naming the TUI process", got.Requests)
			}
		})
	}
	for _, c := range []struct {
		name  string
		procs processes
	}{
		{"kiro-cli acp", under("kiro-cli", "acp", "--agent-engine", "v3")},
		{"kiro-cli serve", under("kiro-cli", "serve", "--port", "8082")},
		{"another process", under("node")},
	} {
		t.Run(c.name, func(t *testing.T) {
			sh := kiroShell(t, newServer(t), "kiro-cli", kiroTUI)
			sh.procs = c.procs
			sh.register("kiro-dev")

			sh.vars["KIRO_SESSION_ID"] = kiroCleared
			got := sh.hook("session_start", sh.fixture("session_start-clear"))
			if got.Code != 0 || got.Stdout != registerLine+"\n" {
				t.Errorf("exit %d, stdout %q; want only the register line", got.Code, got.Stdout)
			}
			if len(got.Requests) != 1 || got.Requests[0].Query != "" {
				t.Errorf("requests %v, want one GET /v1/me with no proc", got.Requests)
			}
		})
	}
}

func TestKiroCLIPrompt(t *testing.T) {
	srv := newServer(t)
	sh := kiroShell(t, srv, "kiro-cli", kiroSession)
	t.Run("unregistered", func(t *testing.T) {
		got := sh.hook("prompt", sh.fixture("prompt"))
		if got.Code != 0 || got.Stdout != registerLine+"\n" {
			t.Errorf("exit %d, stdout %q; want 0 and the register line as context", got.Code, got.Stdout)
		}
		if len(got.Requests) != 1 || got.Requests[0].Path != "/v1/me" || got.Requests[0].Query != "turn=start" {
			t.Errorf("requests %v, want one GET /v1/me?turn=start", got.Requests)
		}
	})
	t.Run("registered", func(t *testing.T) {
		sh.register("kiro-dev")
		srv.clock.advance(5 * time.Minute)
		got := sh.hook("prompt", sh.fixture("prompt"))
		if got.Code != 0 || strings.Count(got.Stdout, "\n") != 1 ||
			!strings.HasPrefix(got.Stdout, "grelot: @kiro-dev · 0 unread · desc set 5 min ago · ") || !strings.HasSuffix(got.Stdout, " · grelot help\n") {
			t.Errorf("exit %d, stdout %q; want 0 and the one turn line as context", got.Code, got.Stdout)
		}
	})
}

func TestKiroCLIStop(t *testing.T) {
	t.Run("unregistered", func(t *testing.T) {
		sh := kiroShell(t, newServer(t), "kiro-cli", kiroSession)
		got := sh.hook("stop", sh.fixture("stop"))
		if b := parseBlock(t, got.Stdout); got.Code != 0 || b.Reason != registerLine {
			t.Errorf("exit %d, reason %q; want 0 and the register line", got.Code, b.Reason)
		}
		if body := holdBody(t, got.Requests); body["for"] != "stop" || body["last"] != true {
			t.Errorf("hold body %v, want for stop and last true: the agent server runs one Stop per turn", body)
		}
	})
	t.Run("unread mail", func(t *testing.T) {
		srv := newServer(t)
		sh := kiroShell(t, srv, "kiro-cli", kiroSession)
		sh.register("kiro-dev")
		sh.hook("prompt", sh.fixture("prompt"))
		if got := sh.hook("stop", sh.fixture("stop")); got.Code != 0 || got.Stdout != "" {
			t.Errorf("no mail: exit %d, stdout %q; want 0 and nothing", got.Code, got.Stdout)
		}
		writer := kiroShell(t, srv, "kiro-cli", "sess_writer")
		writer.register("writer-dev")
		writer.post("@kiro-dev", "a question")
		srv.clock.advance(time.Second)
		sh.hook("prompt", sh.fixture("prompt"))
		got := sh.hook("stop", sh.fixture("stop"))
		b := parseBlock(t, got.Stdout)
		if got.Code != 0 || !strings.HasPrefix(b.Reason, "grelot (automated, not the user): unread 1 ") || !strings.Contains(b.Reason, "run grelot inbox before you stop") {
			t.Errorf("exit %d, reason %q; want 0 and the unread hold line", got.Code, b.Reason)
		}
		if strings.Contains(got.Stdout, "a question") {
			t.Errorf("stdout %q carries the message body", got.Stdout)
		}
		if body := holdBody(t, got.Requests); body["last"] != true {
			t.Errorf("hold body %v, want last true", body)
		}
	})
	t.Run("a 426 prints nothing at stop", func(t *testing.T) {
		sh := kiroShell(t, newServer(t), "kiro-cli", kiroSession)
		sh.version = "v0.0.1"
		if got := sh.hook("prompt", sh.fixture("prompt")); !strings.HasPrefix(got.Stdout, "grelot: this grelot server") {
			t.Fatalf("prompt stdout %q, want the 426's line, which this test is about", got.Stdout)
		}
		got := sh.hook("stop", sh.fixture("stop"))
		if got.Code != 0 || got.Stdout != "" || got.Stderr != "" {
			t.Errorf("exit %d, stdout %q, stderr %q; want 0 and nothing: the Stop parser reads only decision and reason", got.Code, got.Stdout, got.Stderr)
		}
	})
}

func TestKiroCLISessionEnd(t *testing.T) {
	for _, reason := range []string{"delete", "other", "shutdown", "client_disconnect"} {
		t.Run(reason, func(t *testing.T) {
			srv := newServer(t)
			in := mustRead(t, filepath.Join("testdata", "golden", "kiro-cli", "session_end-"+reason+".json"))
			var payload struct {
				Session string `json:"session_id"`
			}
			if err := json.Unmarshal(in, &payload); err != nil {
				t.Fatal(err)
			}
			sh := kiroShell(t, srv, "kiro-cli", payload.Session)
			sh.register("kiro-dev")
			got := sh.hook("session_end", in)
			if got.Code != 0 || got.Stdout != "" || got.Stderr != "" {
				t.Errorf("exit %d, stdout %q, stderr %q; want 0 and nothing", got.Code, got.Stdout, got.Stderr)
			}
			if reason != "delete" {
				if len(got.Requests) != 0 {
					t.Errorf("requests %v, want none: only a deleted chat has closed", got.Requests)
				}
				return
			}
			if len(got.Requests) != 1 || got.Requests[0].Path != "/v1/lifecycle" || got.Requests[0].Body != `{"event":"session_end"}` ||
				got.Requests[0].Header.Get("Grelot-Session") != "kiro-cli:"+payload.Session {
				t.Errorf("requests %v, want one session_end report as the deleted chat", got.Requests)
			}
		})
	}
	t.Run("a deleted tangent leaves its parent's identity", func(t *testing.T) {
		sh := kiroShell(t, newServer(t), "kiro-cli", tangentParent)
		sh.register("kiro-dev")
		placeRecord(t, sh, tangentSession)
		got := sh.hook("session_end", sh.fixture("session_end-delete"))
		if got.Code != 0 || got.Stdout != "" || len(got.Requests) != 0 {
			t.Errorf("exit %d, stdout %q, requests %v; want 0, nothing and none", got.Code, got.Stdout, got.Requests)
		}
	})
}

// A tangent shares its parent's shell session: its prompt prints the fork's
// line once, and none of its hooks asks the server anything.
func TestKiroCLITangent(t *testing.T) {
	sh := kiroShell(t, newServer(t), "kiro-cli", tangentParent)
	sh.register("kiro-dev")
	placeRecord(t, sh, tangentSession)
	const forkLine = "grelot: this chat is a fork of another chat; it shares that chat's identity and posts only as a delegate the parent grants\n"
	for i, want := range []string{forkLine, ""} {
		got := sh.hook("prompt", sh.fixture("prompt-tangent"))
		if got.Code != 0 || got.Stdout != want || len(got.Requests) != 0 {
			t.Errorf("prompt %d: exit %d, stdout %q, %d requests; want 0, %q and none", i+1, got.Code, got.Stdout, len(got.Requests), want)
		}
	}
	if got := sh.hook("stop", sh.fixture("stop-tangent")); got.Code != 0 || got.Stdout != "" || len(got.Requests) != 0 {
		t.Errorf("stop: exit %d, stdout %q, %d requests; want 0, nothing and none", got.Code, got.Stdout, len(got.Requests))
	}
}

// A /rewind goes on as the same agent: its first prompt carries the
// same-agent note once, and from then on the session it replaced prints
// nothing, whatever wakes it. Each host's note names its own way to stop
// a grelot-watch run.
func TestKiroRewind(t *testing.T) {
	const told = "grelot: this chat is a rewind of an earlier session; a grelot-watch run started before the rewind still wakes the old session, " +
		"which this chat never sees. "
	for _, tc := range []struct{ provider, note string }{
		{"kiro-cli", told + "Ask the user to stop it (ctrl+g, select the run, ctrl+x twice), then start a new one with the start line below."},
		{"kiro-ide", told + "Ask the user to stop it from the Workflows area above the chat, then start a new one with the start line below."},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			sh := kiroShell(t, newServer(t), tc.provider, rewoundRoot)
			sh.register("kiro-dev")
			placeRecord(t, sh, rewindSession)

			got := sh.hook("prompt", sh.fixture("prompt-rewind"))
			lines := strings.Split(strings.TrimSuffix(got.Stdout, "\n"), "\n")
			if got.Code != 0 || len(lines) != 2 || lines[0] != tc.note || !strings.HasPrefix(lines[1], "grelot: @kiro-dev · 0 unread · ") {
				t.Errorf("first prompt: exit %d, stdout %q; want 0, the note %q, then the turn line", got.Code, got.Stdout, tc.note)
			}
			got = sh.hook("prompt", sh.fixture("prompt-rewind"))
			if strings.Contains(got.Stdout, "rewind") || !strings.HasPrefix(got.Stdout, "grelot: @kiro-dev · 0 unread · ") {
				t.Errorf("second prompt: stdout %q, want the turn line alone", got.Stdout)
			}
			if got := sh.hook("stop", sh.fixture("stop-rewind")); got.Code != 0 || len(got.Requests) != 1 {
				t.Errorf("rewind's stop: exit %d, requests %v; want 0 and the one hold", got.Code, got.Requests)
			}
			for _, name := range []string{"prompt-replaced", "stop-replaced"} {
				if got := sh.hook(strings.Split(name, "-")[0], sh.fixture(name)); got.Code != 0 || got.Stdout != "" || len(got.Requests) != 0 {
					t.Errorf("replaced session's %s: exit %d, stdout %q, %d requests; want 0, nothing and none", name, got.Code, got.Stdout, len(got.Requests))
				}
			}
		})
	}
}

func TestKiroIDE(t *testing.T) {
	set, err := provider.LoadSet(providers.FS)
	if err != nil {
		t.Fatal(err)
	}
	ide, err := set.Get("kiro-ide")
	if err != nil {
		t.Fatal(err)
	}
	cli, err := set.Get("kiro-cli")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("it takes kiro-cli's session, hooks and workflow door", func(t *testing.T) {
		if ide.Evidence != provider.EvidenceUnverified || len(ide.Detect.Env) != 0 {
			t.Errorf("evidence %q, detect %v; want unverified and no detect", ide.Evidence, ide.Detect.Env)
		}
		if ide.Session.Env != cli.Session.Env || ide.Session.Stdin != cli.Session.Stdin {
			t.Errorf("session %+v, want kiro-cli's %+v", ide.Session, cli.Session)
		}
		if a, b := ide.Session.Record, cli.Session.Record; a == nil || a.DirEnv != b.DirEnv || a.DirDefault != b.DirDefault || a.Glob != b.Glob ||
			a.Parent != b.Parent || a.Reason != b.Reason || !slices.Equal(a.SameAgent, b.SameAgent) {
			t.Errorf("record %+v, want kiro-cli's %+v", a, b)
		}
		want := wantWiring(t, "kiro-cli", nil)
		for i := range want {
			want[i].Command = strings.Replace(want[i].Command, " kiro-cli ", " kiro-ide ", 1)
		}
		if got := wantWiring(t, "kiro-ide", nil); !slices.Equal(got, want) {
			t.Errorf("kiro-ide spells hooks %v, want kiro-cli's %v", got, want)
		}
		door := kiroDoor(t, "kiro-ide")
		if door.Start != kiroStart || door.Transport != provider.TransportExit || door.Hold != 45*time.Second {
			t.Errorf("door %+v, want kiro-cli's workflow door", door)
		}
	})
	// The IDE's agent server hosts every chat of the window, so no start
	// there names the process that ran another chat.
	t.Run("it names no process", func(t *testing.T) {
		if len(ide.Session.Process) != 0 || len(ide.Session.ProcessShared) != 0 {
			t.Errorf("process %v, shared %v; want none", ide.Session.Process, ide.Session.ProcessShared)
		}
		sh := kiroShell(t, newServer(t), "kiro-ide", kiroTUI)
		sh.procs = under("kiro-cli", "chat")
		sh.register("kiro-dev")
		sh.vars["KIRO_SESSION_ID"] = kiroCleared
		got := sh.hook("session_start", sh.fixture("session_start-clear"))
		if got.Stdout != registerLine+"\n" || len(got.Requests) != 1 || got.Requests[0].Query != "" {
			t.Errorf("stdout %q, requests %v; want only the register line from one GET with no proc", got.Stdout, got.Requests)
		}
	})
	t.Run("a Kiro shell is detected as kiro-cli", func(t *testing.T) {
		p, err := set.Detect("", func(name string) (string, bool) { return kiroSession, name == "KIRO_SESSION_ID" }, nil)
		if err != nil || p.Name != "kiro-cli" {
			t.Errorf("detected %v, %v; want kiro-cli", p, err)
		}
	})
	t.Run("kiro-cli's payloads", func(t *testing.T) {
		sh := kiroShell(t, newServer(t), "kiro-ide", kiroSession)
		got := sh.hook("session_start", sh.fixture("session_start"))
		if got.Stdout != registerLine+"\n" || len(got.Requests) != 1 || got.Requests[0].Header.Get("Grelot-Session") != "kiro-ide:"+kiroSession {
			t.Errorf("session_start: stdout %q, requests %v; want the register line from one GET as kiro-ide:%s", got.Stdout, got.Requests, kiroSession)
		}
		got = sh.hook("stop", sh.fixture("stop"))
		if b := parseBlock(t, got.Stdout); b.Reason != registerLine || holdBody(t, got.Requests)["last"] != true {
			t.Errorf("stop: stdout %q, want the register block from a last hold", got.Stdout)
		}
		if got := sh.hook("session_end", sh.fixture("session_end-other")); got.Stdout != "" || len(got.Requests) != 0 {
			t.Errorf("session_end other: stdout %q, requests %v; want nothing", got.Stdout, got.Requests)
		}
	})
}
