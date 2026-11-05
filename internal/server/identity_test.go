package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

// reviveBody is a revive request for ttl, by name when name is set.
func reviveBody(name, ttl string) map[string]any {
	b := map[string]any{"host": "host-a/app", "os": "linux", "arch": "amd64", "where": where("a"), "ttl": ttl}
	if name != "" {
		b["name"] = name
	}
	return b
}

// self sends a request from session with Grelot-Self.
func (h *harness) self(session, method, path string, body any) *response {
	h.t.Helper()
	return h.request(method, path, body, map[string]string{headerSession: session, headerSelf: "1"})
}

func identityOf(r *response) map[string]any {
	id, _ := r.body["identity"].(map[string]any)
	return id
}

func TestRevive_refusals(t *testing.T) {
	h := newHarness(t)
	t.Run("a session with no identity and no name is 404", func(t *testing.T) {
		if r := h.as(sessA, http.MethodPost, "/v1/revive", reviveBody("", "2h")); r.status != http.StatusNotFound {
			t.Errorf("got %d %s, want 404", r.status, r.raw)
		}
	})
	t.Run("a name with no dormant identity is 404 naming it", func(t *testing.T) {
		r := h.as(sessA, http.MethodPost, "/v1/revive", reviveBody("nobody-here", "2h"))
		if r.status != http.StatusNotFound || !strings.Contains(r.str("error"), "no dormant identity nobody-here") {
			t.Errorf("got %d %s, want 404 naming nobody-here", r.status, r.raw)
		}
	})
	h.mustRegister(sessA, "grelot-dev")
	t.Run("a session with a live identity is 409", func(t *testing.T) {
		if r := h.as(sessA, http.MethodPost, "/v1/revive", reviveBody("", "2h")); r.status != http.StatusConflict {
			t.Errorf("got %d %s, want 409", r.status, r.raw)
		}
	})
}

func TestRevive_ownDormantIdentityWithTheMailSentWhileDormant(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessB, "store-dev")
	if r := h.as(sessB, http.MethodPatch, "/v1/me", map[string]any{"ttl": "24h", "where": where("a")}); r.status != http.StatusOK {
		t.Fatalf("renew store-dev: %d %s", r.status, r.raw)
	}
	h.mustRegister(sessA, "grelot-dev")
	h.clk.advance(7 * time.Hour)
	h.directPost(sessB, "grelot-dev")

	r := h.as(sessA, http.MethodPost, "/v1/revive", reviveBody("", "2h"))
	if r.status != http.StatusOK {
		t.Fatalf("revive: %d %s, want 200", r.status, r.raw)
	}
	if id := identityOf(r); id["name"] != "grelot-dev" || id["state"] != "live" {
		t.Errorf("revived identity %v, want grelot-dev live", id)
	}
	if got, _ := r.body["unread"].(float64); got != 1 {
		t.Errorf("unread %v, want the 1 direct message sent while dormant", r.body["unread"])
	}
	if _, ok := r.body["no_environment"]; ok {
		t.Error("a revive with a known environment carries the no-environment line")
	}
}

func TestRevive_byNameMovesItWithinItsEnvironment(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "grelot-dev")
	h.clk.advance(7 * time.Hour)
	r := h.as(sessC, http.MethodPost, "/v1/revive", reviveBody("grelot-dev", "2h"))
	if r.status != http.StatusOK || r.body["moved"] != true {
		t.Fatalf("revive by name: %d %s, want 200 moved", r.status, r.raw)
	}
	if me := h.as(sessC, http.MethodGet, "/v1/me", nil); identityOf(me)["name"] != "grelot-dev" || me.str("state") != "live" {
		t.Errorf("GET /v1/me from the new session: %s, want grelot-dev live", me.raw)
	}
}

