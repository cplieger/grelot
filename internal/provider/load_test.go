package provider

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// full spells every key of the schema once, but extends (set_test.go).
const full = `name: example
evidence: measured
notes: docs/hosts/example.md
detect:
  env: [EXAMPLE_SESSION_ID, EXAMPLE_HOME]
session:
  env: EXAMPLE_SESSION_ID
  stdin: session_id
  process: [example, example.exe]
  process_shared: [app-server]
  hooks_without_env: true
  record:
    dir_env: EXAMPLE_HOME
    dir_default: ~/.example
    glob: 'sessions/*/{{.HookSession}}/session.json'
    parent: parentSessionId
    reason: createdReason
    same_agent: [rewind]
    same_agent_note: a fixed note
one_shot:
  env:
    EXAMPLE_ENTRYPOINT: [batch, sdk]
    EXAMPLE_ATTENDED: ["0"]
  args: [-p, exec]
doors:
  - name: watcher
    layer: 2
    transport: exit
    start: run grelot watch
    asset: example/recipe.json
    notes: one per session
    line: '{"line":{{json .Line}}}'
    hold: 45s
    idle: '{"outcome":"idle"}'
    end: '{"end":{{json .Line}}}'
    end_exit: 0
  - name: notify
    layer: 1
    transport: http
    match: {provider: other, host: lab/example}
    url: 'http://example:9847/api/sessions/{{path .Session}}/notify'
    headers:
      Authorization: 'Bearer {{secret "example_token"}}'
      Content-Type: application/json
    body: '{"text":{{json .Line}},"dedupe_key":{{json .DedupeKey}}}'
    dedupe_window: 10m
    gone: [404, 410]
  - name: queue
    layer: 2
    transport: exec
    argv: [example, queue, --thread, '{{.Session}}', --message, '{{.Line}}']
hooks:
  lost_fix: re-trust the hooks in /hooks
  session_start:
    event: SessionStart
    stdin: {agent_id: agent_id}
    source: {path: source, take_over: [clear], fork: [fork]}
    context_limit: 10000
    outputs:
      context: {text: '{{.Context}}'}
  prompt:
    event: UserPromptSubmit
    once: true
    stdin: {prompt: prompt, agent_id: agent_id}
    signal: '(?s)^<event>(?P<line>[^<]*)</event>$'
    outputs:
      context: {text: '{{.Context}}'}
      block: {text: '{"decision":"block","reason":{{json .Reason}}}'}
  stop:
    event: Stop
    once_per_turn: true
    outputs:
      block: {text: '{"decision":"block","reason":{{json .Reason}}}'}
      notice: {text: '{"systemMessage":{{json .Reason}}}', stream: stderr, exit: 2}
  turn_end:
    event: Interrupt
  session_end:
    event: SessionEnd
    budget: 1s
    when: {reason: [delete]}
  subagent_stop:
    event: SubagentStop
    stdin: {agent_id: agent_id, session: agent_id}
`

