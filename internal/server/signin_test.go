//go:build !windows

package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/access/accesstest"
	"github.com/cplieger/grelot/internal/accounts"
	"github.com/cplieger/grelot/internal/auth"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/webui/wire"
)

// testOrigin is the public URL every sign-in test serves at: the passkey
// relying party's origin and the single sign-on callback's.
const testOrigin = "http://localhost:9737"

const (
	ownerName     = "owner"
	ownerPassword = "a long enough test passphrase"
)

// testAuth is the server's sign-in over a fresh accounts file, edits
// applied to its options.
func testAuth(t *testing.T, edits ...func(*auth.Options)) *auth.Auth {
	t.Helper()
	a, _ := testAuthDB(t, edits...)
	return a
}

// testAuthDB is testAuth with its accounts file.
func testAuthDB(t *testing.T, edits ...func(*auth.Options)) (*auth.Auth, *accounts.DB) {
	t.Helper()
	db, err := accounts.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	public, _ := url.Parse(testOrigin)
	logger := slog.New(slog.DiscardHandler)
	o := &auth.Options{Logger: logger, PublicURL: public, Errors: AuthErrors(logger, obs.New())}
	for _, edit := range edits {
		edit(o)
	}
	a, err := auth.New(t.Context(), db, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.WithoutCancel(t.Context())) })
	return a, db
}

func withAuth(a *auth.Auth) func(*Options) { return func(o *Options) { o.Auth = a } }

// newSignedIn is a harness whose browser set up the first account with a
// password and is signed in to it.
func newSignedIn(t *testing.T, edits ...func(*Options)) *harness {
	t.Helper()
	h := newHarnessWith(t, store.Options{}, append([]func(*Options){withAuth(testAuth(t))}, edits...)...)
	h.setUp()
	return h
}

// setUp creates the first account through a setup token, as the setup
// page does, which signs the browser in.
func (h *harness) setUp() {
	h.t.Helper()
	tok, err := h.srv.auth.SetupToken(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	r := h.request(http.MethodPost, "/api/auth/setup/password",
		map[string]string{"token": tok.Value, "username": ownerName, "password": ownerPassword}, noKey)
	if r.status != http.StatusOK {
		h.t.Fatalf("setup: %d %s", r.status, r.raw)
	}
}

// signedInNow reports whether the browser's session opens the web API.
func (h *harness) signedInNow() bool {
	h.t.Helper()
	r := h.request(http.MethodGet, "/api/agents", nil, nil)
	if r.status != http.StatusOK && r.status != http.StatusUnauthorized {
		h.t.Fatalf("GET /api/agents: %d %s, want 200 or 401", r.status, r.raw)
	}
	return r.status == http.StatusOK
}

func (h *harness) passwordSignIn() *response {
	h.t.Helper()
	return h.request(http.MethodPost, "/api/auth/login/password",
		map[string]string{"username": ownerName, "password": ownerPassword}, noKey)
}

// ceremony begins a passkey ceremony at path from testOrigin and returns
// its ID and the options the browser hands the authenticator.
func (h *harness) ceremony(path string, body any) (id string, options []byte) {
	h.t.Helper()
	r := h.request(http.MethodPost, path, body, map[string]string{headerKey: "", "Origin": testOrigin})
	if r.status != http.StatusOK {
		h.t.Fatalf("POST %s: %d %s", path, r.status, r.raw)
	}
	var c struct {
		Ceremony  string          `json:"ceremony"`
		PublicKey json.RawMessage `json:"public_key"`
	}
	if err := json.Unmarshal([]byte(r.raw), &c); err != nil {
		h.t.Fatal(err)
	}
	return c.Ceremony, c.PublicKey
}

// finish completes the ceremony id at path with the authenticator's body.
func (h *harness) finish(path, id string, body []byte) *response {
	h.t.Helper()
	return h.request(http.MethodPost, path, json.RawMessage(body),
		map[string]string{headerKey: "", "Origin": testOrigin, "X-Auth-Ceremony": id})
}

// registerPasskey enrols device on the signed-in account.
func (h *harness) registerPasskey(device *accesstest.PasskeyDevice) {
	h.t.Helper()
	id, options := h.ceremony("/api/auth/passkeys/begin", nil)
	if r := h.finish("/api/auth/passkeys/finish", id, device.Register(options)); r.status != http.StatusCreated {
		h.t.Fatalf("passkey registration: %d %s", r.status, r.raw)
	}
}

func (h *harness) passkeySignIn(device *accesstest.PasskeyDevice) *response {
	h.t.Helper()
	id, options := h.ceremony("/api/auth/login/passkey/begin", nil)
	return h.finish("/api/auth/login/passkey/finish", id, device.Assert(options))
}

func newDevice(t *testing.T) *accesstest.PasskeyDevice {
	d := accesstest.NewPasskeyDevice(t)
	d.Origin = testOrigin
	return d
}

// putMethods replaces the sign-in methods, keeping the retention.
func (h *harness) putMethods(m wire.Methods) *response {
	h.t.Helper()
	return h.request(http.MethodPut, "/api/settings",
		wire.SettingsRequest{Methods: m, RetentionMS: (72 * time.Hour).Milliseconds()}, noKey)
}

// follow sends the browser to location on the harness's server, as a
// redirect would.
func (h *harness) follow(location string) *response {
	h.t.Helper()
	u, err := url.Parse(location)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.request(http.MethodGet, u.RequestURI(), nil, nil)
}

// authorize sends the browser to the identity provider at authURL, which
// approves at once, and returns the callback location it redirects to.
func authorize(t *testing.T, authURL string) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, authURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, testOrigin+"/api/auth/oidc/callback?") {
		t.Fatalf("the identity provider answered %s, Location %q; want a redirect to the callback", resp.Status, loc)
	}
	return loc
}