func TestDormant_onlyMeReviveAndAFreshRegisterPass(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "grelot-dev")
	h.clk.advance(7 * time.Hour)
	for _, tc := range []struct {
		method string
		body   any
	}{
		{http.MethodPatch, map[string]any{"description": "still here"}},
		{http.MethodDelete, nil},
	} {
		t.Run(tc.method+" /v1/me is 410 naming revive", func(t *testing.T) {
			r := h.as(sessA, tc.method, "/v1/me", tc.body)
			if r.status != http.StatusGone || !strings.Contains(r.str("error"), "grelot revive") || r.str("code") != "dormant" {
				t.Errorf("got %d %s, want 410 dormant naming grelot revive", r.status, r.raw)
			}
		})
	}
	t.Run("GET /v1/me answers the dormant state", func(t *testing.T) {
		if r := h.as(sessA, http.MethodGet, "/v1/me", nil); r.status != http.StatusOK || r.str("state") != "dormant" {
			t.Errorf("got %d %s, want 200 dormant", r.status, r.raw)
		}
	})
	t.Run("a fresh register under another name passes", func(t *testing.T) {
		if r := h.as(sessA, http.MethodPost, "/v1/register", registerBody("grelot-dev-2", "2h")); r.status != http.StatusCreated {
			t.Errorf("got %d %s, want 201", r.status, r.raw)
		}
	})
	t.Run("and the dormant one stays revivable by name", func(t *testing.T) {
		if r := h.as(sessC, http.MethodPost, "/v1/revive", reviveBody("grelot-dev", "2h")); r.status != http.StatusOK {
			t.Errorf("got %d %s, want 200", r.status, r.raw)
		}
	})
}

func TestPatchMe(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "grelot-dev")

	t.Run("a ttl with where renews, and the status line shows the time under an hour", func(t *testing.T) {
		r := h.as(sessA, http.MethodPatch, "/v1/me", map[string]any{"ttl": "45m", "where": where("a")})
		if r.status != http.StatusOK || !strings.HasSuffix(r.line(), " · expires in 45 min") {
			t.Errorf("got %d with status %q, want 200 ending with 45 min left", r.status, r.line())
		}
	})
	t.Run("a ttl with no where is 400 naming where", func(t *testing.T) {
		r := h.as(sessA, http.MethodPatch, "/v1/me", map[string]any{"ttl": "2h"})
		if r.status != http.StatusBadRequest || !strings.Contains(r.str("error"), "where is required") {
			t.Errorf("got %d %s, want 400 naming where", r.status, r.raw)
		}
	})
	t.Run("a new description", func(t *testing.T) {
		r := h.as(sessA, http.MethodPatch, "/v1/me", map[string]any{"description": "now the identity routes"})
		if r.status != http.StatusOK || identityOf(r)["description"] != "now the identity routes" {
			t.Errorf("got %d %s, want 200 with the new description", r.status, r.raw)
		}
	})
	t.Run("nothing to set is 400", func(t *testing.T) {
		if r := h.as(sessA, http.MethodPatch, "/v1/me", map[string]any{}); r.status != http.StatusBadRequest {
			t.Errorf("got %d %s, want 400", r.status, r.raw)
		}
	})
}

func TestDeleteMe_retires(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "grelot-dev")
	if r := h.as(sessA, http.MethodDelete, "/v1/me", nil); r.status != http.StatusOK {
		t.Fatalf("DELETE /v1/me: %d %s", r.status, r.raw)
	}
	r := h.as(sessA, http.MethodGet, "/v1/me", nil)
	if r.str("state") != "none" || !strings.Contains(r.line(), "grelot register") {
		t.Errorf("after bye: %s with status %q, want state none and the register command", r.raw, r.line())
	}
	if r := h.as(sessB, http.MethodPost, "/v1/register", registerBody("grelot-dev", "2h")); r.status != http.StatusCreated {
		t.Errorf("the retired name: %d %s, want it free", r.status, r.raw)
	}
}

