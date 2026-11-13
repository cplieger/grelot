package providers_test

import (
	"bytes"
	"encoding/json"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/transport"
	"github.com/cplieger/grelot/providers"
)

// kiroStart is the workflow door's start line, which names where the
// recipe is installed.
const kiroStart = "start the grelot-watch workflow (~/.kiro/workflows/grelot-watch.workflow.json)"

func TestKiroCLIWorkflowDoor(t *testing.T) {
	door := kiroDoor(t, "kiro-cli")
	if door.Transport != provider.TransportExit || door.Layer != 2 || door.Hold != 45*time.Second || door.Start != kiroStart {
		t.Errorf("door %s: %s at layer %d holding %s, start %q; want an exit door at layer 2 holding 45s", door.Name, door.Transport, door.Layer, door.Hold, door.Start)
	}
	signal := transport.SignalLine(7, 2)
	end := transport.EndLine(transport.EndEnded)
	cases := []struct {
		run  func(*transport.Watch) (transport.Step, error)
		want map[string]any
		name string
	}{
		{name: "a nudge", want: map[string]any{"outcome": "new-activity", "cursor": nil, "payload": signal}, run: func(w *transport.Watch) (transport.Step, error) {
			return w.Nudge(t.Context(), &provider.Data{Line: signal})
		}},
		{name: "the hold lapsing", want: map[string]any{"outcome": "idle", "cursor": nil}, run: func(w *transport.Watch) (transport.Step, error) {
			return w.Idle(t.Context(), &provider.Data{})
		}},
		{name: "the end", want: map[string]any{"outcome": "terminal-state", "cursor": nil, "payload": end}, run: func(w *transport.Watch) (transport.Step, error) {
			return w.End(t.Context(), &provider.Data{Line: end})
		}},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			var out bytes.Buffer
			w, err := transport.NewWatch(door, &out, nil)
			if err != nil {
				t.Fatal(err)
			}
			step, err := c.run(w)
			if err != nil {
				t.Fatal(err)
			}
			if !step.Exit || step.Code != 0 {
				t.Errorf("step %+v, want exit 0: the watch reads any other code as idle without reading the line", step)
			}
			if strings.Count(out.String(), "\n") != 1 || !strings.HasSuffix(out.String(), "\n") {
				t.Errorf("stdout %q, want one line", out.String())
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("stdout %q is not JSON: %v", out.String(), err)
			}
			if len(got) != len(c.want) {
				t.Errorf("result %v, want %v", got, c.want)
			}
			for k, v := range c.want {
				if g, ok := got[k]; !ok || g != v {
					t.Errorf("%s = %v, want %v", k, g, v)
				}
			}
		})
	}
}

func kiroDoor(t *testing.T, name string) *provider.Door {
	t.Helper()
	set, err := provider.LoadSet(providers.FS)
	if err != nil {
		t.Fatal(err)
	}
	p, err := set.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(p.Doors, func(d provider.Door) bool { return d.Name == "workflow" })
	if i < 0 {
		t.Fatalf("%s has no workflow door", name)
	}
	return &p.Doors[i]
}

// recipeNode is one node of a workflow recipe, with the keys the
// grelot-watch run depends on.
type recipeNode struct {
	Config          map[string]any `json:"config"`
	Type            string         `json:"type"`
	ID              string         `json:"id"`
	OnMaxIterations string         `json:"onMaxIterations"`
	StopWhen        string         `json:"stopWhen"`
	Handler         string         `json:"handler"`
	Agent           string         `json:"agent"`
	Prompt          string         `json:"prompt"`
	ModelID         string         `json:"modelId"`
	EffortLevel     string         `json:"effortLevel"`
	ModelFallback   string         `json:"onModelNotOffered"`
	Steps           []recipeNode   `json:"steps"`
	MaxIterations   int            `json:"maxIterations"`
}

