//go:build !windows

package cli

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/transport"
)

// newerServer is the version the skew tests' server runs, newer than the
// agents' v1.1.0, and upgrade the line naming it.
const (
	newerServer = "v1.2.0"
	upgrade     = "this client is v1.1.0 and the server v1.2.0: install grelot v1.2.0 with go install github.com/cplieger/grelot@v1.2.0 or from https://github.com/cplieger/grelot/releases/tag/v1.2.0, then check both with grelot version"
)

func refuseAck(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/v1/inbox/ack"
}

// skewPair is watchPair on a newer server that refuses every ack.
func skewPair(t *testing.T) (reader, writer *agent) {
	t.Helper()
	ts := newServerAt(t, newerServer, refuseAck)
	ts.clock.run()
	reader = newAgent(t, ts, "s-reader", "")
	reader.env.Providers = providerSet(t, "alpha", alphaDoors, "beta", betaFile)
	reader.must("register", "reader-dev", "--paths", "/workspace/reader", "--desc", "reading", "--ttl", "2h")
	writer, _ = registerAt(t, ts, "s-writer", "writer-dev")
	return reader, writer
}

func TestRefusal_aNewerServersRefusalNamesTheUpgrade(t *testing.T) {
	ts := newServerAt(t, newerServer, func(r *http.Request) bool { return r.URL.Path == "/v1/posts" })
	writer, _ := registerAt(t, ts, "s-writer", "writer-dev")
	registerAt(t, ts, "s-reader", "reader-dev")
	code, _, stderr := writer.postIn("hello", "@reader-dev", "Question: shape", "--name", "no change", "--paths", "no change", "--desc", "no change")
	if code != exitFailed {
		t.Errorf("exit %d, want %d", code, exitFailed)
	}
	if want := "grelot: the server is newer than this client, which may be why it refused: " + upgrade + "\n"; !strings.HasSuffix(stderr, want) {
		t.Errorf("stderr %q, want it to end with %q", stderr, want)
	}
}

func TestInbox_aRefusedAckNamesTheUpgrade(t *testing.T) {
	reader, writer := skewPair(t)
	writer.postTo("@reader-dev", "hello")
	out := reader.must("inbox")
	if want := "grelot: the server refused the ack, so this page shows again and no watcher wakes for it: " + upgrade + "\n"; !strings.Contains(out, "hello") || !strings.HasSuffix(out, want) {
		t.Errorf("stdout %q, want the page then %q", out, want)
	}
	if out := reader.must("inbox"); !strings.Contains(out, "hello") {
		t.Errorf("second inbox %q, want the unacked page again", out)
	}
}

func TestWatch_stopsOnceTheServerRefusesItsAck(t *testing.T) {
	end := transport.EndLine(endUpgrade)
	t.Run("started after the refusal", func(t *testing.T) {
		reader, writer := skewPair(t)
		writer.postTo("@reader-dev", "hello")
		reader.must("inbox")
		w := reader.start("watch", "--door", "poll")
		if code := w.exit(t); code != 0 {
			t.Errorf("exit %d, want the door's end_exit 0", code)
		}
		if got := w.stdout.String(); got != "end "+end+"\n" {
			t.Errorf("stdout %q, want only the end line", got)
		}
		if got := w.stderr.String(); !strings.Contains(got, "go install github.com/cplieger/grelot@v1.2.0") {
			t.Errorf("stderr %q, want the upgrade named", got)
		}
	})
	t.Run("running at the refusal", func(t *testing.T) {
		defer func(d time.Duration) { maxHold = d }(maxHold)
		maxHold = 100 * time.Millisecond
		reader, writer := skewPair(t)
		w := reader.start("watch")
		writer.postTo("@reader-dev", "first")
		w.waitFor(t, signal(1, 1)+"\n")
		reader.must("inbox")
		if code := w.exit(t); code != 1 {
			t.Errorf("exit %d, want the default end_exit 1", code)
		}
		if got := w.stdout.String(); !strings.HasSuffix(got, "tray s-reader "+end+"\n") {
			t.Errorf("stdout %q, want it to end with the end line", got)
		}
	})
}

func TestHookStop_aClientThatCannotAckHoldsNoTurn(t *testing.T) {
	ts := newServerAt(t, newerServer, refuseAck)
	a := turnAgent(t, ts, hookTurnFile)
	b, _ := registerAt(t, ts, "s-2", "bee-dev")
	b.postTo("@toolbelt-dev", "a question")
	a.prompt("x")
	a.must("inbox")
	ts.clock.advance(time.Second)
	before := len(ts.requests())
	code, out := a.stopHook()
	if code != 0 || blockReason(t, out) != "" {
		t.Errorf("exit %d, stdout %q; want 0 and no hold", code, out)
	}
	if want := `{"systemMessage":"grelot: unread mail stays unread until this client is upgraded: ` + upgrade + `"}` + "\n"; out != want {
		t.Errorf("stdout %q, want the notice %q", out, want)
	}
	for _, r := range ts.requests()[before:] {
		if r.URL == "/v1/events/hold" {
			t.Errorf("the stop hook sent a hold: %s", r.Body)
		}
	}
}

// A one-shot run's output stays its answer alone, so its stop prints no
// upgrade notice; an interactive session's does.
func TestHookStop_aOneShotRunGetsNoCannotAckNotice(t *testing.T) {
	file := strings.Replace(hookTurnFile, "hooks:\n", "one_shot: {env: {ALPHA_MODE: [print]}}\nhooks:\n", 1)
	notice := `{"systemMessage":"grelot: unread mail stays unread until this client is upgraded: ` + upgrade + `"}` + "\n"
	for name, want := range map[string]string{"print": "", "": notice} {
		t.Run("mode "+name, func(t *testing.T) {
			ts := newServerAt(t, newerServer, refuseAck)
			a := hooked(t, newAgent(t, ts, "s-1", ""), "alpha", file)
			if name != "" {
				a.vars["ALPHA_MODE"] = name
			}
			facts := client.LoadFacts(a.env.ConfigDir)
			facts.Version, facts.AckRefused = newerServer, a.env.Version
			client.SaveFacts(a.env.ConfigDir, facts)
			if code, out := a.stopHook(); code != 0 || out != want {
				t.Errorf("exit %d, stdout %q; want 0 and %q", code, out, want)
			}
		})
	}
}
