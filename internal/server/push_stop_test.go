//go:build !windows

package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

func TestPush_aNudgeDueAtRestartIsPushedByTheStartupScan(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t)
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()

		// The re-nudge falls due 3 minutes on, while the server is down.
		h.restartAfter(4 * time.Minute)
		synctest.Wait()
		id := h.idOf(sessA)
		if got, want := rcv.keys(t), []string{pushKey(id, 0, 1), pushKey(id, 0, 2)}; !equalStrings(got, want) {
			t.Errorf("keys = %v, want the re-nudge pushed as the new server starts: %v", got, want)
		}
	})
}

func TestPush_releaseCancelsASendInFlightWithNoErrorLine(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	entered, hung := make(chan struct{}, 1), make(chan struct{})
	cancelled := make(chan struct{}, 1)
	rcv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// net/http notices a closed connection only once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
			cancelled <- struct{}{}
		case <-hung:
		}
	}))
	t.Cleanup(rcv.Close)
	t.Cleanup(func() { close(hung) })

	var h *harness
	noSecretLogged(t, &h)
	h = newHarnessWith(t, store.Options{}, pushFrom("testhost",
		doorFileAt(rcv.URL, "testhost", "door_token", "    match: {provider: kiro-cli}\n")))
	h.mustRegisterAlone(sessA, "reader")
	h.mustRegisterAlone(sessB, "writer")
	h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no push reached the receiver")
	}

	released := make(chan struct{})
	go func() {
		h.srv.Release()
		close(released)
	}()
	select {
	case <-released:
	case <-time.After(30 * time.Second):
		t.Fatal("Release did not return with a send in flight")
	}
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Error("the receiver never saw the send cancelled")
	}
	if got := h.logs.atLeast(slog.LevelWarn); len(got) != 0 {
		t.Errorf("lines at WARN or above = %v, want none for a cancelled send", got)
	}
	if n := h.nudgesVia(string(store.NudgeByPush)); n != 0 {
		t.Errorf("%d nudges via push, want none recorded for a cancelled send", n)
	}
}
