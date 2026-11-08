package providers_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/grelot/internal/hook"
	"github.com/cplieger/grelot/internal/proctree"
	"github.com/cplieger/grelot/internal/provider"
)

// documentedHost is what a host's own hook docs say, the source of every
// expected value below.
type documentedHost struct {
	// context and block are the docs' output shapes for text added at
	// session start and for a hold at turn end.
	context func(text string) any
	block   func(reason string) any
	name    string
	docs    string
	// stdin is the payload field holding the session id.
	stdin string
	// events are the docs' names for session start and turn end.
	events []string
	// door is the watcher door, "" for none.
	door, start string
	// cli is the CLI's executable, "" for one grelot cannot name, and
	// oneShot the docs' argument lists that run one prompt and exit.
	cli     string
	oneShot [][]string
}

var documentedHosts = []documentedHost{
	{
		name:    "copilot-cli",
		docs:    "https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-hooks-reference",
		stdin:   "sessionId",
		events:  []string{"sessionStart", "agentStop"},
		context: func(s string) any { return map[string]any{"additionalContext": s} },
		block:   func(s string) any { return map[string]any{"decision": "block", "reason": s} },
		door:    "async-shell",
		start:   "run `grelot watch --door async-shell` with mode async",
		// https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-command-reference
		cli:     "copilot",
		oneShot: [][]string{{"-p", "fix the tests"}, {"--prompt", "fix the tests"}, {"--prompt=fix the tests"}},
	},
	{
		name:   "antigravity",
		docs:   "https://antigravity.google/docs/hooks/",
		stdin:  "conversationId",
		events: []string{"SessionStart", "Stop"},
		context: func(s string) any {
			return map[string]any{"injectSteps": []any{map[string]any{"systemMessage": map[string]any{"systemMessage": s}}}}
		},
		block: func(s string) any { return map[string]any{"decision": "continue", "reason": s} },
		door:  "background-task",
		start: "run `grelot watch --door background-task` in the background",
		// https://antigravity.google/docs/cli/headless/
		cli: "agy",
		oneShot: [][]string{
			{"-p", "fix the tests"},
			{"--print", "fix the tests"},
			{"--prompt", "fix the tests"},
			{"-p", "fix the tests", "--output-format", "json"},
		},
	},
	{
		name:    "cursor",
		docs:    "https://cursor.com/docs/agent/hooks",
		stdin:   "conversation_id",
		events:  []string{"sessionStart", "stop"},
		context: func(s string) any { return map[string]any{"additional_context": s} },
		block:   func(s string) any { return map[string]any{"followup_message": s} },
		// The CLI's -p (https://cursor.com/docs/cli/reference/parameters)
		// runs under the node its launcher execs, a name Cursor cannot claim.
	},
}

// A file built from docs alone records no shell variable, so it is never
// detected and holds no session of its own.
func TestDocumentedHosts_file(t *testing.T) {
	for _, h := range documentedHosts {
		t.Run(h.name, func(t *testing.T) {
			p := shipped(t, h.name)
			if p.Evidence != provider.EvidenceDocumented || len(p.Detect.Env) != 0 || p.Session.Env != "" || p.Session.Stdin != h.stdin {
				t.Errorf("evidence %q, detect %v, session %+v; want documented, no detect, no session.env, stdin %s (%s)",
					p.Evidence, p.Detect.Env, p.Session, h.stdin, h.docs)
			}
			if p.Notes != "docs/hosts/"+h.name+".md" {
				t.Errorf("notes %q, want docs/hosts/%s.md", p.Notes, h.name)
			}
			hs := &p.Hooks
			if hs.SessionStart == nil || hs.Stop == nil || hs.Prompt != nil || hs.TurnEnd != nil || hs.SessionEnd != nil || hs.SubagentStop != nil {
				t.Fatalf("hooks %+v, want session_start and stop alone", hs)
			}
			if got := []string{hs.SessionStart.Event, hs.Stop.Event}; !slices.Equal(got, h.events) {
				t.Errorf("events %v, want %v (%s)", got, h.events, h.docs)
			}
			if hs.Stop.Outputs.Notice != nil {
				t.Errorf("stop notice %+v, want none: the docs give the stop hook no message for the user", hs.Stop.Outputs.Notice)
			}
		})
	}
}

