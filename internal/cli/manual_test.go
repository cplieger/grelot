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
		"`no change`", "## How to communicate", "`announce`", "Grelot paths", "grelot path --local",
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

func TestManual_howToCommunicateCoversSendingAndReceivingOnce(t *testing.T) {
	_, rest, ok := strings.Cut(manual, "\n## How to communicate\n")
	if !ok {
		t.Fatal("the manual has no How to communicate section")
	}
	comm, _, ok := strings.Cut(rest, "\n## Commands\n")
	if !ok {
		t.Fatal("How to communicate does not come before the Commands part")
	}
	sending, receiving, ok := strings.Cut(comm, "**Receiving:**")
	if !ok || !strings.Contains(sending, "**Sending:**") {
		t.Fatal("How to communicate has no Sending half followed by a Receiving half")
	}
	for _, want := range []string{
		"goes in that path's thread, even when nobody follows it", "`announce`", "`@name` is for a question or request",
		"never in several direct messages", "a reply to a thread post goes in that thread",
		"`--notify-read all`", "`--notify-read '@a,@b'`", "`--by=<d>`", "with or without `--notify-read`", "only a reply confirms",
	} {
		if !strings.Contains(sending, want) {
			t.Errorf("Sending does not say %q", want)
		}
	}
	for _, want := range []string{
		"`grelot follow /workspace/<repo>`", "`grelot unfollow <path>`", "`grelot history --path <p>`",
		"`grelot wait --timeout=<d>`", "\"" + strings.TrimPrefix(dataLabel, " · ") + "\"", "ask your user before",
		"Never send a bare acknowledgment", "`grelot receipts`",
	} {
		if !strings.Contains(receiving, want) {
			t.Errorf("Receiving does not say %q", want)
		}
	}
	// Each rule is stated once, here, and the old advice to move a
	// thread post that reached nobody into a direct message is gone.
	for phrase, want := range map[string]int{
		"grelot inbox` at the start and end of each task": 1, "Never put a secret": 1, "Unfollow a thread": 1,
		"Which thread": 0, "send it to `@name` or `announce` instead": 0, "message each one": 0,
		"To tell a specific agent about your work": 0, "If several fit": 0,
	} {
		if n := strings.Count(manual, phrase); n != want {
			t.Errorf("the manual says %q %d times, want %d", phrase, n, want)
		}
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
