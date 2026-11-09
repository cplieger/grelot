//go:build !windows

package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

// isClosed reports whether ch is closed, without blocking.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestHub_aCommitBetweenTheReadAndTheBlockWakesTheWait(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "alpha")
	h.mustRegister(sessB, "bravo")
	h.mustRegister(sessC, "charlie")
	bravo := h.idOf(sessB)
	h.as(sessB, http.MethodPost, "/v1/inbox/ack", map[string]any{"upto": 1 << 40})

	wake, release := h.srv.hub.take(bravo)
	defer release()
	page, err := h.store.Inbox(t.Context(), store.Caller{Session: sessB}, 10)
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Fatalf("bravo's inbox before the post holds %d entries, want none", len(page.Entries))
	}
	t.Run("a commit for another identity leaves it blocked", func(t *testing.T) {
		h.directPost(sessA, "charlie")
		if isClosed(wake) {
			t.Error("a post to charlie woke bravo's wait")
		}
	})
	h.directPost(sessA, "bravo")
	if !isClosed(wake) {
		t.Fatal("a post to bravo after the read left its wait blocked")
	}
	t.Run("the next take blocks until the next commit", func(t *testing.T) {
		again, rel := h.srv.hub.take(bravo)
		defer rel()
		if isClosed(again) {
			t.Fatal("a fresh take is already closed")
		}
		h.directPost(sessC, "bravo")
		if !isClosed(again) {
			t.Error("a second post left the fresh take blocked")
		}
	})
}

func TestHub_wakesEveryWaitOfTheIdentity(t *testing.T) {
	hub := NewHub()
	first, r1 := hub.take(7)
	defer r1()
	second, r2 := hub.take(7)
	defer r2()
	other, r3 := hub.take(8)
	defer r3()
	hub.Woke([]uint64{7, 9})
	if !isClosed(first) || !isClosed(second) {
		t.Errorf("waits of 7 closed %v, %v; want both", isClosed(first), isClosed(second))
	}
	if isClosed(other) {
		t.Error("the wait of 8 woke")
	}
}

func TestHub_aReleasedTakeIsReleasedOnce(t *testing.T) {
	hub := NewHub()
	_, r1 := hub.take(7)
	held, r2 := hub.take(7)
	defer r2()
	r1()
	r1()
	hub.Woke([]uint64{7})
	if !isClosed(held) {
		t.Error("a second release of one take dropped the other wait's channel")
	}
}

func TestBeat_forgetsTheMemoryOfAnIdentityNoLongerLive(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "alpha")
	h.mustRegister(sessB, "bravo")
	h.mustRegister(sessC, "charlie")
	const sessD = "kiro-cli:session-d"
	h.mustRegister(sessD, "delta")
	alpha, bravo, charlie, delta := h.idOf(sessA), h.idOf(sessB), h.idOf(sessC), h.idOf(sessD)
	for _, id := range []uint64{alpha, bravo, charlie, delta} {
		h.srv.presence.sawStop(id)
	}
	h.srv.presence.opened(charlie, sessC)
	h.srv.presence.inTurnOpened(delta)
	for _, s := range []string{sessB, sessC, sessD} {
		if r := h.as(s, http.MethodDelete, "/v1/me", nil); r.status != http.StatusOK && r.status != http.StatusNoContent {
			t.Fatalf("DELETE /v1/me: %d %s", r.status, r.raw)
		}
	}
	h.srv.Beat(t.Context())
	if !h.srv.presence.get(alpha).stopSeen {
		t.Error("live alpha's marks were forgotten")
	}
	if h.srv.presence.get(bravo).stopSeen {
		t.Error("retired bravo's marks were kept")
	}
	if h.srv.presence.get(charlie).open != 1 {
		t.Error("retired charlie's open standing wait was forgotten while it is held")
	}
	if h.srv.presence.get(delta).inTurn != 1 {
		t.Error("retired delta's open in-turn wait was forgotten while it is held")
	}
}

func TestPostLimit_pruneDropsAKeyOnceItsPostsLeftTheMinute(t *testing.T) {
	p := newPostLimit()
	p.take(1, testNow)
	p.take(2, testNow.Add(30*time.Second))
	p.prune(testNow.Add(time.Minute))
	if _, ok := p.m[1]; ok {
		t.Error("key 1, whose one post is a minute old, was kept")
	}
	if _, ok := p.m[2]; !ok {
		t.Error("key 2, with a post 30 s old, was dropped")
	}
}
