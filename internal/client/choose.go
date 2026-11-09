package client

import (
	"fmt"

	"github.com/cplieger/grelot/internal/provider"
)

// Shell is what provider detection reads of the shell a command runs in.
type Shell struct {
	// Lookup reads an environment variable.
	Lookup func(name string) (string, bool)
	// Ancestors are the client's ancestor process names, nearest first.
	Ancestors []string
}

// Choose picks the provider a command runs under: flag (--provider), then
// the config's provider:, then detection. Its errors are the provider
// package's one-line refusals, printed as they are.
func Choose(set *provider.Set, flag string, cfg *Config, sh Shell) (*provider.Provider, error) {
	override := flag
	if override == "" {
		override = cfg.Provider
	}
	return set.Detect(override, sh.Lookup, sh.Ancestors)
}

// Session is the Grelot-Session value for p in this shell. A provider with
// no session variable, or a shell that does not set it, answers a
// *provider.NoSessionError and never an empty session.
func Session(p *provider.Provider, lookup func(string) (string, bool)) (string, error) {
	id, err := p.SessionID(lookup)
	if err != nil {
		return "", err
	}
	return p.Name + ":" + id, nil
}

// ProviderNote is the line register prints when flag names a provider
// that neither the config's provider: nor detection picks, so later
// commands without the flag would send another session; "" otherwise.
func ProviderNote(set *provider.Set, flag string, cfg *Config, sh Shell) string {
	if flag == "" || flag == cfg.Provider {
		return ""
	}
	found := "no single provider"
	if cfg.Provider != "" {
		found = "provider " + cfg.Provider + " from the client config"
	} else if p, err := set.Detect("", sh.Lookup, sh.Ancestors); err == nil {
		if p.Name == flag {
			return ""
		}
		found = "provider " + p.Name
	}
	return fmt.Sprintf("grelot: this shell gives %s, not %s; add provider: %s to the client config, or every grelot command here without --provider %s sends another session",
		found, flag, flag, flag)
}
