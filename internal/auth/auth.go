//go:build !windows

// Package auth runs access's account service over the accounts file: the
// web UI's sign-in routes and session check, the settings that switch its
// methods, and the operator's commands (the setup token, a recovery token
// and the machines' API keys). It does not build on Windows, where the
// accounts file does not.
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
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/access/accesshttp"
	"github.com/cplieger/access/account"
	"github.com/cplieger/access/oidc"
	"github.com/cplieger/access/ratelimit"
	"github.com/cplieger/access/webauthn"
	"github.com/cplieger/grelot/internal/accounts"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Session lifetimes: a day unused, a month at most. A session's or key's
// last use is written at most once a minute.
const (
	idleTimeout      = 24 * time.Hour
	absTimeout       = 30 * 24 * time.Hour
	activityThrottle = time.Minute
)

// SetupTTL is how long a setup token stays valid.
const SetupTTL = 24 * time.Hour

// keyPrefix starts every machine key, so a leaked one is recognisable.
const keyPrefix = "grelot_"

// realm names grelot in every Bearer challenge.
const realm = "grelot"

// operator is the admin socket as access's events name it.
var operator = account.OutOfBand("admin socket")

// Auth is access's account service on one accounts file.
type Auth struct {
	db       *accounts.DB
	store    *accounts.Store
	limiter  *ratelimit.Limiter
	svc      *account.Service
	public   *url.URL
	rp       *webauthn.RelyingParty
	provider atomic.Pointer[oidc.Provider]
	log      *slog.Logger
	// stopDiscovery ends the single sign-on discovery retries, and
	// discovered closes once they have ended.
	stopDiscovery  context.CancelFunc
	discovered     chan struct{}
	now            func() time.Time
	oidcLabel      string
	routes         []accesshttp.Route
	putMu          sync.Mutex
	oidcConfigured bool
}

// Token is a one-time secret, the page that takes it and the end of its
// validity.
type Token struct {
	Expires time.Time
	Value   string
	Link    string
}

// Key is a machine's API key as the operator lists it; LastUsed is zero for
// a key never used.
type Key struct {
	Created  time.Time
	LastUsed time.Time
	Label    string
}

// Options are what New needs beside the accounts file.
type Options struct {
	Logger *slog.Logger
	// PublicURL is the origin browsers use, which names the passkeys'
	// relying party, the single sign-on callback and the links of tokens.
	// Required.
	PublicURL *url.URL
	// Errors writes every refusal of the sign-in routes. Required.
	Errors access.ErrorWriter
	// Now is the account service's clock; nil is time.Now.
	Now func() time.Time
	// OIDC is the single sign-on provider; the zero value is none.
	OIDC OIDC
	// TrustedProxies are the proxies whose X-Forwarded-For names the
	// client the rate limits count.
	TrustedProxies []netip.Prefix
	// IdleTimeout is how long a session may sit unused; zero is a day.
	IdleTimeout time.Duration
}

