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
