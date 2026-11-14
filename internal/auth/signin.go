//go:build !windows

package auth

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/access/accesshttp"
	"github.com/cplieger/access/account"
	"github.com/cplieger/access/oidc"
	"github.com/cplieger/access/webauthn"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/accounts"
)

// The sign-in routes' paths, and the page the single sign-on callback
// sends a browser to when it does not sign in.
const (
	Prefix     = "/api/auth"
	oidcPrefix = Prefix + "/oidc"
	signInPage = "/sign-in"
)

// OIDC is a single sign-on provider's registration; Label names it on the
// sign-in page, "single sign-on" when empty.
type OIDC struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	Label        string
}

// configured reports whether o names a provider.
func (o *OIDC) configured() bool { return o.Issuer != "" }

// CallbackURL is the single sign-on callback under public, which the
// provider's registration must list.
func CallbackURL(public *url.URL) string { return public.String() + oidcPrefix + "/callback" }

// Refusals of PutSettings.
var (
	// ErrRecentAuth: the sign-in methods change only under a sign-in or
	// step-up of the last 10 minutes.
	ErrRecentAuth = errors.New("auth: a recent sign-in is required")
	// ErrWouldLockOut: the change leaves the signed-in account no method
	// it can sign in with.
	ErrWouldLockOut = errors.New("auth: the change leaves no way to sign in")
)

func signInRoutes(svc *account.Service, errs access.ErrorWriter, pending *access.Sealer, logger *slog.Logger) ([]accesshttp.Route, error) {
	h, err := accesshttp.New(accesshttp.Config{
		Account: svc, Errors: errs, PendingSealer: pending, Logger: logger,
		Prefix: Prefix, OIDCPrefix: oidcPrefix, SignInPath: signInPage,
	})
	if err != nil {
		return nil, err
	}
	return h.Routes(), nil
}

// Routes are access's routes under Prefix (sign-in, account, sessions
// and API keys), for the server's mux.
func (a *Auth) Routes() []accesshttp.Route { return slices.Clone(a.routes) }

// Authenticator is access's request authenticator, for the server's gate.
func (a *Auth) Authenticator() *access.Authenticator { return a.svc.Authenticator() }

func clientAddr(r *http.Request, trusted []netip.Prefix) access.ClientAddr {
	return clientVia(httpserver.ResolveClient(r, trusted...))
}

// clientVia maps the resolver's answer as access's docs/configuration.md
// gives it; ViaDirect stays last, or a trusted proxy that forwards no
// client reads as a direct, often loopback, client.
func clientVia(res httpserver.ClientResolution) access.ClientAddr {
	c := access.ClientAddr{Peer: res.Peer}
	switch {
	case res.Forwarded:
		c.Via, c.Forwarded = access.ViaProxy, res.IP
	case res.PeerTrusted:
		c.Via = access.ViaProxyNoClient
	case res.UntrustedForwarding:
		c.Via = access.ViaUntrustedForwarding
	default:
		c.Via = access.ViaDirect
	}
	return c
}

// rulesFor is access's rules with the methods m leaves on.
func rulesFor(m accounts.Methods) account.Rules {
	return account.Rules{KeyPrefix: keyPrefix, DisablePasswordLogin: !m.Password}
}

func (a *Auth) rules() account.Rules { return rulesFor(a.db.Current().Methods) }

// relyingParty and oidcProvider are nil while their method is off, which
// is how access turns passkeys and single sign-on off.
func (a *Auth) relyingParty() *webauthn.RelyingParty {
	if !a.db.Current().Methods.Passkeys {
		return nil
	}
	return a.rp
}

func (a *Auth) oidcProvider() *oidc.Provider {
	if !a.db.Current().Methods.OIDC {
		return nil
	}
	return a.provider.Load()
}

// The waits between single sign-on discovery attempts double from
// discoveryRetry to discoveryRetryMax; discoveryFirst bounds the attempt
// New waits for. oidcClient is the provider's HTTP client, nil being
// access's default.
var (
	discoveryRetry    = 30 * time.Second
	discoveryRetryMax = 10 * time.Minute
	discoveryFirst    = 10 * time.Second
	oidcClient        *http.Client
)

