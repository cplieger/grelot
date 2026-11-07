//go:build !windows

package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/where"
)

func TestRun_unknownCommand(t *testing.T) {
	a := newAgent(t, newServer(t), "s-1", "")
	code, _, errOut := a.run("bogus")
	if code != 2 || !strings.Contains(errOut, `unknown command "bogus"`) || !strings.Contains(errOut, "grelot help") {
		t.Errorf("exit %d, stderr %q; want 2 naming the command and grelot help", code, errOut)
	}
}

func TestRun_helpFlagsPrintTheManual(t *testing.T) {
	a := newAgent(t, newServer(t), "s-1", "")
	want := a.must("help")
	for _, flag := range []string{"--help", "-h"} {
		t.Run(flag, func(t *testing.T) {
			code, out, errOut := a.run(flag)
			if code != 0 || out != want || errOut != "" {
				t.Errorf("grelot %s: exit %d, stdout %d bytes, stderr %q; want 0 and grelot help's text", flag, code, len(out), errOut)
			}
		})
	}
}

func TestRun_usageErrorsNameTheCommandsHelp(t *testing.T) {
	a := newAgent(t, newServer(t), "s-1", "")
	for _, args := range [][]string{{"register"}, {"register", "a", "b"}, {"me", "--bogus"}, {"renew", "--ttl"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, _, errOut := a.run(args...)
			if code != 2 || !strings.Contains(errOut, "run grelot "+args[0]+" --help") {
				t.Errorf("exit %d, stderr %q; want 2 naming grelot %s --help", code, errOut, args[0])
			}
		})
	}
}

func TestRun_commandHelpPrintsItsSection(t *testing.T) {
	a := newAgent(t, newServer(t), "s-1", "")
	for _, args := range [][]string{{"register", "--help"}, {"help", "register"}} {
		out := a.must(args...)
		if !strings.HasPrefix(out, "### grelot register\n") || strings.Contains(out, "### grelot revive") ||
			!strings.Contains(out, "--take-over") {
			t.Errorf("grelot %s = %q, want register's section alone", strings.Join(args, " "), out)
		}
	}
}

func TestRun_helpNeedsNoConfig(t *testing.T) {
	a := newAgent(t, newServer(t), "s-1", "")
	a.env.ConfigDir = t.TempDir()
	if out := a.must("help"); out != manual {
		t.Errorf("grelot help printed %d bytes, want the manual's %d", len(out), len(manual))
	}
	code, _, errOut := a.run("help", "bogus")
	if code != 2 || !strings.Contains(errOut, "grelot help lists every command") {
		t.Errorf("exit %d, stderr %q; want 2 pointing at grelot help", code, errOut)
	}
}

func TestRun_noConfigNamesTheFile(t *testing.T) {
	a := newAgent(t, newServer(t), "s-1", "")
	a.env.ConfigDir = t.TempDir()
	code, _, errOut := a.run("me")
	if code != 1 || !strings.Contains(errOut, filepath.Join(a.env.ConfigDir, "grelot", "config.yaml")) {
		t.Errorf("exit %d, stderr %q; want 1 naming the config file to write", code, errOut)
	}
}

// grelot me in a shell that holds no host session, a plain terminal, checks
// the server and the token, then says why it has no identity to show.
func TestRun_noSession(t *testing.T) {
	ts := newServer(t)
	a := newAgent(t, ts, "s-1", "")
	delete(a.vars, "ALPHA_ID")
	reached := "grelot: the server at " + ts.URL + " answers and accepts this token\n"
	inAgent := "grelot: run grelot me in your agent's shell for its status line and the register command\n"
	t.Run("no provider detected", func(t *testing.T) {
		code, out, errOut := a.run("me")
		if code != 0 || errOut != "" || !strings.HasPrefix(out, reached) || !strings.HasSuffix(out, inAgent) ||
			!strings.Contains(out, "\ngrelot cannot tell which host this shell runs in (none matched)") {
			t.Errorf("exit %d, stdout %q, stderr %q; want 0, the server check, the detection line and where to run it", code, out, errOut)
		}
	})
	t.Run("a provider whose variable is unset", func(t *testing.T) {
		code, out, errOut := a.run("--provider", "alpha", "me")
		want := reached + "grelot: provider alpha keeps its session id in ALPHA_ID, which this shell does not set; run with --provider <name> for the host this shell runs in\n" + inAgent
		if code != 0 || errOut != "" || out != want {
			t.Errorf("exit %d, stdout %q, stderr %q; want 0 and %q", code, out, errOut, want)
		}
	})
	t.Run("a token the server refuses", func(t *testing.T) {
		b := newAgent(t, ts, "s-1", "")
		delete(b.vars, "ALPHA_ID")
		writeClientConfig(t, b.env.ConfigDir, "server: "+ts.URL+"\ntoken: wrong-token\n")
		code, out, errOut := b.run("me")
		if code != 1 || strings.Contains(out, "accepts this token") || !strings.Contains(errOut, client.ConfigFile(b.env.ConfigDir)) {
			t.Errorf("exit %d, stdout %q, stderr %q; want 1 and the refusal naming the config file", code, out, errOut)
		}
	})
	t.Run("an unreachable server", func(t *testing.T) {
		b := newAgent(t, &testServer{URL: unreachableURL(t)}, "s-1", "")
		delete(b.vars, "ALPHA_ID")
		code, out, errOut := b.run("me")
		if code != 1 || out != "" || errOut == "" {
			t.Errorf("exit %d, stdout %q, stderr %q; want 1 and the unreachable line", code, out, errOut)
		}
	})
}