func TestLoadFull(t *testing.T) {
	p, err := Load("example", []byte(full))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Run("doors ordered by layer then as listed", func(t *testing.T) {
		var names []string
		for _, d := range p.Doors {
			names = append(names, d.Name)
		}
		if want := []string{"notify", "watcher", "queue"}; !slices.Equal(names, want) {
			t.Errorf("door order = %v, want %v", names, want)
		}
	})
	t.Run("http door", func(t *testing.T) {
		d := p.Doors[0]
		if d.Match != (Match{Provider: "other", Host: "lab/example"}) {
			t.Errorf("match = %+v", d.Match)
		}
		if d.DedupeWindow != 10*time.Minute {
			t.Errorf("dedupe_window = %v, want 10m", d.DedupeWindow)
		}
		if !slices.Equal(d.Gone, []int{404, 410}) {
			t.Errorf("gone = %v", d.Gone)
		}
		if d.Headers["Content-Type"] != "application/json" {
			t.Errorf("headers = %v", d.Headers)
		}
	})
	t.Run("line door keys", func(t *testing.T) {
		d := p.Doors[1]
		if d.Hold != 45*time.Second || d.EndExit != 0 || d.Idle != `{"outcome":"idle"}` {
			t.Errorf("hold %v, end_exit %d, idle %q", d.Hold, d.EndExit, d.Idle)
		}
	})
	t.Run("session", func(t *testing.T) {
		s := p.Session
		if s.Env != "EXAMPLE_SESSION_ID" || s.Stdin != "session_id" || !slices.Equal(s.ProcessShared, []string{"app-server"}) || !s.HooksWithoutEnv {
			t.Errorf("session = %+v", s)
		}
		if s.Record == nil || s.Record.Glob != "sessions/*/{{.HookSession}}/session.json" || !slices.Equal(s.Record.SameAgent, []string{"rewind"}) {
			t.Errorf("record = %+v", s.Record)
		}
	})
	t.Run("hooks", func(t *testing.T) {
		h := p.Hooks
		if h.Prompt == nil || h.Prompt.SignalPattern() == nil || !h.Prompt.Once || h.Prompt.Stdin.Prompt != "prompt" {
			t.Fatalf("prompt = %+v", h.Prompt)
		}
		if h.Stop == nil || !h.Stop.OncePerTurn || h.Stop.Outputs.Notice.Stream != StreamStderr || h.Stop.Outputs.Notice.Exit != 2 {
			t.Errorf("stop = %+v", h.Stop)
		}
		if h.SessionEnd == nil || h.SessionEnd.Budget != time.Second || !slices.Equal(h.SessionEnd.When["reason"], []string{"delete"}) {
			t.Errorf("session_end = %+v", h.SessionEnd)
		}
		if h.SessionStart == nil || h.SessionStart.Source == nil || !slices.Equal(h.SessionStart.Source.TakeOver, []string{"clear"}) || h.SessionStart.ContextLimit != 10000 {
			t.Errorf("session_start = %+v", h.SessionStart)
		}
		if h.TurnEnd == nil || h.SubagentStop == nil || h.SubagentStop.Stdin.Session != "agent_id" {
			t.Errorf("turn_end %v, subagent_stop %+v", h.TurnEnd, h.SubagentStop)
		}
		if h.LostFix != "re-trust the hooks in /hooks" {
			t.Errorf("lost_fix = %q", h.LostFix)
		}
	})
	t.Run("one_shot", func(t *testing.T) {
		o := p.OneShot
		if !slices.Equal(o.Env["EXAMPLE_ENTRYPOINT"], []string{"batch", "sdk"}) || !slices.Equal(o.Env["EXAMPLE_ATTENDED"], []string{"0"}) || len(o.Env) != 2 {
			t.Errorf("one_shot.env = %v", o.Env)
		}
		if !slices.Equal(o.Args, []string{"-p", "exec"}) {
			t.Errorf("one_shot.args = %v", o.Args)
		}
	})
	t.Run("set keys", func(t *testing.T) {
		for _, k := range []string{"name", "detect", "session", "one_shot", "doors", "hooks", "notes", "evidence"} {
			if !p.has(k) {
				t.Errorf("Has(%q) = false", k)
			}
		}
	})
}

