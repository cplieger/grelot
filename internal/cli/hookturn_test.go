package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