func TestRun_providerFlag(t *testing.T) {
	ts := newServer(t)
	a := newAgent(t, ts, "s-1", "")
	a.vars["BETA_ID"] = "b-1"
	out := a.must("--provider", "beta", "register", "tool-dev", "--paths", "/workspace/tool", "--desc", "x", "--ttl", "1h")
	if got := ts.last(t, http.MethodPost, "/v1/register").Header.Get("Grelot-Session"); got != "beta:b-1" {
		t.Errorf("Grelot-Session = %q, want beta:b-1", got)
	}
	if !strings.Contains(out, "grelot: this shell gives no single provider, not beta; add provider: beta to the client config") {
		t.Errorf("output = %q, want the provider note", out)
	}
	if code, _, errOut := a.run("--provider"); code != 2 || !strings.Contains(errOut, "--provider needs a provider name") {
		t.Errorf("exit %d, stderr %q; want 2 for --provider with no name", code, errOut)
	}
}

func TestRun_selfSendsTheHeader(t *testing.T) {
	ts := newServer(t)
	a := newAgent(t, ts, "s-1", "")
	a.must("--self", "me")
	if got := ts.last(t, http.MethodGet, "/v1/me").Header.Get("Grelot-Self"); got != "1" {
		t.Errorf("Grelot-Self = %q, want 1", got)
	}
	a.must("me")
	if got := ts.last(t, http.MethodGet, "/v1/me").Header.Get("Grelot-Self"); got != "" {
		t.Errorf("Grelot-Self = %q without --self, want none", got)
	}
}

func TestRequests_carryNoKeyOrRawValue(t *testing.T) {
	ts := newServer(t)
	a, clone := registerAgent(t, ts, "s-1")
	a.must("set", "--paths", clone+"/internal")
	a.must("renew", "--ttl", "3h")
	a.must("me")
	a.must("who")
	envKey, err := os.ReadFile(where.EnvironmentFile(a.env.ConfigDir))
	if err != nil {
		t.Fatal(err)
	}
	checkoutKey, err := os.ReadFile(where.CheckoutFile(filepath.Join(clone, ".git")))
	if err != nil {
		t.Fatal(err)
	}
	envID := where.Environment(t.Context(), where.EnvironmentFile(a.env.ConfigDir), false)
	for _, req := range ts.requests() {
		all := req.URL + req.Body + headerText(req.Header)
		for name, secret := range map[string]string{
			"the environment key": strings.TrimSpace(string(envKey)), "a checkout key": strings.TrimSpace(string(checkoutKey)),
			"the remote": "cplieger/toolbelt",
		} {
			if strings.Contains(all, secret) {
				t.Errorf("%s %s carries %s", req.Method, req.URL, name)
			}
		}
		if strings.Contains(req.URL, envID) || strings.Contains(req.URL, testMachine) {
			t.Errorf("%s %s carries a where value in its URL", req.Method, req.URL)
		}
	}
}

func headerText(h http.Header) string {
	var b strings.Builder
	for k, vs := range h {
		b.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
	}
	return b.String()
}

func TestRegister_rawMachineValueNeverSent(t *testing.T) {
	ts := newServer(t)
	a := newAgent(t, ts, "s-1", "")
	a.env.Machine = where.Machine
	machine := where.Machine(t.Context())
	sources := map[string]string{"dmi": "/sys/class/dmi/id/product_uuid", "machine-id": "/etc/machine-id"}
	kind, _, _ := strings.Cut(machine, ":")
	raw, err := os.ReadFile(sources[kind])
	if machine == "" || err != nil {
		t.Skip("this host gives grelot no machine id to read")
	}
	a.must("register", "tool-dev", "--paths", "/workspace/tool", "--desc", "x", "--ttl", "1h")
	value := strings.TrimSpace(string(raw))
	for _, req := range ts.requests() {
		all := strings.ToLower(req.URL + req.Body + headerText(req.Header))
		for _, v := range []string{strings.ToLower(value), strings.ReplaceAll(strings.ToLower(value), "-", "")} {
			if strings.Contains(all, v) {
				t.Errorf("%s %s carries the raw machine value", req.Method, req.URL)
			}
		}
	}
	if !strings.Contains(ts.last(t, http.MethodPost, "/v1/register").Body, `"machine":"`+machine+`"`) {
		t.Errorf("the registration does not carry the machine id %s", machine)
	}
}

func TestRun_aWrongTokenNamesWhereTheTokenIs(t *testing.T) {
	ts := newServer(t)
	a := newAgent(t, ts, "s-1", "")
	writeClientConfig(t, a.env.ConfigDir, "server: "+ts.URL+"\ntoken: wrong\n")
	code, _, errOut := a.run("me")
	file := filepath.Join(a.env.ConfigDir, "grelot", "config.yaml")
	if code != 1 || !strings.Contains(errOut, file) || !strings.Contains(errOut, "GRELOT_TOKEN_FILE") || !strings.Contains(errOut, "grelot me") {
		t.Errorf("exit %d, stderr %q; want 1 naming %s, GRELOT_TOKEN_FILE and grelot me", code, errOut, file)
	}
}
