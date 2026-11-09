//go:build !windows

package server

import (
	"io/fs"
	"log/slog"
	"os"

	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/provider"
)

const msgProviderInvalid = "provider invalid"

// loadDoors reads the server's providers folder once: Options.Providers,
// else Config.ProvidersDir, else none. Each file it skips is one ERROR line
// naming the file, the key and the problem, never a value, and counts in
// providers_invalid; a folder it cannot read is one ERROR line, no door,
// and a count of 1.
func loadDoors(o *Options, log *slog.Logger, m *obs.Metrics) *provider.ServerDoors {
	fsys := o.Providers
	if fsys == nil && o.Config.ProvidersDir != "" {
		fsys = os.DirFS(o.Config.ProvidersDir)
	}
	if fsys == nil {
		return &provider.ServerDoors{}
	}
	return loadDoorsFrom(fsys, log, m)
}

func loadDoorsFrom(fsys fs.FS, log *slog.Logger, m *obs.Metrics) *provider.ServerDoors {
	doors, skipped, err := provider.LoadServerDir(fsys)
	if err != nil {
		log.Error(msgProviderInvalid, "key", string(keyProvidersDir), "problem", err.Error())
		m.ProvidersInvalid.Set(1)
		return &provider.ServerDoors{}
	}
	m.ProvidersInvalid.Set(float64(len(skipped)))
	for _, le := range skipped {
		log.Error(msgProviderInvalid, "provider", le.Provider, "key", le.Key, "problem", le.Problem)
	}
	return doors
}
