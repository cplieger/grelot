package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/client"
)

// hookTurnFile is alpha with every turn and lifecycle role. Its prompt and
// stop blocks are JSON, its stop notice a systemMessage, and its session
// end acts only for a deleted chat.
const hookTurnFile = hookAlphaFile + `  prompt:
    event: Prompt
    stdin: {agent_id: agent_id, prompt: prompt}
    outputs:
      context: {text: "{{.Context}}"}
      block: {text: '{"decision":"block","reason":{{json .Reason}}}'}
  stop:
    event: Stop
    stdin: {agent_id: agent_id}
    outputs:
      block: {text: '{"decision":"block","reason":{{json .Reason}}}'}
      notice: {text: '{"systemMessage":{{json .Reason}}}'}
  turn_end: {event: TurnEnd, stdin: {agent_id: agent_id}}
  session_end: {event: End, stdin: {agent_id: agent_id}, when: {reason: [delete]}}
  subagent_stop: {event: SubStop, stdin: {agent_id: agent_id}}
`

const (
	registerLine = "grelot: not registered. Register before you start work: run `grelot help`, then `grelot register` as it says."
	blockPrefix  = `{"decision":"block","reason":`
)

// turnAgent is a registered agent of session s-1 with the turn roles.
func turnAgent(t *testing.T, ts *testServer, file string) *agent {
	t.Helper()
	a, _ := registerAgent(t, ts, "s-1")
	return hooked(t, a, "alpha", file)
}

// prompt runs the prompt hook of session s-1 with text as the prompt.
func (a *agent) prompt(text string) (code int, stdout string) {
	in, _ := json.Marshal(map[string]string{"session_id": "s-1", "prompt": text})
	code, stdout, _ = a.hook("prompt", string(in))
	return code, stdout
}

// blockReason is the reason of a JSON block line, "" when out is none.
func blockReason(t *testing.T, out string) string {
	t.Helper()
	if !strings.HasPrefix(out, blockPrefix) {
		return ""
	}
	var b struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatalf("block %q is not JSON: %v", out, err)
	}
	return b.Reason
}

// firstUnread is the seq of the first unread delivery of session, read
// without acking it.
func firstUnread(t *testing.T, ts *testServer, session string) uint64 {
	t.Helper()
	var page client.Inbox
	ts.call(t, session, true, &client.Request{Method: http.MethodGet, Path: "/v1/inbox"}, &page)
	if len(page.Entries) == 0 {
		t.Fatal("no unread delivery")
	}
	return page.Entries[0].Seq
}

func words(s string) int {
	n := 0
	for f := range strings.FieldsSeq(s) {
		if f != "·" {
			n++
		}
	}
	return n
}

func TestHookPrompt_theLineOfARegisteredSession(t *testing.T) {
	ts := newServer(t)
	a := turnAgent(t, ts, hookTurnFile)
	ts.clock.advance(5 * time.Minute)
	code, out := a.prompt("do the thing")
	want := "grelot: @toolbelt-dev · 0 unread · desc set 5 min ago · grelot help\n"
	if code != 0 || out != want {
		t.Errorf("exit %d, stdout %q; want 0 and %q", code, out, want)
	}
	t.Run("it opens the turn as its own identity", func(t *testing.T) {
		got := ts.last(t, http.MethodGet, "/v1/me")
		if !strings.Contains(got.URL, "turn=start") || strings.Contains(got.URL, "signal=") {
			t.Errorf("URL = %q, want turn=start and no signal", got.URL)
		}
		if got.Header.Get("Grelot-Self") != "1" || got.Header.Get("Grelot-Session") != "alpha:s-1" {
			t.Errorf("headers = %v, want Grelot-Self and the session", got.Header)
		}
	})
	t.Run("it counts unread mail", func(t *testing.T) {
		b, _ := registerAt(t, ts, "s-2", "bee-dev")
		b.postTo("@toolbelt-dev", "a question")
		if _, out := a.prompt("go on"); !strings.HasPrefix(out, "grelot: @toolbelt-dev · 1 unread · ") {
			t.Errorf("stdout %q, want 1 unread", out)
		}
	})
}