func TestSignIn_aSetupTokenCreatesTheFirstAccountOnce(t *testing.T) {
	h := newHarnessWith(t, store.Options{}, withAuth(testAuth(t)))
	if h.signedInNow() {
		t.Fatal("the web API answered before any account existed")
	}
	tok, err := h.srv.auth.SetupToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	r := h.request(http.MethodPost, "/api/auth/setup/password",
		map[string]string{"token": tok.Value, "username": ownerName, "password": ownerPassword}, noKey)

	if r.status != http.StatusOK || !h.signedInNow() {
		t.Fatalf("setup with the token: %d %s, and signed in afterwards: %v; want 200 and signed in", r.status, r.raw, h.signedInNow())
	}
	if _, err := h.srv.auth.SetupToken(t.Context()); err == nil {
		t.Error("a second setup token was minted once an account existed")
	}
	h.browser.forget()
	again := h.request(http.MethodPost, "/api/auth/setup/password",
		map[string]string{"token": tok.Value, "username": "second", "password": ownerPassword}, noKey)
	if again.status != http.StatusConflict || again.str("code") != "auth_setup_complete" || again.str("error") == "" {
		t.Errorf("a second setup: %d %s, want 409 auth_setup_complete in the web API's envelope", again.status, again.raw)
	}
}

func TestSignIn_aPasswordSignsInAndSigningOutEndsTheSession(t *testing.T) {
	h := newSignedIn(t)
	h.browser.forget()
	if h.signedInNow() {
		t.Fatal("a browser with no cookie reached the web API")
	}

	if r := h.passwordSignIn(); r.status != http.StatusOK || !h.signedInNow() {
		t.Fatalf("password sign-in: %d %s, want 200 and signed in", r.status, r.raw)
	}
	kept := cookiesOf(h.browser)
	if r := h.request(http.MethodPost, "/api/auth/logout", nil, noKey); r.status != http.StatusNoContent {
		t.Fatalf("sign out: %d %s, want 204", r.status, r.raw)
	}

	if h.signedInNow() {
		t.Error("the browser is still signed in after signing out")
	}
	h.browser.mu.Lock()
	h.browser.cookies = kept
	h.browser.mu.Unlock()
	if h.signedInNow() {
		t.Error("the signed-out session's cookie still opens the web API")
	}
}

// cookiesOf copies the browser's cookies.
func cookiesOf(b *browser) map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return maps.Clone(b.cookies)
}

