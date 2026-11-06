package providers_test

import (
	"bytes"
	"slices"
	"testing"

	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/transport"
	"github.com/cplieger/grelot/providers"
)

// claudeStart is the background door's start line, the first watcher door.
const claudeStart = "run `grelot watch --door background` with run_in_background"

func TestClaudeCodeDoors(t *testing.T) {
	p := shipped(t, "claude-code")
	var names []string
	for _, d := range p.Doors {
		names = append(names, d.Name)
	}
	if !slices.Equal(names, []string{"background", "monitor"}) {
		t.Fatalf("doors %v, want background, then monitor", names)
	}
	signal := transport.SignalLine(7, 2)
	end := transport.EndLine(transport.EndEnded)
	cases := []struct {
		door      *provider.Door
		start     string
		transport provider.Transport
		nudgeExit bool
	}{
		{door: &p.Doors[0], start: claudeStart, transport: provider.TransportExit, nudgeExit: true},
		{door: &p.Doors[1], start: "`grelot watch --door monitor` under Monitor, timeout_ms 1800000", transport: provider.TransportStdout},
	}
	for _, c := range cases {
		t.Run(c.door.Name, func(t *testing.T) {
			if c.door.Transport != c.transport || c.door.Layer != 2 || c.door.Start != c.start || c.door.Hold != 0 {
				t.Errorf("door %+v, want a layer 2 %s door with no hold, start %q", c.door, c.transport, c.start)
			}
			var out bytes.Buffer
			w, err := transport.NewWatch(c.door, &out, nil)
			if err != nil {
				t.Fatal(err)
			}
			step, err := w.Nudge(t.Context(), &provider.Data{Line: signal})
			if err != nil || step.Exit != c.nudgeExit || step.Code != 0 || out.String() != signal+"\n" {
				t.Errorf("nudge: step %+v, %v, stdout %q; want the signal line alone, exit %v with 0", step, err, out.String(), c.nudgeExit)
			}
			out.Reset()
			step, err = w.End(t.Context(), &provider.Data{Line: end})
			if err != nil || !step.Exit || step.Code != 1 || out.String() != end+"\n" {
				t.Errorf("end: step %+v, %v, stdout %q; want the end line alone and exit 1, the default end code", step, err, out.String())
			}
		})
	}
}

func TestClaudeCodeHookEvents(t *testing.T) {
	want := wantWiring(t, "claude-code", nil)
	var events []string
	for _, w := range want {
		events = append(events, w.Event)
	}
	if !slices.Equal(events, []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"}) {
		t.Errorf("the provider spells events %v, want SessionStart, UserPromptSubmit, Stop and SessionEnd", events)
	}
}

// shipped is the named provider as the embedded set loads it.
func shipped(t *testing.T, name string) *provider.Provider {
	t.Helper()
	set, err := provider.LoadSet(providers.FS)
	if err != nil {
		t.Fatal(err)
	}
	p, err := set.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
