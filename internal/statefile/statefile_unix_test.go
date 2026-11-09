//go:build unix

package statefile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReplace_mode0600UnderOpenUmask(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	file := filepath.Join(t.TempDir(), "sub", "state")
	if err := Replace(file, []byte("x")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("directory mode = %o, want 700", got)
	}
}