func TestKiroCLIWatchRecipe(t *testing.T) {
	door := kiroDoor(t, "kiro-cli")
	if door.Asset != "kiro-cli/grelot-watch.workflow.json" || !strings.Contains(door.Start, "/"+path.Base(door.Asset)+")") {
		t.Errorf("asset %q, start %q; want the recipe, installed under its own name", door.Asset, door.Start)
	}
	var recipe struct {
		Name  string       `json:"name"`
		Steps []recipeNode `json:"steps"`
	}
	if err := json.Unmarshal(mustRead(t, door.Asset), &recipe); err != nil {
		t.Fatal(err)
	}
	if recipe.Name != "grelot-watch" || len(recipe.Steps) != 1 {
		t.Fatalf("recipe %q with %d top nodes, want grelot-watch with one", recipe.Name, len(recipe.Steps))
	}
	outer := recipe.Steps[0]
	t.Run("two nested repeats that stop on the end output", func(t *testing.T) {
		if outer.Type != "repeat" || outer.MaxIterations != 1000 || outer.OnMaxIterations != "pause" || outer.StopWhen != "watch.terminal" {
			t.Errorf("outer %+v, want a repeat of 1000 that pauses at its cap and stops on watch.terminal", outer)
		}
		if len(outer.Steps) != 1 {
			t.Fatalf("outer holds %d nodes, want the inner repeat", len(outer.Steps))
		}
		inner := outer.Steps[0]
		if inner.Type != "repeat" || inner.MaxIterations != 1000 || inner.OnMaxIterations != "continue" || inner.StopWhen != "watch.terminal" {
			t.Errorf("inner %+v, want a repeat of 1000 that continues at its cap and stops on watch.terminal", inner)
		}
	})
	if len(outer.Steps) != 1 || len(outer.Steps[0].Steps) != 2 {
		t.Fatal("the inner repeat does not hold the watch and the notifier")
	}
	watch, notify := outer.Steps[0].Steps[0], outer.Steps[0].Steps[1]
	t.Run("a command watch running one poll", func(t *testing.T) {
		if watch.Type != "watch" || watch.ID != "watch" || watch.Handler != "command" {
			t.Errorf("watch %+v, want a command watch with id watch", watch)
		}
		want := map[string]any{"command": "grelot watch --door workflow", "pollIntervalSec": 10.0, "commandTimeoutSec": 300.0}
		if len(watch.Config) != len(want) {
			t.Errorf("config %v, want %v", watch.Config, want)
		}
		for k, v := range want {
			if watch.Config[k] != v {
				t.Errorf("config %s = %v, want %v", k, watch.Config[k], v)
			}
		}
		if hold := door.Hold.Seconds(); hold >= 300 {
			t.Errorf("hold %vs, want well under the 300 s command timeout", hold)
		}
	})
	t.Run("the notifier relays the watch's output", func(t *testing.T) {
		if notify.Type != "step" || notify.Agent != "grelot-notifier" || notify.Prompt != "{{watch.output}}" {
			t.Errorf("notifier %+v, want a grelot-notifier step prompted with the watch's output", notify)
		}
		// Each new-mail line bills one notifier turn, so the step pins the cheapest Claude model
		// rather than inheriting the chat's.
		if notify.ModelID != "claude-haiku-5.5" || notify.EffortLevel != "low" {
			t.Errorf("notifier runs on %q at %q, want claude-haiku-5.5 at low", notify.ModelID, notify.EffortLevel)
		}
		// An account not offered Haiku must still get its wake-ups, on the chat's model.
		if notify.ModelFallback != "inherit" {
			t.Errorf("notifier onModelNotOffered = %q, want inherit", notify.ModelFallback)
		}
		agent := string(mustRead(t, filepath.Join("kiro-cli", "grelot-notifier.md")))
		front, _, ok := strings.Cut(strings.TrimPrefix(agent, "---\n"), "\n---\n")
		if !strings.HasPrefix(agent, "---\n") || !ok {
			t.Fatalf("grelot-notifier.md has no front matter")
		}
		lines := strings.Split(front, "\n")
		if !slices.Contains(lines, "name: grelot-notifier") || !slices.Contains(lines, "tools: []") {
			t.Errorf("front matter %q, want name grelot-notifier and no tools", front)
		}
		if !strings.Contains(agent, "`send_message`") || !strings.Contains(agent, "`info`") {
			t.Errorf("grelot-notifier.md does not relay the line with send_message at info")
		}
	})
}

func TestKiroHookEvents(t *testing.T) {
	wantEvents := []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"}
	for _, name := range []string{"kiro-cli", "kiro-ide"} {
		t.Run(name, func(t *testing.T) {
			want := wantWiring(t, name, nil)
			var events []string
			for _, w := range want {
				events = append(events, w.Event)
			}
			if !slices.Equal(events, wantEvents) {
				t.Errorf("the provider spells events %v, want %v", events, wantEvents)
			}
		})
	}
}
