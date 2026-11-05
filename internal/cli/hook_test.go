package cli

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/client"
)

// hookAlphaFile is alpha with a session_start hook: its stdin carries
// session_id, agent_id and the start source, and each session's record
// lives at <ALPHA_RECORDS>/<id>.json.
const hookAlphaFile = `name: alpha
detect: {env: [ALPHA_ID]}
session:
  env: ALPHA_ID
  stdin: session_id
  process: [alphacli]
  process_shared: [--serve]
  record:
    dir_env: ALPHA_RECORDS
    glob: "{{.HookSession}}.json"
    parent: parent
    reason: reason
    same_agent: [rewind]
    same_agent_note: "this chat is a rewind"
hooks:
  session_start:
    event: Start
    stdin: {agent_id: agent_id}
    source: {path: source, take_over: [clear], fork: [branch]}
    outputs: {context: {text: "{{.Context}}"}}
`

// hooked gives a its hook provider files, with records under a temp dir.
func hooked(t *testing.T, a *agent, files ...string) *agent {
	t.Helper()
	if len(files) == 0 {
		files = []string{"alpha", hookAlphaFile}
	}
	a.env.Providers = providerSet(t, append(files, "beta", betaFile)...)
	a.vars["ALPHA_RECORDS"] = t.TempDir()
	return a
}

// hook runs grelot hook alpha <role> with stdin.
func (a *agent) hook(role, stdin string) (code int, stdout, stderr string) {
	a.env.Stdin = strings.NewReader(stdin)
	defer func() { a.env.Stdin = strings.NewReader("") }()
	return a.run("hook", "alpha", role)
}

func (a *agent) record(session, body string) {
	a.t.Helper()
	if err := os.WriteFile(filepath.Join(a.vars["ALPHA_RECORDS"], session+".json"), []byte(body), 0o600); err != nil {
		a.t.Fatal(err)
	}
}

func TestHookSessionStart_unregistered(t *testing.T) {
	ts := newServer(t)
	a := hooked(t, newAgent(t, ts, "s-1", ""))
	code, out, errOut := a.hook("session_start", `{"session_id":"s-1","source":"startup"}`)
	want := "grelot: not registered. Register before you start work: run `grelot help`, then `grelot register` as it says.\n"
	if code != 0 || out != want || errOut != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want 0 and only %q", code, out, errOut, want)
	}
}

func TestHookSessionStart_live(t *testing.T) {
	ts := newServer(t)
	a, _ := registerAgent(t, ts, "s-1")
	hooked(t, a)
	for _, source := range []string{"startup", "resume", "compact"} {
		t.Run(source, func(t *testing.T) {
			code, out, _ := a.hook("session_start", `{"session_id":"s-1","source":"`+source+`"}`)
			card := strings.Index(out, "@toolbelt-dev · box")
			status := strings.Index(out, "grelot: @toolbelt-dev · 0 unread")
			help := strings.Index(out, "# grelot help\n")
			if code != 0 || card < 0 || status < card || help < status {
				t.Errorf("exit %d, stdout %q; want 0 and the card, the status line, then the manual", code, out)
			}
		})
	}
	t.Run("it acts for its own identity with Grelot-Self", func(t *testing.T) {
		got := ts.last(t, http.MethodGet, "/v1/me")
		if got.Header.Get("Grelot-Self") != "1" || got.Header.Get("Grelot-Session") != "alpha:s-1" {
			t.Errorf("headers = %v, want Grelot-Self 1 and the session", got.Header)
		}
	})
}

func TestHookSessionStart_dormantGetsTheReviveOffer(t *testing.T) {
	ts := newServer(t)
	a, _ := registerAgent(t, ts, "s-1")
	hooked(t, a)
	ts.clock.advance(3 * time.Hour)
	code, out, _ := a.hook("session_start", `{"session_id":"s-1","source":"resume"}`)
	if code != 0 || !strings.HasPrefix(out, "grelot: your identity toolbelt-dev expired") ||
		!strings.Contains(out, "grelot revive --ttl <d>") || !strings.Contains(out, "dormant since 2026-11-05 11:00 UTC") {
		t.Errorf("exit %d, stdout %q; want the revive offer, then the dormant card", code, out)
	}
}