func TestHookPrompt_descriptionReminder(t *testing.T) {
	ts := newServer(t)
	a := turnAgent(t, ts, hookTurnFile)
	const reminder = "still true? `grelot set --desc` if not"

	ts.clock.advance(59 * time.Minute)
	if _, out := a.prompt("x"); strings.Contains(out, reminder) {
		t.Errorf("at 59 min: stdout %q, want no reminder", out)
	}
	ts.clock.advance(2 * time.Minute)
	if _, out := a.prompt("x"); out != "grelot: @toolbelt-dev · 0 unread · desc set 1 h ago · "+reminder+" · grelot help\n" {
		t.Errorf("at 61 min: stdout %q, want the reminder", out)
	}
	a.must("renew", "--ttl", "8h")
	ts.clock.advance(59 * time.Minute)
	if _, out := a.prompt("x"); strings.Contains(out, reminder) {
		t.Errorf("59 min after the reminder: stdout %q, want none", out)
	}
	ts.clock.advance(2 * time.Minute)
	if _, out := a.prompt("x"); !strings.Contains(out, reminder) {
		t.Errorf("61 min after the reminder: stdout %q, want it again", out)
	}
	t.Run("a new description resets the age", func(t *testing.T) {
		a.must("set", "--desc", "reviewing the client")
		ts.clock.advance(2 * time.Minute)
		if _, out := a.prompt("x"); out != "grelot: @toolbelt-dev · 0 unread · desc set 2 min ago · grelot help\n" {
			t.Errorf("stdout %q, want the age from the set", out)
		}
	})
	t.Run("a post with a new description resets it too", func(t *testing.T) {
		ts.clock.advance(3 * time.Hour)
		a.mustPost("news", "announce", "News", "--name", "no change", "--paths", "no change", "--desc", "posting news")
		if _, out := a.prompt("x"); !strings.Contains(out, "desc set 0 min ago") {
			t.Errorf("stdout %q, want the age from the post", out)
		}
	})
}

func TestHookPrompt_namesTheStartLineWhenTheWakeIsDown(t *testing.T) {
	ts := newServer(t)
	file := hookTurnFile + "doors:\n  - {name: tray, layer: 2, transport: stdout, start: grelot watch --door tray}\n"
	a := turnAgent(t, ts, file)
	_, out := a.prompt("x")
	want := "grelot: @toolbelt-dev · 0 unread · desc set 0 min ago · wake: down, start: grelot watch --door tray · grelot help\n"
	if out != want {
		t.Errorf("stdout %q, want %q", out, want)
	}
}

func TestHookPrompt_unregisteredAndDormant(t *testing.T) {
	ts := newServer(t)
	t.Run("unregistered", func(t *testing.T) {
		a := hooked(t, newAgent(t, ts, "s-9", ""), "alpha", hookTurnFile)
		in := `{"session_id":"s-9","prompt":"x"}`
		if code, out, _ := a.hook("prompt", in); code != 0 || out != registerLine+"\n" {
			t.Errorf("exit %d, stdout %q; want 0 and %q", code, out, registerLine)
		}
	})
	t.Run("dormant", func(t *testing.T) {
		a := turnAgent(t, ts, hookTurnFile)
		ts.clock.advance(3 * time.Hour)
		want := "grelot: not registered: your identity @toolbelt-dev is dormant. Run `grelot revive --ttl <d>` to go on as it, or `grelot help`, then `grelot register` as it says.\n"
		if _, out := a.prompt("x"); out != want {
			t.Errorf("stdout %q, want %q", out, want)
		}
	})
}

// The turn-start line stays under 30 words in every state.
func TestHookPrompt_underThirtyWords(t *testing.T) {
	ts := newServer(t)
	file := hookTurnFile + "doors:\n  - {name: tray, layer: 2, transport: stdout, start: grelot watch --door tray}\n"
	a := turnAgent(t, ts, file)
	b, _ := registerAt(t, ts, "s-2", "bee-dev")
	for range 12 {
		b.postTo("@toolbelt-dev", "x")
	}
	ts.clock.advance(90 * time.Minute)
	lines := map[string]string{}
	_, lines["live, with the reminder and the start line"] = a.prompt("x")
	ts.clock.advance(time.Hour)
	_, lines["dormant"] = a.prompt("x")
	u := hooked(t, newAgent(t, ts, "s-9", ""), "alpha", hookTurnFile)
	_, lines["unregistered"], _ = u.hook("prompt", `{"session_id":"s-9"}`)
	down := hooked(t, newAgent(t, &testServer{URL: closedURL(t)}, "s-1", ""), "alpha", hookTurnFile)
	_, lines["the server unreachable"], _ = down.hook("prompt", `{"session_id":"s-1"}`)
	old := hooked(t, newAgent(t, ts, "s-9", ""), "alpha", hookTurnFile)
	old.env.Version = "v0.0.1"
	_, lines["a client too old"], _ = old.hook("prompt", `{"session_id":"s-9"}`)
	for state, line := range lines {
		t.Run(strings.ReplaceAll(state, " ", "_"), func(t *testing.T) {
			if line == "" || strings.Count(line, "\n") != 1 || words(line) >= 30 {
				t.Errorf("line %q: %d words, want one line under 30", line, words(line))
			}
		})
	}
	if !strings.Contains(lines["live, with the reminder and the start line"], "still true?") {
		t.Errorf("live line %q lacks the reminder it was meant to measure", lines["live, with the reminder and the start line"])
	}
}

func closedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + l.Addr().String()
	_ = l.Close()
	return u
}

