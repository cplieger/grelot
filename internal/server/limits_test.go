//go:build !windows

package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/grelot/internal/auth"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/store"
)

// withLimits runs the server on l, Limits{} for every default.
func withLimits(l Limits) func(*Options) { return func(o *Options) { o.Limits = l } }

func TestLimits_eachZeroFieldTakesItsDefault(t *testing.T) {
	got := Limits{}.orDefault()
	if want := (Limits{Rate: 20, Burst: 40, KeyWaits: 128, ServerWaits: 1024}); got != want {
		t.Errorf("Limits{} = %+v, want %+v", got, want)
	}
	if got := (Limits{Rate: 5, KeyWaits: 2}).orDefault(); got.Rate != 5 || got.KeyWaits != 2 || got.Burst != 40 || got.ServerWaits != 1024 {
		t.Errorf("Limits{Rate: 5, KeyWaits: 2} = %+v, want the two set and the defaults beside them", got)
	}
}

func TestLimits_aKeyPastItsBurstIs429UntilATokenIsBack(t *testing.T) {
	h := newHarnessWith(t, store.Options{}, withLimits(Limits{}))
	for i := range 40 {
		if r := h.request(http.MethodGet, "/v1/version", nil, nil); r.status != http.StatusOK {
			t.Fatalf("request %d of the burst: %d %s, want 200", i+1, r.status, r.raw)
		}
	}

	r := h.request(http.MethodGet, "/v1/version", nil, nil)

	if r.status != http.StatusTooManyRequests || r.str("code") != "rate_limited" {
		t.Fatalf("the 41st request: %d %s, want 429 rate_limited", r.status, r.raw)
	}
	if got := r.header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After %q, want 1: a token is back in 50 ms, rounded up to whole seconds", got)
	}
	if r.header.Get(headerVersion) != testVersion {
		t.Errorf("the 429 names version %q, want %q", r.header.Get(headerVersion), testVersion)
	}
	if got := h.scrape()[`grelot_requests_limited_total{limit="rate"}`]; got != 1 {
		t.Errorf(`grelot_requests_limited_total{limit="rate"} = %v, want 1`, got)
	}
	key, err := h.srv.auth.CreateKey(t.Context(), "other-machine")
	if err != nil {
		t.Fatal(err)
	}
	if r := h.request(http.MethodGet, "/v1/version", nil, map[string]string{"Authorization": "Bearer " + key}); r.status != http.StatusOK {
		t.Errorf("another key while the first is out of tokens: %d %s, want 200", r.status, r.raw)
	}
	h.srv.Beat(t.Context())
	if r := h.request(http.MethodGet, "/v1/version", nil, nil); r.status != http.StatusTooManyRequests {
		t.Errorf("after a sweep with no time passed: %d, want 429: an empty bucket is not forgotten", r.status)
	}
	h.clk.advance(50 * time.Millisecond)
	if r := h.request(http.MethodGet, "/v1/version", nil, nil); r.status != http.StatusOK {
		t.Errorf("50 ms later, at 20 a second: %d %s, want 200", r.status, r.raw)
	}
}

// session is the session name registers from.
func session(name string) string { return "kiro-cli:session-" + name }

// registerUnder registers name alone on its own work path with key.
func registerUnder(h *harness, key, name string) {
	h.t.Helper()
	if r := h.request(http.MethodPost, "/v1/register", registerBody(name, "6h", "/workspace/"+name), withBearer(session(name), key)); r.status != http.StatusCreated {
		h.t.Fatalf("register %s: %d %s", name, r.status, r.raw)
	}
}

// standingWait sends name's standing wait of 55 s with key.
func standingWait(h *harness, key, name string) *pending {
	headers := withBearer(session(name), key)
	headers[headerSelf] = "1"
	return h.async(http.MethodPost, "/v1/events/wait", map[string]any{"wait": "55s"}, headers)
}

// heldWaits registers each of names with key and opens its standing wait,
// returning them once every one holds.
func heldWaits(h *harness, key string, names ...string) []*pending {
	h.t.Helper()
	var out []*pending
	for _, name := range names {
		registerUnder(h, key, name)
		out = append(out, standingWait(h, key, name))
	}
	synctest.Wait()
	for i, p := range out {
		if r := p.now(); r != nil {
			h.t.Fatalf("the wait of %s answered %d %s, want it held", names[i], r.status, r.raw)
		}
	}
	return out
}

func names(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix + "-" + strconv.Itoa(i+1)
	}
	return out
}

