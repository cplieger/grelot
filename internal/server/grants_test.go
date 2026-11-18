//go:build !windows

package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

func TestReservations_reserveQueueRenewAndHandOff(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "holder")
	h.mustRegister(sessB, "waiter")
	path := repoGrelot + "/internal"
	body := map[string]any{"path": path, "reason": "editing", "ttl_ms": durMS("1h")}
	r := h.as(sessA, http.MethodPost, "/v1/reservations", body)
	if r.status != http.StatusCreated {
		t.Fatalf("reserve: %d %s", r.status, r.raw)
	}
	held := r.body["reservation"].(map[string]any)
	if held["state"] != "held" || held["holder"] != "holder" {
		t.Fatalf("reservation = %v, want held by holder", held)
	}
	if r := h.as(sessB, http.MethodPost, "/v1/reservations", body); r.status != http.StatusConflict || !strings.Contains(r.str("error"), "holder") {
		t.Errorf("a conflicting reserve: %d %s, want 409 naming the holder", r.status, r.raw)
	}
	body["queue"] = true
	r = h.as(sessB, http.MethodPost, "/v1/reservations", body)
	if r.status != http.StatusCreated || r.body["reservation"].(map[string]any)["state"] != "queued" {
		t.Fatalf("queue: %d %s", r.status, r.raw)
	}
	if behind, _ := r.body["behind"].(map[string]any); behind["holder"] != "holder" {
		t.Errorf("behind = %v, want the holder", behind)
	}
	list := h.as(sessB, http.MethodGet, "/v1/reservations?path="+repoGrelot, nil)
	if n := len(list.body["items"].([]any)); list.status != http.StatusOK || n != 2 {
		t.Errorf("list: %d with %d entries, want the grant and the queue entry", list.status, n)
	}
	id := fmt.Sprint(held["id"])
	h.clk.advance(10 * time.Minute)
	r = h.as(sessA, http.MethodPatch, "/v1/reservations/"+id, map[string]any{"ttl_ms": durMS("2h")})
	if r.status != http.StatusOK || r.body["reservation"].(map[string]any)["expires_at"] != h.clk.now().Add(2*time.Hour).Format(time.RFC3339) {
		t.Errorf("renew: %d %s, want the expiry 2 h from now", r.status, r.raw)
	}
	if r := h.as(sessA, http.MethodDelete, "/v1/reservations/"+id, nil); r.status != http.StatusOK {
		t.Fatalf("release: %d %s", r.status, r.raw)
	}
	list = h.as(sessB, http.MethodGet, "/v1/reservations?path="+path, nil)
	rs := list.body["items"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["state"] != "held" || rs[0].(map[string]any)["holder"] != "waiter" {
		t.Errorf("after the release = %v, want the waiter holding it", rs)
	}
	for _, path := range []string{"/v1/reservations/x", "/v1/reservations/0"} {
		if r := h.as(sessA, http.MethodDelete, path, nil); r.status != http.StatusBadRequest {
			t.Errorf("DELETE %s: %d, want 400", path, r.status)
		}
	}
	if r := h.as(sessA, http.MethodDelete, "/v1/reservations/"+id, nil); r.status/100 != 4 {
		t.Errorf("a stale id: %d, want a refusal", r.status)
	}
}