func TestHookPrompt_sendsTheSignalItCarries(t *testing.T) {
	ts := newServer(t)
	a := turnAgent(t, ts, hookTurnFile)
	b, _ := registerAt(t, ts, "s-2", "bee-dev")
	b.postTo("@toolbelt-dev", "a question")
	seq := firstUnread(t, ts, "alpha:s-1")
	line := signal(int(seq), 1)

	t.Run("an unread signal runs the turn", func(t *testing.T) {
		if code, out := a.prompt(line); code != 0 || !strings.HasPrefix(out, "grelot: @toolbelt-dev · 1 unread") {
			t.Errorf("exit %d, stdout %q; want the turn's line", code, out)
		}
		if got := ts.last(t, http.MethodGet, "/v1/me"); !strings.Contains(got.URL, "turn=start") || !strings.Contains(got.URL, "signal="+strconv.FormatUint(seq, 10)) {
			t.Errorf("URL = %q, want turn=start and signal=%d", got.URL, seq)
		}
	})
	near := map[string]string{
		"text after the line":  line + " please",
		"another count format": strings.Replace(line, ": 1 new", ": 01 new", 1),
		"a typed lookalike":    "[grelot:" + strconv.FormatUint(seq, 10) + "] run grelot inbox",
	}
	for name, p := range near {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			if _, out := a.prompt(p); !strings.HasPrefix(out, "grelot: @toolbelt-dev · 1 unread") {
				t.Errorf("stdout %q, want the turn's line", out)
			}
			if got := ts.last(t, http.MethodGet, "/v1/me"); strings.Contains(got.URL, "signal=") {
				t.Errorf("URL = %q, want no signal for a prompt that is not one", got.URL)
			}
		})
	}
}

// meServer answers every request with a live identity's GET /v1/me body:
// unread its counts, signalRead whether read has reached the signal sent.
func meServer(t *testing.T, unread string, signalRead bool) *testServer {
	t.Helper()
	body := fmt.Sprintf(`{"state":"live","identity":{"name":"toolbelt-dev","registered_at":"2026-11-05T09:00:00Z"},"unread":%s,"version":"v1.1.0","path_root":"/workspace","signal_read":%t}`, unread, signalRead)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(hs.Close)
	return &testServer{URL: hs.URL}
}

// The stale-signal block follows the server's answer to the signal, never
// the unread counts: a signal is blocked once read reaches its seq.
func TestHookPrompt_staleSignalBlock(t *testing.T) {
	line := signal(5, 1)
	stale := "signal 5 is stale"
	t.Run("read reached the signal while newer wake mail is unread", func(t *testing.T) {
		a := hooked(t, newAgent(t, meServer(t, `{"wake":1,"direct":1,"total":1}`, true), "s-1", ""), "alpha", hookTurnFile)
		if code, out := a.prompt(line); code != 0 || !strings.Contains(blockReason(t, out), stale) {
			t.Errorf("exit %d, stdout %q; want the block", code, out)
		}
	})
	t.Run("read below the signal with only quiet mail past it", func(t *testing.T) {
		a := hooked(t, newAgent(t, meServer(t, `{"cards":2,"total":2}`, false), "s-1", ""), "alpha", hookTurnFile)
		if code, out := a.prompt(line); code != 0 || !strings.HasPrefix(out, "grelot: @toolbelt-dev · 2 unread") {
			t.Errorf("exit %d, stdout %q; want the turn's line", code, out)
		}
	})
	t.Run("never for a prompt that is no signal", func(t *testing.T) {
		a := hooked(t, newAgent(t, meServer(t, `{"total":0}`, true), "s-1", ""), "alpha", hookTurnFile)
		if _, out := a.prompt(line + " please"); !strings.HasPrefix(out, "grelot: @toolbelt-dev · 0 unread") {
			t.Errorf("stdout %q, want the turn's line", out)
		}
	})
	t.Run("never where block is not spelled", func(t *testing.T) {
		noBlock := strings.Replace(hookTurnFile, "      block: {text: '{\"decision\":\"block\",\"reason\":{{json .Reason}}}'}\n  stop:", "  stop:", 1)
		a := hooked(t, newAgent(t, meServer(t, `{"total":0}`, true), "s-1", ""), "alpha", noBlock)
		if _, out := a.prompt(line); !strings.HasPrefix(out, "grelot: @toolbelt-dev · 0 unread") {
			t.Errorf("stdout %q, want the turn's line", out)
		}
	})
}