// New builds the service on db, its secrets sealing the device cookies and
// naming the failure records. Close stops it. A provider in o.OIDC is
// discovered before New returns or, when it cannot be reached, retried in
// the background; single sign-on is off until it answers.
func New(ctx context.Context, db *accounts.DB, o *Options) (*Auth, error) {
	if o.PublicURL == nil || o.Errors == nil {
		return nil, errors.New("auth: Options.PublicURL and Options.Errors are required")
	}
	logger := cmp.Or(o.Logger, slog.Default())
	idle := cmp.Or(o.IdleTimeout, idleTimeout)
	store, secrets := db.Access(), db.Secrets()
	trusted := slices.Clone(o.TrustedProxies)
	authn, err := access.New(store, access.Config{
		Logger: logger, Realm: realm, IdleTimeout: idle, AbsTimeout: absTimeout, ActivityThrottle: min(activityThrottle, idle/2),
		Cookie: access.DefaultCookieConfig(), ClientAddr: func(r *http.Request) access.ClientAddr { return clientAddr(r, trusted) },
	})
	if err != nil {
		return nil, err
	}
	rp, err := webauthn.New(webauthn.RPConfig{
		ID: o.PublicURL.Hostname(), DisplayName: "grelot", Origins: []string{o.PublicURL.String()},
	})
	if err != nil {
		return nil, err
	}
	hasher, err := access.NewHasher(access.HasherConfig{})
	if err != nil {
		return nil, err
	}
	devices, err := access.NewSealer(secrets.DeviceKey)
	if err != nil {
		return nil, err
	}
	pending, err := access.NewSealer(secrets.PendingKey)
	if err != nil {
		return nil, err
	}
	limits := ratelimit.DefaultConfig()
	limits.Devices, limits.Logger = devices, logger
	limiter, err := ratelimit.New(context.WithoutCancel(ctx), limits)
	if err != nil {
		return nil, err
	}
	a := &Auth{
		db: db, store: store, limiter: limiter, public: o.PublicURL, rp: rp, log: logger, now: o.Now,
		oidcLabel: o.OIDC.Label, oidcConfigured: o.OIDC.Configured(), discovered: make(chan struct{}),
	}
	if a.now == nil {
		a.now = time.Now
	}
	a.svc, err = account.New(ctx, account.Config{
		Store: store, Policy: account.SingleOwnerPolicy{},
		Authenticator: authn, Hasher: hasher, Limiter: limiter, Rules: a.rules,
		RP: a.relyingParty, OIDC: a.oidcProvider,
		FailureKey: secrets.FailureKey, Logger: logger, Now: o.Now,
	})
	if err != nil {
		return nil, errors.Join(err, limiter.Shutdown(ctx))
	}
	if a.routes, err = signInRoutes(a.svc, o.Errors, pending, logger); err != nil {
		return nil, errors.Join(err, a.svc.Shutdown(ctx), limiter.Shutdown(ctx))
	}
	if err := a.startDiscovery(ctx, o.OIDC); err != nil {
		a.stopDiscovery()
		return nil, errors.Join(err, a.svc.Shutdown(ctx), limiter.Shutdown(ctx))
	}
	return a, nil
}

// Close stops the single sign-on discovery, the service's sweep and the
// limiter, waiting for each or for ctx.
func (a *Auth) Close(ctx context.Context) error {
	a.stopDiscovery()
	select {
	case <-a.discovered:
	case <-ctx.Done():
	}
	return errors.Join(a.svc.Shutdown(ctx), a.limiter.Shutdown(ctx))
}

// SetupToken mints the token that creates the first account, valid for
// SetupTTL, and voids any earlier one. Once an account exists it refuses.
func (a *Auth) SetupToken(ctx context.Context) (Token, error) {
	expires := time.Now().Add(SetupTTL)
	tok, err := a.svc.IssueSetupToken(ctx, operator, SetupTTL)
	if errors.Is(err, account.ErrSetupComplete) {
		return Token{}, errors.New("setup is done: an account exists; to regain it, run grelot recovery <username>")
	}
	if err != nil {
		return Token{}, err
	}
	return Token{Value: tok.Reveal(), Expires: expires, Link: a.link("setup", tok.Reveal())}, nil
}

// Recovery mints a recovery token for the account named username, valid
// for access's out-of-band default of 15 minutes, voiding its earlier ones.
func (a *Auth) Recovery(ctx context.Context, username string) (Token, error) {
	unknown := fmt.Errorf("no account is named %q", username)
	name, err := access.NormalizeUsername(username)
	if err != nil {
		return Token{}, unknown
	}
	u, found, err := a.store.UserByUsername(ctx, name)
	switch {
	case err != nil:
		return Token{}, err
	case !found:
		return Token{}, unknown
	}
	issued, err := a.svc.IssueRecovery(ctx, operator, u.ID, 0)
	if errors.Is(err, account.ErrNotFound) {
		return Token{}, unknown
	}
	if err != nil {
		return Token{}, err
	}
	return Token{Value: issued.Plaintext.Reveal(), Expires: issued.ExpiresAt, Link: a.link("recover", issued.Plaintext.Reveal())}, nil
}