func TestDelegates_grantGateAndRevoke(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "parent")
	if r := h.as(sessA, http.MethodPost, "/v1/delegates", map[string]any{"label": "review"}); r.status != http.StatusBadRequest {
		t.Errorf("a grant with no ttl: %d %s, want 400", r.status, r.raw)
	}
	g := h.as(sessA, http.MethodPost, "/v1/delegates", map[string]any{"label": "review", "ttl_ms": durMS("1h")})
	if g.status != http.StatusCreated || !strings.HasPrefix(g.str("link"), "grelot-link:") || g.str("handle") != "parent/review" {
		t.Fatalf("grant: %d %s", g.status, g.raw)
	}
	r := h.as(sessA, http.MethodGet, "/v1/inbox", nil)
	if r.status != http.StatusConflict || r.str("code") != whyDelegateGate {
		t.Errorf("an unnamed request with a link out: %d %s, want the gate's 409", r.status, r.raw)
	}
	if n := len(h.logs.lines(slog.LevelInfo, "request refused")); n != 1 {
		t.Errorf("request refused lines = %d, want 1", n)
	}
	self := map[string]string{wire.HeaderSession: sessA, wire.HeaderSelf: "1"}
	if r := h.request(http.MethodGet, "/v1/inbox", nil, self); r.status != http.StatusOK {
		t.Errorf("the same request with Self: %d %s", r.status, r.raw)
	}
	r = h.request(http.MethodDelete, "/v1/delegates/review", nil, self)
	if r.status != http.StatusOK || r.body["link"] != true {
		t.Errorf("revoke: %d %s, want the link revoked", r.status, r.raw)
	}
	if r := h.as(sessA, http.MethodGet, "/v1/inbox", nil); r.status != http.StatusOK {
		t.Errorf("after the revoke the gate still holds: %d %s", r.status, r.raw)
	}
}

func TestLifecycle_sessionEndSubagentStopAndTurnEnd(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "parent")
	handle := h.grantDelegate(sessA, "review")
	self := map[string]string{wire.HeaderSession: sessA, wire.HeaderSelf: "1"}
	life := func(headers map[string]string, event, agent string) *response {
		return h.request(http.MethodPost, "/v1/lifecycle", map[string]any{"event": event, "agent_id": agent}, headers)
	}
	me := func() map[string]any {
		r := h.request(http.MethodGet, "/v1/me", nil, self)
		if r.status != http.StatusOK {
			t.Fatalf("me: %d %s", r.status, r.raw)
		}
		id, _ := r.body["identity"].(map[string]any)
		return id
	}
	ended := func() bool {
		st, err := h.store.Status(t.Context(), store.Caller{Key: h.keyID(), Session: sessA, Self: true})
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		return st.Identity.Ended
	}
	if r := life(map[string]string{wire.HeaderSession: sessA}, eventSubagentStop, "internal-agent"); r.status != http.StatusOK {
		t.Errorf("subagent_stop for an unbound id: %d %s, want 200 past the gate", r.status, r.raw)
	}
	if n := len(h.logs.lines(slog.LevelInfo, "request refused")); n != 0 {
		t.Errorf("subagent_stop logged %d request refused lines", n)
	}
	if r := life(self, eventSessionEnd, "agent-1"); r.status != http.StatusOK || ended() {
		t.Errorf("session_end with an agent_id: %d %s, want nothing marked", r.status, r.raw)
	}
	if r := life(self, eventTurnEnd, ""); r.status != http.StatusOK {
		t.Errorf("turn_end: %d %s", r.status, r.raw)
	}
	if r := h.request(http.MethodGet, "/v1/inbox", nil, map[string]string{wire.HeaderSession: sessA, wire.HeaderAs: handle}); r.status != http.StatusOK {
		t.Errorf("the parent's subagent_stop ended its delegate: %d %s, want it still acting", r.status, r.raw)
	}

	second := h.grantDelegate(sessA, "tests")
	if r := life(self, eventSessionEnd, ""); r.status != http.StatusOK || !ended() {
		t.Fatalf("session_end: %d %s, want the session marked ended", r.status, r.raw)
	}
	if r := h.request(http.MethodGet, "/v1/inbox", nil, map[string]string{wire.HeaderSession: sessA, wire.HeaderAs: second}); r.status/100 != 4 {
		t.Errorf("a delegate outlived its parent's session end: %d %s", r.status, r.raw)
	}
	id := me()
	expires, _ := time.Parse(time.RFC3339, fmt.Sprint(id["expires_at"]))
	if id["state"] != stateLive || expires.After(h.clk.now().Add(time.Hour)) {
		t.Errorf("after session_end = %v, want the identity kept with its lease capped at an hour", id)
	}
	if r := life(self, "compact", ""); r.status != http.StatusBadRequest {
		t.Errorf("an unknown event: %d, want 400", r.status)
	}
}

