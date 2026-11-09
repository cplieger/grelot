package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_unknownCommand(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if code := run([]string{"bogus"}, io.Discard, &stderr); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "bogus"`) {
		t.Errorf("stderr %q does not name the command", stderr.String())
	}
}

// isolateConfig points os.UserConfigDir at a temporary folder on every
// system: HOME covers macOS, XDG_CONFIG_HOME Linux and AppData Windows.
func isolateConfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData", "Roaming"))
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, home) {
		t.Fatalf("os.UserConfigDir() = %s, outside the temporary home %s", dir, home)
	}
}

func TestRun_clientLoadsTheShippedProvidersUnderTheUsersOwn(t *testing.T) {
	isolateConfig(t)
	t.Setenv("KIRO_SESSION_ID", "")
	t.Setenv("GRELOT_TOKEN_FILE", "")
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(configDir, "grelot")
	if err := os.MkdirAll(filepath.Join(dir, "providers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("server: http://127.0.0.1:1\ntoken: test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inbox := func(t *testing.T) string {
		t.Helper()
		var stderr bytes.Buffer
		if code := run([]string{"--provider", "kiro-cli", "inbox"}, io.Discard, &stderr); code != 1 {
			t.Errorf("exit %d, want 1; stderr %q", code, stderr.String())
		}
		return stderr.String()
	}
	t.Run("a shipped file with no file on disk", func(t *testing.T) {
		if got := inbox(t); !strings.Contains(got, "KIRO_SESSION_ID") {
			t.Errorf("stderr %q, want the shipped kiro-cli file's session variable", got)
		}
	})
	t.Run("a user file replaces the shipped one", func(t *testing.T) {
		file := filepath.Join(dir, "providers", "kiro-cli.yaml")
		if err := os.WriteFile(file, []byte("name: kiro-cli\nsession: {env: USER_FILE_SESSION}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := inbox(t); !strings.Contains(got, "USER_FILE_SESSION") || strings.Contains(got, "KIRO_SESSION_ID") {
			t.Errorf("stderr %q, want the user file's session variable only", got)
		}
	})
}

// stamp sets main.version as -ldflags -X would, for one test that runs
// before the parallel ones.
func stamp(t *testing.T, v string) {
	t.Helper()
	old := version
	version = v
	t.Cleanup(func() { version = old })
}

// A build stamped with a version the server would refuse prints it, and
// sends the server the development version instead.
func TestRun_versionSendsTheWireVersion(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GRELOT_TOKEN_FILE", "")
	const pseudo = "v1.2.4-0.20261105101010-0123456789ab"
	stamp(t, pseudo)
	sent := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sent <- r.Header.Get("Grelot-Version"):
		default:
		}
		w.Header().Set("Grelot-Version", "v1.2.3")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"version":"v1.2.3"}`)
	}))
	t.Cleanup(srv.Close)
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(configDir, "grelot")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("server: "+srv.URL+"\ntoken: test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if want := "grelot client " + pseudo + "\ngrelot server v1.2.3\n"; stdout.String() != want {
		t.Errorf("stdout %q, want %q", stdout.String(), want)
	}
	select {
	case got := <-sent:
		if got != "dev" {
			t.Errorf("Grelot-Version sent %q, want dev", got)
		}
	default:
		t.Error("the server got no request")
	}
}

func TestRun_clientVerbs(t *testing.T) {
	isolateConfig(t)
	cases := []struct {
		name     string
		args     []string
		wantCode int
		stdout   string
		stderr   string
	}{
		{name: "help prints the manual", args: []string{"help"}, stdout: "### grelot register"},
		{name: "a verb's help prints its section", args: []string{"register", "--help"}, stdout: "### grelot register"},
		{name: "self before the verb", args: []string{"--self", "help"}, stdout: "## Commands"},
		{name: "provider before the verb", args: []string{"--provider", "any", "help"}, stdout: "## Commands"},
		{name: "provider with no name", args: []string{"--provider"}, wantCode: 2, stderr: "--provider needs a provider name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(c.args, &stdout, &stderr); code != c.wantCode {
				t.Errorf("exit %d, want %d; stderr %q", code, c.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), c.stdout) {
				t.Errorf("stdout does not hold %q:\n%s", c.stdout, stdout.String())
			}
			if !strings.Contains(stderr.String(), c.stderr) {
				t.Errorf("stderr %q does not hold %q", stderr.String(), c.stderr)
			}
		})
	}
}
