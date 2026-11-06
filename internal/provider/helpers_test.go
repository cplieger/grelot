package provider

import (
	"os"
	"testing"
)

// mustSymlink links newname to oldname, and skips the test where this user
// may not create links.
func mustSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	err := os.Symlink(oldname, newname)
	switch {
	case err == nil:
	case symlinkUnavailable(err):
		t.Skipf("this user may not create symlinks: %v", err)
	default:
		t.Fatal(err)
	}
}
