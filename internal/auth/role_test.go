//go:build !windows

package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/access/account"
	"github.com/cplieger/grelot/internal/accounts"
	"github.com/cplieger/grelot/internal/auth"
	"github.com/cplieger/grelot/internal/wire"
)

// principals are one of each principal grelot meets, each authenticated
// by the service's own verifiers from a credential the product mints.
type principals struct {
	session, recovery, owner, agent, both, scopeless access.Principal
	// external carries a session's and a key's fields but comes from no
	// built-in verifier, as a custom verifier's or the bypass's would.
	external access.Principal
}

// sessionCookie is access's default session cookie name under its secure
// posture, which auth.New keeps.
var sessionCookie = "__Host-" + access.DefaultCookieConfig().Name

func authenticate(t *testing.T, a *auth.Auth, r *http.Request) access.Principal {
	t.Helper()
	p, err := a.Authenticator().Authenticate(r)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return p
}

func bearer(t *testing.T, a *auth.Auth, key string) access.Principal {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/version", http.NoBody)
	r.Header.Set("Authorization", "Bearer "+key)
	return authenticate(t, a, r)
}

func withCookie(t *testing.T, a *auth.Auth, token string, recovery bool) access.Principal {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/settings", http.NoBody)
	if recovery {
		r = r.WithContext(access.AllowRecoverySession(r.Context()))
	}
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	return authenticate(t, a, r)
}

// mintThrough mints a key with scopes through access itself, as a session
// minting through access's key route does: grelot's own CreateKey mints
// one scope.
func mintThrough(t *testing.T, a *auth.Auth, owner int64, label string, scopes ...string) string {
	t.Helper()
	created, err := a.Service().CreateAPIKey(t.Context(), account.OutOfBand("test"), owner, account.NewKey{Label: label, Scopes: scopes})
	if err != nil {
		t.Fatalf("mint %s with scopes %v: %v", label, scopes, err)
	}
	return created.Plaintext.Reveal()
}

func mint(t *testing.T, a *auth.Auth, label, scope string) string {
	t.Helper()
	key, err := a.CreateKey(t.Context(), label, scope)
	if err != nil {
		t.Fatalf("CreateKey(%s, %s): %v", label, scope, err)
	}
	return key
}

func newPrincipals(t *testing.T) (*auth.Auth, *principals) {
	t.Helper()
	a, _ := newAuth(t)
	tok, err := a.SetupToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	signed, err := a.Service().SetupWithPassword(t.Context(), client, account.SetupPassword{
		Token: access.NewSecret(tok.Value), Password: access.NewSecret(password), Username: ownerName,
	})
	if err != nil {
		t.Fatal(err)
	}
	ps := &principals{session: withCookie(t, a, signed.Token.Reveal(), false)}
	ps.owner = bearer(t, a, mint(t, a, "owner-key", wire.ScopeOwner))
	ps.agent = bearer(t, a, mint(t, a, "agent-key", wire.ScopeAgent))
	ps.both = bearer(t, a, mintThrough(t, a, signed.User.ID, "both-key", wire.ScopeAgent, wire.ScopeOwner))
	ps.scopeless = bearer(t, a, mintThrough(t, a, signed.User.ID, "scopeless-key"))
	rt, err := a.Recovery(t.Context(), ownerName)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := a.Service().StartRecovery(t.Context(), client, access.NewSecret(rt.Value))
	if err != nil {
		t.Fatal(err)
	}
	ps.recovery = withCookie(t, a, rec.Issued.Token.Reveal(), true)
	ps.external = access.Principal{
		User: ps.session.User, SessionID: ps.session.SessionID, AuthTime: time.Now(), Method: access.MethodAPIKey,
		KeyID: ps.owner.KeyID, Scopes: []string{wire.ScopeOwner, wire.ScopeAgent},
	}
	return a, ps
}

func TestRoleOf_givesEveryPrincipalOneRole(t *testing.T) {
	_, ps := newPrincipals(t)
	cases := []struct {
		name string
		p    access.Principal
		want auth.Role
	}{
		{"a full session", ps.session, auth.RoleOwner},
		{"a recovery session", ps.recovery, auth.RoleNone},
		{"an owner key", ps.owner, auth.RoleOwner},
		{"an agent key", ps.agent, auth.RoleAgent},
		{"a key with both scopes", ps.both, auth.RoleOwner},
		{"a key with no scope", ps.scopeless, auth.RoleAgent},
		{"a principal no built-in verifier made", ps.external, auth.RoleNone},
		{"the zero principal", access.Principal{}, auth.RoleNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := auth.RoleOf(c.p); got != c.want {
				t.Errorf("RoleOf = %v, want %v", got, c.want)
			}
		})
	}
}