func TestLoadDefaults(t *testing.T) {
	p, err := Load("plain", []byte(`name: plain
doors:
  - {name: out, layer: 2, transport: stdout}
  - {name: push, layer: 1, transport: http, url: 'http://receiver/x', body: '{}'}
hooks:
  stop:
    event: Stop
    outputs:
      block: {text: x}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Run("match provider is the file's name", func(t *testing.T) {
		if got := p.Doors[0].Match.Provider; got != "plain" {
			t.Errorf("match.provider = %q, want plain", got)
		}
	})
	t.Run("line, end and end_exit", func(t *testing.T) {
		d := p.Doors[1]
		if d.Line != "{{.Line}}" || d.End != "{{.Line}}" || d.EndExit != 1 {
			t.Errorf("line %q, end %q, end_exit %d", d.Line, d.End, d.EndExit)
		}
	})
	t.Run("output stream stdout, exit 0", func(t *testing.T) {
		o := p.Hooks.Stop.Outputs.Block
		if o.Stream != StreamStdout || o.Exit != 0 {
			t.Errorf("stream %q, exit %d", o.Stream, o.Exit)
		}
	})
	t.Run("unset keys", func(t *testing.T) {
		if p.has("detect") || p.has("session") || p.has("extends") {
			t.Errorf("Has reports keys the file left out")
		}
	})
	t.Run("take_over_every_start", func(t *testing.T) {
		q, err := Load("plain", []byte("name: plain\nhooks:\n  session_start: {event: S, take_over_every_start: true, source: {path: source, fork: [fork]}}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if r := q.Hooks.SessionStart; !r.TakeOverEveryStart || !slices.Equal(r.Source.Fork, []string{"fork"}) {
			t.Errorf("session_start = %+v, want take_over_every_start beside a fork source", r)
		}
	})
	t.Run("end follows line", func(t *testing.T) {
		q, err := Load("plain", []byte("name: plain\ndoors:\n  - {name: out, layer: 2, transport: exit, line: 'L {{.Line}}'}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if q.Doors[0].End != "L {{.Line}}" {
			t.Errorf("end = %q, want the door's line", q.Doors[0].End)
		}
	})
}

func TestLoadRefuses(t *testing.T) {
	door := func(extra string) string {
		return "name: example\ndoors:\n  - " + extra + "\n"
	}
	cases := []struct {
		name, file string
		want       []string // substrings of the error, in order of the key then the problem
	}{
		{"unknown top key", "name: example\nsurprise: 1\n", []string{`unknown configuration key "surprise"`}},
		{"before_tool role", "name: example\nhooks:\n  before_tool: {event: PreToolUse}\n", []string{`"before_tool"`}},
		{"after_tool role", "name: example\nhooks:\n  after_tool: {event: PostToolUse}\n", []string{`"after_tool"`}},
		{"shell hooks key", "name: example\nhooks:\n  shell: {}\n", []string{`"shell"`}},
		{"tool stdin path", "name: example\nhooks:\n  prompt: {event: P, stdin: {tool_name: x}}\n", []string{`"tool_name"`}},
		{"deny output", "name: example\nhooks:\n  stop: {event: S, outputs: {deny: {text: x}}}\n", []string{`"deny"`}},
		{"rewrite output", "name: example\nhooks:\n  prompt: {event: P, outputs: {rewrite: {text: x}}}\n", []string{`"rewrite"`}},
		{"block at session_start", "name: example\nhooks:\n  session_start: {event: S, outputs: {block: {text: x}}}\n", []string{`"block"`}},
		{"outputs on a lifecycle role", "name: example\nhooks:\n  session_end: {event: E, outputs: {}}\n", []string{`"outputs"`}},
		{"stdin session outside subagent_stop", "name: example\nhooks:\n  session_end: {event: E, stdin: {session: agent_id}}\n", []string{`"session"`}},
		{"subagent_stop session path", "name: example\nhooks:\n  subagent_stop: {event: E, stdin: {session: 'a..b'}}\n", []string{"hooks.subagent_stop.stdin.session", "stdin field path"}},
		{"signal outside prompt", "name: example\nhooks:\n  stop: {event: S, signal: x}\n", []string{`"signal"`}},
		{"once_per_turn outside stop", "name: example\nhooks:\n  prompt: {event: P, once_per_turn: true}\n", []string{`"once_per_turn"`}},
		{"second document", "name: example\n---\nname: other\n", []string{"more than one YAML document"}},
		{"environment reference", "name: example\nnotes: '${HOME} and ${SECRET_X}'\n", []string{"${HOME}", "${SECRET_X}"}},
		{"name missing", "notes: x\n", []string{"name: required"}},
		{"name grammar", "name: Example\n", []string{"name: must match"}},
		{"name differs from file", "name: other\n", []string{"name:", `"other"`, `"example.yaml"`}},
		{"extends itself", "name: example\nextends: example\n", []string{"extends:"}},
		{"detect with extends", "name: example\nextends: base\ndetect: {env: [X]}\n", []string{"detect:", "only by name"}},
		{"bad evidence", "name: example\nevidence: guessed\n", []string{"evidence:"}},
		{"detect env name", "name: example\ndetect: {env: [BAD-NAME]}\n", []string{"detect.env[0]"}},
		{"session env name", "name: example\nsession: {env: 'A B'}\n", []string{"session.env"}},
		{"process with a directory", "name: example\nsession: {process: [/usr/bin/x]}\n", []string{"session.process[0]"}},
		{"hooks_without_env without env", "name: example\nsession: {stdin: session_id, hooks_without_env: true}\n", []string{"session.hooks_without_env", "needs session.env and session.stdin"}},
		{"hooks_without_env without stdin", "name: example\nsession: {env: A, hooks_without_env: true}\n", []string{"session.hooks_without_env", "needs session.env and session.stdin"}},
		{"process_shared without process", "name: example\nsession: {process_shared: [app-server]}\n", []string{"session.process_shared", "needs session.process"}},
		{"lost_fix of blanks", "name: example\nhooks: {lost_fix: '  '}\n", []string{"hooks.lost_fix", "one line"}},
		{"lost_fix of two lines", "name: example\nhooks: {lost_fix: \"open /hooks\\nthen trust\"}\n", []string{"hooks.lost_fix", "one line"}},
		{"lost_fix with an escape", "name: example\nhooks: {lost_fix: \"open \\e[31m/hooks\"}\n", []string{"hooks.lost_fix", "one line"}},
		{"one_shot unknown key", "name: example\none_shot: {argv: [x]}\n", []string{`"argv"`}},
		{"one_shot env name", "name: example\none_shot: {env: {BAD-NAME: [x]}}\n", []string{"one_shot.env", `"BAD-NAME"`, "not an environment variable name"}},
		{"one_shot env without values", "name: example\none_shot: {env: {EXAMPLE_MODE: []}}\n", []string{"one_shot.env.EXAMPLE_MODE", "at least one value"}},
		{"one_shot env empty value", "name: example\none_shot: {env: {EXAMPLE_MODE: [batch, '']}}\n", []string{"one_shot.env.EXAMPLE_MODE", "must not be empty"}},
		{"one_shot empty arg", "name: example\nsession: {process: [example]}\none_shot: {args: [-p, '']}\n", []string{"one_shot.args[1]", "must not be empty"}},
		{"one_shot arg of = alone", "name: example\nsession: {process: [example]}\none_shot: {args: [--prompt=, '=']}\n", []string{"one_shot.args[1]", "name before ="}},
		{"one_shot arg of dashes before =", "name: example\nsession: {process: [example]}\none_shot: {args: ['--=x']}\n", []string{"one_shot.args[0]", "name before ="}},
		{"one_shot arg of dashes alone", "name: example\nsession: {process: [example]}\none_shot: {args: [-p, '--']}\n", []string{"one_shot.args[1]", "needs the option's name"}},
		{"one_shot word holding =", "name: example\nsession: {process: [example]}\none_shot: {args: [format=json]}\n", []string{"one_shot.args[0]", "starts with -"}},
		{"one_shot args without process", "name: example\none_shot: {args: [-p]}\n", []string{"one_shot.args", "needs session.process"}},
		{"record without a directory", "name: example\nsession: {record: {glob: g, parent: p, reason: r}}\n", []string{"session.record", "dir_env or dir_default"}},
		{"record without glob", "name: example\nsession: {record: {dir_default: d, parent: p, reason: r}}\n", []string{"session.record.glob"}},
		{"record without parent", "name: example\nsession: {record: {dir_default: d, glob: g, reason: r}}\n", []string{"session.record.parent"}},
		{"socket transport", door("{name: ss, layer: 2, transport: socket}"), []string{"doors[0].transport"}},
		{"http on layer 2", door("{name: hh, layer: 2, transport: http, url: u, body: b}"), []string{"doors[0].layer", "layer 1"}},
		{"exit on layer 1", door("{name: ee, layer: 1, transport: exit}"), []string{"doors[0].layer", "layer 2"}},
		{"argv on http", door("{name: hh, layer: 1, transport: http, url: u, body: b, argv: [x]}"), []string{"doors[0].argv", "http transport"}},
		{"url on exec", door("{name: ee, layer: 2, transport: exec, argv: [x], url: u}"), []string{"doors[0].url", "exec transport"}},
		{"line on exec", door("{name: ee, layer: 2, transport: exec, argv: [x], line: l}"), []string{"doors[0].line"}},
		{"match on stdout", door("{name: oo, layer: 2, transport: stdout, match: {}}"), []string{"doors[0].match"}},
		{"hold on http", door("{name: hh, layer: 1, transport: http, url: u, body: b, hold: 45s}"), []string{"doors[0].hold"}},
		{"end_exit on exec", door("{name: ee, layer: 2, transport: exec, argv: [x], end_exit: 0}"), []string{"doors[0].end_exit"}},
		{"url missing", door("{name: hh, layer: 1, transport: http, body: b}"), []string{"doors[0].url", "required"}},
		{"body missing", door("{name: hh, layer: 1, transport: http, url: u}"), []string{"doors[0].body", "required"}},
		{"argv missing", door("{name: ee, layer: 2, transport: exec}"), []string{"doors[0].argv", "required"}},
		{"argv empty", door("{name: ee, layer: 2, transport: exec, argv: []}"), []string{"doors[0].argv"}},
		{"hold without idle", door("{name: oo, layer: 2, transport: exit, hold: 45s}"), []string{"doors[0].hold", "needs idle"}},
		{"idle without hold", door("{name: oo, layer: 2, transport: exit, idle: i}"), []string{"doors[0].idle", "needs hold"}},
		{"end_exit range", door("{name: oo, layer: 2, transport: exit, end_exit: 256}"), []string{"doors[0].end_exit"}},
		{"dedupe_window under 60s", door("{name: hh, layer: 1, transport: http, url: u, body: b, dedupe_window: 59s}"), []string{"doors[0].dedupe_window", "60s"}},
		{"gone 401", door("{name: hh, layer: 1, transport: http, url: u, body: b, gone: [401]}"), []string{"doors[0].gone[0]"}},
		{"gone 500", door("{name: hh, layer: 1, transport: http, url: u, body: b, gone: [404, 500]}"), []string{"doors[0].gone[1]"}},
		{"header name", door("{name: hh, layer: 1, transport: http, url: u, body: b, headers: {'Bad Header': x}}"), []string{"doors[0].headers"}},
		{"header names differing in case", door("{name: hh, layer: 1, transport: http, url: u, body: b, headers: {Content-Type: application/json, content-type: text/plain}}"), []string{"doors[0].headers", `"Content-Type"`, `"content-type"`, "differ only in case"}},
		{"door name", door("{name: Bad, layer: 2, transport: stdout}"), []string{"doors[0].name"}},
		{"duplicate door", "name: example\ndoors:\n  - {name: aa, layer: 2, transport: stdout}\n  - {name: aa, layer: 2, transport: exit}\n", []string{"doors[1].name", "twice"}},
		{"asset outside folder", door("{name: oo, layer: 2, transport: stdout, asset: ../x}"), []string{"doors[0].asset"}},
		{"event missing", "name: example\nhooks:\n  stop: {outputs: {block: {text: x}}}\n", []string{"hooks.stop.event"}},
		{"budget over the default", "name: example\nhooks:\n  session_end: {event: E, budget: 11s}\n", []string{"hooks.session_end.budget"}},
		{"when without values", "name: example\nhooks:\n  session_end: {event: E, when: {reason: []}}\n", []string{"hooks.session_end.when.reason"}},
		{"when path", "name: example\nhooks:\n  prompt: {event: P, when: {'a..b': [x]}}\n", []string{"hooks.prompt.when"}},
		{"output text missing", "name: example\nhooks:\n  stop: {event: S, outputs: {block: {stream: stdout}}}\n", []string{"hooks.stop.outputs.block.text"}},
		{"output stream", "name: example\nhooks:\n  stop: {event: S, outputs: {notice: {text: x, stream: tty}}}\n", []string{"hooks.stop.outputs.notice.stream"}},
		{"source path missing", "name: example\nhooks:\n  session_start: {event: S, source: {take_over: [clear]}}\n", []string{"hooks.session_start.source.path"}},
		{"context_limit below 0", "name: example\nhooks:\n  session_start: {event: S, context_limit: -1}\n", []string{"hooks.session_start.context_limit", "byte count"}},
		{"take_over beside take_over_every_start", "name: example\nhooks:\n  session_start: {event: S, take_over_every_start: true, source: {path: source, take_over: [clear]}}\n", []string{"hooks.session_start.take_over_every_start", "source.take_over"}},
		{"signal not RE2", "name: example\nhooks:\n  prompt: {event: P, signal: '(?<=x)'}\n", []string{"hooks.prompt.signal", "RE2"}},
		{"signal without line group", "name: example\nhooks:\n  prompt: {event: P, signal: '^(x)$'}\n", []string{"hooks.prompt.signal", "line"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := Load("example", []byte(c.file))
			if err == nil {
				t.Fatalf("Load accepted the file: %+v", p)
			}
			if le, ok := errors.AsType[*LoadError](err); !ok || le.Provider != "example" {
				t.Errorf("error %T %v is not a LoadError for example", err, err)
			}
			msg := err.Error()
			at := 0
			for _, w := range c.want {
				i := strings.Index(msg[at:], w)
				if i < 0 {
					t.Fatalf("error %q lacks %q after offset %d", msg, w, at)
				}
				at += i + len(w)
			}
		})
	}
}

func TestLoadErrorQuotesNoValue(t *testing.T) {
	_, err := Load("example", []byte("name: example\ndoors:\n  - {name: hh, layer: 1, transport: http, url: u, body: b, headers: {X: hunter2}, gone: [500]}\n"))
	if err == nil {
		t.Fatal("Load accepted the file")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q quotes a header value", err)
	}
}
