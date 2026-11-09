//go:build unix

package canon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClones_fileIsPrivate(t *testing.T) {
	isolateGit(t)
	base := tempDir(t)
	clone := filepath.Join(base, "toolbelt")
	gitRepo(t, clone, "origin", "https://github.com/cplieger/toolbelt")
	config := filepath.Join(base, "config")
	if _, err := New("/workspace", nil, NewClones(config)).Path(t.Context(), clone); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(config, "grelot", "clones"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("clones file mode %v, want no group or other access", fi.Mode().Perm())
	}
}
