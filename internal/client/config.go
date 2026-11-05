// Package client is the grelot command line's core: its config, the API
// client every verb and hook sends through, the provider a shell runs
// under, and the status line printed before every answer.
package client

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/cplieger/go/yamlenv"
	"github.com/cplieger/grelot/internal/canon"
	"github.com/cplieger/grelot/internal/statefile"
)

// EnvTokenFile names a file holding the bearer token, which wins over the
// config file's token.
const EnvTokenFile = "GRELOT_TOKEN_FILE" //nolint:gosec // an environment variable name, not a credential

// maxConfigFile bounds the config and token file reads.
const maxConfigFile = 64 << 10

// Config is the client config file, config.yaml under <user config
// dir>/grelot. Repos maps a normalized remote to the repo name its paths
// use; every key is normalized and every value one path component.
type Config struct {
	Repos    map[string]string `yaml:"repos"`
	Server   string            `yaml:"server"`
	Token    string            `yaml:"token"`
	Host     string            `yaml:"host"`
	Provider string            `yaml:"provider"`
}

// Dir is grelot's folder under the user config dir.
func Dir(configDir string) string { return filepath.Join(configDir, "grelot") }

// ConfigFile is the client config's path under the user config dir.
func ConfigFile(configDir string) string { return filepath.Join(Dir(configDir), "config.yaml") }

// LoadConfig reads the client config under configDir, takes the token from
// the file EnvTokenFile names when lookup finds it set, and defaults Host
// to hostname's answer. Every error is one line naming the file and the fix.
func LoadConfig(configDir string, lookup func(string) (string, bool), hostname func() (string, error)) (*Config, error) {
	file := ConfigFile(configDir)
	info, err := os.Stat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("grelot has no client config: write %s with server: <url> and token: <token>; run grelot help for the keys", file)
	}
	if err != nil {
		return nil, fmt.Errorf("grelot cannot read its client config %s: %w", file, err)
	}
	data, err := statefile.Read(file, maxConfigFile)
	if err != nil {
		return nil, fmt.Errorf("grelot cannot read its client config %s: %w", file, err)
	}
	cfg := &Config{}
	unresolved, err := yamlenv.Load(data, cfg, func(string) bool { return true },
		yamlenv.WithLookupEnv(func(string) (string, bool) { return "", false }),
		yamlenv.WithSanitizeOptions(yamlenv.WithUnknownKeyEcho(true)))
	if err != nil {
		return nil, fmt.Errorf("client config %s: %w", file, err)
	}
	if len(unresolved) > 0 {
		return nil, fmt.Errorf("client config %s: environment references are not read: ${%s}; write the value, or the token's file in %s",
			file, strings.Join(unresolved, "}, ${"), EnvTokenFile)
	}
	if err := checkMode(info, cfg.Token != ""); err != nil {
		return nil, fmt.Errorf("client config %s %w; run chmod 600 %s", file, err, file)
	}
	if err := cfg.finish(lookup, hostname); err != nil {
		return nil, fmt.Errorf("client config %s: %w", file, err)
	}
	return cfg, nil
}

func (c *Config) finish(lookup func(string) (string, bool), hostname func() (string, error)) error {
	server, err := checkServer(c.Server)
	if err != nil {
		return err
	}
	c.Server = server
	if f, ok := lookup(EnvTokenFile); ok && f != "" {
		data, err := statefile.Read(f, maxConfigFile)
		if err != nil || data == nil {
			return fmt.Errorf("the token file %s that %s names cannot be read", f, EnvTokenFile)
		}
		c.Token = string(data)
	}
	c.Token = strings.TrimSpace(c.Token)
	if c.Token == "" {
		return fmt.Errorf("token is missing: set token: <token>, or %s to a file holding it", EnvTokenFile)
	}
	if c.Host = strings.TrimSpace(c.Host); c.Host == "" {
		if h, err := hostname(); err == nil {
			c.Host = h
		}
	}
	c.Provider = strings.TrimSpace(c.Provider)
	return c.normalizeRepos()
}

// checkServer answers the server URL with no trailing slash: http or https,
// a host, and no user, query or fragment.
func checkServer(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("server is missing: set server: <url of the grelot server>")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("server %q is not an http or https URL with a host and no user, query or fragment", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// normalizeRepos keys the alias map by normalized remote, since every
// lookup normalizes first, and refuses a value that is not one path
// component, since it becomes one in every canonical path.
func (c *Config) normalizeRepos() error {
	if len(c.Repos) == 0 {
		return nil
	}
	out := make(map[string]string, len(c.Repos))
	for remote, name := range c.Repos {
		norm, ok := canon.NormalizeRemote(remote)
		if !ok && !strings.Contains(remote, "://") {
			// The normalized form, host/owner/name, carries no scheme.
			norm, ok = canon.NormalizeRemote("https://" + remote)
		}
		if !ok {
			return fmt.Errorf("repos: %q is not a git remote with a host", remote)
		}
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			return fmt.Errorf("repos: %s: %q must be one path component, the repo name", remote, name)
		}
		if prev, dup := out[norm]; dup && prev != name {
			return fmt.Errorf("repos: two spellings of %s name %q and %q", norm, prev, name)
		}
		out[norm] = name
	}
	c.Repos = out
	return nil
}

// checkMode refuses a config file others can read when it holds a token.
// Go ignores the mode bits on Windows, where the user's AppData ACL guards
// the file (https://pkg.go.dev/os#Chmod).
func checkMode(info fs.FileInfo, holdsToken bool) error {
	if runtime.GOOS == "windows" || !holdsToken || info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	return fmt.Errorf("holds the token and is mode %o, readable by others", info.Mode().Perm())
}
