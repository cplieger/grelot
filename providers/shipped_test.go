package providers_test

import (
	"bytes"
	"cmp"
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
// the 30 s every other hook line takes: Codex clamps a SessionEnd hook to
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

// launchMargin is the host's time to start a hook before grelot runs, as
// measured on a slow Windows host: Codex's PowerShell launch took 7 s at
// the median and 10 s at times.
const launchMargin = 10 * time.Second

// Every shipped hook's timeout holds the host's launch and the role's
// server budget, so the hook prints its own could-not-check line before
// the host kills it. The timeouts in hookTimeouts are the host's clamp.
func TestShipped_hookTimeoutsHoldTheBudget(t *testing.T) {
	set, err := provider.LoadSet(providers.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range shippedNames(t) {
		p, err := set.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		hs := &p.Hooks
		roles := map[string]*provider.Role{}
		if hs.SessionStart != nil {
			roles[hs.SessionStart.Event] = &hs.SessionStart.Role
		}
		if hs.Prompt != nil {
			roles[hs.Prompt.Event] = &hs.Prompt.Role
		}
		if hs.Stop != nil {
			roles[hs.Stop.Event] = &hs.Stop.Role
		}
		if hs.TurnEnd != nil {
			roles[hs.TurnEnd.Event] = &hs.TurnEnd.Role
		}
		if hs.SessionEnd != nil {
			roles[hs.SessionEnd.Event] = &hs.SessionEnd.Role
		}
		if hs.SubagentStop != nil {
			roles[hs.SubagentStop.Event] = &hs.SubagentStop.Role
		}
		if len(roles) == 0 {
			continue
		}
		for _, w := range hookWiring(t, name) {
			if _, clamped := hookTimeouts[name][w.Event]; clamped {
				continue
			}
			budget := cmp.Or(roles[w.Event].Budget, provider.DefaultBudget)
			if got := time.Duration(w.Timeout) * time.Second; got < budget+launchMargin {
				t.Errorf("%s %s: timeout %s, want at least the %s budget and %s to launch", name, w.Event, got, budget, launchMargin)
			}
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
		"another timeout":      {`"timeout": 30`, `"timeout": 60`},
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
