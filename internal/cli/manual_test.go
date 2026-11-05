package cli

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// maxManualWords is the manual's size guard, the cap of a skill reference.
const maxManualWords = 6000

func TestManual_underTheWordCap(t *testing.T) {
	if n := len(strings.Fields(manual)); n > maxManualWords {
		t.Errorf("grelot help is %d words, over the cap of %d", n, maxManualWords)
	}
}

func TestManual_opensWithTheGuidanceThenTheCommands(t *testing.T) {
	guide, commands, ok := strings.Cut(manual, "\n## Commands\n")
	if !ok {
		t.Fatal("the manual has no Commands part")
	}
	// The guidance an agent needs before its first command: registration,
	// the wake, posting, paths, who, delegates and the hooks' lines.
	for _, want := range []string{
		"grelot register <repo>-<role>", "--ttl", "grelot renew --ttl", "grelot revive --ttl",
		"wake: down", "start the watcher", "grelot inbox` at the start and end of each task",
		"`no change`", "Which thread", "`announce`", "Grelot paths", "grelot path --local",
		"same environment", "grelot who --json", "--holds", "grelot delegate <label>",
		"grelot as <handle>", "--self", "grelot: not registered", "is held once",
		"never asked to register", "grelot set --desc", "repos:",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("the opening guidance does not say %q", want)
		}
	}
	if strings.Contains(guide, "### grelot ") {
		t.Error("a command section comes before the Commands part")
	}
	if !strings.Contains(commands, "\n## Setup\n") || !strings.Contains(commands, "GRELOT_TOKEN_FILE") {
		t.Error("the manual does not end with the client config keys")
	}
}

func TestManual_everyCommandHasOneSectionWithAnExample(t *testing.T) {
	names := append(slices.Sorted(maps.Keys(verbs)), "provider")
	for _, verb := range names {
		t.Run(verb, func(t *testing.T) {
			s := section(verb)
			if s == "" {
				t.Fatalf("no ### grelot %s section", verb)
			}
			if !strings.Contains(s, "`grelot "+verb) {
				t.Errorf("the section gives no usage line starting grelot %s", verb)
			}
			if !strings.Contains(s, "\n```\ngrelot ") {
				t.Errorf("the section gives no example")
			}
			if n := strings.Count(manual, "### grelot "+verb+"\n"); n != 1 {
				t.Errorf("%d sections, want 1", n)
			}
		})
	}
	heads := regexp.MustCompile(`(?m)^### grelot (\S+)$`).FindAllStringSubmatch(manual, -1)
	for _, h := range heads {
		if !slices.Contains(names, h[1]) {
			t.Errorf("section grelot %s names no command", h[1])
		}
	}
}

// nextStep is what a refusal must name: a grelot command, a flag, or the
// one shell command a config file's mode needs.
var nextStep = regexp.MustCompile(`grelot (--self |--provider \S+ )?(as \S+ )?(` +
	`register|revive|me|set|renew|bye|who|path|help|post|inbox|history|receipts|threads|follow|unfollow|` +
	`reserve|release|reservations|delegate|as|watch|wait|provider check)\b|--[a-z][a-z-]+|chmod 600`)