// End to end against the server: the signal is blocked from the moment
// read reaches its seq, and runs its turn while that seq is unread.
func TestHookPrompt_staleSignalBlockAgainstTheServer(t *testing.T) {
	ts := newServer(t)
	a := turnAgent(t, ts, hookTurnFile)
	b, _ := registerAt(t, ts, "s-2", "bee-dev")
	a.must("inbox")
	for i := range 3 {
		b.postTo("@toolbelt-dev", fmt.Sprintf("question %d", i+1))
	}
	var page client.Inbox
	ts.call(t, "alpha:s-1", true, &client.Request{Method: http.MethodGet, Path: "/v1/inbox"}, &page)
	if len(page.Entries) != 3 {
		t.Fatalf("inbox holds %d entries, want the 3 questions", len(page.Entries))
	}
	sig := page.Entries[1].Seq
	line := signal(int(sig), 1)
	stale := fmt.Sprintf("signal %d is stale", sig)
	ackUpto := func(seq uint64) {
		var left client.Acked
		ts.call(t, "alpha:s-1", true, &client.Request{Method: http.MethodPost, Path: "/v1/inbox/ack", Body: map[string]uint64{"upto": seq}}, &left)
	}

	ackUpto(page.Entries[0].Seq)
	t.Run("read one below the signal runs the turn", func(t *testing.T) {
		if code, out := a.prompt(line); code != 0 || !strings.HasPrefix(out, "grelot: @toolbelt-dev · 2 unread") {
			t.Errorf("exit %d, stdout %q; want the turn's line", code, out)
		}
	})
	ackUpto(sig)
	t.Run("read at the signal with newer mail unread blocks it", func(t *testing.T) {
		if code, out := a.prompt(line); code != 0 || !strings.Contains(blockReason(t, out), stale) {
			t.Errorf("exit %d, stdout %q; want the block naming %q", code, out, stale)
		}
	})
}

func TestHookPrompt_signalPattern(t *testing.T) {
	ts := newServer(t)
	file := strings.Replace(hookTurnFile, "    event: Prompt\n",
		"    event: Prompt\n    signal: '(?s)^<note>\\n<event>(?P<line>[^<\\n]*)</event>\\n</note>$'\n", 1)
	a := turnAgent(t, ts, file)
	b, _ := registerAt(t, ts, "s-2", "bee-dev")
	b.postTo("@toolbelt-dev", "a question")
	seq := firstUnread(t, ts, "alpha:s-1")
	line := signal(int(seq), 1)

	a.prompt("<note>\n<event>" + line + "</event>\n</note>")
	if got := ts.last(t, http.MethodGet, "/v1/me"); !strings.Contains(got.URL, "signal="+strconv.FormatUint(seq, 10)) {
		t.Errorf("a match: URL = %q, want signal=%d", got.URL, seq)
	}
	cases := map[string]string{
		"a near-miss wrapper":           "<note>\n<event>" + line + "</event>\n</note>\nmore",
		"the bare line":                 line,
		"captured text not a signal":    "<note>\n<event>[grelot:" + strconv.FormatUint(seq, 10) + "] hello</event>\n</note>",
		"a captured end line, not mail": "<note>\n<event>[grelot:end] grelot (automated, not the user): this watcher stopped (none); run grelot me</event>\n</note>",
	}
	for name, p := range cases {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			a.prompt(p)
			if got := ts.last(t, http.MethodGet, "/v1/me"); strings.Contains(got.URL, "signal=") {
				t.Errorf("URL = %q, want no signal", got.URL)
			}
		})
	}
}

func TestHookPrompt_childrenAndForks(t *testing.T) {
	ts := newServer(t)
	a := turnAgent(t, ts, hookTurnFile)
	a.record("tangent", `{"parent":"s-1","reason":"tangent"}`)
	a.record("r-1", `{"parent":"s-1","reason":"rewind"}`)
	a.record("step", `{}`)

	t.Run("a subagent and a step print nothing and ask nothing", func(t *testing.T) {
		for _, in := range []string{`{"session_id":"s-1","agent_id":"sub"}`, `{"session_id":"step"}`} {
			before := len(ts.requests())
			if code, out, _ := a.hook("prompt", in); code != 0 || out != "" {
				t.Errorf("%s: exit %d, stdout %q; want nothing", in, code, out)
			}
			if n := len(ts.requests()) - before; n != 0 {
				t.Errorf("%s: %d requests, want none", in, n)
			}
		}
	})
	t.Run("a fork's line once, from its first prompt", func(t *testing.T) {
		if _, out, _ := a.hook("prompt", `{"session_id":"tangent"}`); out != "grelot: this chat is a fork of another chat; it shares that chat's identity and posts only as a delegate the parent grants\n" {
			t.Errorf("first: stdout %q, want the fork line", out)
		}
		if _, out, _ := a.hook("prompt", `{"session_id":"tangent"}`); out != "" {
			t.Errorf("second: stdout %q, want nothing", out)
		}
	})
	t.Run("a rewind gets the same-agent note once, then the line", func(t *testing.T) {
		_, out, _ := a.hook("prompt", `{"session_id":"r-1"}`)
		if out != "grelot: this chat is a rewind\ngrelot: @toolbelt-dev · 0 unread · desc set 0 min ago · grelot help\n" {
			t.Errorf("first: stdout %q, want the note, then the line", out)
		}
		if _, out, _ := a.hook("prompt", `{"session_id":"r-1"}`); strings.Contains(out, "rewind") {
			t.Errorf("second: stdout %q, want no note", out)
		}
	})
	t.Run("the session the rewind replaced prints nothing", func(t *testing.T) {
		if code, out := a.prompt("x"); code != 0 || out != "" {
			t.Errorf("exit %d, stdout %q; want nothing", code, out)
		}
	})
}

