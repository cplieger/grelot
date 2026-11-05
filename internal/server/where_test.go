package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWhere_requiredOnRegisterReviveAndRenew(t *testing.T) {
	h := newHarness(t)
	reg := registerBody("grelot-dev", "6h")
	delete(reg, "where")
	rev := reviveBody("", "2h")
	delete(rev, "where")
	for _, tc := range []struct {
		name, method, path string
		body               map[string]any
		setup              func()
	}{
		{"register", http.MethodPost, "/v1/register", reg, nil},
		{"revive", http.MethodPost, "/v1/revive", rev, nil},
		{"renew", http.MethodPatch, "/v1/me", map[string]any{"ttl": "2h"}, func() { h.mustRegister(sessA, "grelot-dev") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup()
			}
			r := h.as(sessA, tc.method, tc.path, tc.body)
			if r.status != http.StatusBadRequest || !strings.Contains(r.str("error"), "where is required") {
				t.Errorf("got %d %s, want 400 naming where", r.status, r.raw)
			}
		})
	}
}

func TestWhere_malformedMemberIs400NamingTheField(t *testing.T) {
	h := newHarness(t)
	checkout := func(repo, id, remote string) map[string]any {
		w := where("a")
		w["checkouts"] = []any{map[string]any{"repo": repo, "id": id, "remote": remote}}
		return w
	}
	hex := strings.Repeat("1", 32)
	member := func(key, value string) map[string]any {
		w := where("a")
		w[key] = value
		return w
	}
	for _, tc := range []struct {
		name  string
		where map[string]any
		field string
	}{
		{"a machine of no known kind", member("machine", "bios:"+strings.Repeat("1", 32)), "where.machine"},
		{"a machine with upper-case hex", member("machine", "dmi:"+strings.Repeat("A", 32)), "where.machine"},
		{"an environment of 31 digits", member("environment", strings.Repeat("a", 31)), "where.environment"},
		{"a checkout id that is not hex", checkout(repoGrelot, strings.Repeat("z", 32), hex), "where.checkouts[0].id"},
		{"a checkout for a repo no work path lies in", checkout("/workspace/other", hex, hex), "where.checkouts[0].repo"},
		{"a checkout with no remote", checkout(repoGrelot, hex, ""), "where.checkouts[0].remote"},
		{"a remote of 31 digits", checkout(repoGrelot, hex, hex[:31]), "where.checkouts[0].remote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := registerBody("grelot-dev", "6h")
			body["where"] = tc.where
			r := h.as(sessA, http.MethodPost, "/v1/register", body)
			if r.status != http.StatusBadRequest || !strings.Contains(r.str("error"), tc.field) {
				t.Errorf("got %d %s, want 400 naming %s", r.status, r.raw, tc.field)
			}
		})
	}
}

func TestWhere_emptyEnvironmentIsAcceptedWithTheNoEnvironmentLine(t *testing.T) {
	h := newHarness(t)
	body := registerBody("grelot-dev", "6h")
	body["where"] = where("")
	r := h.as(sessA, http.MethodPost, "/v1/register", body)
	line := r.str("no_environment")
	if r.status != http.StatusCreated || !strings.Contains(line, "this session has no environment id") ||
		!strings.Contains(line, "another session cannot hand this identity over or revive it") {
		t.Errorf("got %d %s, want 201 with the no-environment line", r.status, r.raw)
	}
	if known := identityOf(r)["environment_known"]; known != false {
		t.Errorf("environment_known %v, want false", known)
	}
	t.Run("a known environment gets no such line", func(t *testing.T) {
		if r := h.as(sessB, http.MethodPost, "/v1/register", registerBody("store-dev", "6h")); r.str("no_environment") != "" {
			t.Errorf("register with an environment: %s", r.raw)
		}
	})
}

