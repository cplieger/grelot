//go:build unix

package where

import (
	"os"
	"syscall"
	"testing"
)

func TestEnvironment_keyFileMode(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	file := EnvironmentFile(t.TempDir())
	if Environment(t.Context(), file, false) == "" {
		t.Fatal("no id")
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}
