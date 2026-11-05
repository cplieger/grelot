package providers_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

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
	},
	{
		name:    "cursor",
		docs:    "https://cursor.com/docs/agent/hooks",
		stdin:   "conversation_id",
		events:  []string{"sessionStart", "stop"},
		context: func(s string) any { return map[string]any{"additional_context": s} },
		block:   func(s string) any { return map[string]any{"followup_message": s} },
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

// With no session in the shell, every hook acts as nobody: it prints
// nothing, exits 0 and asks the server nothing.
func TestDocumentedHosts_hooksWriteNothing(t *testing.T) {
	for _, h := range documentedHosts {
		for _, role := range []string{"session_start", "stop"} {
			t.Run(h.name+"_"+role, func(t *testing.T) {
				sh := newShell(t, newServer(t), h.name, nil)
				got := sh.hook(role, sh.fixture(role))
				if got.Code != 0 || got.Stdout != "" || got.Stderr != "" || len(got.Requests) != 0 {
					t.Errorf("exit %d, stdout %q, stderr %q, requests %v; want 0, nothing printed and none sent", got.Code, got.Stdout, got.Stderr, got.Requests)
				}
			})
		}
	}
}

func TestDocumentedHosts_identityCommandsRefuse(t *testing.T) {
	for _, h := range documentedHosts {
		t.Run(h.name, func(t *testing.T) {
			sh := newShell(t, newServer(t), h.name, nil)
			got := sh.run("", "--provider", h.name, "me")
			want := "provider " + h.name + " records no session variable"
			if got.Code == 0 || !strings.Contains(got.Stderr, want) || len(got.Requests) != 0 {
				t.Errorf("exit %d, stderr %q, requests %v; want a failure naming %q and none sent", got.Code, got.Stderr, got.Requests, want)
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