func TestAllow_admitsTheRolesEachScopeNames(t *testing.T) {
	_, ps := newPrincipals(t)
	named := []struct {
		name string
		p    access.Principal
	}{
		{"session", ps.session}, {"recovery", ps.recovery}, {"owner key", ps.owner}, {"agent key", ps.agent},
		{"both-scope key", ps.both}, {"scopeless key", ps.scopeless}, {"external", ps.external},
	}
	admits := map[string][]string{
		wire.ScopeOwner:        {"session", "owner key", "both-scope key"},
		wire.ScopeAgent:        {"agent key", "scopeless key"},
		wire.ScopeAgentOrOwner: {"session", "owner key", "both-scope key", "agent key", "scopeless key"},
		"admin":                nil,
		"":                     nil,
	}
	for scope, want := range admits {
		for _, n := range named {
			t.Run(scope+" "+n.name, func(t *testing.T) {
				if got := auth.Allow(n.p, scope); got != slices.Contains(want, n.name) {
					t.Errorf("Allow(%s, %q) = %v, want %v", n.name, scope, got, !got)
				}
			})
		}
	}
}

func TestCreateKey_mintsTheOneScopeItIsGiven(t *testing.T) {
	a, _ := newAuth(t)
	setUp(t, a)
	for _, scope := range []string{wire.ScopeAgent, wire.ScopeOwner} {
		t.Run(scope, func(t *testing.T) {
			p := bearer(t, a, mint(t, a, scope+"-machine", scope))
			if !slices.Equal(p.Scopes, []string{scope}) {
				t.Errorf("the key's scopes = %v, want [%s]", p.Scopes, scope)
			}
		})
	}
	for _, scope := range []string{"", "admin", wire.ScopeAgentOrOwner} {
		t.Run("scope "+scope+" is refused", func(t *testing.T) {
			if key, err := a.CreateKey(t.Context(), "refused-"+scope, scope); err == nil {
				t.Errorf("CreateKey with scope %q = %q, want a refusal", scope, key)
			}
		})
	}
	if keys, err := a.Keys(t.Context()); err != nil || len(keys) != 2 {
		t.Errorf("keys after the refusals = %+v, %v; want the two minted", keys, err)
	}
}

func TestKeys_namesEachKeysScope(t *testing.T) {
	a, _ := newPrincipals(t)
	keys, err := a.Keys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, k := range keys {
		got[k.Label] = k.Scope
	}
	want := map[string]string{
		"owner-key": wire.ScopeOwner, "agent-key": wire.ScopeAgent, "both-key": wire.ScopeOwner, "scopeless-key": wire.ScopeAgent,
	}
	for label, scope := range want {
		if got[label] != scope {
			t.Errorf("key %s lists scope %q, want %q", label, got[label], scope)
		}
	}
}

func TestRotateKey_keepsTheKeysScope(t *testing.T) {
	a, _ := newAuth(t)
	setUp(t, a)
	mint(t, a, "laptop", wire.ScopeOwner)
	rotated, err := a.RotateKey(t.Context(), "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if p := bearer(t, a, rotated); !slices.Equal(p.Scopes, []string{wire.ScopeOwner}) {
		t.Errorf("the rotated key's scopes = %v, want [owner]", p.Scopes)
	}
	if keys, _ := a.Keys(t.Context()); len(keys) != 1 || keys[0].Scope != wire.ScopeOwner {
		t.Errorf("keys after the rotation = %+v, want laptop with scope owner", keys)
	}
}

func TestPutSettings_aKeyNeverChangesTheMethods(t *testing.T) {
	a, ps := newPrincipals(t)
	stored := a.Settings().Settings
	methods := stored
	methods.Methods.OIDC = false
	for _, n := range []struct {
		name string
		p    access.Principal
	}{{"owner key", ps.owner}, {"both-scope key", ps.both}} {
		t.Run(n.name, func(t *testing.T) {
			if err := a.PutSettings(t.Context(), &n.p, methods); !errors.Is(err, auth.ErrPersonReserved) {
				t.Errorf("PutSettings of the methods by an %s = %v, want ErrPersonReserved", n.name, err)
			}
			if got := a.Settings().Settings; got != stored {
				t.Errorf("settings after the refusal = %+v, want %+v", got, stored)
			}
		})
	}
	retention := stored
	retention.Retention = 5 * time.Hour
	if err := a.PutSettings(t.Context(), &ps.owner, retention); err != nil {
		t.Fatalf("PutSettings of the retention by an owner key = %v, want nil", err)
	}
	methods = a.Settings().Settings
	methods.Methods.OIDC = false
	if err := a.PutSettings(t.Context(), &ps.session, methods); err != nil {
		t.Errorf("PutSettings of the methods by a session just signed in = %v, want nil", err)
	}
	want := accounts.Settings{Revision: 2, Retention: 5 * time.Hour, Methods: methods.Methods}
	if got := a.Settings().Settings; got != want {
		t.Errorf("settings after both saves = %+v, want %+v", got, want)
	}
}
