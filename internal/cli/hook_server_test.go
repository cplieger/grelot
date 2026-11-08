//go:build !windows

package cli

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/client"
)

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
	want := registerStart + "\n"
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
	if code != 0 || !strings.HasPrefix(out, "grelot: your identity @toolbelt-dev expired") ||
		!strings.Contains(out, "grelot revive --ttl=<d>") || !strings.Contains(out, "dormant since 2026-11-05 11:00 UTC") {
		t.Errorf("exit %d, stdout %q; want the revive offer, then the dormant card", code, out)
	}
}

// A start prints the richest of its texts whose rendered bytes fit the
// host's context limit: the whole text, then without the manual, then
// without the card, the last printed whatever its size.
func TestHookSessionStart_contextLimit(t *testing.T) {
	const event = "    event: Start\n"
	limited := func(limit int, template string) string {
		file := strings.Replace(hookAlphaFile, event, event+"    context_limit: "+strconv.Itoa(limit)+"\n", 1)
		return strings.Replace(file, `{text: "{{.Context}}"}`, template, 1)
	}
	start := func(t *testing.T, a *agent, file string) string {
		t.Helper()
		hooked(t, a, "alpha", file)
		code, out, errOut := a.hook("session_start", `{"session_id":"s-1","source":"startup"}`)
		if code != 0 || errOut != "" {
			t.Fatalf("exit %d, stderr %q; want 0 and nothing on stderr", code, errOut)
		}
		return out
	}
	ts := newServer(t)
	a, _ := registerAgent(t, ts, "s-1")
	full := start(t, a, hookAlphaFile)
	card := full[:strings.Index(full, "grelot: @toolbelt-dev · 0 unread")]
	status := full[len(card) : len(card)+strings.Index(full[len(card):], "\n")+1]
	if !strings.HasPrefix(card, "@toolbelt-dev · box") || !strings.HasSuffix(full, manual) {
		t.Fatalf("stdout %q, want the card, the status line and the manual with no limit", full)
	}
	noManual := card + status + lineManualLeftOut + "\n"
	noCard := status + lineCardLeftOut + "\n"

	cases := []struct {
		name, template, want string
		limit                int
	}{
		{"the whole text at its size", `{text: "{{.Context}}"}`, full, len(full)},
		{"no manual a byte under it", `{text: "{{.Context}}"}`, noManual, len(full) - 1},
		{"no manual at its size", `{text: "{{.Context}}"}`, noManual, len(noManual)},
		{"no card a byte under it", `{text: "{{.Context}}"}`, noCard, len(noManual) - 1},
		{"no card under its own size", `{text: "{{.Context}}"}`, noCard, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := start(t, a, limited(c.limit, c.template)); got != c.want {
				t.Errorf("limit %d: stdout %q, want %q", c.limit, got, c.want)
			}
		})
	}
	t.Run("the rendered bytes count", func(t *testing.T) {
		out := start(t, a, limited(len(noManual), `{text: '{"c":{{json .Context}}}'}`))
		if !strings.HasPrefix(out, `{"c":`) || strings.Contains(out, "@toolbelt-dev · box") || !strings.Contains(out, "`grelot me` for your card") {
			t.Errorf("stdout %q, want the envelope around the text without the card, whose raw bytes alone fit", out)
		}
	})
	t.Run("a dormant card keeps the offer first", func(t *testing.T) {
		ts.clock.advance(3 * time.Hour)
		out := start(t, a, limited(2000, `{text: "{{.Context}}"}`))
		if !strings.HasPrefix(out, "grelot: your identity @toolbelt-dev expired") || !strings.Contains(out, "dormant since ") ||
			!strings.HasSuffix(out, lineManualLeftOut+"\n") || len(out) > 2000 {
			t.Errorf("stdout %q, want the revive offer, the dormant card and the left-out line within 2000 bytes", out)
		}
	})
}

