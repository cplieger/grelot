//go:build !windows

package server

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

// stateFields is the integer fields of the state lines logged so far.
func (h *harness) stateFields() []map[string]int64 {
	var out []map[string]int64
	for _, r := range h.logs.lines(slog.LevelInfo, msgState) {
		m := map[string]int64{}
		r.Attrs(func(a slog.Attr) bool {
			m[a.Key] = a.Value.Int64()
			return true
		})
		out = append(out, m)
	}
	return out
}

func wantFields(t *testing.T, label string, got, want map[string]int64) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s = %d, want %d", label, k, got[k], v)
		}
	}
}

func TestLogState_logsAtOnceAndEveryMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		h.mustRegisterAlone(sessA, "reader")
		h.mustRegisterAlone(sessB, "writer")
		for _, sess := range []string{sessA, sessB} {
			if r := h.watch(sess, time.Second); r.status != http.StatusNoContent {
				t.Fatalf("wait from %s: %d %s", sess, r.status, r.raw)
			}
		}
		if r := h.as(sessB, http.MethodPost, "/v1/lifecycle", map[string]any{"event": "session_end"}); r.status >= 300 {
			t.Fatalf("session_end: %d %s", r.status, r.raw)
		}
		h.mustPost(sessA, map[string]any{"agent": "writer"}, "hello")
		time.Sleep(30 * time.Second)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			h.srv.LogState(ctx)
			close(done)
		}()
		synctest.Wait()
		lines := h.stateFields()
		if len(lines) != 1 {
			t.Fatalf("%d state lines at start, want 1", len(lines))
		}
		wantFields(t, "first line", lines[0], map[string]int64{
			"live": 2, "dormant": 0, "online": 1,
			"threads_announce": 1, "threads_path": 0, "threads_direct": 1, "threads_cards": 1,
			"reservations": 0, "queued": 0, "unread": 1, "oldest_unread_ms": 30_000,
			"max_bytes": 512 << 20,
		})
		if l := lines[0]; l["file_bytes"] <= 0 || l["in_use_bytes"] != l["file_bytes"]-l["free_bytes"] {
			t.Errorf("bytes: file %d free %d in use %d, want a file and in use = file - free",
				l["file_bytes"], l["free_bytes"], l["in_use_bytes"])
		}

		time.Sleep(stateEvery - time.Nanosecond)
		synctest.Wait()
		if n := len(h.stateFields()); n != 1 {
			t.Errorf("%d state lines just before a minute, want 1", n)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		lines = h.stateFields()
		if len(lines) != 2 {
			t.Fatalf("%d state lines after a minute, want 2", len(lines))
		}
		wantFields(t, "second line", lines[1], map[string]int64{
			"online": 0, "unread": 1, "oldest_unread_ms": 90_000,
		})

		cancel()
		<-done
		if n := len(h.stateFields()); n != 2 {
			t.Errorf("%d state lines after the stop, want 2", n)
		}
	})
}

func TestLogState_aClosedStoreLogsNoLine(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	h.srv.LogState(ctx)
	if n := len(h.stateFields()); n != 0 {
		t.Errorf("%d state lines from a closed store, want 0", n)
	}
	if n := len(h.logs.lines(slog.LevelWarn, msgStateFailed)); n != 0 {
		t.Errorf("%d %q lines during shutdown, want 0", n, msgStateFailed)
	}
}

func TestLogState_aFailedCountIsOneWarn(t *testing.T) {
	h := newHarness(t)
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	h.srv.logState(t.Context())
	if n := len(h.stateFields()); n != 0 {
		t.Errorf("%d state lines from a failed count, want 0", n)
	}
	if n := len(h.logs.lines(slog.LevelWarn, msgStateFailed)); n != 1 {
		t.Errorf("%d %q lines, want 1", n, msgStateFailed)
	}
}
