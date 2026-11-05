package providers_test

import (
	"bytes"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/providers"
)

// hookTimeouts are the hook-file timeouts, in seconds, that differ from
// the 10 s every other hook line takes: Codex clamps a SessionEnd hook to
// 3 s and warns when a file asks for more (SESSION_END_MAX_TIMEOUT_SEC,
// codex-rs/hooks/src/events/session_end.rs).
var hookTimeouts = map[string]map[string]int{"codex": {"SessionEnd": 3}}

// shippedNames lists the shipped provider files by name.
func shippedNames(t *testing.T) []string {
	t.Helper()
	files, err := fs.Glob(providers.FS, "*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("shipped files %v, %v", files, err)
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, strings.TrimSuffix(f, ".yaml"))
	}
	return names
}

// Every shipped file passes grelot provider check, its notes name its docs
// page, and each door asset it names ships beside it.
func TestShipped_checkNotesAndAssets(t *testing.T) {
	for _, name := range shippedNames(t) {
		t.Run(name, func(t *testing.T) {
			data, err := fs.ReadFile(providers.FS, name+".yaml")
			if err != nil {
				t.Fatal(err)
			}
			if err := provider.Check(name, data); err != nil {
				t.Errorf("provider check: %v", err)
			}
			p := shipped(t, name)
			if want := "docs/hosts/" + name + ".md"; p.Notes != want {
				t.Errorf("notes %q, want %q", p.Notes, want)
			}
			for _, d := range p.Doors {
				if d.Asset == "" {
					continue
				}
				if _, err := os.Stat(d.Asset); err != nil {
					t.Errorf("door %s: asset %s: %v", d.Name, d.Asset, err)
				}
			}
		})
	}
}

// A file that spells hook roles ships a hook file wiring exactly those
// roles, and only such a file does; every install folder belongs to a
// shipped file.
func TestShipped_hookFilesWireEveryRole(t *testing.T) {
	names := shippedNames(t)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			want := wantWiring(t, name, hookTimeouts[name])
			_, err := os.Stat(name + "/hooks.json")
			switch {
			case len(want) == 0 && err == nil:
				t.Errorf("%s/hooks.json ships for a file that spells no hook role", name)
			case len(want) == 0:
			case err != nil:
				t.Errorf("the file spells hook roles and %s/hooks.json is missing: %v", name, err)
			default:
				if got := hookWiring(t, name); !sameWiring(got, want) {
					t.Errorf("hooks.json wires %v, want %v", got, want)
				}
			}
		})
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != "testdata" && !slices.Contains(names, e.Name()) {
			t.Errorf("folder %s names no shipped provider file", e.Name())
		}
	}
}

// The wiring check fails a hook file that runs another role under an
// event, or that gives a hook another timeout.
func TestShipped_aMiswiredHookFileFails(t *testing.T) {
	data := mustRead(t, "claude-code/hooks.json")
	want := wantWiring(t, "claude-code", nil)
	for name, edit := range map[string][2]string{
		"another role at Stop": {"grelot hook claude-code stop", "grelot hook claude-code session_end"},
		"another timeout":      {`"timeout": 10`, `"timeout": 60`},
	} {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			if !bytes.Contains(data, []byte(edit[0])) {
				t.Fatalf("claude-code/hooks.json holds no %s to plant over", edit[0])
			}
			planted := bytes.Replace(data, []byte(edit[0]), []byte(edit[1]), 1)
			if got := parseWiring(t, "claude-code", planted); sameWiring(got, want) {
				t.Errorf("a hook file with %s passes the check", name)
			}
		})
	}
}

// A turn line stays under 30 words in its longest state, a description
// reminder and a down wake with its start line. That line is measured once
// with Claude Code's start line; every other door's start line must fit
// the words left.
func TestShipped_startLinesKeepTheTurnLineUnderThirtyWords(t *testing.T) {
	srv := newServer(t)
	sh := claudeShell(t, srv, claudeTUI)
	sh.register("claude-dev")
	peer := claudeShell(t, srv, claudeFork)
	peer.register("peer-dev")
	for range 12 {
		peer.post("@claude-dev", "x")
	}
	srv.clock.advance(90 * time.Minute)
	got := sh.hook("prompt", sh.fixture("prompt-typed"))
	line := strings.TrimSuffix(got.Stdout, "\n")
	if strings.Contains(line, "\n") || !strings.Contains(line, "12 unread") || !strings.Contains(line, "still true?") || !strings.Contains(line, "start: "+claudeStart) {
		t.Fatalf("turn line %q, want one line with 12 unread, the reminder and the start line", line)
	}
	rest := turnWords(line) - turnWords(claudeStart)
	for _, name := range shippedNames(t) {
		for _, d := range shipped(t, name).Doors {
			if d.Start == "" {
				continue
			}
			if n := rest + turnWords(d.Start); n >= 30 {
				t.Errorf("%s door %s: start %q makes the longest turn line %d words, want under 30", name, d.Name, d.Start, n)
			}
		}
	}
}

// turnWords counts a turn line's words as its 30-word ceiling does: the
// separator dots are not words.
func turnWords(s string) int {
	n := 0
	for f := range strings.FieldsSeq(s) {
		if f != "\u00b7" {
			n++
		}
	}
	return n
}
