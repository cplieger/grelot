//go:build !windows

package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/cplieger/access"
	"github.com/cplieger/access/oidc"
	"github.com/cplieger/access/webauthn"
	"github.com/cplieger/go/envx"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/pathinside"
	"github.com/cplieger/grelot/internal/auth"
	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/store"
)

// Config is the server's configuration, read from the environment once at
// startup.
type Config struct {
	// PublicURL is the origin browsers reach the server at, with no path.
	PublicURL *url.URL
	// OIDC is the single sign-on provider; its zero value is none.
	OIDC      auth.OIDC
	PathRoot  string
	StorePath string
	// AccountsPath is the file of the settings and secrets a store reset
	// keeps.
	AccountsPath string
	// ProvidersDir is the folder of the provider files whose http doors
	// the server pushes through.
	ProvidersDir string
	// TrustedProxies are the reverse proxies whose X-Forwarded-For names
	// the client.
	TrustedProxies []netip.Prefix
	// MaxBytes is the store's ceiling on the bytes in use.
	MaxBytes          int64
	DormantTTL        time.Duration
	ReservationMaxTTL time.Duration
}

// The environment keys LoadConfig reads.
const (
	keyPathRoot          envx.Key = "GRELOT_PATH_ROOT"
	keyStorePath         envx.Key = "STORE_PATH"
	keyAccountsPath      envx.Key = "ACCOUNTS_PATH"
	keyMaxBytes          envx.Key = "STORE_MAX_BYTES"
	keyDormantTTL        envx.Key = "DORMANT_TTL"
	keyReservationMaxTTL envx.Key = "RESERVATION_MAX_TTL"
	keyProvidersDir      envx.Key = "PROVIDERS_DIR"
	keyListenAddr        envx.Key = "LISTEN_ADDR"
	keyOpsListenAddr     envx.Key = "OPS_LISTEN_ADDR"
	keyAdminSocket       envx.Key = "ADMIN_SOCKET"
	keyPublicURL         envx.Key = "PUBLIC_URL"
	keyTrustedProxies    envx.Key = "TRUSTED_PROXIES"
	keyOIDCIssuer        envx.Key = "OIDC_ISSUER"
	keyOIDCClientID      envx.Key = "OIDC_CLIENT_ID"
	keyOIDCClientSecret  envx.Key = "OIDC_CLIENT_SECRET" //nolint:gosec // an environment variable name, not a credential
	keyOIDCLabel         envx.Key = "OIDC_LABEL"
)

const defaultAdminSocket = "/data/admin.sock"

// maxSocketPath is the longest path a Unix socket address holds on both
// server systems: sun_path is 104 bytes on macOS, its NUL included (108 on
// Linux).
const maxSocketPath = 103

// AdminSocket is the path of the admin socket the operator's commands dial:
// ADMIN_SOCKET cut as ListenAddr cuts, or /data/admin.sock. It reads no
// other setting, so those commands need no token. The error names the key
// when the path is not absolute, names no file or is too long for a socket.
func AdminSocket() (string, error) {
	p := cmp.Or(strings.TrimFunc(envx.String(keyAdminSocket), invisible), defaultAdminSocket)
	switch {
	case !filepath.IsAbs(p) || !namesFile(p):
		return "", fmt.Errorf("%s must be the absolute path of a socket file, such as %s", keyAdminSocket, defaultAdminSocket)
	case len(p) > maxSocketPath:
		return "", fmt.Errorf("%s is %d bytes long, over the %d a socket path holds", keyAdminSocket, len(p), maxSocketPath)
	}
	return p, nil
}

// The ports the image exposes.
const (
	defaultListenAddr    = ":9737"
	defaultOpsListenAddr = ":9738"
)

// ListenAddr is the address of the API: LISTEN_ADDR with its edge spaces
// and invisible runes cut, or :9737 when nothing is left.
func ListenAddr() string {
	return cmp.Or(strings.TrimFunc(envx.String(keyListenAddr), invisible), defaultListenAddr)
}

// OpsListenAddr is the address of /healthz and /metrics, which health
// probes: OPS_LISTEN_ADDR cut as ListenAddr cuts, or :9738. It reads no
// other setting, so health needs no token.
func OpsListenAddr() string {
	return cmp.Or(strings.TrimFunc(envx.String(keyOpsListenAddr), invisible), defaultOpsListenAddr)
}

