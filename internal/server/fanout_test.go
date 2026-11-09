//go:build !windows

package server

import (
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"
)

// postNotices posts from session to to and returns the answer's notices.
func (h *harness) postNotices(session string, to any, subject string) []string {
	h.t.Helper()
	r := h.as(session, http.MethodPost, "/v1/posts", postBody(to, subject, "a body"))
	if r.status != http.StatusCreated {
		h.t.Fatalf("post %q to %v: %d %s", subject, to, r.status, r.raw)
	}
	raw, _ := r.body["notices"].([]any)
	out := make([]string, 0, len(raw))
	for _, n := range raw {
		s, _ := n.(string)
		out = append(out, s)
	}
	return out
}

func agent(name string) map[string]any { return map[string]any{"agent": name} }

func TestPost_theSameSubjectToASecondAgentHintsAtAThread(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "sender")
	h.mustRegister(sessB, "first")
	h.mustRegister(sessC, "second")
	h.mustRegister(sessD, "third")
	hinted := func(notices []string) bool { return slices.Contains(notices, fanOutHint) }

	t.Run("first copy", func(t *testing.T) {
		if n := h.postNotices(sessA, agent("first"), "bolttable v1.0.0 is out"); hinted(n) {
			t.Errorf("notices = %q, want no hint on the first direct post", n)
		}
	})
	t.Run("again to the same agent", func(t *testing.T) {
		if n := h.postNotices(sessA, agent("first"), "bolttable v1.0.0 is out"); hinted(n) {
			t.Errorf("notices = %q, want no hint for a second post in one conversation", n)
		}
	})
	t.Run("second agent, other case and a Re: prefix", func(t *testing.T) {
		if n := h.postNotices(sessA, agent("second"), "RE:  Bolttable V1.0.0 is out "); !hinted(n) {
			t.Errorf("notices = %q, want the fan-out hint", n)
		}
	})
	t.Run("the receiver replies", func(t *testing.T) {
		if n := h.postNotices(sessC, agent("sender"), "Re: bolttable v1.0.0 is out"); hinted(n) {
			t.Errorf("notices = %q, want no hint: the receiver sent this subject to nobody else", n)
		}
	})
	t.Run("the sender answers the reply", func(t *testing.T) {
		if n := h.postNotices(sessA, agent("second"), "Re: bolttable v1.0.0 is out"); hinted(n) {
			t.Errorf("notices = %q, want no hint for a reply in an ongoing conversation", n)
		}
	})
	t.Run("a different subject", func(t *testing.T) {
		if n := h.postNotices(sessA, agent("third"), "bolttable v1.0.1 is out"); hinted(n) {
			t.Errorf("notices = %q, want no hint for another subject", n)
		}
	})
	t.Run("after the hour", func(t *testing.T) {
		h.clk.advance(fanOutWindow + time.Second)
		if n := h.postNotices(sessA, agent("third"), "bolttable v1.0.0 is out"); hinted(n) {
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
	if n := h.postNotices(sessA, agent("late"), "Re: the schema"); slices.Contains(n, fanOutHint) {
		t.Errorf("answer to the late asker: notices = %q, want no hint for a reply", n)
	}
	h.clk.advance(15 * time.Minute)
	if n := h.postNotices(sessA, agent("early"), "Re: the schema"); slices.Contains(n, fanOutHint) {
		t.Errorf("answer to the early asker, 70 min on: notices = %q, want no hint for a reply", n)
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
	if n := h.postNotices(sessA, agent("reader"), "the freeze"); slices.Contains(n, fanOutHint) {
		t.Errorf("notices = %q, want no hint: the announce, path and refused posts are not direct copies", n)
	}
}

func TestFanOut_holdsAtMostItsCapAndOnlyTheReplyWindow(t *testing.T) {
	var f fanOut
	start := time.Unix(1_000_000, 0)
	f.record(1, 10, "the oldest", start)
	for i := range fanOutCap {
		f.record(2, 20, "filler "+strconv.Itoa(i), start)
	}
	if f.record(1, 11, "the oldest", start) {
		t.Errorf("hint after the cap dropped the first copy, want none")
	}
	if len(f.sent) != fanOutCap {
		t.Errorf("window holds %d posts, want the cap, %d", len(f.sent), fanOutCap)
	}
	f.record(3, 30, "late", start.Add(time.Minute))
	f.prune(start.Add(replyWindow))
	if len(f.sent) != 1 || f.sent[0].subject != "late" {
		t.Errorf("after the reply window it holds %d posts, want only the one inside it", len(f.sent))
	}
}