// holdersQuery is the GET /v1/holders target for paths.
func holdersQuery(paths ...string) string {
	q := url.Values{"path": paths}
	return "/v1/holders?" + q.Encode()
}

func TestHolders_route(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "holder")
	h.mustRegister(sessB, "writer")
	held := repoGrelot + "/internal"
	if r := h.as(sessA, http.MethodPost, "/v1/reservations", map[string]any{"path": held, "reason": "editing", "ttl_ms": time.Hour.Milliseconds()}); r.status != http.StatusCreated {
		t.Fatalf("reserve: %d %s", r.status, r.raw)
	}
	file, free := held+"/a.go", repoGrelot+"/README.md"

	t.Run("one entry per grant another identity holds over the queried paths", func(t *testing.T) {
		r := h.as(sessB, http.MethodGet, holdersQuery(file, free, held+"/b.go"), nil)
		entries, _ := r.body["holders"].([]any)
		if r.status != http.StatusOK || len(entries) != 1 {
			t.Fatalf("GET: %d %s, want 200 and one entry", r.status, r.raw)
		}
		e := entries[0].(map[string]any)
		g, _ := e["held"].(map[string]any)
		if len(e) != 1 || g["holder"] != "holder" || g["path"] != held || g["reason"] != "editing" || g["state"] != "held" ||
			g["expires_at"] != testNow.Add(time.Hour).Format(time.RFC3339) {
			t.Errorf("entry %v, want only held: @holder's grant on %s until %s", e, held, testNow.Add(time.Hour).Format(time.RFC3339))
		}
	})
	t.Run("the caller's own grant is no entry", func(t *testing.T) {
		r := h.as(sessA, http.MethodGet, holdersQuery(file), nil)
		if r.status != http.StatusOK || !strings.Contains(r.raw, `"holders":[]`) {
			t.Errorf("GET as the holder: %d %s, want 200 and an empty list", r.status, r.raw)
		}
	})
	t.Run("the caller's queue entry", func(t *testing.T) {
		q := h.as(sessB, http.MethodPost, "/v1/reservations", map[string]any{"path": held, "reason": "next", "ttl_ms": (30 * time.Minute).Milliseconds(), "queue": true})
		if q.status != http.StatusCreated {
			t.Fatalf("queue: %d %s", q.status, q.raw)
		}
		id := q.body["reservation"].(map[string]any)["id"]
		r := h.as(sessB, http.MethodGet, holdersQuery(file), nil)
		entries, _ := r.body["holders"].([]any)
		if len(entries) != 1 {
			t.Fatalf("GET: %d %s, want one entry", r.status, r.raw)
		}
		queued, _ := entries[0].(map[string]any)["queued"].(map[string]any)
		if queued["id"] != id || queued["state"] != "queued" || queued["path"] != held {
			t.Errorf("queued %v, want the caller's queue entry %v on %s", queued, id, held)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		many := make([]string, 257)
		for i := range many {
			many[i] = fmt.Sprintf("%s/f%d", repoGrelot, i)
		}
		for name, c := range map[string]struct {
			session, target string
			status          int
		}{
			"no path":           {sessB, "/v1/holders", http.StatusBadRequest},
			"over 256 paths":    {sessB, holdersQuery(many...), http.StatusBadRequest},
			"a relative path":   {sessB, holdersQuery("workspace/grelot"), http.StatusBadRequest},
			"no identity":       {sessC, holdersQuery(file), http.StatusGone},
			"no session header": {"", holdersQuery(file), http.StatusBadRequest},
		} {
			t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
				if r := h.as(c.session, http.MethodGet, c.target, nil); r.status != c.status {
					t.Errorf("GET: %d %s, want %d", r.status, r.raw, c.status)
				}
			})
		}
	})
}