func TestHookPrompt_once(t *testing.T) {
	ts := newServer(t)
	a := turnAgent(t, ts, strings.Replace(hookTurnFile, "    event: Prompt\n", "    event: Prompt\n    once: true\n", 1))
	if _, out := a.prompt("x"); out == "" {
		t.Error("first: printed nothing, want the line")
	}
	if _, out := a.prompt("x"); out != "" {
		t.Errorf("second: stdout %q, want nothing", out)
	}
}

// pointAt rewrites a's client config to send to url.
func (a *agent) pointAt(url string) {
	a.t.Helper()
	writeClientConfig(a.t, a.env.ConfigDir, fmt.Sprintf("server: %s\ntoken: test-token\nhost: box\n", url))
}

// A role under once is suppressed only after its first successful print,
// so an outage does not spend it.
func TestHook_onceSpendsOnlyAnAnswer(t *testing.T) {
	const noCheck = "could not check this session's identity (the server cannot be reached)"
	onceFile := strings.NewReplacer(
		"    event: Start\n", "    event: Start\n    once: true\n",
		"    event: Prompt\n", "    event: Prompt\n    once: true\n",
		"    event: Stop\n", "    event: Stop\n    once: true\n",
	).Replace(hookTurnFile)
	for _, role := range []string{"session_start", "prompt", "stop"} {
		t.Run(role, func(t *testing.T) {
			ts := newServer(t)
			a := hooked(t, newAgent(t, ts, "s-1", ""), "alpha", onceFile)
			stdin := `{"session_id":"s-1","prompt":"x"}`
			a.pointAt(closedURL(t))
			if _, out, _ := a.hook(role, stdin); !strings.Contains(out, noCheck) {
				t.Fatalf("down: stdout %q, want the could-not-check line", out)
			}
			a.pointAt(ts.URL)
			if _, out, _ := a.hook(role, stdin); !strings.Contains(out, "grelot: not registered.") {
				t.Errorf("after the outage: stdout %q, want the register line", out)
			}
			if _, out, _ := a.hook(role, stdin); out != "" {
				t.Errorf("after the answer: stdout %q, want nothing", out)
			}
		})
	}
}

// A prompt the server did not answer prints neither the hourly reminder
// nor the same-agent note, so the next answered prompt still carries
// them. The reminder still fires at most once an hour.
func TestHookPrompt_aFailedCheckSpendsNoMark(t *testing.T) {
	const reminder = "still true? `grelot set --desc` if not"
	ts := newServer(t)
	a := turnAgent(t, ts, hookTurnFile)
	a.record("r-1", `{"parent":"s-1","reason":"rewind"}`)
	ts.clock.advance(61 * time.Minute)
	t.Run("the reminder", func(t *testing.T) {
		a.pointAt(closedURL(t))
		if _, out := a.prompt("x"); strings.Contains(out, reminder) || !strings.Contains(out, "could not check") {
			t.Fatalf("down: stdout %q, want only the could-not-check line", out)
		}
		a.pointAt(ts.URL)
		if _, out := a.prompt("x"); !strings.Contains(out, reminder) {
			t.Errorf("up: stdout %q, want the reminder", out)
		}
	})
	t.Run("the note", func(t *testing.T) {
		a.pointAt(closedURL(t))
		if _, out, _ := a.hook("prompt", `{"session_id":"r-1"}`); strings.Contains(out, "rewind") || !strings.Contains(out, "could not check") {
			t.Fatalf("down: stdout %q, want only the could-not-check line", out)
		}
		a.pointAt(ts.URL)
		if _, out, _ := a.hook("prompt", `{"session_id":"r-1"}`); !strings.HasPrefix(out, "grelot: this chat is a rewind\n") {
			t.Errorf("up: stdout %q, want the note first", out)
		}
	})
}

// A stale-signal block prints no reminder, so it spends none.
func TestHookPrompt_aStaleBlockSpendsNoReminder(t *testing.T) {
	ts := meServer(t, `{"total":0,"wake":0}`, true)
	a := hooked(t, newAgent(t, ts, "s-1", ""), "alpha", hookTurnFile)
	a.env.Now = func() time.Time { return time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC) }
	if _, out := a.prompt(signal(5, 1)); blockReason(t, out) == "" {
		t.Fatalf("stdout %q, want the stale block", out)
	}
	if _, out := a.prompt("x"); !strings.Contains(out, "still true? `grelot set --desc` if not") {
		t.Errorf("stdout %q, want the reminder the block did not print", out)
	}
}

func TestHookPrompt_failsOpenAndThe426(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		a := hooked(t, newAgent(t, &testServer{URL: closedURL(t)}, "s-1", ""), "alpha", hookTurnFile)
		want := "grelot: could not check this session's identity (the server cannot be reached); nothing is held\n"
		if code, out := a.prompt("x"); code != 0 || out != want {
			t.Errorf("exit %d, stdout %q; want 0 and %q", code, out, want)
		}
	})
	t.Run("an old client", func(t *testing.T) {
		ts := newServer(t)
		a := hooked(t, newAgent(t, ts, "s-1", ""), "alpha", hookTurnFile)
		a.env.Version = "v0.0.1"
		if _, out := a.prompt("x"); !strings.HasPrefix(out, "grelot: this grelot server (v1.1.0-dev.4) does not speak client v0.0.1") {
			t.Errorf("stdout %q, want the 426's line as context", out)
		}
	})
}