// CheckListenAddrs refuses an operations address on the API's port, which
// would either fail to bind or put /metrics wherever the API is reachable.
// Ports compare as the numbers net.Listen resolves, so 09737 is 9737. Two
// port-0 addresses pass, as does one with no port or a port that does not
// resolve, which the listen itself refuses.
func CheckListenAddrs(api, ops string) error {
	apiPort, aok := listenPort(api)
	opsPort, ook := listenPort(ops)
	if !aok || !ook || apiPort != opsPort || apiPort == 0 {
		return nil
	}
	return fmt.Errorf("%s must name another port than %s (%d): /healthz and /metrics stay off the API's port",
		keyOpsListenAddr, keyListenAddr, apiPort)
}

func listenPort(addr string) (int, bool) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	n, err := net.DefaultResolver.LookupPort(context.Background(), "tcp", port)
	return n, err == nil
}

// invisible reports a rune a pasted value can carry that net.Listen cannot
// resolve, where strings.TrimSpace keeps zero-width runes.
func invisible(r rune) bool { return r == ' ' || !unicode.IsPrint(r) }

// The defaults of the optional keys.
const (
	defaultStorePath    = "/data/grelot.db"
	defaultAccountsPath = "/data/accounts.db"
	// defaultProvidersDir holds no file in the image, so a server with no
	// folder mounted pushes to no host.
	defaultProvidersDir = "/providers"
)

// LoadConfig reads the configuration from the environment. Its error names
// the key to fix and never a secret's value.
func LoadConfig() (Config, error) {
	c := Config{
		PathRoot:          protopath.DefaultRoot,
		StorePath:         defaultStorePath,
		AccountsPath:      defaultAccountsPath,
		MaxBytes:          store.DefaultMaxBytes,
		DormantTTL:        store.DefaultDormantTTL,
		ReservationMaxTTL: store.DefaultReservationMaxTTL,
		ProvidersDir:      defaultProvidersDir,
	}
	var errs []error
	if v := envx.String(keyPathRoot); v != "" {
		c.PathRoot = v
	}
	if !protopath.IsCanonical(c.PathRoot) {
		errs = append(errs, fmt.Errorf("%s must be a clean absolute path such as /workspace", keyPathRoot))
	}
	if v := envx.String(keyStorePath); v != "" {
		c.StorePath = v
	}
	if !namesFile(c.StorePath) {
		errs = append(errs, fmt.Errorf("%s must name the database file, such as %s", keyStorePath, defaultStorePath))
	}
	if v := envx.String(keyAccountsPath); v != "" {
		c.AccountsPath = v
	}
	switch {
	case !namesFile(c.AccountsPath):
		errs = append(errs, fmt.Errorf("%s must name the accounts file, such as %s", keyAccountsPath, defaultAccountsPath))
	case filepath.Clean(c.AccountsPath) == filepath.Clean(c.StorePath):
		errs = append(errs, fmt.Errorf("%s must differ from %s: a store reset must not reach the accounts", keyAccountsPath, keyStorePath))
	}
	if v := envx.String(keyProvidersDir); v != "" {
		c.ProvidersDir = v
	}
	if !path.IsAbs(c.ProvidersDir) || !pathinside.IsCanonical(c.ProvidersDir) {
		errs = append(errs, fmt.Errorf("%s must be a clean absolute path such as /providers", keyProvidersDir))
	}
	errs = append(errs, c.readLimits()...)
	errs = append(errs, c.readSignIn()...)
	return c, errors.Join(errs...)
}

// readSignIn reads what the web UI's sign-in needs into c.
func (c *Config) readSignIn() []error {
	var errs []error
	u, err := publicURL(envx.String(keyPublicURL))
	if err != nil {
		errs = append(errs, err)
	}
	c.PublicURL = u
	proxies, invalid := httpserver.ParseCIDRs(strings.Split(envx.String(keyTrustedProxies), ","))
	if len(invalid) > 0 {
		errs = append(errs, fmt.Errorf("%s must list the reverse proxies as addresses or CIDRs separated by commas, for example 172.18.0.0/16; %.64q is neither", keyTrustedProxies, invalid[0]))
	}
	c.TrustedProxies = proxies
	o, err := readOIDC(u)
	if err != nil {
		errs = append(errs, err)
	}
	c.OIDC = o
	return errs
}

