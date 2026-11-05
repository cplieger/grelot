package providers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/transport"
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
)

// registerLine is the turn-start text of a session with no identity.
const registerLine = "grelot: not registered. Register before you start work: run `grelot help`, then `grelot register` as it says."

// kiroStart is the workflow door's start line, which names where the
// recipe is installed.
const kiroStart = "start the grelot-watch workflow (~/.kiro/workflows/grelot-watch.workflow.json)"

// kiroShell is a shell of the named provider, kiro-cli or kiro-ide, whose
// KIRO_SESSION_ID is session, with its own KIRO_HOME.
func kiroShell(t *testing.T, srv *testServer, name, session string) *shell {
	t.Helper()
	sh := newShell(t, srv, name, map[string]string{"KIRO_SESSION_ID": session, "KIRO_HOME": t.TempDir()})
	sh.fixtures = "kiro-cli"
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
// nothing, whatever wakes it.
func TestKiroCLIRewind(t *testing.T) {
	sh := kiroShell(t, newServer(t), "kiro-cli", rewoundRoot)
	sh.register("kiro-dev")
	placeRecord(t, sh, rewindSession)
	const note = "grelot: this chat is a rewind of an earlier session; a grelot-watch run started before the rewind still wakes the old session, " +
		"which this chat never sees. Ask the user to stop it (ctrl+g, select the run, ctrl+x twice), then start a new one with the start line below."

	got := sh.hook("prompt", sh.fixture("prompt-rewind"))
	lines := strings.Split(strings.TrimSuffix(got.Stdout, "\n"), "\n")
	if got.Code != 0 || len(lines) != 2 || lines[0] != note || !strings.HasPrefix(lines[1], "grelot: @kiro-dev · 0 unread · ") {
		t.Errorf("first prompt: exit %d, stdout %q; want 0, the note, then the turn line", got.Code, got.Stdout)
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
}

func TestKiroCLIWorkflowDoor(t *testing.T) {
	door := kiroDoor(t, "kiro-cli")
	if door.Transport != provider.TransportExit || door.Layer != 2 || door.Hold != 45*time.Second || door.Start != kiroStart {
		t.Errorf("door %s: %s at layer %d holding %s, start %q; want an exit door at layer 2 holding 45s", door.Name, door.Transport, door.Layer, door.Hold, door.Start)
	}
	signal := transport.SignalLine(7, 2)
	end := transport.EndLine(transport.EndEnded)
	cases := []struct {
		run  func(*transport.Watch) (transport.Step, error)
		want map[string]any
		name string
	}{
		{name: "a nudge", want: map[string]any{"outcome": "new-activity", "cursor": nil, "payload": signal}, run: func(w *transport.Watch) (transport.Step, error) {
			return w.Nudge(t.Context(), &provider.Data{Line: signal})
		}},
		{name: "the hold lapsing", want: map[string]any{"outcome": "idle", "cursor": nil}, run: func(w *transport.Watch) (transport.Step, error) {
			return w.Idle(t.Context(), &provider.Data{})
		}},
		{name: "the end", want: map[string]any{"outcome": "terminal-state", "cursor": nil, "payload": end}, run: func(w *transport.Watch) (transport.Step, error) {
			return w.End(t.Context(), &provider.Data{Line: end})
		}},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			var out bytes.Buffer
			w, err := transport.NewWatch(door, &out, nil)
			if err != nil {
				t.Fatal(err)
			}
			step, err := c.run(w)
			if err != nil {
				t.Fatal(err)
			}
			if !step.Exit || step.Code != 0 {
				t.Errorf("step %+v, want exit 0: the watch reads any other code as idle without reading the line", step)
			}
			if strings.Count(out.String(), "\n") != 1 || !strings.HasSuffix(out.String(), "\n") {
				t.Errorf("stdout %q, want one line", out.String())
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("stdout %q is not JSON: %v", out.String(), err)
			}
			if len(got) != len(c.want) {
				t.Errorf("result %v, want %v", got, c.want)
			}
			for k, v := range c.want {
				if g, ok := got[k]; !ok || g != v {
					t.Errorf("%s = %v, want %v", k, g, v)
				}
			}
		})
	}
}

func kiroDoor(t *testing.T, name string) *provider.Door {
	t.Helper()
	set, err := provider.LoadSet(providers.FS)
	if err != nil {
		t.Fatal(err)
	}
	p, err := set.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(p.Doors, func(d provider.Door) bool { return d.Name == "workflow" })
	if i < 0 {
		t.Fatalf("%s has no workflow door", name)
	}
	return &p.Doors[i]
}

// recipeNode is one node of a workflow recipe, with the keys the
// grelot-watch run depends on.
type recipeNode struct {
	Config          map[string]any `json:"config"`
	Type            string         `json:"type"`
	ID              string         `json:"id"`
	OnMaxIterations string         `json:"onMaxIterations"`
	StopWhen        string         `json:"stopWhen"`
	Handler         string         `json:"handler"`
	Agent           string         `json:"agent"`
	Prompt          string         `json:"prompt"`
	Steps           []recipeNode   `json:"steps"`
	MaxIterations   int            `json:"maxIterations"`
}