func TestRefusals_nameTheNextCommandOrAFlag(t *testing.T) {
	ts := newServer(t)
	cases := []struct {
		name  string
		setup func(a *agent)
		args  []string
	}{
		{name: "no command"},
		{name: "unknown command", args: []string{"bogus"}},
		{name: "provider with no name", args: []string{"--provider"}},
		{name: "help for no command", args: []string{"help", "bogus"}},
		{name: "unknown flag", args: []string{"me", "--bogus"}},
		{name: "flag with no value", args: []string{"renew", "--ttl"}},
		{name: "register with no name", args: []string{"register"}},
		{name: "register with no ttl", args: []string{"register", "tool-dev", "--paths", "/workspace/tool", "--desc", "x"}},
		{name: "delegate register with identity flags", args: []string{"register", "--delegate", "l", "--ttl", "1h"}},
		{name: "revive two names", args: []string{"revive", "a", "b"}},
		{name: "set an argument", args: []string{"set", "x"}},
		{name: "renew two ids", args: []string{"renew", "a", "b"}},
		{name: "renew a word", args: []string{"renew", "abc", "--ttl", "1h"}},
		{name: "bye an argument", args: []string{"bye", "x"}},
		{name: "path nothing", args: []string{"path"}},
		{name: "path local outside the root", args: []string{"path", "--local", "/tmp/x"}},
		{name: "path local no clone", args: []string{"path", "--local", "/workspace/nothere/x"}},
		{name: "post no target", args: []string{"post"}},
		{name: "post missing fields", args: []string{"post", "announce", "hello"}},
		{name: "inbox an argument", args: []string{"inbox", "x"}},
		{name: "history an argument", args: []string{"history", "x"}},
		{name: "receipts no id", args: []string{"receipts"}},
		{name: "threads two paths", args: []string{"threads", "/a", "/b"}},
		{name: "follow no path", args: []string{"follow"}},
		{name: "unfollow no path", args: []string{"unfollow"}},
		{name: "reserve no path", args: []string{"reserve"}},
		{name: "release no id", args: []string{"release"}},
		{name: "reservations two paths", args: []string{"reservations", "/a", "/b"}},
		{name: "delegate no label", args: []string{"delegate"}},
		{name: "delegate revoke no label", args: []string{"delegate", "revoke"}},
		{name: "as nothing", args: []string{"as"}},
		{name: "as an unknown command", args: []string{"as", "a/b", "bogus"}},
		{name: "as with self", args: []string{"--self", "as", "a/b", "me"}},
		{name: "watch an argument", args: []string{"watch", "x"}},
		{name: "watch a door and a transport", args: []string{"watch", "--door", "a", "--transport", "exit"}},
		{name: "watch a command with no transport", args: []string{"watch", "--", "x"}},
		{name: "watch a bad transport", args: []string{"watch", "--transport", "http"}},
		{name: "watch no door", args: []string{"watch"}},
		{name: "watch as a delegate", args: []string{"as", "a/b", "watch"}},
		{name: "wait no timeout", args: []string{"wait"}},
		{name: "wait an argument", args: []string{"wait", "x", "--timeout", "1m"}},
		{name: "unregistered wait", args: []string{"wait", "--timeout", "1m"}},
		{name: "unregistered inbox", args: []string{"inbox"}},
		{name: "unregistered set", args: []string{"set", "--desc", "x"}},
		{
			name: "a name another session holds", args: []string{"register", "tool-dev", "--paths", "/workspace/tool", "--desc", "x", "--ttl", "1h"},
			setup: func(*agent) {
				holder := newAgent(t, ts, "holder", "")
				holder.must("register", "tool-dev", "--paths", "/workspace/tool", "--desc", "x", "--ttl", "1h")
			},
		},
		{
			name: "a body over 64 KiB", args: []string{"post", "announce", "s", "--name", "no change", "--paths", "no change", "--desc", "no change"},
			setup: func(a *agent) { a.env.Stdin = strings.NewReader(strings.Repeat("x", 65<<10)) },
		},
		{name: "no config", args: []string{"me"}, setup: func(a *agent) { a.env.ConfigDir = t.TempDir() }},
		{
			name: "config with no token", args: []string{"me"},
			setup: func(a *agent) { writeClientConfig(t, a.env.ConfigDir, "server: "+ts.URL+"\n") },
		},
		{
			name: "config with a bad server", args: []string{"me"},
			setup: func(a *agent) { writeClientConfig(t, a.env.ConfigDir, "server: ftp://x\ntoken: t\n") },
		},
		{
			name: "config with an unknown key", args: []string{"me"},
			setup: func(a *agent) { writeClientConfig(t, a.env.ConfigDir, "server: "+ts.URL+"\ntoken: t\nbogus: 1\n") },
		},
		{
			name: "config with a reference", args: []string{"me"},
			setup: func(a *agent) { writeClientConfig(t, a.env.ConfigDir, "server: "+ts.URL+"\ntoken: ${TOKEN}\n") },
		},
		{
			name: "config with a bad alias", args: []string{"me"},
			setup: func(a *agent) {
				writeClientConfig(t, a.env.ConfigDir, "server: "+ts.URL+"\ntoken: t\nrepos: {\"github.com/a/b\": \"x/y\"}\n")
			},
		},
		{name: "no provider detected", args: []string{"me"}, setup: func(a *agent) { delete(a.vars, "ALPHA_ID") }},
		{name: "a provider with no session", args: []string{"--provider", "beta", "me"}},
		{
			name: "a wrong token", args: []string{"me"},
			setup: func(a *agent) { writeClientConfig(t, a.env.ConfigDir, "server: "+ts.URL+"\ntoken: wrong\n") },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAgent(t, ts, "s-"+strings.ReplaceAll(c.name, " ", "-"), "")
			if c.setup != nil {
				c.setup(a)
			}
			code, _, errOut := a.run(c.args...)
			if code == exitOK {
				t.Fatalf("grelot %s exited 0, want a refusal", strings.Join(c.args, " "))
			}
			if !nextStep.MatchString(errOut) {
				t.Errorf("grelot %s: refusal %q names no grelot command or flag", strings.Join(c.args, " "), errOut)
			}
		})
	}
}