func TestSignIn_aPasskeySetsUpTheAccountAndSignsIn(t *testing.T) {
	h := newHarnessWith(t, store.Options{}, withAuth(testAuth(t)))
	tok, err := h.srv.auth.SetupToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	device := newDevice(t)
	id, options := h.ceremony("/api/auth/setup/passkey/begin", map[string]string{"token": tok.Value, "username": ownerName})
	if r := h.finish("/api/auth/setup/passkey/finish", id, device.Register(options)); r.status != http.StatusOK {
		t.Fatalf("setup with a passkey: %d %s, want 200", r.status, r.raw)
	}
	h.browser.forget()

	r := h.passkeySignIn(device)

	if r.status != http.StatusOK || !h.signedInNow() {
		t.Errorf("passkey sign-in: %d %s, want 200 and signed in", r.status, r.raw)
	}
}

// idpAuth is a sign-in whose single sign-on is idp, labelled label.
func idpAuth(t *testing.T, idp *accesstest.IdP, label string) *auth.Auth {
	return testAuth(t, func(o *auth.Options) {
		o.OIDC = auth.OIDC{Issuer: idp.Issuer(), ClientID: idp.ClientID(), ClientSecret: idp.ClientSecret(), Label: label}
	})
}

func TestSignIn_singleSignOnLinksThenSignsIn(t *testing.T) {
	idp := accesstest.NewIdP(t, accesstest.IdPOptions{})
	h := newHarnessWith(t, store.Options{}, withAuth(idpAuth(t, idp, "Test ID")))
	if r := h.request(http.MethodGet, "/api/sign-in", nil, nil); r.status != http.StatusOK || r.str("oidc_label") != "Test ID" {
		t.Errorf("GET /api/sign-in with no session: %d %s, want 200 naming Test ID", r.status, r.raw)
	}
	h.setUp()
	start := h.request(http.MethodPost, "/api/auth/oidc/start/link", map[string]string{"redirect": "/settings"}, noKey)
	if start.status != http.StatusOK {
		t.Fatalf("link start: %d %s", start.status, start.raw)
	}
	linked := h.follow(authorize(t, start.str("url")))
	if loc := linked.header.Get("Location"); linked.status != http.StatusFound || !strings.HasPrefix(loc, "/settings") || strings.Contains(loc, "error") {
		t.Fatalf("link callback: %d, Location %q; want a redirect back to /settings", linked.status, loc)
	}
	if r := h.request(http.MethodPost, "/api/auth/logout", nil, noKey); r.status != http.StatusNoContent {
		t.Fatalf("sign out: %d %s", r.status, r.raw)
	}

	begin := h.request(http.MethodGet, "/api/auth/oidc/start/signin?redirect=/messages", nil, nil)
	if begin.status != http.StatusFound {
		t.Fatalf("sign-in start: %d %s, want 302 to the provider", begin.status, begin.raw)
	}
	done := h.follow(authorize(t, begin.header.Get("Location")))

	if loc := done.header.Get("Location"); done.status != http.StatusFound || loc != "/messages" || !h.signedInNow() {
		t.Errorf("sign-in callback: %d, Location %q, signed in %v; want 302 to /messages and signed in", done.status, loc, h.signedInNow())
	}
}

