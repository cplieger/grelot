//go:build !windows

package server

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

// noticesOf is a post answer's notices.
func noticesOf(r *response) []string {
	raw, _ := r.body["notices"].([]any)
	out := make([]string, 0, len(raw))
	for _, n := range raw {
		s, _ := n.(string)
		out = append(out, s)
	}
	return out
}

// postNotices posts from session to to and returns the answer's notices.
func (h *harness) postNotices(session string, to any, subject string) []string {
	h.t.Helper()
	r := h.as(session, http.MethodPost, "/v1/posts", postBody(to, subject, "a body"))
	if r.status != http.StatusCreated {
		h.t.Fatalf("post %q to %v: %d %s", subject, to, r.status, r.raw)
	}
	return noticesOf(r)
}

// copyHinted reports whether notices hold the hint that one text for
// several agents belongs in a thread.
func copyHinted(notices []string) bool {
	return slices.ContainsFunc(notices, func(n string) bool {
		return strings.Contains(n, "the same text for several agents belongs in one post")
	})
}

func agent(name string) map[string]any { return map[string]any{"agent": name} }

func TestPost_theSameSubjectToASecondAgentHintsAtAThread(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "first")
	h.mustRegister(sessC, "second")
	h.mustRegister(sessD, "third")

	t.Run("first copy", func(t *testing.T) {
		if n := h.postNotices(sessA, agent("first"), "bolttable v1.0.0 is out"); copyHinted(n) {
			t.Errorf("notices = %q, want no hint on the first direct post", n)
		}
	})
	t.Run("again to the same agent", func(t *testing.T) {
		if n := h.postNotices(sessA, agent("first"), "bolttable v1.0.0 is out"); copyHinted(n) {
			t.Errorf("notices = %q, want no hint for a second post in one conversation", n)
		}
	})
	t.Run("second agent, other case and a Re: prefix", func(t *testing.T) {
		if n := h.postNotices(sessA, agent("second"), "RE:  Bolttable V1.0.0 is out "); !copyHinted(n) {
			t.Errorf("notices = %q, want the fan-out hint", n)
		}
	})
	t.Run("the receiver replies", func(t *testing.T) {
		if n := h.postNotices(sessC, agent("sender"), "Re: bolttable v1.0.0 is out"); copyHinted(n) {
			t.Errorf("notices = %q, want no hint: the receiver sent this subject to nobody else", n)
		}
	})
	t.Run("the sender answers the reply", func(t *testing.T) {
		if n := h.postNotices(sessA, agent("second"), "Re: bolttable v1.0.0 is out"); copyHinted(n) {
			t.Errorf("notices = %q, want no hint for a reply in an ongoing conversation", n)
		}
	})
	t.Run("a different subject", func(t *testing.T) {
		if n := h.postNotices(sessA, agent("third"), "bolttable v1.0.1 is out"); copyHinted(n) {
			t.Errorf("notices = %q, want no hint for another subject", n)
		}
	})
	t.Run("after the hour", func(t *testing.T) {
		h.clk.advance(time.Hour + time.Second)
		if n := h.postNotices(sessA, agent("third"), "bolttable v1.0.0 is out"); copyHinted(n) {
			t.Errorf("notices = %q, want no hint once the earlier copies are over an hour old", n)
		}
	})
}

func TestPost_aReplyToAQuestionOverAnHourOldHintsNothing(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "answerer")
	h.mustRegister(sessB, "early")
	h.mustRegister(sessC, "late")
	h.postNotices(sessB, agent("answerer"), "the schema")
	h.clk.advance(50 * time.Minute)
	h.postNotices(sessC, agent("answerer"), "the schema")
	h.clk.advance(5 * time.Minute)
	if n := h.postNotices(sessA, agent("late"), "Re: the schema"); copyHinted(n) {
		t.Errorf("answer to the late asker: notices = %q, want no hint for a reply", n)
	}
	h.clk.advance(15 * time.Minute)
	if n := h.postNotices(sessA, agent("early"), "Re: the schema"); copyHinted(n) {
		t.Errorf("answer to the early asker, 70 min on: notices = %q, want no hint for a reply", n)
	}
}

func TestPost_aReplyToAQuestionTheThreadStillHoldsHintsNothing(t *testing.T) {
	h := newHarnessWith(t, store.Options{Retention: func() time.Duration { return 168 * time.Hour }})
	sessions := []string{sessA, sessB, sessC}
	h.mustRegister(sessA, "answerer")
	h.mustRegister(sessB, "early")
	h.mustRegister(sessC, "late")
	h.postNotices(sessB, agent("answerer"), "the schema")
	for range 24 {
		h.clk.advance(4 * time.Hour)
		for _, s := range sessions {
			if r := h.as(s, http.MethodPatch, "/v1/me", map[string]any{"ttl": "6h", "where": where("a")}); r.status != http.StatusOK {
				t.Fatalf("renew %s: %d %s", s, r.status, r.raw)
			}
		}
	}
	h.postNotices(sessC, agent("answerer"), "the schema")
	h.postNotices(sessA, agent("late"), "Re: the schema")
	h.clk.advance(5 * time.Minute)
	if n := h.postNotices(sessA, agent("early"), "Re: the schema"); copyHinted(n) {
		t.Errorf("answer to a 96 h old question under a 168 h retention: notices = %q, want no hint for a reply", n)
	}
}

func TestPost_aResentCopyAnswersItsHintAgain(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "first")
	h.mustRegister(sessC, "second")
	h.postNotices(sessA, agent("first"), "the freeze")
	body := postBody(agent("second"), "the freeze", "a body")
	for _, try := range []string{"first answer", "re-send"} {
		r := h.request(http.MethodPost, "/v1/posts", body, keyHeader(sessA, "copy-key-0000001"))
		if r.status != http.StatusCreated || !copyHinted(noticesOf(r)) {
			t.Errorf("%s: %d notices %q, want 201 with the fan-out hint", try, r.status, noticesOf(r))
		}
	}
}

func TestPost_onlyDeliveredDirectPostsCountTowardTheHint(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "reader")
	h.postNotices(sessA, "announce", "the freeze")
	h.postNotices(sessA, map[string]any{"path": repoGrelot}, "the freeze")
	if r := h.as(sessA, http.MethodPost, "/v1/posts", postBody(agent("nobody"), "the freeze", "a body")); r.status != http.StatusNotFound {
		t.Fatalf("post to an unknown name: %d %s, want 404", r.status, r.raw)
	}
	if n := h.postNotices(sessA, agent("reader"), "the freeze"); copyHinted(n) {
		t.Errorf("notices = %q, want no hint: the announce, path and refused posts are not direct copies", n)
	}
}
