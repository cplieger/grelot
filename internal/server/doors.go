//go:build !windows

package server

import (
	"io/fs"
	"log/slog"
	"os"

	"github.com/cplieger/grelot/internal/provider"
)

const msgProviderInvalid = "provider invalid"

// loadDoors reads the server's providers folder once: Options.Providers,
// else Config.ProvidersDir, else none. Each file it skips is one ERROR line
// naming the file, the key and the problem, never a value; a folder it
// cannot read is one ERROR line and no door.
func loadDoors(o *Options, log *slog.Logger) *provider.ServerDoors {
	fsys := o.Providers
	if fsys == nil && o.Config.ProvidersDir != "" {
		fsys = os.DirFS(o.Config.ProvidersDir)
	}
	if fsys == nil {
		return &provider.ServerDoors{}
	}
	return loadDoorsFrom(fsys, log)
}

func loadDoorsFrom(fsys fs.FS, log *slog.Logger) *provider.ServerDoors {
	doors, skipped, err := provider.LoadServerDir(fsys)
	if err != nil {
		log.Error(msgProviderInvalid, "key", string(keyProvidersDir), "problem", err.Error())
		return &provider.ServerDoors{}
	}
	for _, le := range skipped {
		log.Error(msgProviderInvalid, "provider", le.Provider, "key", le.Key, "problem", le.Problem)
	}
	return doors
}
