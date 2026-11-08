//go:build !windows

package server

import (
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/cplieger/go/envx"
	"github.com/cplieger/go/pathinside"
	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/store"
)

// Config is the server's configuration, read from the environment once at
// startup.
type Config struct {
	Token     string
	PathRoot  string
	StorePath string
	// ProvidersDir is the folder of the provider files whose http doors
	// the server pushes through.
	ProvidersDir string
	// MaxBytes is the store's ceiling on the bytes in use.
	MaxBytes          int64
	ThreadTTL         time.Duration
	DormantTTL        time.Duration
	ReservationMaxTTL time.Duration
}

// The environment keys LoadConfig reads.
const (
	keyToken             envx.Key = "GRELOT_TOKEN" //nolint:gosec // an environment variable name, not a credential
	keyPathRoot          envx.Key = "GRELOT_PATH_ROOT"
	keyStorePath         envx.Key = "STORE_PATH"
	keyMaxBytes          envx.Key = "STORE_MAX_BYTES"
	keyThreadTTL         envx.Key = "THREAD_TTL"
	keyDormantTTL        envx.Key = "DORMANT_TTL"
	keyReservationMaxTTL envx.Key = "RESERVATION_MAX_TTL"
	keyProvidersDir      envx.Key = "PROVIDERS_DIR"
)

// The defaults of the optional keys.
const (
	DefaultStorePath = "/data/grelot.db"
	// DefaultProvidersDir holds no file in the image, so a server with no
	// folder mounted pushes to no host.
	DefaultProvidersDir = "/providers"
)

// LoadConfig reads the configuration from the environment. Its error names
// the key to fix and never a secret's value.
func LoadConfig() (Config, error) {
	c := Config{
		PathRoot:          protopath.DefaultRoot,
		StorePath:         DefaultStorePath,
		MaxBytes:          store.DefaultMaxBytes,
		ThreadTTL:         store.DefaultThreadTTL,
		DormantTTL:        store.DefaultDormantTTL,
		ReservationMaxTTL: store.DefaultReservationMaxTTL,
		ProvidersDir:      DefaultProvidersDir,
	}
	var errs []error
	token, err := envx.Secret(keyToken)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s (or %s_FILE) must hold the bearer token every client sends", keyToken, keyToken))
	}
	c.Token = token
	if v := envx.String(keyPathRoot); v != "" {
		c.PathRoot = v
	}
	if !protopath.IsCanonical(c.PathRoot) {
		errs = append(errs, fmt.Errorf("%s must be a clean absolute path such as /workspace", keyPathRoot))
	}
	if v := envx.String(keyStorePath); v != "" {
		c.StorePath = v
	}
	if v := envx.String(keyProvidersDir); v != "" {
		c.ProvidersDir = v
	}
	if !path.IsAbs(c.ProvidersDir) || !pathinside.IsCanonical(c.ProvidersDir) {
		errs = append(errs, fmt.Errorf("%s must be a clean absolute path such as /providers", keyProvidersDir))
	}
	errs = append(errs, c.readLimits()...)
	return c, errors.Join(errs...)
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
	}{{&c.ThreadTTL, keyThreadTTL}, {&c.DormantTTL, keyDormantTTL}, {&c.ReservationMaxTTL, keyReservationMaxTTL}} {
		switch v, ok, err := envx.DurationStrict(d.key); {
		case err != nil || (ok && v <= 0):
			errs = append(errs, fmt.Errorf("%s must be a positive duration such as %s=72h", d.key, d.key))
		case ok:
			*d.v = v
		}
	}
	return errs
}
