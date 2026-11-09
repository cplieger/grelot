package client

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig_full(t *testing.T) {
	dir := writeConfig(t, `server: https://grelot.example.com/
token: "  test-token  "
host: laptop
provider: alpha
repos:
  git@GitLab.Example.com:team/docs.git: team-docs
  https://github.com/owner/tool: tool-fork
`, 0o600)
	cfg, err := LoadConfig(dir, env(), noHostname)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server != "https://grelot.example.com" {
		t.Errorf("Server = %q, want the URL with no trailing slash", cfg.Server)
	}
	if cfg.Token != "test-token" {
		t.Errorf("Token = %q, want it trimmed", cfg.Token)
	}
	if cfg.Host != "laptop" || cfg.Provider != "alpha" {
		t.Errorf("Host, Provider = %q, %q; want laptop, alpha", cfg.Host, cfg.Provider)
	}
	want := map[string]string{"gitlab.example.com/team/docs": "team-docs", "github.com/owner/tool": "tool-fork"}
	if !maps.Equal(cfg.Repos, want) {
		t.Errorf("Repos = %v, want each key normalized: %v", cfg.Repos, want)
	}
}

func TestLoadConfig_hostDefaultsToHostname(t *testing.T) {
	dir := writeConfig(t, "server: http://127.0.0.1:9737\ntoken: test-token\n", 0o600)
	cfg, err := LoadConfig(dir, env(), func() (string, error) { return "box", nil })
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Host != "box" {
		t.Errorf("Host = %q, want the hostname box", cfg.Host)
	}
}

func TestLoadConfig_tokenFile(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("wins over the config's token", func(t *testing.T) {
		dir := writeConfig(t, "server: https://g.example\ntoken: config-token\n", 0o600)
		cfg, err := LoadConfig(dir, env(EnvTokenFile, tokenFile), noHostname)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.Token != "file-token" {
			t.Errorf("Token = %q, want file-token", cfg.Token)
		}
	})
	t.Run("alone", func(t *testing.T) {
		dir := writeConfig(t, "server: https://g.example\n", 0o600)
		cfg, err := LoadConfig(dir, env(EnvTokenFile, tokenFile), noHostname)
		if err != nil || cfg.Token != "file-token" {
			t.Errorf("LoadConfig = %+v, %v; want token file-token", cfg, err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		dir := writeConfig(t, "server: https://g.example\n", 0o600)
		missing := filepath.Join(t.TempDir(), "none")
		_, err := LoadConfig(dir, env(EnvTokenFile, missing), noHostname)
		if err == nil || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), EnvTokenFile) {
			t.Errorf("err = %v, want one naming %s and %s", err, missing, EnvTokenFile)
		}
	})
}

func TestLoadConfig_missingFileNamesItAndTheKeys(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadConfig(dir, env(), noHostname)
	if err == nil {
		t.Fatal("LoadConfig with no file succeeded")
	}
	for _, want := range []string{ConfigFile(dir), "server:", "token:", "grelot help"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err, want)
		}
	}
}

func TestLoadConfig_refusals(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"no server", "token: test-token\n", "server is missing"},
		{"another scheme", "server: ftp://g.example\ntoken: t\n", "not an http or https URL"},
		{"no host", "server: https://\ntoken: t\n", "not an http or https URL"},
		{"a user", "server: https://me:pw@g.example\ntoken: t\n", "not an http or https URL"},
		{"a query", "server: https://g.example/?a=1\ntoken: t\n", "not an http or https URL"},
		{"a fragment", "server: https://g.example/#x\ntoken: t\n", "not an http or https URL"},
		{"no token", "server: https://g.example\n", "token is missing"},
		{"a blank token", "server: https://g.example\ntoken: '  '\n", "token is missing"},
		{"an unknown key", "server: https://g.example\ntoken: t\nroot: /workspace\n", "root"},
		{"a second document", "server: https://g.example\ntoken: t\n---\nhost: x\n", "document"},
		{"an environment reference", "server: https://g.example\ntoken: ${GRELOT_SECRET}\n", "${GRELOT_SECRET}"},
		{"a repo remote with no host", "server: https://g.example\ntoken: t\nrepos: {/srv/git/docs: docs}\n", "/srv/git/docs"},
		{"an empty repo name", "server: https://g.example\ntoken: t\nrepos: {github.com/o/docs: ''}\n", "one path component"},
		{"a dot-dot repo name", "server: https://g.example\ntoken: t\nrepos: {github.com/o/docs: '..'}\n", "one path component"},
		{"a dot repo name", "server: https://g.example\ntoken: t\nrepos: {github.com/o/docs: '.'}\n", "one path component"},
		{"a repo name with a slash", "server: https://g.example\ntoken: t\nrepos: {github.com/o/docs: team/docs}\n", "one path component"},
		{"a repo name with a backslash", "server: https://g.example\ntoken: t\nrepos: {github.com/o/docs: 'team\\docs'}\n", "one path component"},
		{"two spellings with two names", "server: https://g.example\ntoken: t\nrepos: {github.com/o/docs: a, 'git@github.com:o/docs.git': b}\n", "two spellings"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeConfig(t, c.content, 0o600)
			_, err := LoadConfig(dir, env(), noHostname)
			if err == nil {
				t.Fatal("LoadConfig succeeded, want a refusal")
			}
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), ConfigFile(dir)) {
				t.Errorf("err = %q, want it to name %q and the file", err, c.want)
			}
		})
	}
}

func TestLoadConfig_twoSpellingsWithOneNameAccepted(t *testing.T) {
	dir := writeConfig(t, "server: https://g.example\ntoken: t\nrepos: {github.com/o/docs: d, 'git@github.com:o/docs.git': d}\n", 0o600)
	cfg, err := LoadConfig(dir, env(), noHostname)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !maps.Equal(cfg.Repos, map[string]string{"github.com/o/docs": "d"}) {
		t.Errorf("Repos = %v, want one entry", cfg.Repos)
	}
}