func TestMe_liveIdentityStatus(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "grelot-dev")
	ctx := t.Context()
	own := store.Caller{Session: sessA, Self: true}
	if _, err := h.store.Reserve(ctx, &store.Reserving{Path: repoGrelot + "/main.go", Reason: "editing", Caller: own, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	g, err := h.store.Delegate(ctx, &store.Grant{Caller: own, Label: "tests", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if r := h.as(sessA, http.MethodPost, "/v1/register", map[string]any{"delegate": g.Link, "description": "writing tests"}); r.status != http.StatusCreated {
		t.Fatalf("register --delegate: %d %s", r.status, r.raw)
	}
	if _, err := h.store.Delegate(ctx, &store.Grant{Caller: own, Label: "review", TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}

	r := h.self(sessA, http.MethodGet, "/v1/me", nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET /v1/me: %d %s", r.status, r.raw)
	}
	t.Run("its reservation", func(t *testing.T) {
		rs, _ := r.body["reservations"].([]any)
		first, _ := rs[0].(map[string]any)
		if len(rs) != 1 || first["path"] != repoGrelot+"/main.go" || first["state"] != "held" {
			t.Errorf("reservations %v, want the held main.go", r.body["reservations"])
		}
	})
	t.Run("its live delegate and its unused link", func(t *testing.T) {
		ds, _ := r.body["delegates"].([]any)
		ls, _ := r.body["links"].([]any)
		d, _ := ds[0].(map[string]any)
		l, _ := ls[0].(map[string]any)
		if len(ds) != 1 || d["handle"] != "grelot-dev/tests" || d["description"] != "writing tests" || d["bound"] != false {
			t.Errorf("delegates %v, want the unbound grelot-dev/tests", ds)
		}
		if len(ls) != 1 || l["handle"] != "grelot-dev/review" {
			t.Errorf("links %v, want grelot-dev/review", ls)
		}
	})
	t.Run("machine and environment known, by kind only", func(t *testing.T) {
		id := identityOf(r)
		if id["machine_kind"] != "dmi" || id["environment_known"] != true {
			t.Errorf("identity %v, want machine_kind dmi and environment_known", id)
		}
	})
	t.Run("offline, push none, the path root", func(t *testing.T) {
		if r.body["online"] != false || r.str("push") != "none" || r.str("path_root") != DefaultPathRoot {
			t.Errorf("online %v push %v path_root %v", r.body["online"], r.body["push"], r.body["path_root"])
		}
	})
	h.push.set(h.idOf(sessA), pushOK)
	t.Run("push ok once a door serves it", func(t *testing.T) {
		if got := h.self(sessA, http.MethodGet, "/v1/me", nil).str("push"); got != "ok" {
			t.Errorf("push %q, want ok", got)
		}
	})
}

func TestDelegateGate_refusesASessionWithDelegatesNamingNeither(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "grelot-dev")
	if _, err := h.store.Delegate(t.Context(), &store.Grant{Caller: store.Caller{Session: sessA}, Label: "tests", TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method string
		body   any
	}{{http.MethodGet, nil}, {http.MethodPatch, map[string]any{"description": "x"}}, {http.MethodDelete, nil}} {
		t.Run(tc.method+" /v1/me is 409 naming both fixes", func(t *testing.T) {
			r := h.as(sessA, tc.method, "/v1/me", tc.body)
			msg := r.str("error")
			if r.status != http.StatusConflict || r.str("code") != "delegate_gate" || !strings.Contains(msg, "grelot as") || !strings.Contains(msg, "--self") {
				t.Errorf("got %d %s, want 409 delegate_gate naming grelot as and --self", r.status, r.raw)
			}
			if !strings.HasPrefix(r.line(), "grelot: @grelot-dev") {
				t.Errorf("status %q, want the caller's line on the refusal", r.line())
			}
		})
	}
	t.Run("Grelot-Self passes", func(t *testing.T) {
		if r := h.self(sessA, http.MethodGet, "/v1/me", nil); r.status != http.StatusOK {
			t.Errorf("got %d %s, want 200", r.status, r.raw)
		}
	})
	t.Run("the identity is untouched", func(t *testing.T) {
		if r := h.self(sessA, http.MethodGet, "/v1/me", nil); identityOf(r)["description"] != "building the HTTP API" {
			t.Errorf("identity %v, want its description unchanged", identityOf(r))
		}
	})
}