func TestKiroCLIWatchRecipe(t *testing.T) {
	door := kiroDoor(t, "kiro-cli")
	if door.Asset != "kiro-cli/grelot-watch.workflow.json" || !strings.Contains(door.Start, "/"+path.Base(door.Asset)+")") {
		t.Errorf("asset %q, start %q; want the recipe, installed under its own name", door.Asset, door.Start)
	}
	var recipe struct {
		Name  string       `json:"name"`
		Steps []recipeNode `json:"steps"`
	}
	if err := json.Unmarshal(mustRead(t, door.Asset), &recipe); err != nil {
		t.Fatal(err)
	}
	if recipe.Name != "grelot-watch" || len(recipe.Steps) != 1 {
		t.Fatalf("recipe %q with %d top nodes, want grelot-watch with one", recipe.Name, len(recipe.Steps))
	}
	outer := recipe.Steps[0]
	t.Run("two nested repeats that stop on the end output", func(t *testing.T) {
		if outer.Type != "repeat" || outer.MaxIterations != 1000 || outer.OnMaxIterations != "pause" || outer.StopWhen != "watch.terminal" {
			t.Errorf("outer %+v, want a repeat of 1000 that pauses at its cap and stops on watch.terminal", outer)
		}
		if len(outer.Steps) != 1 {
			t.Fatalf("outer holds %d nodes, want the inner repeat", len(outer.Steps))
		}
		inner := outer.Steps[0]
		if inner.Type != "repeat" || inner.MaxIterations != 1000 || inner.OnMaxIterations != "continue" || inner.StopWhen != "watch.terminal" {
			t.Errorf("inner %+v, want a repeat of 1000 that continues at its cap and stops on watch.terminal", inner)
		}
	})
	if len(outer.Steps) != 1 || len(outer.Steps[0].Steps) != 2 {
		t.Fatal("the inner repeat does not hold the watch and the notifier")
	}
	watch, notify := outer.Steps[0].Steps[0], outer.Steps[0].Steps[1]
	t.Run("a command watch running one poll", func(t *testing.T) {
		if watch.Type != "watch" || watch.ID != "watch" || watch.Handler != "command" {
			t.Errorf("watch %+v, want a command watch with id watch", watch)
		}
		want := map[string]any{"command": "grelot watch --door workflow", "pollIntervalSec": 10.0, "commandTimeoutSec": 300.0}
		if len(watch.Config) != len(want) {
			t.Errorf("config %v, want %v", watch.Config, want)
		}
		for k, v := range want {
			if watch.Config[k] != v {
				t.Errorf("config %s = %v, want %v", k, watch.Config[k], v)
			}
		}
		if hold := door.Hold.Seconds(); hold >= 300 {
			t.Errorf("hold %vs, want well under the 300 s command timeout", hold)
		}
	})
	t.Run("the notifier relays the watch's output", func(t *testing.T) {
		if notify.Type != "step" || notify.Agent != "grelot-notifier" || notify.Prompt != "{{watch.output}}" {
			t.Errorf("notifier %+v, want a grelot-notifier step prompted with the watch's output", notify)
		}
		agent := string(mustRead(t, filepath.Join("kiro-cli", "grelot-notifier.md")))
		front, _, ok := strings.Cut(strings.TrimPrefix(agent, "---\n"), "\n---\n")
		if !strings.HasPrefix(agent, "---\n") || !ok {
			t.Fatalf("grelot-notifier.md has no front matter")
		}
		lines := strings.Split(front, "\n")
		if !slices.Contains(lines, "name: grelot-notifier") || !slices.Contains(lines, "tools: []") {
			t.Errorf("front matter %q, want name grelot-notifier and no tools", front)
		}
		if !strings.Contains(agent, "`send_message`") || !strings.Contains(agent, "`info`") {
			t.Errorf("grelot-notifier.md does not relay the line with send_message at info")
		}
	})
}

func TestKiroHookFiles(t *testing.T) {
	wantEvents := []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"}
	for _, name := range []string{"kiro-cli", "kiro-ide"} {
		t.Run(name, func(t *testing.T) {
			want := wantWiring(t, name, nil)
			var events []string
			for _, w := range want {
				events = append(events, w.Event)
			}
			if !slices.Equal(events, wantEvents) {
				t.Errorf("the provider spells events %v, want %v", events, wantEvents)
			}
			if got := hookWiring(t, name); !slices.Equal(got, want) {
				t.Errorf("hooks.json wires %v, want %v", got, want)
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
		if ide.Session.Env != cli.Session.Env || ide.Session.Stdin != cli.Session.Stdin || ide.Session.Record == nil {
			t.Errorf("session %+v, want kiro-cli's %+v", ide.Session, cli.Session)
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