func TestSignIn_eachMethodIsRefusedOnceSwitchedOff(t *testing.T) {
	idp := accesstest.NewIdP(t, accesstest.IdPOptions{})
	h := newHarnessWith(t, store.Options{}, withAuth(idpAuth(t, idp, "Test ID")))
	h.setUp()
	device := newDevice(t)
	h.registerPasskey(device)

	t.Run("password", func(t *testing.T) {
		if r := h.putMethods(wire.Methods{Passkeys: true, OIDC: true}); r.status != http.StatusOK {
			t.Fatalf("switching passwords off: %d %s", r.status, r.raw)
		}
		h.browser.forget()
		if r := h.passwordSignIn(); r.status != http.StatusForbidden || r.str("code") != "auth_password_login_disabled" {
			t.Errorf("password sign-in: %d %s, want 403 auth_password_login_disabled", r.status, r.raw)
		}
		if r := h.passkeySignIn(device); r.status != http.StatusOK {
			t.Fatalf("passkey sign-in: %d %s, want 200", r.status, r.raw)
		}
	})
	t.Run("passkeys", func(t *testing.T) {
		if r := h.putMethods(wire.Methods{Password: true, OIDC: true}); r.status != http.StatusOK {
			t.Fatalf("switching passkeys off: %d %s", r.status, r.raw)
		}
		r := h.request(http.MethodPost, "/api/auth/login/passkey/begin", nil, map[string]string{headerKey: "", "Origin": testOrigin})
		if r.status != http.StatusBadRequest || r.str("code") != "auth_passkeys_unavailable" {
			t.Errorf("passkey sign-in start: %d %s, want 400 auth_passkeys_unavailable", r.status, r.raw)
		}
	})
	t.Run("single sign-on", func(t *testing.T) {
		if r := h.putMethods(wire.Methods{Password: true, Passkeys: true}); r.status != http.StatusOK {
			t.Fatalf("switching single sign-on off: %d %s", r.status, r.raw)
		}
		r := h.request(http.MethodGet, "/api/auth/oidc/start/signin", nil, nil)
		if r.status != http.StatusBadRequest || r.str("code") != "auth_oidc_unavailable" {
			t.Errorf("single sign-on start: %d %s, want 400 auth_oidc_unavailable", r.status, r.raw)
		}
	})
}

func TestSettings_aChangeLeavingNoWayToSignInIsRefused(t *testing.T) {
	t.Run("password off with no passkey", func(t *testing.T) {
		h := newSignedIn(t)

		r := h.putMethods(wire.Methods{Passkeys: true, OIDC: true})

		if r.status != http.StatusConflict || r.str("code") != "would_lock_out" {
			t.Errorf("switching off the only method used: %d %s, want 409 would_lock_out", r.status, r.raw)
		}
		if got := webGet[wire.Settings](h, "/api/settings"); !got.Methods.Password {
			t.Errorf("settings after the refusal %+v, want passwords still on", got)
		}
	})
	t.Run("password and passkeys off with a passkey", func(t *testing.T) {
		h := newSignedIn(t)
		h.registerPasskey(newDevice(t))

		r := h.putMethods(wire.Methods{OIDC: true})

		if r.status != http.StatusConflict || r.str("code") != "would_lock_out" {
			t.Errorf("switching off both methods used: %d %s, want 409 would_lock_out", r.status, r.raw)
		}
	})
}

func TestWeb_aFailingAccountsFileAnswers503(t *testing.T) {
	a, db := testAuthDB(t)
	h := newHarnessWith(t, store.Options{}, withAuth(a))
	h.setUp()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	r := h.request(http.MethodGet, "/api/agents", nil, nil)

	if r.status != http.StatusServiceUnavailable || r.str("code") != "auth_unavailable" || r.header.Get("Retry-After") != "5" {
		t.Errorf("GET /api/agents with the accounts file closed: %d %s Retry-After %q, want 503 auth_unavailable, Retry-After 5",
			r.status, r.raw, r.header.Get("Retry-After"))
	}
}

func TestSettings_aMethodChangeNeedsARecentSignIn(t *testing.T) {
	var ahead atomic.Int64
	a := testAuth(t, func(o *auth.Options) {
		o.Now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	})
	h := newHarnessWith(t, store.Options{}, withAuth(a))
	h.setUp()
	ahead.Store(int64(11 * time.Minute))

	if r := h.putMethods(wire.Methods{Password: true}); r.status != http.StatusForbidden || r.str("code") != "recent_auth_required" {
		t.Errorf("a method change 11 minutes after sign-in: %d %s, want 403 recent_auth_required", r.status, r.raw)
	}
	retention := wire.SettingsRequest{Methods: wire.Methods{Password: true, Passkeys: true, OIDC: true}, RetentionMS: (5 * time.Hour).Milliseconds()}
	if r := h.request(http.MethodPut, "/api/settings", retention, noKey); r.status != http.StatusOK {
		t.Errorf("a retention change 11 minutes after sign-in: %d %s, want 200", r.status, r.raw)
	}
	if r := h.request(http.MethodPost, "/api/auth/stepup/password", map[string]string{"password": ownerPassword}, noKey); r.status != http.StatusNoContent {
		t.Fatalf("step-up: %d %s, want 204", r.status, r.raw)
	}
	if r := h.putMethods(wire.Methods{Password: true}); r.status != http.StatusOK {
		t.Errorf("the method change after a step-up: %d %s, want 200", r.status, r.raw)
	}
}

