//go:build !windows

package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

// entriesOf is the entries list of an inbox or history answer.
func entriesOf(t *testing.T, r *response) []map[string]any {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	raw, _ := r.body["entries"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

// mustRegisterAlone registers name on a work path of its own, so no join
// card reaches another test agent.
func (h *harness) mustRegisterAlone(session, name string) {
	h.t.Helper()
	if r := h.as(session, http.MethodPost, "/v1/register", registerBody(name, "6h", "/workspace/"+name)); r.status != http.StatusCreated {
		h.t.Fatalf("register %s: %d %s", name, r.status, r.raw)
	}
}

func (h *harness) mustPost(session string, to any, subject string) float64 {
	h.t.Helper()
	r := h.as(session, http.MethodPost, "/v1/posts", postBody(to, subject, "a body"))
	if r.status != http.StatusCreated {
		h.t.Fatalf("post %q: %d %s", subject, r.status, r.raw)
	}
	id, _ := r.body["message"].(float64)
	return id
}

func TestInbox_fetchMovesNothingAndTheAckMarksRead(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "reader")
	h.mustRegisterAlone(sessB, "writer")
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "first")
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "second")
	h.clk.advance(2 * time.Minute)
	page := entriesOf(t, h.as(sessA, http.MethodGet, "/v1/inbox", nil))
	again := entriesOf(t, h.as(sessA, http.MethodGet, "/v1/inbox", nil))
	if len(page) != 2 || len(again) != 2 || page[0]["subject"] != "first" || page[1]["from"] != "writer" {
		t.Fatalf("inbox = %v then %v, want both posts twice, oldest first", page, again)
	}
	if one := entriesOf(t, h.as(sessA, http.MethodGet, "/v1/inbox?limit=1", nil)); len(one) != 1 {
		t.Errorf("limit=1 gave %d entries", len(one))
	}
	r := h.as(sessA, http.MethodPost, "/v1/inbox/ack", map[string]any{"upto": page[1]["seq"]})
	if r.status != http.StatusOK || r.body["read"] != page[1]["seq"] {
		t.Fatalf("ack: %d %s", r.status, r.raw)
	}
	if after := entriesOf(t, h.as(sessA, http.MethodGet, "/v1/inbox", nil)); len(after) != 0 {
		t.Errorf("inbox after the ack = %v, want nothing", after)
	}
	lines := h.logs.lines(slog.LevelInfo, "inbox read")
	if len(lines) != 1 {
		t.Fatalf("inbox read lines = %d, want 1", len(lines))
	}
	attrs := map[string]string{}
	lines[0].Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	if attrs["count"] != "2" || attrs["oldest_delay_ms"] != "120000" {
		t.Errorf("inbox read = %v, want count 2 and oldest_delay_ms 120000", attrs)
	}
	if r := h.as(sessA, http.MethodPost, "/v1/inbox/ack", map[string]any{"upto": page[1]["seq"]}); r.status != http.StatusOK {
		t.Errorf("a repeated ack: %d %s", r.status, r.raw)
	}
	if n := len(h.logs.lines(slog.LevelInfo, "inbox read")); n != 1 {
		t.Errorf("a repeated ack wrote an inbox read line")
	}
	if r := h.as(sessA, http.MethodGet, "/v1/inbox?limit=x", nil); r.status != http.StatusBadRequest {
		t.Errorf("limit=x: %d, want 400", r.status)
	}
}

func TestHistory_routeMapsEveryFilterAndMovesNothing(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "asker")
	h.mustRegisterAlone(sessB, "peer")
	h.mustRegisterAlone(sessC, "herald")
	h.mustPost(sessA, map[string]any{"agent": "peer"}, "question")
	h.clk.advance(time.Hour)
	h.mustPost(sessB, map[string]any{"agent": "asker"}, "answer")
	h.clk.advance(time.Minute)
	h.mustPost(sessC, "announce", "notice")
	subjects := func(query string) []string {
		var out []string
		for _, e := range entriesOf(t, h.as(sessA, http.MethodGet, "/v1/history"+query, nil)) {
			out = append(out, fmt.Sprint(e["subject"]))
		}
		return out
	}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", []string{"question", "answer", "notice"}},
		{"?with=@peer", []string{"question", "answer"}},
		{"?from=@me", []string{"question"}},
		{"?from=@peer", []string{"answer"}},
		{"?announce=1", []string{"notice"}},
		{"?grep=ANSW", []string{"answer"}},
		{"?since=30m", []string{"answer", "notice"}},
		{"?until=" + testNow.Add(time.Minute).Format(time.RFC3339), []string{"question"}},
		{"?limit=1", []string{"notice"}},
		{"?with=peer&since=30m", []string{"answer"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			if got := subjects(tc.query); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("history%s = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
	if unread := entriesOf(t, h.as(sessA, http.MethodGet, "/v1/inbox", nil)); len(unread) != 2 {
		t.Errorf("inbox after history = %d entries, want both still unread", len(unread))
	}
	for _, q := range []string{"?since=soon", "?thread=x", "?limit=-1", "?full=yes", "?from=@me&with=nobody"} {
		if r := h.as(sessA, http.MethodGet, "/v1/history"+q, nil); r.status/100 != 4 {
			t.Errorf("history%s: %d, want a refusal", q, r.status)
		}
	}
}

func TestHistory_lastReadIsTheLastAckedPage(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "asker")
	h.mustRegisterAlone(sessB, "peer")
	h.mustPost(sessB, map[string]any{"agent": "asker"}, "first")
	page := entriesOf(t, h.as(sessA, http.MethodGet, "/v1/inbox", nil))
	h.as(sessA, http.MethodPost, "/v1/inbox/ack", map[string]any{"upto": page[0]["seq"]})
	h.mustPost(sessB, map[string]any{"agent": "asker"}, "second")
	got := entriesOf(t, h.as(sessA, http.MethodGet, "/v1/history?last_read=1", nil))
	if len(got) != 1 || got[0]["subject"] != "first" {
		t.Errorf("last_read = %v, want the acked page alone", got)
	}
}

func TestThreads_listsLivePathThreadsUnderAPath(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "poster")
	h.mustPost(sessA, map[string]any{"path": repoGrelot + "/internal"}, "inner")
	h.mustPost(sessA, map[string]any{"path": "/workspace/other"}, "outer")
	paths := func(query string) []string {
		r := h.as(sessA, http.MethodGet, "/v1/threads"+query, nil)
		if r.status != http.StatusOK {
			t.Fatalf("threads%s: %d %s", query, r.status, r.raw)
		}
		var out []string
		for _, th := range r.body["threads"].([]any) {
			out = append(out, th.(map[string]any)["path"].(string))
		}
		return out
	}
	if got := paths("?path=" + repoGrelot); fmt.Sprint(got) != "["+repoGrelot+"/internal]" {
		t.Errorf("threads under %s = %v", repoGrelot, got)
	}
	if got := paths(""); len(got) != 2 {
		t.Errorf("every thread = %v, want 2", got)
	}
	if r := h.as(sessA, http.MethodGet, "/v1/threads?path=/workspace/grelot/", nil); r.status != http.StatusBadRequest {
		t.Errorf("a non-canonical path: %d, want 400", r.status)
	}
	if r := h.as(sessB, http.MethodGet, "/v1/threads", nil); r.status != http.StatusGone {
		t.Errorf("a session with no identity: %d, want 410", r.status)
	}
}

