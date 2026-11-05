package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validProvider = `name: sample
doors:
  - name: watcher
    layer: 2
    transport: stdout
    line: '{"line":{{json .Line}}}'
`

func writeProvider(t *testing.T, file, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), file)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRun_provider_check(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		args     func(t *testing.T) []string
		stderr   []string
		wantCode int
	}{
		{
			name:     "valid file",
			args:     func(t *testing.T) []string { return []string{writeProvider(t, "sample.yaml", validProvider)} },
			wantCode: 0,
		},
		{
			name: "unknown key",
			args: func(t *testing.T) []string {
				return []string{writeProvider(t, "sample.yaml", validProvider+"bogus: 1\n")}
			},
			wantCode: 1, stderr: []string{"provider sample:", "bogus"},
		},
		{
			name: "a line meant as JSON that is not",
			args: func(t *testing.T) []string {
				return []string{writeProvider(t, "sample.yaml", strings.Replace(validProvider, "{{json .Line}}", `"{{.Line}}"`, 1))}
			},
			wantCode: 1, stderr: []string{"provider sample: doors[0].line:", "not JSON"},
		},
		{
			name:     "name taken from the file name",
			args:     func(t *testing.T) []string { return []string{writeProvider(t, "other.yaml", validProvider)} },
			wantCode: 1, stderr: []string{"provider other: name:"},
		},
		{
			name:     "file not named .yaml",
			args:     func(t *testing.T) []string { return []string{writeProvider(t, "sample.yml", validProvider)} },
			wantCode: 1, stderr: []string{"<name>.yaml"},
		},
		{
			name:     "missing file",
			args:     func(t *testing.T) []string { return []string{filepath.Join(t.TempDir(), "sample.yaml")} },
			wantCode: 1, stderr: []string{"grelot provider check:", "sample.yaml"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			args := append([]string{"provider", "check"}, c.args(t)...)
			if code := run(args, &stderr); code != c.wantCode {
				t.Errorf("exit %d, want %d; stderr %q", code, c.wantCode, stderr.String())
			}
			if c.wantCode == 0 && stderr.Len() != 0 {
				t.Errorf("stderr %q, want nothing", stderr.String())
			}
			for _, want := range c.stderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr %q does not contain %q", stderr.String(), want)
				}
			}
		})
	}
}

func TestRun_provider_usage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"provider"}, {"provider", "check"}, {"provider", "lint", "a.yaml"}, {"provider", "check", "a.yaml", "b.yaml"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			if code := run(args, &stderr); code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), "grelot provider check <file>") {
				t.Errorf("stderr %q does not name the usage", stderr.String())
			}
		})
	}
}