// stopHook runs the stop hook of session s-1.
func (a *agent) stopHook() (code int, stdout string) {
	code, stdout, _ = a.hook("stop", `{"session_id":"s-1"}`)
	return code, stdout
}

func holdBody(t *testing.T, ts *testServer) map[string]any {
	t.Helper()
	var b map[string]any
	if err := json.Unmarshal([]byte(ts.last(t, http.MethodPost, "/v1/events/hold").Body), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func lifecycleEvents(ts *testServer) []string {
	var out []string
	for _, r := range ts.requests() {
		if r.Method == http.MethodPost && r.URL == "/v1/lifecycle" {
			out = append(out, r.Body)
		}
	}
	return out
}

func TestHookStop_holdsOnceForUnreadMail(t *testing.T) {
	ts := newServer(t)
	a := turnAgent(t, ts, hookTurnFile)
	a.prompt("x")
	if code, out := a.stopHook(); code != 0 || out != "" {
		t.Errorf("no mail: exit %d, stdout %q; want nothing", code, out)
	}
	if got := holdBody(t, ts); got["for"] != "stop" || got["last"] != false {
		t.Errorf("hold body %v, want for stop and last false", got)
	}

	b, _ := registerAt(t, ts, "s-2", "bee-dev")
	b.postTo("@toolbelt-dev", "a question")
	ts.clock.advance(time.Second)
	a.prompt("x")
	_, out := a.stopHook()
	if got := blockReason(t, out); got != "grelot (automated, not the user): unread 1 (1 direct; 1 wake); run grelot inbox before you stop" {
		t.Errorf("mail: stdout %q, want the block naming the count and grelot inbox", out)
	}
	if strings.Contains(out, "a question") {
		t.Errorf("stdout %q carries the message body", out)
	}
	t.Run("a second stop in the turn holds nothing and ends the turn", func(t *testing.T) {
		holds := len(lifecycleEvents(ts))
		b.postTo("@toolbelt-dev", "another")
		if _, out := a.stopHook(); out != "" {
			t.Errorf("stdout %q, want nothing", out)
		}
		ev := lifecycleEvents(ts)
		if len(ev) != holds+1 || !strings.Contains(ev[len(ev)-1], `"turn_end"`) {
			t.Errorf("lifecycle reports %v, want one turn_end", ev)
		}
	})
	t.Run("the next turn holds again", func(t *testing.T) {
		ts.clock.advance(time.Second)
		a.prompt("x")
		if _, out := a.stopHook(); blockReason(t, out) == "" {
			t.Errorf("stdout %q, want the block for the new mail", out)
		}
	})
}

func TestHookStop_holdsAnUnregisteredSessionOnce(t *testing.T) {
	ts := newServer(t)
	a := hooked(t, newAgent(t, ts, "s-1", ""), "alpha", hookTurnFile)
	a.prompt("x")
	if _, out := a.stopHook(); blockReason(t, out) != registerLine {
		t.Errorf("first: stdout %q, want the block with %q", out, registerLine)
	}
	if _, out := a.stopHook(); out != "" {
		t.Errorf("second: stdout %q, want nothing", out)
	}
	t.Run("a dormant identity is held with the revive line", func(t *testing.T) {
		d := turnAgent(t, ts, hookTurnFile)
		ts.clock.advance(3 * time.Hour)
		d.prompt("x")
		if _, out := d.stopHook(); !strings.Contains(blockReason(t, out), "grelot revive --ttl <d>") {
			t.Errorf("stdout %q, want the revive line", out)
		}
	})
}

func TestHookStop_last(t *testing.T) {
	ts := newServer(t)
	t.Run("once_per_turn sends last", func(t *testing.T) {
		a := turnAgent(t, ts, strings.Replace(hookTurnFile, "    event: Stop\n", "    event: Stop\n    once_per_turn: true\n", 1))
		a.stopHook()
		if got := holdBody(t, ts); got["last"] != true {
			t.Errorf("hold body %v, want last true", got)
		}
	})
	t.Run("a stop that cannot block sends last", func(t *testing.T) {
		a := turnAgent(t, ts, strings.Replace(hookTurnFile, "      block: {text: '{\"decision\":\"block\",\"reason\":{{json .Reason}}}'}\n      notice:", "      notice:", 1))
		a.stopHook()
		if got := holdBody(t, ts); got["last"] != true {
			t.Errorf("hold body %v, want last true", got)
		}
	})
}

func TestHookStop_aDownWakeAloneHoldsNothing(t *testing.T) {
	ts := newServer(t)
	a := turnAgent(t, ts, hookTurnFile)
	ts.clock.run()
	ts.call(t, "alpha:s-1", true, &client.Request{Method: http.MethodPost, Path: "/v1/events/wait", Body: map[string]string{"wait": "10ms"}}, nil)
	ts.clock.advance(5 * time.Minute)
	a.prompt("x")
	if _, out := a.stopHook(); out != "" {
		t.Errorf("stdout %q, want nothing: the prompt line names the start line", out)
	}
	if !strings.Contains(ts.lastAnswer(t), `"down":true`) {
		t.Errorf("hold answer %s, want the server's down notice this test is about", ts.lastAnswer(t))
	}
}

func TestHookStop_noticesAndChildren(t *testing.T) {
	t.Run("the 426 as notice", func(t *testing.T) {
		ts := newServer(t)
		a := hooked(t, newAgent(t, ts, "s-1", ""), "alpha", hookTurnFile)
		a.env.Version = "v0.0.1"
		_, out := a.stopHook()
		if !strings.HasPrefix(out, `{"systemMessage":"grelot: this grelot server (v1.1.0-dev.4) does not speak client v0.0.1`) {
			t.Errorf("stdout %q, want the 426's line as the notice", out)
		}
		t.Run("nothing where notice is not spelled", func(t *testing.T) {
			hooked(t, a, "alpha", strings.Replace(hookTurnFile, "      notice: {text: '{\"systemMessage\":{{json .Reason}}}'}\n", "", 1))
			if _, out := a.stopHook(); out != "" {
				t.Errorf("stdout %q, want nothing", out)
			}
		})
	})
	t.Run("fails open", func(t *testing.T) {
		failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal server error"}`))
		}))
		t.Cleanup(failing.Close)
		a := hooked(t, newAgent(t, &testServer{URL: failing.URL}, "s-1", ""), "alpha", hookTurnFile)
		want := `{"systemMessage":"grelot: could not check this session's identity (the server answered 500: internal server error); nothing is held"}` + "\n"
		if code, out := a.stopHook(); code != 0 || out != want {
			t.Errorf("exit %d, stdout %q; want 0 and %q", code, out, want)
		}
	})
	t.Run("a child holds nothing and asks nothing", func(t *testing.T) {
		ts := newServer(t)
		a := hooked(t, newAgent(t, ts, "s-1", ""), "alpha", hookTurnFile)
		a.record("step", `{}`)
		for _, in := range []string{`{"session_id":"s-1","agent_id":"sub"}`, `{"session_id":"step"}`} {
			before := len(ts.requests())
			if _, out, _ := a.hook("stop", in); out != "" {
				t.Errorf("%s: stdout %q, want nothing", in, out)
			}
			if n := len(ts.requests()) - before; n != 0 {
				t.Errorf("%s: %d requests, want none", in, n)
			}
		}
	})
}