func TestReceipts_routeForTheSenderAndRecipientsOnly(t *testing.T) {
	h := newHarness(t)
	h.mustRegisterAlone(sessA, "sender")
	h.mustRegisterAlone(sessB, "reader")
	h.mustRegisterAlone(sessC, "outsider")
	id := h.mustPost(sessA, map[string]any{"agent": "reader"}, "hello")
	path := fmt.Sprintf("/v1/messages/%d/receipts", int(id))
	state := func() string {
		r := h.as(sessA, http.MethodGet, path, nil)
		if r.status != http.StatusOK {
			t.Fatalf("receipts: %d %s", r.status, r.raw)
		}
		rs := r.body["recipients"].([]any)
		if len(rs) != 1 {
			t.Fatalf("recipients = %v, want the reader", rs)
		}
		e := rs[0].(map[string]any)
		return fmt.Sprint(e["name"], " ", e["state"], " ", e["read_at"] != nil)
	}
	if got := state(); got != "reader unread false" {
		t.Errorf("before the read: %s", got)
	}
	page := entriesOf(t, h.as(sessB, http.MethodGet, "/v1/inbox", nil))
	h.as(sessB, http.MethodPost, "/v1/inbox/ack", map[string]any{"upto": page[0]["seq"]})
	if got := state(); got != "reader read true" {
		t.Errorf("after the read: %s", got)
	}
	if r := h.as(sessC, http.MethodGet, path, nil); r.status != http.StatusForbidden {
		t.Errorf("an outsider: %d, want 403", r.status)
	}
	if r := h.as(sessA, http.MethodGet, "/v1/messages/x/receipts", nil); r.status != http.StatusBadRequest {
		t.Errorf("a bad id: %d, want 400", r.status)
	}
	if r := h.as(sessA, http.MethodGet, "/v1/messages/999/receipts", nil); r.status != http.StatusNotFound {
		t.Errorf("an unknown id: %d, want 404", r.status)
	}
}

func TestFollows_followAndUnfollowALiveThread(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "follower")
	h.mustRegister(sessB, "poster")
	h.mustPost(sessB, map[string]any{"path": "/workspace/other"}, "outer")
	h.mustPost(sessB, map[string]any{"path": repoGrelot}, "inner")
	follows := func(r *response) []any {
		if r.status != http.StatusOK {
			t.Fatalf("%d %s", r.status, r.raw)
		}
		return r.body["identity"].(map[string]any)["follows"].([]any)
	}
	if got := follows(h.as(sessA, http.MethodPut, "/v1/follows?path=/workspace/other", nil)); fmt.Sprint(got) != "[/workspace/other]" {
		t.Errorf("follows after follow = %v", got)
	}
	if got := follows(h.as(sessA, http.MethodDelete, "/v1/follows?path=/workspace/other", nil)); len(got) != 0 {
		t.Errorf("follows after unfollow = %v", got)
	}
	if r := h.as(sessA, http.MethodPut, "/v1/follows?path=/workspace/none", nil); r.status != http.StatusNotFound {
		t.Errorf("follow with no thread: %d, want 404", r.status)
	}
	h.as(sessA, http.MethodPut, "/v1/follows?path="+repoGrelot, nil)
	if r := h.as(sessA, http.MethodDelete, "/v1/follows?path="+repoGrelot, nil); r.status != http.StatusConflict {
		t.Errorf("unfollow under a work path: %d %s, want 409", r.status, r.raw)
	}
}
