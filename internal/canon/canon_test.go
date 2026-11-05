package canon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cplieger/grelot/internal/where"
)

func TestPath_oneFileFromThreeClones(t *testing.T) {
	isolateGit(t)
	const want = "/workspace/toolbelt/engine.go"

	win := windowsResolver("/workspace", fakeGit{
		"C:/whatever/workfolder/toolbelt": {
			Toplevel: "C:/whatever/workfolder/toolbelt",
			GitDir:   "C:/whatever/workfolder/toolbelt/.git",
			Remote:   "git@github.com:cplieger/toolbelt.git",
		},
	}, []string{"C:/", "C:/whatever", "C:/whatever/workfolder", "C:/whatever/workfolder/toolbelt"}, nil)
	loc, err := win.Path(t.Context(), `C:\whatever\workfolder\toolbelt\engine.go`)
	if err != nil || loc.Canonical != want {
		t.Errorf("Windows clone: got %q, %v; want %q", loc.Canonical, err, want)
	}

	base := tempDir(t)
	container := filepath.Join(base, "workspace", "toolbelt")
	gitRepo(t, container, "origin", "https://github.com/cplieger/toolbelt.git")
	writeFile(t, filepath.Join(container, "engine.go"))
	laptop := filepath.Join(base, "home", "u", "src", "toolbelt")
	gitRepo(t, laptop, "origin", "git@GitHub.com:cplieger/toolbelt")
	r := New("/workspace", nil, nil)
	for _, local := range []string{
		filepath.Join(container, "engine.go"),
		filepath.Join(laptop, "engine.go"), // not written yet
	} {
		loc, err := r.Path(t.Context(), local)
		if err != nil || loc.Canonical != want {
			t.Errorf("%s: got %q, %v; want %q", local, loc.Canonical, err, want)
		}
	}

	sub := filepath.Join(laptop, "internal", "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	loc, err = r.Path(t.Context(), filepath.Join(laptop, "new", "dir", "f.go"))
	if err != nil || loc.Canonical != "/workspace/toolbelt/new/dir/f.go" {
		t.Errorf("directories not made yet: got %q, %v", loc.Canonical, err)
	}
	loc, err = r.Path(t.Context(), filepath.Join(sub, "y.go"))
	if err != nil || loc.Canonical != "/workspace/toolbelt/internal/x/y.go" {
		t.Errorf("subdirectory: got %q, %v", loc.Canonical, err)
	}
	if loc.Clone == nil || loc.Clone.Toplevel != laptop || loc.Clone.Remote != "github.com/cplieger/toolbelt" || loc.Clone.Repo != "toolbelt" {
		t.Errorf("subdirectory clone: got %+v", loc.Clone)
	}
}

func TestPath_cloneWithNoRemoteIsLocal(t *testing.T) {
	isolateGit(t)
	dir := filepath.Join(tempDir(t), "notes")
	gitRepo(t, dir)
	file := filepath.Join(dir, "todo.md")
	loc, err := New("/workspace", nil, nil).Path(t.Context(), file)
	if err != nil || loc.Canonical != filepath.ToSlash(file) {
		t.Errorf("got %q, %v; want the local path %q", loc.Canonical, err, file)
	}
	if loc.Clone == nil || loc.Clone.Remote != "" || loc.Clone.Repo != "" {
		t.Errorf("clone: got %+v, want one with no remote", loc.Clone)
	}
}

func TestPath_remoteChoice(t *testing.T) {
	isolateGit(t)
	tests := []struct {
		name    string
		remotes []string
		want    string
	}{
		{"origin among several", []string{"upstream", "https://github.com/up/docs", "origin", "https://github.com/me/handbook"}, "/workspace/handbook/a.md"},
		{"the only remote", []string{"upstream", "https://github.com/up/docs"}, "/workspace/docs/a.md"},
		{"several and no origin", []string{"a", "https://github.com/a/docs", "b", "https://github.com/b/docs"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(tempDir(t), "clone")
			gitRepo(t, dir, tt.remotes...)
			file := filepath.Join(dir, "a.md")
			want := tt.want
			if want == "" {
				want = filepath.ToSlash(file)
			}
			loc, err := New("/workspace", nil, nil).Path(t.Context(), file)
			if err != nil || loc.Canonical != want {
				t.Errorf("got %q, %v; want %q", loc.Canonical, err, want)
			}
		})
	}
}

func TestPath_alias(t *testing.T) {
	isolateGit(t)
	dir := filepath.Join(tempDir(t), "docs")
	gitRepo(t, dir, "origin", "git@gitlab.example.com:team/docs.git")
	r := New("/workspace", map[string]string{"gitlab.example.com/team/docs": "team-docs"}, nil)
	loc, err := r.Path(t.Context(), filepath.Join(dir, "README.md"))
	if err != nil || loc.Canonical != "/workspace/team-docs/README.md" {
		t.Errorf("got %q, %v; want /workspace/team-docs/README.md", loc.Canonical, err)
	}
}

func TestPath_outsideAClone(t *testing.T) {
	isolateGit(t)
	base := tempDir(t)
	r := New("/workspace", nil, nil)
	loc, err := r.Path(t.Context(), filepath.Join(base, "a", "..", "b", "f.txt"))
	if want := filepath.ToSlash(filepath.Join(base, "b", "f.txt")); err != nil || loc.Canonical != want || loc.Clone != nil {
		t.Errorf("cleaned: got %+v, %v; want %q", loc, err, want)
	}

	t.Chdir(base)
	loc, err = r.Path(t.Context(), "rel/f.txt")
	if want := filepath.ToSlash(filepath.Join(base, "rel", "f.txt")); err != nil || loc.Canonical != want {
		t.Errorf("relative: got %q, %v; want %q", loc.Canonical, err, want)
	}

	win := windowsResolver("/workspace", fakeGit{}, []string{"C:/", "C:/x"}, nil)
	for local, want := range map[string]string{
		`C:\x\y`:         "/c/x/y",
		"D:/Data/f.txt":  "/d/Data/f.txt",
		`c:\x\..\z\w.go`: "/c/z/w.go",
		`C:\`:            "/c",
	} {
		loc, err := win.Path(t.Context(), local)
		if err != nil || loc.Canonical != want {
			t.Errorf("Windows %q: got %q, %v; want %q", local, loc.Canonical, err, want)
		}
	}
}

func TestPath_ignoresAGitDirFromTheEnvironment(t *testing.T) {
	isolateGit(t)
	base := tempDir(t)
	other := filepath.Join(base, "other")
	gitRepo(t, other, "origin", "https://github.com/cplieger/other")
	dir := filepath.Join(base, "toolbelt")
	gitRepo(t, dir, "origin", "https://github.com/cplieger/toolbelt")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	loc, err := New("/workspace", nil, nil).Path(t.Context(), filepath.Join(dir, "f.go"))
	if err != nil || loc.Canonical != "/workspace/toolbelt/f.go" {
		t.Errorf("got %q, %v; want /workspace/toolbelt/f.go", loc.Canonical, err)
	}
}

func TestPath_brokenRepositoryIsAnError(t *testing.T) {
	isolateGit(t)
	dir := tempDir(t)
	writeFile(t, filepath.Join(dir, ".git"))
	if loc, err := New("/workspace", nil, nil).Path(t.Context(), filepath.Join(dir, "f.go")); err == nil {
		t.Errorf("got %+v and no error for a repository git cannot read", loc)
	}
}

func TestPath_checkoutPerWorktreeAndCopy(t *testing.T) {
	isolateGit(t)
	base := tempDir(t)
	primary := filepath.Join(base, "toolbelt")
	gitRepo(t, primary, "origin", "https://github.com/cplieger/toolbelt")
	git(t, primary, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init")
	linked := filepath.Join(base, "toolbelt-wt")
	git(t, primary, "worktree", "add", "-q", linked)

	r := New("/workspace", nil, nil)
	ids := map[string]string{}
	for _, dir := range []string{primary, linked} {
		loc, err := r.Path(t.Context(), filepath.Join(dir, "f.go"))
		if err != nil || loc.Canonical != "/workspace/toolbelt/f.go" || loc.Clone == nil {
			t.Fatalf("%s: got %+v, %v", dir, loc, err)
		}
		id := where.Checkout(t.Context(), loc.Clone.GitDir, loc.Clone.Toplevel)
		if _, err := os.Stat(where.CheckoutFile(loc.Clone.GitDir)); id == "" || err != nil {
			t.Errorf("%s: id %q, key file in its git dir: %v", dir, id, err)
		}
		ids[loc.Clone.GitDir] = id
	}
	if len(ids) != 2 || ids[filepath.Join(primary, ".git")] == ids[filepath.Join(primary, ".git", "worktrees", "toolbelt-wt")] {
		t.Errorf("worktrees share a git dir or an id: %v", ids)
	}

	copied := filepath.Join(base, "copy")
	if err := os.CopyFS(copied, os.DirFS(primary)); err != nil {
		t.Fatal(err)
	}
	loc, err := r.Path(t.Context(), filepath.Join(copied, "f.go"))
	if err != nil || loc.Clone == nil {
		t.Fatalf("copy: got %+v, %v", loc, err)
	}
	if got := where.Checkout(t.Context(), loc.Clone.GitDir, loc.Clone.Toplevel); got == "" || got == ids[filepath.Join(primary, ".git")] {
		t.Errorf("copy: id %q, original %q", got, ids[filepath.Join(primary, ".git")])
	}
}
