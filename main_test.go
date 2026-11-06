package main

import (
	"bytes"
	"io"
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

func TestRun_clientVerbs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
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