func TestLimits_theNinthWaitOfAKeyIsRefusedUntilOneAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t, withLimits(Limits{KeyWaits: 8}))
		key, err := h.machineKey()
		if err != nil {
			t.Fatal(err)
		}
		held := heldWaits(h, key, names("held", 8)...)
		registerUnder(h, key, "writer")
		registerUnder(h, key, "ninth")

		r := standingWait(h, key, "ninth").answer()

		if r.status != http.StatusTooManyRequests || r.str("code") != "too_many_waits" {
			t.Fatalf("a ninth wait of one key: %d %s, want 429 too_many_waits", r.status, r.raw)
		}
		if msg := r.str("error"); !strings.Contains(msg, "holds 8 waits") || !strings.Contains(msg, "the client retries") {
			t.Errorf("the refusal says %q, want the key's 8 and that the client retries", msg)
		}
		if got := h.scrape()[`grelot_requests_limited_total{limit="waits_key"}`]; got != 1 {
			t.Errorf(`grelot_requests_limited_total{limit="waits_key"} = %v, want 1`, got)
		}
		in := h.request(http.MethodGet, "/v1/inbox?wait=10s", nil, withBearer(session("ninth"), key))
		if in.status != http.StatusTooManyRequests || in.str("code") != "too_many_waits" {
			t.Errorf("an in-turn wait past the cap: %d %s, want 429 too_many_waits", in.status, in.raw)
		}
		if r := h.request(http.MethodGet, "/v1/inbox", nil, withBearer(session("ninth"), key)); r.status != http.StatusOK {
			t.Errorf("an inbox read with no wait past the cap: %d %s, want 200: it holds nothing", r.status, r.raw)
		}

		h.mustPost(session("writer"), map[string]any{"agent": "held-1"}, "wake up")
		if r := held[0].answer(); r.status != http.StatusOK {
			t.Fatalf("held-1's wait on its mail: %d %s, want 200 with the nudge", r.status, r.raw)
		}
		again := standingWait(h, key, "ninth")
		synctest.Wait()
		if r := again.now(); r != nil {
			t.Errorf("the ninth wait once one of the eight answered: %d %s, want it held", r.status, r.raw)
		}
	})
}

func TestLimits_theServerCapsTheWaitsOfEveryKeyTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t, withLimits(Limits{KeyWaits: 2, ServerWaits: 3}))
		first, err := h.machineKey()
		if err != nil {
			t.Fatal(err)
		}
		second, err := h.srv.auth.CreateKey(t.Context(), "second-machine")
		if err != nil {
			t.Fatal(err)
		}
		heldWaits(h, first, "a-1", "a-2")
		heldWaits(h, second, "b-1")
		registerUnder(h, second, "b-2")

		r := standingWait(h, second, "b-2").answer()

		if r.status != http.StatusTooManyRequests || r.str("code") != "too_many_waits" {
			t.Fatalf("a wait under its key's cap past the server's: %d %s, want 429 too_many_waits", r.status, r.raw)
		}
		page := h.scrape()
		if got := page[`grelot_requests_limited_total{limit="waits_server"}`]; got != 1 {
			t.Errorf(`grelot_requests_limited_total{limit="waits_server"} = %v, want 1`, got)
		}
		if got := page[`grelot_requests_limited_total{limit="waits_key"}`]; got != 0 {
			t.Errorf(`grelot_requests_limited_total{limit="waits_key"} = %v, want 0`, got)
		}
	})
}

func TestLimits_everyAuthFailureReasonAndLimitStartsAtZero(t *testing.T) {
	h := newHarness(t)
	page := h.scrape()
	series := []string{
		`grelot_requests_limited_total{limit="rate"}`, `grelot_requests_limited_total{limit="waits_key"}`,
		`grelot_requests_limited_total{limit="waits_server"}`,
	}
	// access's gate.failure and gate.forbidden reasons, then grelot's own.
	for _, reason := range []string{
		"key_unknown", "key_expired", "key_retired", "account_disabled", "session_expired", "session_unknown",
		"credential_invalid", "key_required", "session_required",
		"recovery_session", "cross_origin", "rate_limited", "wrong_key_identity", "other",
	} {
		series = append(series, `grelot_auth_failures_total{reason="`+reason+`"}`)
	}
	for _, series := range series {
		if v, ok := page[series]; !ok || v != 0 {
			t.Errorf("%s = %v (served %t), want 0 served", series, v, ok)
		}
	}
}