func TestWhere_storedValuesInNoResponseOrLogLine(t *testing.T) {
	h := newHarness(t)
	machine := "dmi:" + strings.Repeat("3c", 16)
	env := strings.Repeat("5e", 16)
	checkoutID := strings.Repeat("9d", 16)
	remote := strings.Repeat("7b", 16)
	seeded := map[string]any{
		"machine": machine, "environment": env,
		"checkouts": []any{map[string]any{"repo": repoGrelot, "id": checkoutID, "remote": remote}},
	}
	reg := registerBody("grelot-dev", "6h")
	reg["where"], reg["proc"] = seeded, "4242:1700000000"
	hint := map[string]string{headerSession: sessB, headerEnvironment: env, headerMachine: machine}

	var answers []*response
	answers = append(answers,
		h.as(sessA, http.MethodPost, "/v1/register", reg),
		h.as(sessA, http.MethodGet, "/v1/me", nil),
		h.as(sessA, http.MethodPatch, "/v1/me", map[string]any{"ttl": "2h", "where": seeded}),
		h.request(http.MethodGet, "/v1/me?proc=4242:1700000000", nil, hint),
		h.as(sessC, http.MethodGet, "/v1/identities", nil),
	)
	h.clk.advance(3 * time.Hour)
	answers = append(answers, h.as(sessC, http.MethodGet, "/v1/identities?all=1", nil))
	answers = append(answers,
		h.as(sessA, http.MethodGet, "/v1/me", nil),
		h.request(http.MethodGet, "/v1/me?proc=4242:1700000000", nil, hint),
		h.as(sessA, http.MethodPost, "/v1/revive", map[string]any{"ttl": "2h", "where": seeded}),
		h.as(sessA, http.MethodDelete, "/v1/me", nil),
	)
	if got := answers[3].body["hint"]; got == nil {
		t.Fatalf("no take-over hint from the seeded environment: %s", answers[3].raw)
	}
	for i, r := range answers {
		text := r.raw + "\n" + strings.Join(flatHeaders(r.header), "\n")
		for _, v := range []string{machine, strings.TrimPrefix(machine, "dmi:"), env, checkoutID, remote} {
			if strings.Contains(text, v) {
				t.Errorf("answer %d carries a seeded where value: %s", i, text)
			}
		}
	}
	logs := h.logs.text()
	for _, v := range []string{strings.TrimPrefix(machine, "dmi:"), env, checkoutID, remote} {
		if strings.Contains(logs, v) {
			t.Errorf("a log line carries a seeded where value %s:\n%s", v, logs)
		}
	}
}

func flatHeaders(h http.Header) []string {
	var out []string
	for k, vs := range h {
		for _, v := range vs {
			out = append(out, k+": "+v)
		}
	}
	return out
}

func TestMe_takeOverHint(t *testing.T) {
	h := newHarness(t)
	const proc = "4242:1700000000"
	reg := registerBody("grelot-dev", "6h")
	reg["proc"] = proc
	if r := h.as(sessA, http.MethodPost, "/v1/register", reg); r.status != http.StatusCreated {
		t.Fatalf("register: %d %s", r.status, r.raw)
	}
	same := where("a")
	sameEnv, _ := same["environment"].(string)
	sameMachine, _ := same["machine"].(string)
	hintFor := func(headers map[string]string, query string) *response {
		headers[headerSession] = sessB
		return h.request(http.MethodGet, "/v1/me?proc="+proc+query, nil, headers)
	}
	hintOf := func(r *response) map[string]any {
		hint, _ := r.body["hint"].(map[string]any)
		return hint
	}

	t.Run("the live identity under that proc in the caller's environment", func(t *testing.T) {
		got := hintOf(hintFor(map[string]string{headerEnvironment: sameEnv, headerMachine: sameMachine}, ""))
		line, _ := got["line"].(string)
		if got["name"] != "grelot-dev" || got["state"] != "live" || !strings.Contains(line, "grelot register grelot-dev --take-over --ttl") {
			t.Errorf("hint %v, want grelot-dev live with the take-over command", got)
		}
	})
	t.Run("the environment only from its header, never the query", func(t *testing.T) {
		r := hintFor(map[string]string{}, "&environment="+sameEnv+"&machine="+sameMachine)
		if hintOf(r) != nil {
			t.Errorf("hint %v from query parameters", hintOf(r))
		}
		if strings.Contains(h.logs.text(), sameEnv) {
			t.Error("a log line holds the environment id sent in the query")
		}
	})
	t.Run("none from another environment", func(t *testing.T) {
		if got := hintOf(hintFor(map[string]string{headerEnvironment: strings.Repeat("b", 32), headerMachine: sameMachine}, "")); got != nil {
			t.Errorf("hint %v for another environment", got)
		}
	})
	t.Run("a malformed environment header is 400", func(t *testing.T) {
		r := hintFor(map[string]string{headerEnvironment: "not-hex"}, "")
		if r.status != http.StatusBadRequest || !strings.Contains(r.str("error"), "environment") {
			t.Errorf("got %d %s, want 400 naming the environment", r.status, r.raw)
		}
	})
	h.clk.advance(7 * time.Hour)
	t.Run("a dormant one is offered as a revive", func(t *testing.T) {
		got := hintOf(hintFor(map[string]string{headerEnvironment: sameEnv, headerMachine: sameMachine}, ""))
		line, _ := got["line"].(string)
		if got["state"] != "dormant" || !strings.Contains(line, "grelot revive grelot-dev --ttl") {
			t.Errorf("hint %v, want the revive offer", got)
		}
	})
}
