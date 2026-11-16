//go:build !windows

package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/access/accesstest"
	"github.com/cplieger/grelot/internal/accounts"
	"github.com/cplieger/grelot/internal/auth"
)

// failing fails its first requests with a transport error, as a provider
// that is down does, then sends the rest on.
type failing struct{ left atomic.Int32 }

func (f *failing) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.left.Add(-1) >= 0 {
		return nil, errors.New("the provider is down")
	}
	return http.DefaultTransport.RoundTrip(r)
}

func openDB(t *testing.T) *accounts.DB {
	t.Helper()
	db, err := accounts.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func idpOptions(idp *accesstest.IdP) *auth.Options {
	o := testAuthOptions()
	o.OIDC = auth.OIDC{Issuer: idp.Issuer(), ClientID: idp.ClientID(), ClientSecret: idp.ClientSecret(), Label: "Test ID"}
	return o
}

func TestNew_singleSignOnComesOnOnceItsProviderAnswers(t *testing.T) {
	idp := accesstest.NewIdP(t, accesstest.IdPOptions{})
	down := &failing{}
	down.left.Store(3)
	auth.SetDiscovery(t, &http.Client{Transport: down}, 200*time.Millisecond)

	a, err := auth.New(t.Context(), openDB(t), idpOptions(idp))
	if err != nil {
		t.Fatalf("New with its provider down: %v, want it to start", err)
	}
	t.Cleanup(func() { _ = a.Close(context.WithoutCancel(t.Context())) })

	if a.Service().OIDCProvider() != nil {
		t.Fatal("single sign-on is on before its provider answered")
	}
	deadline := time.Now().Add(10 * time.Second)
	for a.Service().OIDCProvider() == nil {
		if time.Now().After(deadline) {
			t.Fatal("single sign-on is still off 10 s after its provider came back")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestClose_endsTheDiscoveryRetries(t *testing.T) {
	idp := accesstest.NewIdP(t, accesstest.IdPOptions{})
	down := &failing{}
	down.left.Store(1 << 30)
	auth.SetDiscovery(t, &http.Client{Transport: down}, time.Hour)
	a, err := auth.New(t.Context(), openDB(t), idpOptions(idp))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	err = a.Close(ctx)

	if err != nil || ctx.Err() != nil {
		t.Errorf("Close = %v with ctx %v, want it to stop the hour-long retry wait at once", err, ctx.Err())
	}
}

func TestNew_refusesAProviderRegistrationAccessRefuses(t *testing.T) {
	o := testAuthOptions()
	o.OIDC = auth.OIDC{Issuer: "ftp://id.example.com", ClientID: "grelot", ClientSecret: "client-secret"}

	if _, err := auth.New(t.Context(), openDB(t), o); err == nil {
		t.Error("New with an ftp issuer succeeded, want a refusal")
	}
}

func TestOIDCLabel_namesTheConfiguredProviderOnly(t *testing.T) {
	down := &failing{}
	down.left.Store(1 << 30)
	auth.SetDiscovery(t, &http.Client{Transport: down}, time.Hour)
	for _, tc := range []struct {
		name string
		oidc auth.OIDC
		want string
	}{
		{"none configured", auth.OIDC{Label: "Home ID"}, ""},
		{"no label", auth.OIDC{Issuer: "https://id.example.com", ClientID: "grelot", ClientSecret: "client-secret"}, "single sign-on"},
		{"a label", auth.OIDC{Issuer: "https://id.example.com", ClientID: "grelot", ClientSecret: "client-secret", Label: "Home ID"}, "Home ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := testAuthOptions()
			o.OIDC = tc.oidc
			a, err := auth.New(t.Context(), openDB(t), o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = a.Close(context.WithoutCancel(t.Context())) })
			if got := a.OIDCLabel(); got != tc.want {
				t.Errorf("OIDCLabel() = %q, want %q", got, tc.want)
			}
			if got := a.Settings().OIDCConfigured; got != (tc.want != "") {
				t.Errorf("OIDCConfigured = %v, want %v", got, tc.want != "")
			}
		})
	}
}

func TestClientAddr_saysHowTheClientWasReached(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, tc := range []struct {
		name, remote, forwardedFor string
		via                        access.Via
		ip                         string
		known                      bool
	}{
		{"a direct client", "192.0.2.10:4000", "", access.ViaDirect, "192.0.2.10", true},
		{"a trusted proxy forwarding a client", "10.0.0.2:4000", "198.51.100.7", access.ViaProxy, "198.51.100.7", true},
		{"a trusted proxy forwarding no client", "10.0.0.2:4000", "", access.ViaProxyNoClient, "10.0.0.2", false},
		{"an untrusted peer forwarding", "192.0.2.10:4000", "198.51.100.7", access.ViaUntrustedForwarding, "192.0.2.10", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/auth/me", http.NoBody)
			r.RemoteAddr = tc.remote
			if tc.forwardedFor != "" {
				r.Header.Set("X-Forwarded-For", tc.forwardedFor)
			}
			c := auth.ClientAddr(r, proxies)
			if c.Via != tc.via {
				t.Errorf("Via = %v, want %v", c.Via, tc.via)
			}
			if got := c.IP().String(); got != tc.ip {
				t.Errorf("IP() = %s, want %s", got, tc.ip)
			}
			if c.ClientKnown() != tc.known {
				t.Errorf("ClientKnown() = %v, want %v", c.ClientKnown(), tc.known)
			}
		})
	}
	t.Run("a peer with no address is no known client", func(t *testing.T) {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/auth/me", http.NoBody)
		r.RemoteAddr = "@"
		if c := auth.ClientAddr(r, proxies); c.ClientKnown() {
			t.Errorf("ClientAddr = %+v, want no known client", c)
		}
	})
}
