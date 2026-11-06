package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
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
	closed := unreachableURL(t)

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
	closed := unreachableURL(t)

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