// A fault the hook cannot get past, a client config it cannot read or a 2xx
// answer that is not JSON, fails open like an outage: one line naming the
// fault, and nothing held.
func TestHook_failsOpenOnAFaultItCannotRead(t *testing.T) {
	garbled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "<html>sign in to the proxy</html>")
	}))
	t.Cleanup(garbled.Close)
	roles := []struct {
		role, route string
		notice      bool
	}{
		{role: "session_start", route: "GET /v1/me"},
		{role: "prompt", route: "GET /v1/me"},
		{role: "stop", route: "POST /v1/events/hold", notice: true},
	}
	for _, r := range roles {
		t.Run(r.role, func(t *testing.T) {
			check := func(t *testing.T, a *agent, names string) {
				t.Helper()
				code, out, _ := a.hook(r.role, `{"session_id":"s-1","prompt":"x"}`)
				line := strings.TrimSuffix(out, "\n")
				if r.notice {
					var n struct {
						SystemMessage string `json:"systemMessage"`
					}
					if err := json.Unmarshal([]byte(line), &n); err != nil {
						t.Fatalf("stdout %q, want the line as the notice", out)
					}
					line = n.SystemMessage
				}
				if code != 0 || strings.Count(out, "\n") != 1 || !strings.HasPrefix(line, "grelot: could not check this session's identity (") ||
					!strings.HasSuffix(line, "); nothing is held") || !strings.Contains(line, names) || strings.Contains(line, "cannot be reached") {
					t.Errorf("exit %d, stdout %q; want 0 and one could-not-check line naming %q", code, out, names)
				}
			}
			t.Run("no client config", func(t *testing.T) {
				a := hooked(t, newAgent(t, &testServer{URL: garbled.URL}, "s-1", ""), "alpha", hookTurnFile)
				file := client.ConfigFile(a.env.ConfigDir)
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				check(t, a, file)
			})
			t.Run("a 2xx that is not JSON", func(t *testing.T) {
				a := hooked(t, newAgent(t, &testServer{URL: garbled.URL}, "s-1", ""), "alpha", hookTurnFile)
				check(t, a, r.route+" is not the JSON this client reads")
			})
		})
	}
}

