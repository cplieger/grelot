//go:build !windows

package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/auth"
	"github.com/cplieger/grelot/internal/auth/authtest"
	"github.com/cplieger/grelot/internal/store"
)

// apiKeys is access's own key list route, which the web UI's API keys
// section reads.
const apiKeys = "/api/auth/apikeys"

// listedKey is one row of access's key list, the fields these tests read.
type listedKey struct {
	Label string `json:"label"`
	ID    int64  `json:"id"`
}

// createKey mints a key labelled label through access's route from the
// signed-in browser.
func (h *harness) createKey(label string) *response {
	h.t.Helper()
	return h.request(http.MethodPost, apiKeys, map[string]any{"label": label, "scopes": []string{}}, noKey)
}

// listKeys is access's key list as the signed-in browser reads it.
func (h *harness) listKeys() []listedKey {
	h.t.Helper()
	r := h.request(http.MethodGet, apiKeys, nil, nil)
	if r.status != http.StatusOK {
		h.t.Fatalf("GET %s: %d %s", apiKeys, r.status, r.raw)
	}
	var out []listedKey
	if err := json.Unmarshal([]byte(r.raw), &out); err != nil {
		h.t.Fatalf("GET %s: decode: %v", apiKeys, err)
	}
	return out
}

// withBearer is a /v1 request's headers for session sending key.
func withBearer(session, key string) map[string]string {
	return map[string]string{headerSession: session, "Authorization": "Bearer " + key}
}

func TestKeys_aKeyMintedInTheWebUIOpensTheAgentAPIUntilItIsRevoked(t *testing.T) {
	h := newSignedIn(t)

	created := h.createKey("laptop")

	if created.status != http.StatusCreated {
		t.Fatalf("POST %s: %d %s, want 201", apiKeys, created.status, created.raw)
	}
	plaintext := created.str("key")
	if !strings.HasPrefix(plaintext, "grelot_") {
		t.Fatalf("key %q, want one starting grelot_", plaintext)
	}
	if r := h.request(http.MethodPost, "/v1/register", registerBody("laptop-dev", "6h"), withBearer(sessA, plaintext)); r.status != http.StatusCreated {
		t.Errorf("register with the new key: %d %s, want 201", r.status, r.raw)
	}
	if !h.signedInNow() {
		t.Error("the browser is signed out after the mint rotated its session")
	}
	list := h.request(http.MethodGet, apiKeys, nil, nil)
	if strings.Contains(list.raw, plaintext) {
		t.Errorf("GET %s carries the key itself: %s", apiKeys, list.raw)
	}
	details, _ := created.body["details"].(map[string]any)
	id, _ := details["id"].(float64)
	path := apiKeys + "/" + strconv.FormatInt(int64(id), 10)
	if r := h.request(http.MethodDelete, path, nil, noKey); r.status != http.StatusNoContent {
		t.Fatalf("DELETE %s: %d %s, want 204", path, r.status, r.raw)
	}
	if r := h.request(http.MethodGet, "/v1/version", nil, withBearer(sessA, plaintext)); r.status != http.StatusUnauthorized {
		t.Errorf("GET /v1/version with the revoked key: %d %s, want 401", r.status, r.raw)
	}
}

func TestKeys_theAdminSocketsKeysAreInTheWebUIsList(t *testing.T) {
	h := newSignedIn(t)
	if _, err := h.srv.auth.CreateKey(t.Context(), "desktop"); err != nil {
		t.Fatal(err)
	}
	if r := h.createKey("laptop"); r.status != http.StatusCreated {
		t.Fatalf("mint: %d %s", r.status, r.raw)
	}

	got := h.listKeys()

	labels := make([]string, len(got))
	for i := range got {
		labels[i] = got[i].Label
	}
	if strings.Join(labels, ",") != "desktop,laptop" {
		t.Errorf("listed labels %q, want desktop (the admin socket's) then laptop", labels)
	}
}

func TestKeys_aMintNeedsARecentSignIn(t *testing.T) {
	var ahead atomic.Int64
	a := testAuth(t, func(o *auth.Options) {
		o.Now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	})
	h := newHarnessWith(t, store.Options{}, withAuth(a))
	h.setUp()
	ahead.Store(int64(11 * time.Minute))

	r := h.createKey("laptop")

	if r.status != http.StatusForbidden || r.str("code") != "auth_recent_auth_required" {
		t.Errorf("a mint 11 minutes after sign-in: %d %s, want 403 auth_recent_auth_required", r.status, r.raw)
	}
	if got := h.listKeys(); len(got) != 0 {
		t.Errorf("keys after the refusal %+v, want none", got)
	}
}

