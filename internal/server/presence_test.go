//go:build !windows

package server

import (
	"net/http"
	"testing"
	"time"
)

func TestPresence_onlineWhileAWaitIsOpenAndFor90sAfter(t *testing.T) {
	p := newPresence()
	const id = 7
	if p.online(id, testNow) {
		t.Error("online before any wait")
	}
	p.opened(id)
	if !p.online(id, testNow.Add(10*time.Minute)) {
		t.Error("offline while a wait is open")
	}
	p.opened(id)
	p.closed(id, testNow)
	if !p.online(id, testNow.Add(time.Hour)) {
		t.Error("offline while a second wait is still open")
	}
	p.closed(id, testNow)
	if !p.online(id, testNow.Add(89*time.Second)) {
		t.Error("offline 89 s after the last wait closed")
	}
	if p.online(id, testNow.Add(90*time.Second)) {
		t.Error("online 90 s after the last wait closed")
	}
	if p.online(id+1, testNow) {
		t.Error("another identity online")
	}
}

func TestPresence_aTurnIsKeptOnlyOnceTheStopHookWasSeen(t *testing.T) {
	p := newPresence()
	const id = 7
	p.turnSet(id, testNow)
	if _, open := p.turnOpen(id, testNow); open {
		t.Error("a turn is open for an identity whose stop hook was never seen")
	}
	p.sawStop(id)
	p.turnSet(id, testNow)
	if until, open := p.turnOpen(id, testNow.Add(time.Minute)); !open || !until.Equal(testNow.Add(5*time.Minute)) {
		t.Errorf("turnOpen = %v, %v; want open until 5 minutes after the prompt", until, open)
	}
	p.registered(id)
	p.turnSet(id, testNow)
	if _, open := p.turnOpen(id, testNow); open {
		t.Error("a turn is open after a new registration, whose stop hook is not seen yet")
	}
}

func TestPresence_aTurnLapses5MinutesAfterTheLastHookAndClosesAtItsEnd(t *testing.T) {
	p := newPresence()
	const id = 7
	p.sawStop(id)
	p.turnSet(id, testNow)
	p.turnSet(id, testNow.Add(2*time.Minute))
	if _, open := p.turnOpen(id, testNow.Add(6*time.Minute)); !open {
		t.Error("closed 4 minutes after the last hold")
	}
	if _, open := p.turnOpen(id, testNow.Add(7*time.Minute)); open {
		t.Error("open 5 minutes after the last hold")
	}
	p.sawStop(id + 1)
	p.turnSet(id+1, testNow.Add(8*time.Minute))
	p.turnSet(id, testNow.Add(8*time.Minute))
	p.turnClosed(id)
	if _, open := p.turnOpen(id, testNow.Add(8*time.Minute)); open {
		t.Error("open after its turn end was reported")
	}
	if _, open := p.turnOpen(id+1, testNow.Add(8*time.Minute)); !open {
		t.Error("another identity's turn closed with it")
	}
}

func TestPresence_theDownNoticeIsOncePerEpisodeAfterAStandingWait(t *testing.T) {
	p := newPresence()
	const id = 7
	if p.tellDown(id) {
		t.Error("owed to an identity that never held a standing wait")
	}
	p.opened(id)
	p.closed(id, testNow)
	if !p.tellDown(id) {
		t.Fatal("not owed once its standing wait is gone")
	}
	if p.tellDown(id) {
		t.Error("owed twice in one down episode")
	}
	p.opened(id)
	p.closed(id, testNow)
	if !p.tellDown(id) {
		t.Error("not owed in the episode after the next standing wait")
	}
	if p.tellDown(id + 1) {
		t.Error("owed to another identity")
	}
	p.registered(id)
	if p.tellDown(id) {
		t.Error("owed after a new registration that held no standing wait")
	}
}

// markStopAndWait gives id a seen stop hook and a held standing wait.
func markStopAndWait(p *presence, id uint64) {
	p.sawStop(id)
	p.opened(id)
	p.closed(id, testNow)
}

func TestRegistration_startsTheSessionMarksAfresh(t *testing.T) {
	afresh := func(t *testing.T, h *harness, id uint64) {
		t.Helper()
		if m := h.srv.presence.get(id); m.stopSeen || m.held {
			t.Errorf("stop seen %v, standing wait held %v; want neither after the registration", m.stopSeen, m.held)
		}
	}
	t.Run("a register from the identity's own session keeps them", func(t *testing.T) {
		h := newHarness(t)
		h.mustRegister(sessA, "grelot-dev")
		id := h.idOf(sessA)
		markStopAndWait(h.srv.presence, id)
		if r := h.as(sessA, http.MethodPost, "/v1/register", registerBody("grelot-dev", "6h")); r.status != http.StatusOK {
			t.Fatalf("register again: %d %s", r.status, r.raw)
		}
		if m := h.srv.presence.get(id); !m.stopSeen || !m.held {
			t.Errorf("stop seen %v, held %v; want both kept", m.stopSeen, m.held)
		}
	})
	t.Run("a take-over", func(t *testing.T) {
		h := newHarness(t)
		h.mustRegister(sessA, "grelot-dev")
		id := h.idOf(sessA)
		markStopAndWait(h.srv.presence, id)
		body := registerBody("grelot-dev", "6h")
		body["take_over"] = true
		if r := h.as(sessB, http.MethodPost, "/v1/register", body); r.status != http.StatusCreated || r.body["moved"] != true {
			t.Fatalf("take-over: %d %s", r.status, r.raw)
		}
		afresh(t, h, id)
	})
	t.Run("a revival", func(t *testing.T) {
		h := newHarness(t)
		h.mustRegister(sessA, "grelot-dev")
		id := h.idOf(sessA)
		markStopAndWait(h.srv.presence, id)
		h.clk.advance(7 * time.Hour)
		if r := h.as(sessA, http.MethodPost, "/v1/revive", reviveBody("", "2h")); r.status != http.StatusOK {
			t.Fatalf("revive: %d %s", r.status, r.raw)
		}
		afresh(t, h, id)
	})
}

func TestPresence_countsInTurnWaits(t *testing.T) {
	p := newPresence()
	const id = 7
	holds := func(id uint64) bool {
		_, ok := p.inTurnHolds(id, 1, testNow)
		return ok
	}
	p.inTurnOpened(id)
	p.inTurnOpened(id)
	p.inTurnClosed(id, 0, testNow)
	if !holds(id) {
		t.Error("closed while one of two in-turn waits is still open")
	}
	p.inTurnClosed(id, 0, testNow)
	if holds(id) {
		t.Error("open after both closed")
	}
	if holds(id + 1) {
		t.Error("another identity's in-turn wait open")
	}
}

func TestPresence_mailAnInTurnWaitReturnedWaitsForItsAck(t *testing.T) {
	p := newPresence()
	const id = 7
	p.inTurnOpened(id)
	p.inTurnClosed(id, 5, testNow)
	if until, ok := p.inTurnHolds(id, 5, testNow.Add(29*time.Second)); !ok || !until.Equal(testNow.Add(30*time.Second)) {
		t.Errorf("inTurnHolds(seq 5, 29 s on) = %v, %v; want held until 30 s after the return", until, ok)
	}
	if _, ok := p.inTurnHolds(id, 5, testNow.Add(30*time.Second)); ok {
		t.Error("held 30 s after the return")
	}
	if _, ok := p.inTurnHolds(id, 6, testNow); ok {
		t.Error("held mail newer than what the wait returned")
	}
}
