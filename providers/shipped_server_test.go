//go:build !windows

package providers_test

import (
	"strings"
	"testing"
	"time"
)

// A turn line stays under 30 words in its longest state, a description
// reminder and a down wake with its start line. That line is measured once
// with Claude Code's start line; every other door's start line must fit
// the words left.
func TestShipped_startLinesKeepTheTurnLineUnderThirtyWords(t *testing.T) {
	srv := newServer(t)
	sh := claudeShell(t, srv, claudeTUI)
	sh.register("claude-dev")
	peer := claudeShell(t, srv, claudeFork)
	peer.register("peer-dev")
	for range 12 {
		peer.post("@claude-dev", "x")
	}
	srv.clock.advance(90 * time.Minute)
	got := sh.hook("prompt", sh.fixture("prompt-typed"))
	line := strings.TrimSuffix(got.Stdout, "\n")
	if strings.Contains(line, "\n") || !strings.Contains(line, "12 unread") || !strings.Contains(line, "still true?") || !strings.Contains(line, "start: "+claudeStart) {
		t.Fatalf("turn line %q, want one line with 12 unread, the reminder and the start line", line)
	}
	rest := turnWords(line) - turnWords(claudeStart)
	for _, name := range shippedNames(t) {
		for _, d := range shipped(t, name).Doors {
			if d.Start == "" {
				continue
			}
			if n := rest + turnWords(d.Start); n >= 30 {
				t.Errorf("%s door %s: start %q makes the longest turn line %d words, want under 30", name, d.Name, d.Start, n)
			}
		}
	}
}

// turnWords counts a turn line's words as its 30-word ceiling does: the
// separator dots are not words.
func turnWords(s string) int {
	n := 0
	for f := range strings.FieldsSeq(s) {
		if f != "\u00b7" {
			n++
		}
	}
	return n
}