// CreateKey mints a key labelled label for the account and returns its
// plaintext, which nothing stores. access refuses an empty label and one
// another unexpired key carries.
func (a *Auth) CreateKey(ctx context.Context, label string) (string, error) {
	owner, err := a.owner(ctx)
	if err != nil {
		return "", err
	}
	created, err := a.svc.CreateAPIKey(ctx, operator, owner, account.NewKey{Label: label})
	if errors.Is(err, account.ErrKeyLabelTaken) {
		return "", fmt.Errorf("a key labelled %s exists: revoke or rotate it, or pick another label", label)
	}
	if err != nil {
		return "", err
	}
	return created.Plaintext.Reveal(), nil
}

// owner is the ID of the lowest-numbered enabled admin, who owns every
// machine key.
func (a *Auth) owner(ctx context.Context) (int64, error) {
	users, err := a.store.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	if len(users) == 0 {
		return 0, errors.New("no account exists yet: run grelot setup-token and create it first")
	}
	var owner int64
	for i := range users {
		u := &users[i]
		if u.Role == access.RoleAdmin && u.Enabled && (owner == 0 || u.ID < owner) {
			owner = u.ID
		}
	}
	if owner == 0 {
		return 0, errors.New("no enabled admin account exists to own the key")
	}
	return owner, nil
}

// Keys lists every API key, oldest first.
func (a *Auth) Keys(ctx context.Context) ([]Key, error) {
	listed, err := a.svc.AllAPIKeys(ctx, operator)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(listed, func(x, y account.ListedKey) int { return cmp.Compare(x.Key.ID, y.Key.ID) })
	out := make([]Key, len(listed))
	for i := range listed {
		k := &listed[i].Key
		out[i] = Key{Label: k.Label, Created: k.CreatedAt, LastUsed: k.LastUsedAt}
	}
	return out, nil
}

// labelled is the ID of the key labelled label, compared as access compares
// labels (NFC, then full case folding). An unexpired key wins over expired
// ones. Two of the same kind are an error, which only an accounts file
// written before access's label rule can hold.
func (a *Auth) labelled(ctx context.Context, label string) (int64, error) {
	listed, err := a.svc.AllAPIKeys(ctx, operator)
	if err != nil {
		return 0, err
	}
	want, now := labelKey(label), a.now()
	var live, expired []int64
	for i := range listed {
		k := &listed[i].Key
		switch {
		case labelKey(k.Label) != want:
		case !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt):
			expired = append(expired, k.ID)
		default:
			live = append(live, k.ID)
		}
	}
	ids := live
	if len(ids) == 0 {
		ids = expired
	}
	switch len(ids) {
	case 0:
		return 0, fmt.Errorf("no key is labelled %q", label)
	case 1:
		return ids[0], nil
	default:
		return 0, fmt.Errorf("%d keys are labelled %q: revoke each from the API keys section of the web UI's account page", len(ids), label)
	}
}

// labelKey is access's form of a key label (account/keylabel.go), so a
// label the operator types finds the key access refuses to duplicate.
func labelKey(label string) string { return cases.Fold().String(norm.NFC.String(label)) }

// RevokeKey deletes the key labelled label with the keys it minted and the
// tokens they issued.
func (a *Auth) RevokeKey(ctx context.Context, label string) error {
	id, err := a.labelled(ctx, label)
	if err != nil {
		return err
	}
	return a.svc.RevokeAnyAPIKey(ctx, operator, id)
}

// RotateKey gives the key labelled label a new secret and returns it, which
// nothing stores. The old secret works until the new one's first use, or
// for access's overlap at most, so a machine moves to it without a gap.
func (a *Auth) RotateKey(ctx context.Context, label string) (string, error) {
	id, err := a.labelled(ctx, label)
	if err != nil {
		return "", err
	}
	rotated, err := a.svc.RotateAPIKey(ctx, operator, id)
	if err != nil {
		return "", err
	}
	return rotated.Plaintext.Reveal(), nil
}