func TestHookLifecycle(t *testing.T) {
	ts := newServer(t)
	a := turnAgent(t, ts, hookTurnFile)
	a.record("r-1", `{"parent":"s-9","reason":"rewind"}`)
	a.record("step", `{}`)

	report := func(t *testing.T, role, in string) (string, int) {
		t.Helper()
		before := len(lifecycleEvents(ts))
		code, out, errOut := a.hook(role, in)
		if code != 0 || out != "" || errOut != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want 0 and nothing", role, code, out, errOut)
		}
		ev := lifecycleEvents(ts)[before:]
		if len(ev) == 0 {
			return "", 0
		}
		return ev[len(ev)-1], len(ev)
	}
	t.Run("turn_end reports and prints nothing", func(t *testing.T) {
		if got, n := report(t, "turn_end", `{"session_id":"s-1"}`); n != 1 || got != `{"event":"turn_end"}` {
			t.Errorf("reports %d, last %q; want one turn_end", n, got)
		}
		if got := ts.last(t, http.MethodPost, "/v1/lifecycle"); got.Header.Get("Grelot-Self") != "1" {
			t.Errorf("headers = %v, want Grelot-Self", got.Header)
		}
	})
	quiet := map[string]string{
		"turn_end from a subagent":          `{"session_id":"s-1","agent_id":"sub"}`,
		"session_end from a subagent":       `{"session_id":"s-1","agent_id":"sub","reason":"delete"}`,
		"session_end from a step":           `{"session_id":"step","reason":"delete"}`,
		"session_end from a rewind":         `{"session_id":"r-1","reason":"delete"}`,
		"session_end its when filter skips": `{"session_id":"s-1","reason":"shutdown"}`,
	}
	for name, in := range quiet {
		role := strings.Fields(name)[0]
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			if _, n := report(t, role, in); n != 0 {
				t.Errorf("%d reports, want none", n)
			}
		})
	}
	t.Run("session_end of the identity's own session ends it", func(t *testing.T) {
		if got, n := report(t, "session_end", `{"session_id":"s-1","reason":"delete"}`); n != 1 || got != `{"event":"session_end"}` {
			t.Errorf("reports %d, last %q; want one session_end", n, got)
		}
		var me client.Me
		ts.call(t, "alpha:s-1", true, &client.Request{Method: http.MethodGet, Path: "/v1/me"}, &me)
		if me.Online || me.Identity == nil || !me.Identity.Expires.Before(ts.clock.Now().Add(time.Hour+time.Second)) {
			t.Errorf("after session_end: %+v, want offline and the lease capped at an hour", me)
		}
	})
}

func TestHookSessionEnd_givesUpAtItsBudget(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	file := strings.Replace(hookTurnFile, "session_end: {event: End,", "session_end: {event: End, budget: 200ms,", 1)
	a := hooked(t, newAgent(t, &testServer{URL: slow.URL}, "s-1", ""), "alpha", file)
	start := time.Now()
	code, out, _ := a.hook("session_end", `{"session_id":"s-1","reason":"delete"}`)
	if d := time.Since(start); code != 0 || out != "" || d > 2*time.Second {
		t.Errorf("exit %d, stdout %q after %s; want 0 and nothing within the 200ms budget", code, out, d)
	}
}

func TestHookSubagentStop_endsTheDelegateOfItsOwnSession(t *testing.T) {
	ts := newServer(t)
	a := turnAgent(t, ts, hookTurnFile)
	var grant struct {
		Link string `json:"link"`
	}
	ts.call(t, "alpha:s-1", false, &client.Request{
		Method: http.MethodPost, Path: "/v1/delegates", Body: map[string]any{"label": "review", "ttl": "1h"},
	}, &grant)
	sub := newAgent(t, ts, "sub-1", "")
	sub.must("register", "--delegate", grant.Link, "--desc", "reviewing the diff")

	t.Run("a subagent sharing the session sends nothing", func(t *testing.T) {
		before := len(lifecycleEvents(ts))
		a.hook("subagent_stop", `{"session_id":"s-1","agent_id":"x"}`)
		if n := len(lifecycleEvents(ts)) - before; n != 0 {
			t.Errorf("%d reports, want none", n)
		}
	})
	if out := sub.must("me"); !strings.Contains(out, "@toolbelt-dev/review") {
		t.Fatalf("the delegate's me before the stop: %q", out)
	}
	code, out, _ := a.hook("subagent_stop", `{"session_id":"sub-1","agent_id":"x"}`)
	if code != 0 || out != "" {
		t.Errorf("exit %d, stdout %q; want 0 and nothing", code, out)
	}
	got := ts.last(t, http.MethodPost, "/v1/lifecycle")
	if got.Header.Get("Grelot-Session") != "alpha:sub-1" || got.Header.Get("Grelot-Self") != "" || !strings.Contains(got.Body, `"subagent_stop"`) {
		t.Errorf("request %+v, want subagent_stop from the subagent's own session", got)
	}
	if out := sub.must("me"); !strings.Contains(out, "this session has no identity") {
		t.Errorf("the delegate's me after the stop: %q, want no identity", out)
	}
}
