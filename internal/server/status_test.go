package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

func TestStatus_onEveryIdentityResponse(t *testing.T) {
	h := newHarness(t)

	t.Run("a session with no identity gets the register command", func(t *testing.T) {
		r := h.as(sessA, http.MethodGet, "/v1/me", nil)
		if got := r.line(); !strings.HasPrefix(got, "grelot: ") || !strings.Contains(got, "grelot register") || !strings.Contains(got, "--ttl") {
			t.Errorf("status %q, want the register command", got)
		}
	})

	reg := h.as(sessA, http.MethodPost, "/v1/register", registerBody("grelot-dev", "6h"))
	t.Run("the register answer carries the new identity's line", func(t *testing.T) {
		if got, want := reg.line(), "grelot: @grelot-dev · 0 unread · wake: down"; got != want {
			t.Errorf("status %q, want %q", got, want)
		}
	})

	h.mustRegister(sessB, "store-dev")
	h.directPost(sessB, "grelot-dev")
	t.Run("unread mail with its wake count, a quiet join card counted unread only", func(t *testing.T) {
		if got, want := h.as(sessA, http.MethodGet, "/v1/me", nil).line(), "grelot: @grelot-dev · 2 unread (1 wake) · wake: down"; got != want {
			t.Errorf("status %q, want %q", got, want)
		}
	})

	t.Run("a refused request still gets the caller's line", func(t *testing.T) {
		r := h.as(sessC, http.MethodPost, "/v1/register", registerBody("grelot-dev", "6h"))
		if r.status != http.StatusConflict || !strings.Contains(r.line(), "grelot register") {
			t.Errorf("got %d with status %q, want 409 with the register command", r.status, r.line())
		}
	})
}

func TestStatus_remainingTimeOnlyUnderAnHour(t *testing.T) {
	h := newHarness(t)
	if r := h.as(sessA, http.MethodPost, "/v1/register", registerBody("grelot-dev", "45m")); !strings.HasSuffix(r.line(), " · expires in 45 min") {
		t.Errorf("status %q, want it to end with the 45 min left", r.line())
	}
	h.clk.advance(20 * time.Minute)
	if got := h.as(sessA, http.MethodGet, "/v1/me", nil).line(); !strings.HasSuffix(got, " · expires in 25 min") {
		t.Errorf("status %q, want it to end with the 25 min left", got)
	}
	h.mustRegister(sessB, "store-dev")
	if got := h.as(sessB, http.MethodGet, "/v1/me", nil).line(); strings.Contains(got, "expires") {
		t.Errorf("status %q names the expiry of a 6 h lease", got)
	}
}

func TestStatus_dormantGetsTheReviveOffer(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "grelot-dev")
	h.clk.advance(7 * time.Hour)
	r := h.as(sessA, http.MethodGet, "/v1/me", nil)
	if r.status != http.StatusOK || r.str("state") != "dormant" {
		t.Fatalf("GET /v1/me: %d %s, want 200 dormant", r.status, r.raw)
	}
	if got := r.line(); !strings.HasPrefix(got, "grelot: ") || !strings.Contains(got, "grelot revive --ttl") {
		t.Errorf("status %q, want the revive offer", got)
	}
}

func TestStatus_aDelegateIsPullOnly(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "grelot-dev")
	g, err := h.store.Delegate(t.Context(), &store.Grant{Caller: store.Caller{Session: sessA}, Label: "tests", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	r := h.request(http.MethodPost, "/v1/register", map[string]any{"delegate": g.Link, "description": "writing the tests"},
		map[string]string{headerSession: sessA})
	if r.status != http.StatusCreated {
		t.Fatalf("register --delegate: %d %s", r.status, r.raw)
	}
	me := h.request(http.MethodGet, "/v1/me", nil, map[string]string{headerSession: sessA, headerAs: g.Handle})
	if got, want := me.line(), "grelot: @grelot-dev/tests · 0 unread · wake: pull only"; got != want {
		t.Errorf("status %q, want %q", got, want)
	}
}

func TestStatus_wakeState(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "grelot-dev")
	id := h.idOf(sessA)
	wake := func() string {
		_, after, _ := strings.Cut(h.as(sessA, http.MethodGet, "/v1/me", nil).line(), " · wake: ")
		return after
	}

	t.Run("down with no wait and no push", func(t *testing.T) {
		if got := wake(); got != "down" {
			t.Errorf("wake %q, want down", got)
		}
	})
	h.srv.presence.opened(id)
	t.Run("watcher while a standing wait is open", func(t *testing.T) {
		if got := wake(); got != "watcher" {
			t.Errorf("wake %q, want watcher", got)
		}
	})
	h.push.set(id, pushOK)
	t.Run("host push while its push is ok, a wait open or not", func(t *testing.T) {
		if got := wake(); got != "host push" {
			t.Errorf("wake %q, want host push", got)
		}
	})
	for _, st := range []pushState{pushFailing, pushOff} {
		h.push.set(id, st)
		t.Run("watcher while push is "+string(st), func(t *testing.T) {
			if got := wake(); got != "watcher" {
				t.Errorf("wake %q, want watcher", got)
			}
		})
	}
}

func TestCaller_headers(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "grelot-dev")

	t.Run("no session is 400", func(t *testing.T) {
		if r := h.request(http.MethodGet, "/v1/me", nil, nil); r.status != http.StatusBadRequest || !strings.Contains(r.str("error"), "Grelot-Session") {
			t.Errorf("got %d %s, want 400 naming Grelot-Session", r.status, r.raw)
		}
	})
	t.Run("Grelot-As with Grelot-Self is 400", func(t *testing.T) {
		r := h.request(http.MethodGet, "/v1/me", nil, map[string]string{headerSession: sessA, headerAs: "grelot-dev/tests", headerSelf: "1"})
		if r.status != http.StatusBadRequest {
			t.Errorf("got %d %s, want 400", r.status, r.raw)
		}
	})
	t.Run("Grelot-Self other than 1 is 400", func(t *testing.T) {
		if r := h.request(http.MethodGet, "/v1/me", nil, map[string]string{headerSession: sessA, headerSelf: "yes"}); r.status != http.StatusBadRequest {
			t.Errorf("got %d %s, want 400", r.status, r.raw)
		}
	})
	t.Run("Grelot-Self from a session with no delegates acts as its identity", func(t *testing.T) {
		r := h.request(http.MethodGet, "/v1/me", nil, map[string]string{headerSession: sessA, headerSelf: "1"})
		id, _ := r.body["identity"].(map[string]any)
		if r.status != http.StatusOK || id["name"] != "grelot-dev" {
			t.Errorf("got %d %s, want 200 as grelot-dev", r.status, r.raw)
		}
	})
}
