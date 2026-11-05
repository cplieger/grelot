package client

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/cplieger/grelot/internal/provider"
)

// env is a lookup over kv pairs.
func env(kv ...string) func(string) (string, bool) {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func noHostname() (string, error) { return "", os.ErrNotExist }

// writeConfig writes content as the client config under a fresh config dir
// with mode, and returns the dir.
func writeConfig(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	file := ConfigFile(dir)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

// providerSet loads name, content pairs as one provider layer.
func providerSet(t *testing.T, kv ...string) *provider.Set {
	t.Helper()
	fsys := fstest.MapFS{}
	for i := 0; i+1 < len(kv); i += 2 {
		fsys[kv[i]+".yaml"] = &fstest.MapFile{Data: []byte(kv[i+1])}
	}
	s, err := provider.LoadSet(fsys)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	return s
}