func TestHookSessionStart_takeOverHint(t *testing.T) {
	ts := newServer(t)
	a, _ := registerAgent(t, ts, "s-1")
	newSession := func(t *testing.T) *agent {
		b := hooked(t, newAgent(t, ts, "s-2", ""))
		b.env.ConfigDir = a.env.ConfigDir
		return b
	}
	hint := "grelot: you were toolbelt-dev before this session started; to go on as it, run `grelot register toolbelt-dev --take-over --ttl <d>`"

	t.Run("a take_over source gets it", func(t *testing.T) {
		b := newSession(t)
		code, out, _ := b.hook("session_start", `{"session_id":"s-2","source":"clear"}`)
		if code != 0 || !strings.HasPrefix(out, "grelot: not registered.") || !strings.Contains(out, hint) {
			t.Errorf("exit %d, stdout %q; want the register line, then %q", code, out, hint)
		}
		got := ts.last(t, http.MethodGet, "/v1/me")
		if !strings.Contains(got.URL, "proc=200%3A20") {
			t.Errorf("URL = %q, want the CLI ancestor's proc", got.URL)
		}
		if got.Header.Get("Grelot-Machine") != testMachine || got.Header.Get("Grelot-Environment") == "" {
			t.Errorf("headers = %v, want the machine and environment", got.Header)
		}
		if strings.Contains(got.URL, "environment") || strings.Contains(got.URL, "0123456789abcdef") {
			t.Errorf("URL = %q carries where values, which go only in headers", got.URL)
		}
	})
	for _, source := range []string{"startup", "branch"} {
		t.Run("source "+source+" does not", func(t *testing.T) {
			b := newSession(t)
			code, out, _ := b.hook("session_start", `{"session_id":"s-2","source":"`+source+`"}`)
			if code != 0 || !strings.HasPrefix(out, "grelot: not registered.") || strings.Contains(out, "you were") {
				t.Errorf("exit %d, stdout %q; want only the register line", code, out)
			}
			if got := ts.last(t, http.MethodGet, "/v1/me"); strings.Contains(got.URL, "proc=") {
				t.Errorf("URL = %q, want no proc", got.URL)
			}
		})
	}
	t.Run("another environment does not", func(t *testing.T) {
		c := hooked(t, newAgent(t, ts, "s-3", ""))
		if _, out, _ := c.hook("session_start", `{"session_id":"s-3","source":"clear"}`); strings.Contains(out, "you were") {
			t.Errorf("stdout %q, want no hint", out)
		}
	})
}

func TestHookSessionStart_children(t *testing.T) {
	ts := newServer(t)
	a, _ := registerAgent(t, ts, "s-1")
	hooked(t, a)
	a.record("step", `{}`)
	a.record("tangent", `{"parent":"s-1","reason":"tangent"}`)

	quiet := map[string]string{
		"a subagent sharing the session": `{"session_id":"s-1","agent_id":"sub-1"}`,
		"a workflow step":                `{"session_id":"step"}`,
		"a session with no record":       `{"session_id":"lost"}`,
	}
	for name, stdin := range quiet {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			before := len(ts.requests())
			code, out, errOut := a.hook("session_start", stdin)
			if code != 0 || out != "" || errOut != "" {
				t.Errorf("exit %d, stdout %q, stderr %q; want 0 and nothing", code, out, errOut)
			}
			if n := len(ts.requests()) - before; n != 0 {
				t.Errorf("%d requests, want none for the parent", n)
			}
		})
	}
	t.Run("a fork gets its one line once", func(t *testing.T) {
		want := "grelot: this chat is a fork of another chat; it shares that chat's identity and posts only as a delegate the parent grants\n"
		if _, out, _ := a.hook("session_start", `{"session_id":"tangent"}`); out != want {
			t.Errorf("first: stdout %q, want %q", out, want)
		}
		if _, out, _ := a.hook("session_start", `{"session_id":"tangent"}`); out != "" {
			t.Errorf("second: stdout %q, want nothing", out)
		}
	})
}

func TestHookSessionStart_rewindReplacesItsParent(t *testing.T) {
	ts := newServer(t)
	a, _ := registerAgent(t, ts, "s-1")
	hooked(t, a)
	a.record("r-1", `{"parent":"s-1","reason":"rewind"}`)
	if _, out, _ := a.hook("session_start", `{"session_id":"r-1","source":"resume"}`); !strings.Contains(out, "@toolbelt-dev · box") {
		t.Errorf("the rewind: stdout %q, want the environment identity's card", out)
	}
	if code, out, _ := a.hook("session_start", `{"session_id":"s-1","source":"resume"}`); code != 0 || out != "" {
		t.Errorf("the replaced session: exit %d, stdout %q; want 0 and nothing", code, out)
	}
}

