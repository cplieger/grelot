package canon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolateGit keeps the developer's git config (insteadOf rules, safe
// directories) out of the tests.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// tempDir is t.TempDir with its symlinks resolved, as git prints toplevels.
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitRepo creates a repository at dir with the remotes given as name, URL
// pairs.
func gitRepo(t *testing.T, dir string, remotes ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	for i := 0; i+1 < len(remotes); i += 2 {
		git(t, dir, "remote", "add", remotes[i], remotes[i+1])
	}
}

func writeFile(t *testing.T, file string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// outsideForm is the canonical form of a local path in no clone with a
// remote: the path in slash form, and on Windows C:\x\y as /c/x/y.
func outsideForm(p string) string {
	p = filepath.ToSlash(p)
	if runtime.GOOS == "windows" && len(p) > 1 && p[1] == ':' {
		return "/" + strings.ToLower(p[:1]) + p[2:]
	}
	return p
}

// fakeGit answers WorkTree for the directories it lists, for paths of another
// OS than the test's.
type fakeGit map[string]WorkTree

func (f fakeGit) WorkTree(_ context.Context, dir string) (WorkTree, bool, error) {
	wt, ok := f[dir]
	return wt, ok, nil
}

// windowsResolver resolves Windows paths on any OS over the directories and
// files that exist, each in git's slash form.
func windowsResolver(root string, g fakeGit, dirs, files []string) *Resolver {
	isDir, exists := map[string]bool{}, map[string]bool{}
	for _, d := range dirs {
		isDir[d], exists[d] = true, true
	}
	for _, f := range files {
		exists[f] = true
	}
	return &Resolver{
		git:    g,
		isDir:  func(p string) bool { return isDir[p] },
		exists: func(p string) bool { return exists[p] },
		root:   root,
		system: "windows",
	}
}