func TestKeys_anAPIKeyCannotMintAnother(t *testing.T) {
	h := newSignedIn(t)
	key, err := h.machineKey()
	if err != nil {
		t.Fatal(err)
	}

	r := h.request(http.MethodPost, apiKeys, map[string]any{"label": "stolen", "scopes": []string{}},
		map[string]string{headerKey: "", "Authorization": "Bearer " + key, "Cookie": ""})

	if r.status != http.StatusForbidden || r.str("code") != "auth_session_required" {
		t.Errorf("POST %s with a machine key: %d %s, want 403 auth_session_required", apiKeys, r.status, r.raw)
	}
	for _, k := range h.listKeys() {
		if k.Label == "stolen" {
			t.Error("the machine key minted a key")
		}
	}
}

// A key reaches only the identities it registered: a leaked key exposes
// its own machine's agents alone.
func TestKeys_anotherKeyCannotActAsAnIdentity(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")
	stolen, err := h.srv.auth.CreateKey(t.Context(), "other")
	if err != nil {
		t.Fatal(err)
	}

	r := h.request(http.MethodGet, "/v1/me", nil, withBearer(sessA, stolen))

	if r.status != http.StatusForbidden || !strings.Contains(r.str("error"), "another machine's key") {
		t.Errorf("GET /v1/me for alpha's session with another key: %d %s, want 403 naming another machine's key", r.status, r.raw)
	}
	if own := h.as(sessA, http.MethodGet, "/v1/me", nil); own.status != http.StatusOK {
		t.Errorf("GET /v1/me for alpha with the key that registered it: %d %s, want 200", own.status, own.raw)
	}
	if r := h.request(http.MethodPost, "/v1/register", registerBody("beta", "6h"), withBearer(sessB, stolen)); r.status != http.StatusCreated {
		t.Errorf("a register of its own from the other key: %d %s, want 201", r.status, r.raw)
	}
}

// accessLines are the access log's lines for path, as httpserver.Logging
// writes them.
func (h *harness) accessLines(path string) []map[string]string {
	var out []map[string]string
	for _, r := range h.logs.lines(slog.LevelInfo, "http") {
		if a := attrs(r); a["path"] == path {
			out = append(out, a)
		}
	}
	return out
}