func TestHookSessionStart_delegateInItsOwnSession(t *testing.T) {
	ts := newServer(t)
	a, _ := registerAgent(t, ts, "s-1")
	hooked(t, a)
	var grant struct {
		Link string `json:"link"`
	}
	ts.call(t, "alpha:s-1", false, &client.Request{
		Method: http.MethodPost, Path: "/v1/delegates", Body: map[string]any{"label": "review", "ttl": "1h"},
	}, &grant)
	sub := newAgent(t, ts, "sub-1", "")
	sub.must("register", "--delegate", grant.Link, "--desc", "reviewing the diff")

	code, out, _ := a.hook("session_start", `{"session_id":"sub-1","agent_id":"x"}`)
	want := "grelot: you are @toolbelt-dev/review; prefix every grelot command with `grelot as toolbelt-dev/review`\n"
	if code != 0 || out != want {
		t.Errorf("exit %d, stdout %q; want %q", code, out, want)
	}
	if got := ts.last(t, http.MethodGet, "/v1/me"); got.Header.Get("Grelot-Session") != "alpha:sub-1" || got.Header.Get("Grelot-Self") != "" {
		t.Errorf("headers = %v, want the subagent's own session and no Grelot-Self", got.Header)
	}
	t.Run("an own session holding no delegate prints nothing", func(t *testing.T) {
		if _, out, _ := a.hook("session_start", `{"session_id":"nobody","agent_id":"y"}`); out != "" {
			t.Errorf("stdout %q, want nothing", out)
		}
	})
	t.Run("an own session holding an identity of its own prints nothing", func(t *testing.T) {
		own := newAgent(t, ts, "sub-2", "")
		own.must("register", "notes-dev", "--paths", "/workspace/notes", "--desc", "notes", "--ttl", "1h")
		if _, out, _ := a.hook("session_start", `{"session_id":"sub-2","agent_id":"z"}`); out != "" {
			t.Errorf("stdout %q, want nothing, since only a delegate is named by its handle", out)
		}
	})
}

func TestHookSessionStart_once(t *testing.T) {
	ts := newServer(t)
	a := hooked(t, newAgent(t, ts, "s-1", ""), "alpha", strings.Replace(hookAlphaFile, "    event: Start\n", "    event: Start\n    once: true\n", 1))
	if _, out, _ := a.hook("session_start", `{"session_id":"s-1"}`); !strings.HasPrefix(out, "grelot: not registered.") {
		t.Errorf("first: stdout %q, want the register line", out)
	}
	if _, out, _ := a.hook("session_start", `{"session_id":"s-1"}`); out != "" {
		t.Errorf("second: stdout %q, want nothing", out)
	}
}

func TestHookSessionStart_when(t *testing.T) {
	ts := newServer(t)
	a := hooked(t, newAgent(t, ts, "s-1", ""), "alpha", strings.Replace(hookAlphaFile, "    event: Start\n", "    event: Start\n    when: {source: [startup]}\n", 1))
	if _, out, _ := a.hook("session_start", `{"session_id":"s-1","source":"resume"}`); out != "" {
		t.Errorf("resume: stdout %q, want nothing", out)
	}
	if _, out, _ := a.hook("session_start", `{"session_id":"s-1","source":"startup"}`); out == "" {
		t.Error("startup: printed nothing, want the register line")
	}
}

func TestHookSessionStart_outputStreamAndExit(t *testing.T) {
	ts := newServer(t)
	file := strings.Replace(hookAlphaFile, `outputs: {context: {text: "{{.Context}}"}}`,
		`outputs: {context: {text: '{"context": {{json .Context}}}', stream: stderr, exit: 3}}`, 1)
	a := hooked(t, newAgent(t, ts, "s-1", ""), "alpha", file)
	code, out, errOut := a.hook("session_start", `{"session_id":"s-1"}`)
	want := `{"context": "grelot: not registered. Register before you start work: run ` + "`grelot help`, then `grelot register`" + ` as it says."}` + "\n"
	if code != 3 || out != "" || errOut != want {
		t.Errorf("exit %d, stdout %q, stderr %q; want 3 and %q on stderr", code, out, errOut, want)
	}
}