// startDiscovery discovers o's provider once, then in the background until
// it answers. A registration access refuses is New's error.
func (a *Auth) startDiscovery(ctx context.Context, o OIDC) error {
	ctx, a.stopDiscovery = context.WithCancel(context.WithoutCancel(ctx))
	if !o.configured() {
		close(a.discovered)
		return nil
	}
	cfg := oidc.Config{
		IssuerURL: o.Issuer, ClientID: o.ClientID, ClientSecret: access.NewSecret(o.ClientSecret),
		RedirectURL: CallbackURL(a.public), HTTPClient: oidcClient,
	}
	first, cancel := context.WithTimeout(ctx, discoveryFirst)
	err := a.discover(first, &cfg)
	cancel()
	switch {
	case err == nil:
		close(a.discovered)
		return nil
	case errors.Is(err, oidc.ErrConfigInvalid):
		close(a.discovered)
		return fmt.Errorf("single sign-on: %w", err)
	}
	go func() {
		defer close(a.discovered)
		for wait := discoveryRetry; ; wait = min(2*wait, discoveryRetryMax) {
			a.log.Warn("single sign-on is off until its provider answers", "issuer", cfg.IssuerURL, "error", err.Error(), "retry_in", wait.String())
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			if err = a.discover(ctx, &cfg); err == nil || ctx.Err() != nil {
				return
			}
		}
	}()
	return nil
}

func (a *Auth) discover(ctx context.Context, cfg *oidc.Config) error {
	p, err := oidc.NewProvider(ctx, *cfg)
	if err != nil {
		return err
	}
	a.provider.Store(p)
	a.log.Info("single sign-on ready", "issuer", cfg.IssuerURL)
	return nil
}

// link is the page under the public URL that takes token, the token in
// its fragment so it never reaches a log.
func (a *Auth) link(page, token string) string {
	return a.public.JoinPath(page).String() + "#token=" + url.QueryEscape(token)
}

const defaultOIDCLabel = "single sign-on"

// OIDCLabel names the single sign-on provider on the sign-in page; empty
// when none is configured.
func (a *Auth) OIDCLabel() string {
	if !a.oidcConfigured {
		return ""
	}
	return cmp.Or(a.oidcLabel, defaultOIDCLabel)
}

// Settings are the stored settings and whether single sign-on can be on.
type Settings struct {
	accounts.Settings
	// OIDCConfigured reports a provider in the environment; without one,
	// the single sign-on switch does nothing.
	OIDCConfigured bool
}

// Settings returns the stored settings.
func (a *Auth) Settings() Settings {
	return Settings{Settings: a.db.Current(), OIDCConfigured: a.oidcConfigured}
}

// PutSettings stores s for the session p. A change of method needs a
// recent sign-in (ErrRecentAuth) and must leave p's account a method it
// can sign in with (ErrWouldLockOut); a retention out of bounds is
// accounts.ErrInvalid. Calls run one at a time, so two changes never both
// pass the lock-out check.
func (a *Auth) PutSettings(ctx context.Context, p *access.Principal, s accounts.Settings) error {
	a.putMu.Lock()
	defer a.putMu.Unlock()
	if s.Methods != a.db.Current().Methods {
		if err := a.svc.RequireRecentAuth(*p); err != nil {
			if errors.Is(err, access.ErrRecentAuthRequired) {
				return ErrRecentAuth
			}
			return err
		}
		avail, err := a.svc.UsableMethods(ctx, a.store, p.User.ID, a.methodState(s.Methods))
		if err != nil {
			return err
		}
		if !access.CanDisableMethod("", avail) {
			return ErrWouldLockOut
		}
	}
	return a.db.Put(ctx, s)
}

// methodState is what access counts a user's methods under once m is
// stored.
func (a *Auth) methodState(m accounts.Methods) account.MethodState {
	st := account.MethodState{Rules: rulesFor(m)}
	if m.Passkeys {
		st.RPID = a.rp.ID()
	}
	if p := a.provider.Load(); m.OIDC && p != nil {
		st.OIDCIssuer = p.Issuer()
	}
	return st
}
