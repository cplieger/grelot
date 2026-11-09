//go:build !windows

package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPath_oneFileFromClonesAtAnyRootIsOnePath(t *testing.T) {
	isolateGit(t)
	ts := newServer(t)
	a := newAgent(t, ts, "s-1", "")
	base := t.TempDir()
	for _, c := range []struct{ dir, remote string }{
		{filepath.Join(base, "workspace", "toolbelt"), "https://github.com/cplieger/toolbelt.git"},
		{filepath.Join(base, "home", "u", "src", "toolbelt"), "git@GitHub.com:cplieger/toolbelt"},
	} {
		clone := gitClone(t, c.dir, c.remote)
		if out := a.must("path", filepath.Join(clone, "engine.go")); out != "/workspace/toolbelt/engine.go\n" {
			t.Errorf("path in %s = %q, want /workspace/toolbelt/engine.go", c.dir, out)
		}
	}
}

func TestPath_aCloneWithNoRemoteIsLocal(t *testing.T) {
	isolateGit(t)
	a := newAgent(t, newServer(t), "s-1", "")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(dir, "scratch")
	gitClone(t, clone, "")
	file := filepath.Join(clone, "notes.md")
	if out := a.must("path", file); out != filepath.ToSlash(file)+"\n" {
		t.Errorf("path = %q, want the local path %s", out, file)
	}
}

func TestPath_local(t *testing.T) {
	isolateGit(t)
	a := newAgent(t, newServer(t), "s-1", "")
	clone := gitClone(t, filepath.Join(t.TempDir(), "src", "toolbelt"), toolbeltRemote)
	writeFile(t, filepath.Join(clone, "engine.go"))
	a.must("path", clone)

	t.Run("a file in a known clone", func(t *testing.T) {
		if out := a.must("path", "--local", "/workspace/toolbelt/engine.go"); out != filepath.Join(clone, "engine.go")+"\n" {
			t.Errorf("path --local = %q, want %s", out, filepath.Join(clone, "engine.go"))
		}
	})
	t.Run("a repository with no clone here", func(t *testing.T) {
		code, _, errOut := a.run("path", "--local", "/workspace/subflux/x.go")
		if code != 1 || !strings.Contains(errOut, "no clone of the repository /workspace/subflux/x.go names") {
			t.Errorf("exit %d, stderr %q; want 1 saying no clone is known", code, errOut)
		}
	})
	t.Run("a path outside the root", func(t *testing.T) {
		code, _, errOut := a.run("path", "--local", "/tmp/notes.md")
		if code != 1 || !strings.Contains(errOut, "is not under /workspace") || !strings.Contains(errOut, "ask it for the content") {
			t.Errorf("exit %d, stderr %q; want 1 saying the path is local to its sender", code, errOut)
		}
	})
	t.Run("one path only", func(t *testing.T) {
		if code, _, _ := a.run("path"); code != 2 {
			t.Errorf("exit %d, want 2 for no path", code)
		}
	})
}

func TestPath_needsNoSessionOrServer(t *testing.T) {
	ts := newServer(t)
	a := newAgent(t, ts, "", "")
	delete(a.vars, "ALPHA_ID")
	before := len(ts.requests())
	if out := a.must("path", "/srv/x/y"); out != "/srv/x/y\n" {
		t.Errorf("path = %q, want /srv/x/y", out)
	}
	if len(ts.requests()) != before {
		t.Error("grelot path sent a request; it answers from this machine alone")
	}
}

func TestRegister_twoRepositoriesNamedDocs(t *testing.T) {
	isolateGit(t)
	ts := newServer(t)
	a := newAgent(t, ts, "s-1", "")
	first := gitClone(t, filepath.Join(t.TempDir(), "docs"), "https://github.com/cplieger/docs")
	a.must("register", "docs-dev", "--paths", first, "--desc", "docs", "--ttl", "1h")
	second := gitClone(t, filepath.Join(t.TempDir(), "docs"), "https://gitlab.example.com/team/docs.git")

	b := newAgent(t, ts, "s-2", "")
	code, _, errOut := b.run("register", "team-docs-dev", "--paths", second, "--desc", "team docs", "--ttl", "1h")
	if code != 1 || !strings.Contains(errOut, "/workspace/docs is another repository for docs-dev") || !strings.Contains(errOut, "repos:") {
		t.Fatalf("exit %d, stderr %q; want 1 naming the collision and repos:", code, errOut)
	}

	c := newAgent(t, ts, "s-3", "repos:\n  gitlab.example.com/team/docs: team-docs\n")
	out := c.must("register", "team-docs-dev", "--paths", second, "--desc", "team docs", "--ttl", "1h")
	if !strings.Contains(out, "paths: /workspace/team-docs\n") {
		t.Errorf("output = %q, want the alias's path /workspace/team-docs", out)
	}
	if !strings.Contains(ts.last(t, http.MethodPost, "/v1/register").Body, `"repo":"/workspace/team-docs"`) {
		t.Error("the aliased registration's checkout does not name /workspace/team-docs")
	}
}

func TestMe_correctsACachedRoot(t *testing.T) {
	isolateGit(t)
	ts := newServer(t)
	a, clone := registerAgent(t, ts, "s-1")
	if err := os.WriteFile(filepath.Join(a.env.ConfigDir, "grelot", "state", "server.json"),
		[]byte(`{"path_root":"/home/u/src","version":"v1.1.0-dev.4"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := a.must("path", clone); out != "/home/u/src/toolbelt\n" {
		t.Fatalf("path with the stale root = %q, want /home/u/src/toolbelt", out)
	}
	a.must("me")
	if out := a.must("path", clone); out != "/workspace/toolbelt\n" {
		t.Errorf("path after me = %q, want the server's /workspace/toolbelt", out)
	}
}
