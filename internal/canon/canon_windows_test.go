package canon

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPath_bothFormsOnWindows(t *testing.T) {
	isolateGit(t)
	r := New("/workspace", nil, nil)
	loc, err := r.Path(t.Context(), "/workspace/tool/x.go")
	if err != nil || loc.Canonical != "/workspace/tool/x.go" {
		t.Errorf("grelot form: got %q, %v; want /workspace/tool/x.go", loc.Canonical, err)
	}
	local := filepath.Join(tempDir(t), "notes", "f.txt")
	loc, err = r.Path(t.Context(), local)
	want := "/" + strings.ToLower(local[:1]) + filepath.ToSlash(local[2:])
	if err != nil || loc.Canonical != want {
		t.Errorf("local %s: got %q, %v; want %q", local, loc.Canonical, err, want)
	}
}