func TestHookSessionStart_takeOverHint(t *testing.T) {
	ts := newServer(t)
	a, _ := registerAgent(t, ts, "s-1")
	newSession := func(t *testing.T) *agent {
		b := hooked(t, newAgent(t, ts, "s-2", ""))
		b.env.ConfigDir = a.env.ConfigDir
		return b
	}
	hint := "grelot: you were @toolbelt-dev before this session started; to go on as it, run `grelot register toolbelt-dev --take-over --ttl=<d>`"

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

func TestHookSessionStart_takeOverEveryStart(t *testing.T) {
	const sourceLine = "    source: {path: source, take_over: [clear], fork: [branch]}\n"
	files := map[string]string{
		"with a fork source": strings.Replace(hookAlphaFile, sourceLine, "    take_over_every_start: true\n    source: {path: source, fork: [branch]}\n", 1),
		"with no source":     strings.Replace(hookAlphaFile, sourceLine, "    take_over_every_start: true\n", 1),
	}
	for name, file := range files {
		t.Run(name, func(t *testing.T) {
			ts := newServer(t)
			a, _ := registerAgent(t, ts, "s-1")
			for _, stdin := range []string{`{"session_id":"s-2","source":"startup"}`, `{"session_id":"s-2"}`} {
				b := hooked(t, newAgent(t, ts, "s-2", ""), "alpha", file)
				b.env.ConfigDir = a.env.ConfigDir
				if _, out, _ := b.hook("session_start", stdin); !strings.Contains(out, "you were @toolbelt-dev") {
					t.Errorf("%s: stdout %q, want the take-over hint", stdin, out)
				}
				if got := ts.last(t, http.MethodGet, "/v1/me"); !strings.Contains(got.URL, "proc=200%3A20") {
					t.Errorf("%s: URL = %q, want the CLI ancestor's proc", stdin, got.URL)
				}
			}
		})
	}
	t.Run("a fork source does not", func(t *testing.T) {
		ts := newServer(t)
		a, _ := registerAgent(t, ts, "s-1")
		b := hooked(t, newAgent(t, ts, "s-2", ""), "alpha", files["with a fork source"])
		b.env.ConfigDir = a.env.ConfigDir
		if _, out, _ := b.hook("session_start", `{"session_id":"s-2","source":"branch"}`); strings.Contains(out, "you were") {
			t.Errorf("stdout %q, want no hint", out)
		}
		if got := ts.last(t, http.MethodGet, "/v1/me"); strings.Contains(got.URL, "proc=") {
			t.Errorf("URL = %q, want no proc", got.URL)
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
	want := `{"context": "` + registerStart + `"}` + "\n"
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
		"no variable in the shell, a session in the input, no hooks_without_env": func(a *agent) []string {
			delete(a.vars, "ALPHA_ID")
			a.env.Stdin = strings.NewReader(`{"session_id":"s-1","source":"startup"}`)
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

func TestHookSessionStart_anOldClientGetsThe426(t *testing.T) {
	ts := newServer(t)
	a := hooked(t, newAgent(t, ts, "s-1", ""))
	a.env.Version = "v0.0.1"
	code, out, _ := a.hook("session_start", `{"session_id":"s-1"}`)
	if code != 0 || !strings.HasPrefix(out, "grelot: this grelot server (v1.1.0-dev.4) does not speak client v0.0.1; install grelot") {
		t.Errorf("exit %d, stdout %q; want 0 and the 426's line", code, out)
	}
}

// A hook on a client with no config leaves no state behind and prints at
// most the one could-not-check line naming the missing file, whichever
// role runs and whatever kind of session it runs for.
func TestHook_noClientConfigWritesNothing(t *testing.T) {
	a := hooked(t, newAgent(t, &testServer{URL: unreachableURL(t)}, "s-1", ""), "alpha", hookTurnFile)
	file := client.ConfigFile(a.env.ConfigDir)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	a.record("tangent", `{"parent":"s-1","reason":"branch"}`)
	a.record("r1", `{"parent":"s-1","reason":"rewind"}`)
	runs := []struct{ role, stdin string }{
		{"session_start", `{"session_id":"s-1"}`},
		{"prompt", `{"session_id":"s-1","prompt":"x"}`},
		{"stop", `{"session_id":"s-1"}`},
		{"turn_end", `{"session_id":"s-1"}`},
		{"session_end", `{"session_id":"s-1","reason":"delete"}`},
		{"session_start", `{"session_id":"tangent"}`},
		{"prompt", `{"session_id":"tangent","prompt":"x"}`},
		{"prompt", `{"session_id":"r1","prompt":"x"}`},
		{"session_start", `{"session_id":"sub-1","agent_id":"x"}`},
	}
	for _, r := range runs {
		code, out, errOut := a.hook(r.role, r.stdin)
		if code != 0 || errOut != "" || strings.Count(out, "\n") > 1 || (out != "" && !strings.Contains(out, file)) {
			t.Errorf("%s %s: exit %d, stdout %q, stderr %q; want 0 and at most one line naming %s", r.role, r.stdin, code, out, errOut, file)
		}
	}
	if _, err := os.Stat(filepath.Join(client.Dir(a.env.ConfigDir), "state")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("state folder: %v, want none written", err)
	}
}