func TestSettings_storeTheRetentionWithinItsBounds(t *testing.T) {
	h := newSignedIn(t)
	all := wire.Methods{Password: true, Passkeys: true, OIDC: true}
	// 2^58 ms plus 72 h is far past 90 days, though its nanoseconds wrap
	// a 64-bit product to exactly 72 h.
	wraps := int64(1<<58) + (72 * time.Hour).Milliseconds()
	for _, ms := range []int64{(30 * time.Minute).Milliseconds(), (91 * 24 * time.Hour).Milliseconds(), (time.Hour + time.Second).Milliseconds(), -1, wraps} {
		r := h.request(http.MethodPut, "/api/settings", wire.SettingsRequest{Methods: all, RetentionMS: ms}, noKey)
		if r.status != http.StatusBadRequest || r.str("code") != "invalid" {
			t.Errorf("retention_ms %d: %d %s, want 400 invalid", ms, r.status, r.raw)
		}
	}
	r := h.request(http.MethodPut, "/api/settings", wire.SettingsRequest{Methods: all, RetentionMS: (36 * time.Hour).Milliseconds()}, noKey)
	if r.status != http.StatusOK {
		t.Fatalf("retention 36h: %d %s", r.status, r.raw)
	}
	want := wire.Settings{
		Methods: all, RetentionMS: (36 * time.Hour).Milliseconds(),
		RetentionMinMS: time.Hour.Milliseconds(), RetentionMaxMS: (90 * 24 * time.Hour).Milliseconds(),
	}
	if got := webGet[wire.Settings](h, "/api/settings"); got != want {
		t.Errorf("settings %+v, want %+v", got, want)
	}
}

func TestWeb_anAPIKeyOrAForeignOriginIsRefused(t *testing.T) {
	h := newSignedIn(t)
	key, err := h.srv.auth.CreateKey(t.Context(), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("an API key", func(t *testing.T) {
		r := h.request(http.MethodGet, "/api/agents", nil, map[string]string{"Authorization": "Bearer " + key, "Cookie": ""})
		if r.status != http.StatusForbidden || r.str("code") != "auth_session_required" {
			t.Errorf("GET /api/agents with a machine key: %d %s, want 403 auth_session_required", r.status, r.raw)
		}
	})
	t.Run("a cross-site write", func(t *testing.T) {
		r := h.request(http.MethodPost, "/api/posts", wire.PostRequest{Subject: "hi", Body: "from elsewhere", To: wire.PostTarget{Announce: true}},
			map[string]string{"Sec-Fetch-Site": "cross-site"})
		if r.status != http.StatusForbidden || r.str("code") != "auth_cross_origin" {
			t.Errorf("a cross-site post: %d %s, want 403 auth_cross_origin", r.status, r.raw)
		}
		page := webGet[wire.ConversationsPage](h, "/api/conversations")
		for i := range page.Rows {
			if page.Rows[i].Last.Subject == "hi" {
				t.Error("the cross-site post was stored")
			}
		}
	})
}

func TestEvents_aStreamEndsWhenItsSessionLapses(t *testing.T) {
	a := testAuth(t, func(o *auth.Options) { o.IdleTimeout = 2 * time.Second })
	h := newHarnessWith(t, store.Options{}, withAuth(a))
	h.setUp()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.http.URL+"/api/events", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	h.browser.add(req)
	opened := time.Now()
	resp, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/events: %d", resp.StatusCode)
	}

	_, err = io.Copy(io.Discard, resp.Body)

	if err != nil || time.Since(opened) > 10*time.Second {
		t.Errorf("the stream ended after %v with %v, want it closed by the server within the 2 s session", time.Since(opened), err)
	}
}
