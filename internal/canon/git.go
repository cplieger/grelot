package canon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// gitTimeout bounds one git call, which only a hung git reaches: one call
// takes over a second on a slow Windows host and several times that under
// load.
const gitTimeout = 15 * time.Second

type execGit struct{}

var _ gitReader = execGit{}

func (execGit) WorkTree(ctx context.Context, dir string) (workTree, bool, error) {
	out, err := runGit(ctx, dir, "rev-parse", "--show-toplevel", "--absolute-git-dir", "--show-prefix")
	if err != nil {
		if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) && !underDotGit(dir) {
			return workTree{}, false, nil
		}
		return workTree{}, false, err
	}
	lines := strings.Split(strings.TrimRight(out, "\r\n"), "\n")
	if len(lines) < 2 {
		return workTree{}, false, fmt.Errorf("git rev-parse in %s printed %q", dir, out)
	}
	wt := workTree{Toplevel: trimCR(lines[0]), GitDir: trimCR(lines[1])}
	if len(lines) > 2 {
		wt.Prefix = trimCR(lines[2])
	}
	names, err := runGit(ctx, wt.Toplevel, "remote")
	if err != nil {
		return workTree{}, false, err
	}
	if name, ok := pickRemote(strings.Fields(names)); ok {
		u, err := runGit(ctx, wt.Toplevel, "remote", "get-url", name)
		if err != nil {
			return workTree{}, false, err
		}
		wt.Remote = strings.TrimSpace(u)
	}
	return wt, true, nil
}

func trimCR(s string) string { return strings.TrimSuffix(s, "\r") }

// pickRemote is origin, or the only remote; with several and no origin no
// one remote names the repo.
func pickRemote(names []string) (string, bool) {
	if slices.Contains(names, "origin") {
		return "origin", true
	}
	if len(names) == 1 {
		return names[0], true
	}
	return "", false
}

// underDotGit reports whether dir or a parent holds a .git entry: there a
// failing git is a broken or refused repository (such as one git calls of
// dubious ownership), which must not read as a folder outside any clone.
func underDotGit(dir string) bool {
	for d := filepath.Clean(dir); ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed git subcommands; dir is passed as -C's value
	cmd.Env = gitEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w: %s", args[0], dir, err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// gitEnv drops the variables that point git at one repository: a host that
// runs grelot from inside a git hook exports them, and git -C would then
// answer for that repository instead of dir.
func gitEnv(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return name == "GIT_DIR" || name == "GIT_WORK_TREE" || name == "GIT_COMMON_DIR"
	})
}