func TestHook_writesNothing(t *testing.T) {
	ts := newServer(t)
	cases := map[string]func(a *agent) []string{
		"an unknown provider": func(*agent) []string { return []string{"hook", "nosuch", "session_start"} },
		"an invalid provider file": func(a *agent) []string {
			hooked(t, a, "alpha", hookAlphaFile+"stray: key\n")
			return []string{"hook", "alpha", "session_start"}
		},
		"no session in the shell or the input": func(a *agent) []string {
			delete(a.vars, "ALPHA_ID")
			return []string{"hook", "alpha", "session_start"}
		},
		"a provider with no session.env": func(a *agent) []string {
			hooked(t, a, "alpha", strings.Replace(hookAlphaFile, "  env: ALPHA_ID\n", "", 1))
			return []string{"hook", "alpha", "session_start"}
		},
		"a role the provider does not spell": func(*agent) []string { return []string{"hook", "alpha", "stop"} },
		"an unknown role":                    func(*agent) []string { return []string{"hook", "alpha", "after_tool"} },
		"input that is not json": func(a *agent) []string {
			a.env.Stdin = strings.NewReader("not json")
			return []string{"hook", "alpha", "session_start"}
		},
	}
	for name, setup := range cases {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			a := hooked(t, newAgent(t, ts, "s-1", ""))
			args := setup(a)
			before := len(ts.requests())
			code := Run(t.Context(), a.env, args)
			if code != 0 || a.stdout.Len() != 0 || a.stderr.Len() != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q; want 0 and nothing", code, a.stdout, a.stderr)
			}
			if n := len(ts.requests()) - before; n != 0 {
				t.Errorf("%d requests, want none", n)
			}
		})
	}
}

// The hook fails open: when the server cannot say whether the session has
// an identity, it prints one line and holds nothing.
func TestHookSessionStart_failsOpen(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"store unavailable","code":"unavailable"}`))
	}))
	t.Cleanup(failing.Close)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + l.Addr().String()
	_ = l.Close()

	budgeted := strings.Replace(hookAlphaFile, "    event: Start\n", "    event: Start\n    budget: 300ms\n", 1)
	cases := []struct {
		name, url, why string
	}{
		{name: "unreachable", url: closed, why: "the server cannot be reached"},
		{name: "a 5xx", url: failing.URL, why: "the server answered 503: store unavailable"},
		{name: "over budget", url: slow.URL, why: "no answer within 300ms"},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			a := hooked(t, newAgent(t, &testServer{URL: c.url}, "s-1", ""), "alpha", budgeted)
			start := time.Now()
			code, out, _ := a.hook("session_start", `{"session_id":"s-1"}`)
			want := "grelot: could not check this session's identity (" + c.why + "); nothing is held\n"
			if code != 0 || out != want {
				t.Errorf("exit %d, stdout %q; want 0 and %q", code, out, want)
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Errorf("took %s, want the 300ms budget to end it", d)
			}
		})
	}
}

// A subagent's own session fails open like its parent: when its check gets
// no answer it prints the one could-not-check line, within the role's
// budget, and holds nothing.
func TestHookSessionStart_aSubagentFailsOpen(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + l.Addr().String()
	_ = l.Close()

	budgeted := strings.Replace(hookAlphaFile, "    event: Start\n", "    event: Start\n    budget: 300ms\n", 1)
	const subagent = `{"session_id":"sub-1","agent_id":"x"}`
	check := func(t *testing.T, a *agent, why string) {
		t.Helper()
		start := time.Now()
		code, out, _ := a.hook("session_start", subagent)
		want := "grelot: could not check this session's identity (" + why + "); nothing is held\n"
		if code != 0 || out != want {
			t.Errorf("exit %d, stdout %q; want 0 and %q", code, out, want)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("took %s, want the 300ms budget to end it", d)
		}
	}
	t.Run("over_budget", func(t *testing.T) {
		check(t, hooked(t, newAgent(t, &testServer{URL: slow.URL}, "s-1", ""), "alpha", budgeted), "no answer within 300ms")
	})
	t.Run("unreachable", func(t *testing.T) {
		check(t, hooked(t, newAgent(t, &testServer{URL: closed}, "s-1", ""), "alpha", budgeted), "the server cannot be reached")
	})
	t.Run("no_client_config", func(t *testing.T) {
		a := hooked(t, newAgent(t, &testServer{URL: closed}, "s-1", ""), "alpha", budgeted)
		file := client.ConfigFile(a.env.ConfigDir)
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		code, out, _ := a.hook("session_start", subagent)
		if code != 0 || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, "grelot: could not check this session's identity (") ||
			!strings.Contains(out, file) || !strings.HasSuffix(out, "); nothing is held\n") {
			t.Errorf("exit %d, stdout %q; want 0 and one could-not-check line naming %s", code, out, file)
		}
	})
}

func TestHookSessionStart_anOldClientGetsThe426(t *testing.T) {
	ts := newServer(t)
	a := hooked(t, newAgent(t, ts, "s-1", ""))
	a.env.Version = "v0.0.1"
	code, out, _ := a.hook("session_start", `{"session_id":"s-1"}`)
	if code != 0 || !strings.HasPrefix(out, "grelot: this grelot server (v1.1.0-dev.4) does not speak client v0.0.1; install grelot") {
		t.Errorf("exit %d, stdout %q; want 0 and the 426's line", code, out)
	}
}