func TestAuthFailures_countACredentialPresentedAndRefused(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")
	failures := func(reason string) float64 {
		return h.scrape()[`grelot_auth_failures_total{reason="`+reason+`"}`]
	}

	// The reasons are access's gate.failure reasons (access internal/failreason).
	t.Run("an unknown key is key_unknown and no key counts nothing", func(t *testing.T) {
		h.request(http.MethodGet, "/v1/me", nil, map[string]string{"Authorization": "", headerSession: sessA})
		h.request(http.MethodGet, "/v1/me", nil, map[string]string{"Authorization": "Basic b3duZXI6cGFzcw==", headerSession: sessA})
		if got := failures("key_unknown"); got != 0 {
			t.Fatalf("key_unknown %v after a request with no key and one with another scheme, want 0", got)
		}
		r := h.request(http.MethodGet, "/v1/me", nil, map[string]string{"Authorization": "Bearer grelot_unknown", headerSession: sessA})
		if r.status != http.StatusUnauthorized || failures("key_unknown") != 1 {
			t.Errorf("an unknown key: %d, key_unknown %v, want 401 and 1", r.status, failures("key_unknown"))
		}
	})
	t.Run("another key's identity is wrong_key_identity", func(t *testing.T) {
		other, err := h.srv.auth.CreateKey(t.Context(), "other-machine")
		if err != nil {
			t.Fatal(err)
		}
		r := h.request(http.MethodGet, "/v1/me", nil, withBearer(sessA, other))
		if r.status != http.StatusForbidden || failures(obs.AuthWrongKeyIdentity) != 1 {
			t.Errorf("alpha's session with another key: %d %s, wrong_key_identity %v, want 403 and 1", r.status, r.raw, failures(obs.AuthWrongKeyIdentity))
		}
	})
	t.Run("a session on /v1 is key_required", func(t *testing.T) {
		before := failures("key_required")
		r := h.request(http.MethodGet, "/v1/me", nil, map[string]string{"Authorization": "", "Cookie": h.browser.header(), headerSession: sessA})
		if r.status != http.StatusForbidden || failures("key_required") != before+1 {
			t.Errorf("a session on /v1: %d %s, key_required %v, want 403 and %v", r.status, r.raw, failures("key_required"), before+1)
		}
	})
	t.Run("a signed-out session is session_unknown and no cookie counts nothing", func(t *testing.T) {
		dead := h.browser.header()
		if r := h.request(http.MethodPost, "/api/auth/logout", nil, noKey); r.status != http.StatusNoContent {
			t.Fatalf("sign out: %d %s", r.status, r.raw)
		}
		h.request(http.MethodGet, "/api/conversations", nil, nil)
		if got := failures("session_unknown"); got != 0 {
			t.Fatalf("session_unknown %v after a request with no cookie, want 0", got)
		}
		r := h.request(http.MethodGet, "/api/conversations", nil, map[string]string{"Cookie": dead})
		if r.status != http.StatusUnauthorized || failures("session_unknown") != 1 {
			t.Errorf("the signed-out session's cookie: %d, session_unknown %v, want 401 and 1", r.status, failures("session_unknown"))
		}
	})
}

func TestAuthFailures_aSignInTheFailureLimiterRefusesIsRateLimited(t *testing.T) {
	m := obs.New()
	a := testAuth(t, func(o *auth.Options) { o.Errors = AuthErrors(slog.New(slog.DiscardHandler), m) })
	h := newHarnessWith(t, store.Options{}, withAuth(a), func(o *Options) { o.Metrics = m })
	h.setUp()
	wrong := map[string]string{"username": ownerName, "password": "not the passphrase at all"}
	// access's ratelimit.DefaultConfig refuses an address past 10 failed
	// attempts in 15 minutes.
	for i := range 10 {
		if r := h.request(http.MethodPost, "/api/auth/login/password", wrong, noKey); r.status != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: %d %s, want 401", i+1, r.status, r.raw)
		}
	}
	if got := h.scrape()[`grelot_auth_failures_total{reason="rate_limited"}`]; got != 0 {
		t.Fatalf("rate_limited %v after 10 refused passwords, want 0: a wrong password alone counts nothing", got)
	}

	r := h.request(http.MethodPost, "/api/auth/login/password", wrong, noKey)

	if r.status != http.StatusTooManyRequests || r.str("code") != "auth_rate_limited" {
		t.Fatalf("the 11th attempt: %d %s, want 429 auth_rate_limited", r.status, r.raw)
	}
	if got := h.scrape()[`grelot_auth_failures_total{reason="rate_limited"}`]; got != 1 {
		t.Errorf(`grelot_auth_failures_total{reason="rate_limited"} = %v, want 1`, got)
	}
}
