package providers_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/transport"
)

// The threads of the recorded Codex payloads.
const (
	// codexTUI is the Windows TUI thread, codexAfterNew the one /new started
	// in that TUI.
	codexTUI      = "01a19ef0-6415-7941-87f6-9ca8f875c78c"
	codexAfterNew = "01a19ef9-83eb-7c22-9080-cf19bea068a2"
	// codexExec is the Windows codex exec run's.
	codexExec = "01a19def-c194-7ff3-9007-5062de12f462"
	// codexLinux is the Linux TUI thread before /clear, which an Esc
	// interrupted; codexCleared the one /clear started in that TUI.
	codexLinux   = "01a1a4bc-787d-7423-a284-cc692ac519a0"
	codexCleared = "01a1a4be-ae91-7ef2-a7a5-01551eaaa3b3"
	// codexSub is the subagent thread of the derived subagent payloads.
	codexSub = "01a19ef2-1c3d-7a52-8e4f-3b6d9c0a7e15"
)

// codexStart is the queue door's start line.
const codexStart = "`grelot watch --door queue` in a background terminal"

func TestCodexQueueDoor(t *testing.T) {
	p := shipped(t, "codex")
	if len(p.Doors) != 1 {
		t.Fatalf("doors %v, want the queue door alone", p.Doors)
	}
	d := &p.Doors[0]
	if d.Name != "queue" || d.Transport != provider.TransportExec || d.Layer != 2 || d.Start != codexStart || d.Asset != "codex/grelot.rules" {
		t.Errorf("door %+v, want the layer 2 exec door queue, start %q, asset codex/grelot.rules", d, codexStart)
	}
	signal := transport.SignalLine(7, 2)
	argv, err := d.RenderArgv(&provider.Data{Session: codexTUI, Line: signal})
	if want := []string{"codex", "queue", "--thread", codexTUI, "--message", signal}; err != nil || !slices.Equal(argv, want) {
		t.Errorf("argv %q, %v; want %q", argv, err, want)
	}
}

func TestCodexHookEvents(t *testing.T) {
	want := wantWiring(t, "codex", map[string]int{"SessionEnd": 3})
	var events []string
	for _, w := range want {
		events = append(events, w.Event)
	}
	if !slices.Equal(events, []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd", "SubagentStop"}) {
		t.Errorf("the provider spells events %v, want SessionStart, UserPromptSubmit, Stop, SessionEnd and SubagentStop", events)
	}
}

// The rule file allows grelot and nothing that only starts with its name,
// so Codex runs it outside the sandbox with no approval.
func TestCodexRules(t *testing.T) {
	const want = `prefix_rule(
    pattern = ["grelot"],
    decision = "allow",
    justification = "grelot messaging client",
    match = ["grelot inbox", "grelot watch --door queue"],
    not_match = ["grelotx inbox"],
)
`
	got, err := os.ReadFile(filepath.Join("codex", "grelot.rules"))
	if err != nil || string(got) != want {
		t.Errorf("codex/grelot.rules is %q, %v; want exactly %q", got, err, want)
	}
}