// Each output renders the JSON object the host's docs give for it, with
// grelot's text as the one string it carries, written to stdout with exit 0.
func TestDocumentedHosts_outputsTakeTheDocsShapes(t *testing.T) {
	data := &provider.Data{
		Context: "grelot: @sample-dev \u00b7 2 unread\n\"wake\": down \\ start",
		Reason:  "grelot: 2 new \"direct\" messages\nrun grelot inbox",
	}
	for _, h := range documentedHosts {
		p := shipped(t, h.name)
		outputs := map[string]struct {
			out  *provider.Output
			want any
		}{
			"session_start": {p.Hooks.SessionStart.Outputs.Context, h.context(data.Context)},
			"stop":          {p.Hooks.Stop.Outputs.Block, h.block(data.Reason)},
		}
		for role, o := range outputs {
			t.Run(h.name+"_"+role, func(t *testing.T) {
				if o.out == nil {
					t.Fatal("no output")
				}
				text, err := o.out.Render(data)
				if err != nil {
					t.Fatal(err)
				}
				var got any
				if err := json.Unmarshal([]byte(text), &got); err != nil || !reflect.DeepEqual(got, o.want) {
					t.Errorf("renders %s (%v), want %v (%s)", text, err, o.want, h.docs)
				}
				if o.out.Stream != provider.StreamStdout || o.out.Exit != 0 {
					t.Errorf("stream %s, exit %d; want stdout and 0", o.out.Stream, o.out.Exit)
				}
			})
		}
	}
}

// A run of the CLI given a one-shot form its docs name is one-shot, and the
// CLI started with no arguments, or a host naming no CLI, never is.
func TestDocumentedHosts_oneShot(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }
	under := func(name string, args ...string) func() []proctree.Process {
		return func() []proctree.Process {
			return []proctree.Process{
				{Name: "sh", Args: []string{"sh", "-c", "grelot hook"}},
				{Name: name, Args: append([]string{name}, args...)},
			}
		}
	}
	for _, h := range documentedHosts {
		p := shipped(t, h.name)
		t.Run(h.name+"_process", func(t *testing.T) {
			want := []string{h.cli, h.cli + ".exe"}
			if h.cli == "" {
				want = nil
			}
			if !slices.Equal(p.Session.Process, want) {
				t.Errorf("session.process %v, want %v", p.Session.Process, want)
			}
		})
		for _, args := range h.oneShot {
			t.Run(h.name+"_"+strings.Join(args, "_"), func(t *testing.T) {
				for _, name := range []string{h.cli, h.cli + ".exe"} {
					if !hook.OneShot(p, noEnv, under(name, args...)) {
						t.Errorf("%s %q is not one-shot, want one-shot (%s)", name, args, h.docs)
					}
				}
			})
		}
		t.Run(h.name+"_interactive", func(t *testing.T) {
			if hook.OneShot(p, noEnv, under(h.cli)) {
				t.Errorf("%s alone is one-shot, want interactive", h.cli)
			}
			if h.cli == "" && (len(p.OneShot.Args) != 0 || len(p.OneShot.Env) != 0) {
				t.Errorf("one_shot %+v, want none for a host whose CLI grelot cannot name", p.OneShot)
			}
		})
	}
}

func TestDocumentedHosts_doors(t *testing.T) {
	for _, h := range documentedHosts {
		t.Run(h.name, func(t *testing.T) {
			p := shipped(t, h.name)
			if h.door == "" {
				if len(p.Doors) != 0 {
					t.Errorf("doors %+v, want none", p.Doors)
				}
				return
			}
			if len(p.Doors) != 1 {
				t.Fatalf("doors %+v, want the %s door alone", p.Doors, h.door)
			}
			d := &p.Doors[0]
			if d.Name != h.door || d.Transport != provider.TransportExit || d.Layer != 2 || d.Start != h.start {
				t.Errorf("door %+v, want the layer 2 exit door %s, start %q", d, h.door, h.start)
			}
		})
	}
}