// publicURL is raw as the origin browsers use: https, or http to
// localhost, with a domain name for a host, since a passkey's relying
// party cannot be an IP address.
func publicURL(raw string) (*url.URL, error) {
	refused := fmt.Errorf("%s must be the https origin browsers reach grelot at, such as https://grelot.example.com, or http://localhost:9737 on this machine", keyPublicURL)
	u, err := url.Parse(strings.TrimSpace(raw))
	switch {
	case err != nil, u.Host == "", u.User != nil, u.RawQuery != "", u.Fragment != "", strings.Trim(u.Path, "/") != "":
		return nil, refused
	case u.Scheme != "https" && (u.Scheme != "http" || u.Hostname() != "localhost"):
		return nil, refused
	}
	if err := webauthn.ValidateRPID(u.Hostname()); err != nil {
		return nil, fmt.Errorf("%w; its host must be a lowercase domain name, not an IP address", refused)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

var oidcKeys = []envx.Key{keyOIDCIssuer, keyOIDCClientID, keyOIDCClientSecret}

// readOIDC reads the single sign-on provider: all three keys or none, the
// client secret alone readable from a _FILE. Its callback is under public,
// nil when PUBLIC_URL is refused.
func readOIDC(public *url.URL) (auth.OIDC, error) {
	var missing []string
	for _, k := range oidcKeys {
		if envx.String(k) == "" && (k != keyOIDCClientSecret || envx.String(k+"_FILE") == "") {
			missing = append(missing, string(k))
		}
	}
	switch len(missing) {
	case len(oidcKeys):
		return auth.OIDC{}, nil
	case 0:
	default:
		return auth.OIDC{}, fmt.Errorf("single sign-on needs %s, %s and %s (or %s_FILE); %s unset",
			keyOIDCIssuer, keyOIDCClientID, keyOIDCClientSecret, keyOIDCClientSecret, strings.Join(missing, " and "))
	}
	secret, err := envx.Secret(keyOIDCClientSecret)
	if err != nil {
		return auth.OIDC{}, fmt.Errorf("%s (or %s_FILE) could not be read: %w", keyOIDCClientSecret, keyOIDCClientSecret, err)
	}
	o := auth.OIDC{
		Issuer: strings.TrimSpace(envx.String(keyOIDCIssuer)), ClientID: strings.TrimSpace(envx.String(keyOIDCClientID)),
		ClientSecret: secret, Label: strings.TrimSpace(envx.String(keyOIDCLabel)),
	}
	if public == nil {
		return o, nil
	}
	err = oidc.Config{
		IssuerURL: o.Issuer, ClientID: o.ClientID, ClientSecret: access.NewSecret(secret),
		RedirectURL: auth.CallbackURL(public),
	}.Validate()
	if err != nil {
		return auth.OIDC{}, fmt.Errorf("%s must be the provider's https issuer URL with no query: %w", keyOIDCIssuer, err)
	}
	return o, nil
}

// retiredKeys are the settings the server no longer reads, each with what
// replaced it.
var retiredKeys = []struct {
	key  envx.Key
	text string
}{
	{"THREAD_TTL", "THREAD_TTL is no longer read: set the history retention in Settings"},
	{"GRELOT_TOKEN", "GRELOT_TOKEN is no longer read: each machine sends its own key, from grelot key create"},
	{"GRELOT_TOKEN_FILE", "GRELOT_TOKEN_FILE is no longer read: each machine sends its own key, from grelot key create"},
}

// RetiredSettings returns one line for each retired setting the environment
// still sets, for the caller to log; the server starts regardless.
func RetiredSettings() []string {
	var out []string
	for _, r := range retiredKeys {
		if envx.String(r.key) != "" {
			out = append(out, r.text)
		}
	}
	return out
}

// namesFile is bolttable's own rule for a database path, checked here so a
// bad STORE_PATH or ACCOUNTS_PATH fails as a config error naming the key: a path ending in a
// separator, "." or ".." names no file.
func namesFile(p string) bool {
	base := filepath.Base(p)
	return p != "" && !os.IsPathSeparator(p[len(p)-1]) && base != "." && base != ".."
}

// readLimits reads the store's size and lifetime settings into c.
func (c *Config) readLimits() []error {
	var errs []error
	switch n, ok, err := envx.IntStrict(keyMaxBytes); {
	case err != nil || (ok && n <= 0):
		errs = append(errs, fmt.Errorf("%s must be a positive number of bytes", keyMaxBytes))
	case ok:
		c.MaxBytes = int64(n)
	}
	for _, d := range []struct {
		v   *time.Duration
		key envx.Key
	}{{&c.DormantTTL, keyDormantTTL}, {&c.ReservationMaxTTL, keyReservationMaxTTL}} {
		switch v, ok, err := envx.DurationStrict(d.key); {
		case err != nil || (ok && v <= 0):
			errs = append(errs, fmt.Errorf("%s must be a positive duration such as %s=72h", d.key, d.key))
		case ok:
			*d.v = v
		}
	}
	return errs
}
