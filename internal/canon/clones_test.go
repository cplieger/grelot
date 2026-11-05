package canon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocal(t *testing.T) {
	isolateGit(t)
	base := tempDir(t)
	clone := filepath.Join(base, "src", "toolbelt")
	gitRepo(t, clone, "origin", "git@github.com:cplieger/toolbelt.git")
	writeFile(t, filepath.Join(clone, "internal", "engine.go"))
	r := New("/workspace", nil, NewClones(filepath.Join(base, "config")))
	if _, err := r.Path(t.Context(), filepath.Join(clone, "README.md")); err != nil {
		t.Fatal(err)
	}

	got, err := r.Local(t.Context(), "/workspace/toolbelt/internal/engine.go")
	if want := filepath.Join(clone, "internal", "engine.go"); err != nil || got != want {
		t.Errorf("found: got %q, %v; want %q", got, err, want)
	}
	if got, err := r.Local(t.Context(), "/workspace/docs/README.md"); !errors.Is(err, ErrNoClone) {
		t.Errorf("no clone of docs: got %q, %v; want ErrNoClone", got, err)
	}
	for _, p := range []string{"/tmp/notes.md", "/workspace", "/workspacex/toolbelt/a"} {
		if got, err := r.Local(t.Context(), p); !errors.Is(err, ErrNotRepoPath) {
			t.Errorf("%q: got %q, %v; want ErrNotRepoPath", p, got, err)
		}
	}
}

func TestLocal_prefersTheCloneHoldingTheFile(t *testing.T) {
	isolateGit(t)
	base := tempDir(t)
	r := New("/workspace", nil, NewClones(filepath.Join(base, "config")))
	first := filepath.Join(base, "a", "toolbelt")
	second := filepath.Join(base, "b", "toolbelt")
	for _, dir := range []string{first, second} {
		gitRepo(t, dir, "origin", "https://github.com/cplieger/toolbelt")
		if _, err := r.Path(t.Context(), dir); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(second, "new.go"))
	got, err := r.Local(t.Context(), "/workspace/toolbelt/new.go")
	if want := filepath.Join(second, "new.go"); err != nil || got != want {
		t.Errorf("got %q, %v; want %q", got, err, want)
	}
	got, err = r.Local(t.Context(), "/workspace/toolbelt/missing.go")
	if want := filepath.Join(first, "missing.go"); err != nil || got != want {
		t.Errorf("missing everywhere: got %q, %v; want the first clone's %q", got, err, want)
	}
}

func TestLocal_rechecksEachRoot(t *testing.T) {
	isolateGit(t)
	tests := []struct {
		name   string
		change func(t *testing.T, clone string)
	}{
		{"remote changed", func(t *testing.T, clone string) {
			git(t, clone, "remote", "set-url", "origin", "https://github.com/someone/toolbelt")
		}},
		{"clone deleted", func(t *testing.T, clone string) {
			if err := os.RemoveAll(clone); err != nil {
				t.Fatal(err)
			}
		}},
		{"no longer a clone", func(t *testing.T, clone string) {
			if err := os.RemoveAll(filepath.Join(clone, ".git")); err != nil {
				t.Fatal(err)
			}
		}},
		{"now a folder inside another clone of it", func(t *testing.T, clone string) {
			if err := os.RemoveAll(filepath.Join(clone, ".git")); err != nil {
				t.Fatal(err)
			}
			gitRepo(t, filepath.Dir(clone), "origin", "https://github.com/cplieger/toolbelt")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := tempDir(t)
			clone := filepath.Join(base, "toolbelt")
			gitRepo(t, clone, "origin", "https://github.com/cplieger/toolbelt")
			r := New("/workspace", nil, NewClones(filepath.Join(base, "config")))
			if _, err := r.Path(t.Context(), clone); err != nil {
				t.Fatal(err)
			}
			tt.change(t, clone)
			if got, err := r.Local(t.Context(), "/workspace/toolbelt/a.go"); !errors.Is(err, ErrNoClone) {
				t.Errorf("got %q, %v; want ErrNoClone", got, err)
			}
		})
	}
}

func TestLocal_noClonesFile(t *testing.T) {
	isolateGit(t)
	if got, err := New("/workspace", nil, NewClones(tempDir(t))).Local(t.Context(), "/workspace/toolbelt/a.go"); !errors.Is(err, ErrNoClone) {
		t.Errorf("empty config dir: got %q, %v; want ErrNoClone", got, err)
	}
	if got, err := New("/workspace", nil, nil).Local(t.Context(), "/workspace/toolbelt/a.go"); !errors.Is(err, ErrNoClone) {
		t.Errorf("no clones file: got %q, %v; want ErrNoClone", got, err)
	}
}
