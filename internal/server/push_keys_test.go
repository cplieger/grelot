package server

import (
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

func TestPush_theDedupeKeyOfARetryIsUnchangedByNewMailAndNewForAReNudge(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t, pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}},
		pushReply{status: http.StatusAccepted})
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()

		time.Sleep(5 * time.Second)
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "more")
		synctest.Wait()
		if n := len(rcv.requests()); n != 1 {
			t.Fatalf("%d pushes after new mail, want the retry to wait for its Retry-After", n)
		}
		time.Sleep(15 * time.Second)
		synctest.Wait()
		time.Sleep(3 * time.Minute)
		synctest.Wait()

		id := h.idOf(sessA)
		want := []string{pushKey(id, 0, 1), pushKey(id, 0, 1), pushKey(id, 0, 2)}
		if got := rcv.keys(t); !equalStrings(got, want) {
			t.Errorf("keys = %v, want the retry's unchanged by new mail and the re-nudge's new: %v", got, want)
		}
	})
}

func TestPush_theDedupeKeyOfARetryIsUnchangedAcrossARestart(t *testing.T) {
	t.Setenv(doorTokenEnv, doorSecret)
	rcv := newPushReceiver(t, pushReply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "20"}},
		pushReply{status: http.StatusAccepted})
	synctest.Test(t, func(t *testing.T) {
		h := newPushHarness(t, rcv.kiroDoor()...)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		h.mustPost(sessB, map[string]any{"agent": "reader"}, "hello")
		synctest.Wait()

		h.restartAfter(5 * time.Second)
		synctest.Wait()
		key := pushKey(h.idOf(sessA), 0, 1)
		if got := rcv.keys(t); !equalStrings(got, []string{key, key}) {
			t.Errorf("keys = %v, want %s again from the new server", got, key)
		}
	})
}