func TestKeys_eachAgentRequestsAccessLineNamesItsKeysLabel(t *testing.T) {
	h := newSignedIn(t)
	laptop, err := h.srv.auth.CreateKey(t.Context(), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	desk, err := h.srv.auth.CreateKey(t.Context(), "desk.2")
	if err != nil {
		t.Fatal(err)
	}

	// The newer key first, and the older one twice, so a label is read
	// both before and after the server has seen each key.
	h.request(http.MethodGet, "/v1/identities", nil, map[string]string{"Authorization": "Bearer " + desk})
	h.request(http.MethodGet, "/v1/version", nil, map[string]string{"Authorization": "Bearer " + laptop})
	h.request(http.MethodGet, "/v1/version", nil, map[string]string{"Authorization": "Bearer " + laptop})
	h.request(http.MethodGet, "/v1/threads", nil, map[string]string{"Authorization": "Bearer grelot_unknown"})
	h.request(http.MethodGet, "/api/conversations", nil, nil)

	for path, want := range map[string][]string{"/v1/identities": {"desk.2"}, "/v1/version": {"laptop", "laptop"}} {
		var got []string
		for _, line := range h.accessLines(path) {
			got = append(got, line["key"])
		}
		if !slices.Equal(got, want) {
			t.Errorf("the key of each access line of %s: %q, want %q", path, got, want)
		}
	}
	for _, path := range []string{"/v1/threads", "/api/conversations"} {
		lines := h.accessLines(path)
		if len(lines) != 1 {
			t.Fatalf("access lines of %s: %v, want one", path, lines)
		}
		if k, ok := lines[0]["key"]; ok {
			t.Errorf("the access line of %s names key=%q, want none: no key was admitted", path, k)
		}
	}
}

// registerWith registers alpha from sessA with key and returns the answer.
func (h *harness) registerWith(key string) *response {
	h.t.Helper()
	return h.request(http.MethodPost, "/v1/register", registerBody("alpha", "6h"), withBearer(sessA, key))
}

// mintKey mints a key labelled label in the signed-in browser and returns
// it with its ID.
func (h *harness) mintKey(label string) (key string, id int64) {
	h.t.Helper()
	r := h.createKey(label)
	if r.status != http.StatusCreated {
		h.t.Fatalf("mint %s: %d %s", label, r.status, r.raw)
	}
	details, _ := r.body["details"].(map[string]any)
	n, _ := details["id"].(float64)
	return r.str("key"), int64(n)
}

// recoverWithoutKeepingKeys runs the account recovery a recovery link
// opens, its body leaving keep_keys out, as the shared screen does by
// default.
func (h *harness) recoverWithoutKeepingKeys() {
	h.t.Helper()
	tok, err := h.srv.auth.Recovery(h.t.Context(), ownerName)
	if err != nil {
		h.t.Fatal(err)
	}
	if r := h.request(http.MethodPost, "/api/auth/recovery", map[string]string{"token": tok.Value}, noKey); r.status != http.StatusOK {
		h.t.Fatalf("recovery start: %d %s", r.status, r.raw)
	}
	if r := h.request(http.MethodPost, "/api/auth/recovery/password", map[string]string{"password": "another long test passphrase"}, noKey); r.status/100 != 2 {
		h.t.Fatalf("recovery password: %d %s", r.status, r.raw)
	}
}

// A machine's sessions keep their identities when its key goes away and a
// new one replaces it, however the old one went.
func TestKeys_aSessionKeepsItsIdentityUnderItsMachinesNewKey(t *testing.T) {
	for _, c := range []struct {
		replace func(h *harness, old string, oldID int64) string
		name    string
	}{
		{name: "the admin socket revokes it and creates another", replace: func(h *harness, _ string, _ int64) string {
			if err := h.srv.auth.RevokeKey(h.t.Context(), "laptop"); err != nil {
				h.t.Fatal(err)
			}
			return authtest.Key(h.t, h.srv.auth, "laptop")
		}},
		{name: "the web UI revokes it and mints another", replace: func(h *harness, _ string, oldID int64) string {
			if r := h.request(http.MethodDelete, apiKeys+"/"+strconv.FormatInt(oldID, 10), nil, noKey); r.status != http.StatusNoContent {
				h.t.Fatalf("web revoke: %d %s", r.status, r.raw)
			}
			key, _ := h.mintKey("laptop")
			return key
		}},
		{name: "a recovery deletes it and the socket creates another", replace: func(h *harness, old string, _ int64) string {
			h.recoverWithoutKeepingKeys()
			if r := h.request(http.MethodGet, "/v1/version", nil, withBearer(sessA, old)); r.status != http.StatusUnauthorized {
				h.t.Fatalf("the old key after the recovery: %d %s, want 401: the recovery kept it", r.status, r.raw)
			}
			return authtest.Key(h.t, h.srv.auth, "laptop")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newSignedIn(t)
			old, oldID := h.mintKey("laptop")
			if r := h.registerWith(old); r.status != http.StatusCreated {
				t.Fatalf("register under the first key: %d %s", r.status, r.raw)
			}

			key := c.replace(h, old, oldID)

			if r := h.registerWith(key); r.status != http.StatusOK || !strings.Contains(r.raw, `"alpha"`) {
				t.Errorf("the same session's register under the new key: %d %s, want 200 with alpha", r.status, r.raw)
			}
			if r := h.request(http.MethodGet, "/v1/me", nil, withBearer(sessA, key)); r.status != http.StatusOK || !strings.Contains(r.raw, `"alpha"`) {
				t.Errorf("GET /v1/me under the new key: %d %s, want 200 as alpha", r.status, r.raw)
			}
			other := authtest.Key(t, h.srv.auth, "desktop")
			if r := h.request(http.MethodGet, "/v1/me", nil, withBearer(sessA, other)); r.status != http.StatusForbidden {
				t.Errorf("GET /v1/me from another live key: %d %s, want 403: the identity is the new key's", r.status, r.raw)
			}
		})
	}
}

// A label names one key whichever route minted it.
func TestKeys_aLabelNamesOneKeyOnEveryMintRoute(t *testing.T) {
	h := newSignedIn(t)
	h.mintKey("laptop")
	if _, err := h.srv.auth.CreateKey(t.Context(), "desktop"); err != nil {
		t.Fatal(err)
	}

	// access refuses a label another unexpired key carries, after case
	// folding, with 409 auth_key_label_taken, and an empty one as a bad
	// request.
	for _, label := range []string{"laptop", "desktop", "LAPTOP"} {
		r := h.createKey(label)
		if r.status != http.StatusConflict || r.str("code") != "auth_key_label_taken" {
			t.Errorf("web mint of %q: %d %s, want 409 auth_key_label_taken", label, r.status, r.raw)
		}
	}
	if r := h.createKey(""); r.status != http.StatusBadRequest || r.str("code") != "auth_bad_request" {
		t.Errorf("web mint of an empty label: %d %s, want 400 auth_bad_request", r.status, r.raw)
	}
	if _, err := h.srv.auth.CreateKey(t.Context(), "laptop"); err == nil || !strings.Contains(err.Error(), "revoke or rotate it") {
		t.Errorf("socket create of the web-minted label: %v, want the label refused", err)
	}
	if err := h.srv.auth.RevokeKey(t.Context(), "laptop"); err != nil {
		t.Fatalf("revoke laptop: %v", err)
	}
	if got := h.listKeys(); len(got) != 1 || got[0].Label != "desktop" {
		t.Errorf("keys after revoking laptop %+v, want desktop alone", got)
	}
}
