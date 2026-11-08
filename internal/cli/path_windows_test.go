//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Git Bash with MSYS_NO_PATHCONV unset hands grelot path --local
// /workspace/grelot/README.md over as <install>/workspace/grelot/README.md.
func TestPath_localNamesTheTypedPathAfterAnMSYSRewrite(t *testing.T) {
	install := filepath.ToSlash(t.TempDir())
	marker := filepath.Join(install, "usr", "bin", "msys-2.0.dll")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MSYSTEM", "MINGW64")
	a := newAgent(t, &testServer{URL: "http://127.0.0.1:1"}, "s-1", "")
	code, _, errOut := a.run("path", "--local", install+"/workspace/grelot/README.md")
	if code != 1 || !strings.Contains(errOut, "no clone of the repository /workspace/grelot/README.md names") {
		t.Errorf("exit %d, stderr %q; want 1 naming /workspace/grelot/README.md", code, errOut)
	}
}
