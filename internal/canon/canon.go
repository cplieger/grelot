// Package canon maps local paths to grelot's canonical form and back. A path
// inside a git clone with a remote is <root>/<repo>/<rel>, named by the
// remote and never by where the clone sits, so every host names one file the
// same way; any other path stays local, cleaned, in slash form.
package canon

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// workTree is what git reports for a directory inside a working tree.
type workTree struct {
	Toplevel string // in git's slash form
	GitDir   string // this working tree's own git dir
	Prefix   string // the directory relative to Toplevel: "" or ending in "/"
	Remote   string // origin's URL, else the only remote's, else ""
}

// gitReader reads working trees; execGit runs the git binary.
type gitReader interface {
	// WorkTree describes the working tree holding dir, answering false when
	// dir is in none.
	WorkTree(ctx context.Context, dir string) (workTree, bool, error)
}

// Clone is the working tree a path lies in.
type Clone struct {
	Toplevel string
	GitDir   string
	Remote   string // normalized; "" when the clone has no usable remote
	Repo     string // the canonical <repo>; "" when Remote is
}

// Location is a local path's canonical form.
type Location struct {
	Clone     *Clone // nil outside any working tree
	Canonical string
}

// Resolver canonicalizes paths under one path root.
type Resolver struct {
	git    gitReader
	isDir  func(string) bool
	exists func(string) bool
	clones *Clones
	repos  map[string]string
	root   string
	system string
	msys   string // MSYSTEM: set in an MSYS shell such as Git Bash
	home   string
	temp   string
}

// New is the Resolver for this host under the server's path root, with the
// client config's repos aliases (normalized remote to <repo>). It records
// each clone it meets in clones, which may be nil.
func New(root string, repos map[string]string, clones *Clones) *Resolver {
	home, _ := os.UserHomeDir()
	return &Resolver{
		git:    execGit{},
		isDir:  func(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() },
		exists: func(p string) bool { _, err := os.Stat(p); return err == nil },
		clones: clones,
		repos:  repos,
		root:   path.Clean(root),
		system: runtime.GOOS,
		msys:   os.Getenv("MSYSTEM"),
		home:   home,
		temp:   os.TempDir(),
	}
}

// Path is the canonical form of local, a path on this host, relative ones
// against the working directory. It fails when git or the working directory
// does.
func (r *Resolver) Path(ctx context.Context, local string) (Location, error) {
	if p, ok := r.unconverted(local); ok {
		return Location{Canonical: p}, nil
	}
	if p, ok := r.grelotForm(local); ok {
		return Location{Canonical: p}, nil
	}
	p, err := r.absolute(local)
	if err != nil {
		return Location{}, err
	}
	dir, tail := r.nearestDir(p)
	if dir == "" {
		return Location{Canonical: r.outside(p)}, nil
	}
	wt, ok, err := r.git.WorkTree(ctx, dir)
	if err != nil {
		return Location{}, err
	}
	if !ok {
		return Location{Canonical: r.outside(p)}, nil
	}
	c := r.clone(wt)
	if c.Remote == "" {
		return Location{Canonical: r.outside(p), Clone: &c}, nil
	}
	r.clones.record(c.Remote, c.Toplevel)
	return Location{Canonical: path.Join(r.root, c.Repo, wt.Prefix, tail), Clone: &c}, nil
}

func (r *Resolver) clone(wt workTree) Clone {
	c := Clone{Toplevel: wt.Toplevel, GitDir: wt.GitDir}
	if norm, ok := NormalizeRemote(wt.Remote); ok {
		c.Remote, c.Repo = norm, repoName(norm, r.repos)
	}
	return c
}

// absolute is local made absolute and cleaned, in slash form; a Windows path
// keeps its drive, C:/x.
func (r *Resolver) absolute(local string) (string, error) {
	p := r.slash(local)
	if !r.isAbs(p) {
		a, err := filepath.Abs(local)
		if err != nil {
			return "", err
		}
		p = r.slash(a)
	}
	return r.clean(p), nil
}

// grelotForm is local cleaned when, on Windows, it is a rooted path with no
// drive (/workspace/x): the form the manual tells agents to write repository
// paths in. Resolving it would join it to the current drive and give
// /<drive>/workspace/x.
func (r *Resolver) grelotForm(local string) (string, bool) {
	p := r.slash(local)
	if !r.windows() || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return "", false
	}
	return path.Clean(p), true
}

// msysMarker is the file that marks a folder as an MSYS install's root, the
// folder an MSYS shell calls /: Git for Windows and MSYS2 both ship it.
const msysMarker = "/usr/bin/msys-2.0.dll"

// unconverted is the grelot form a path under the root had before an MSYS
// shell rewrote it. Such a shell hands a native program the argument
// /workspace/x as <install>/workspace/x (C:/Program Files/Git/workspace/x in
// Git Bash), so a path is taken back when MSYSTEM is set, it is a drive path
// holding the root, the part before the root is an MSYS install, and that
// install holds no folder of the root's name, which would make the path a
// real local one.
func (r *Resolver) unconverted(local string) (string, bool) {
	if !r.windows() || r.msys == "" || r.root == "/" {
		return "", false
	}
	p := r.slash(local)
	if !hasDrive(p) {
		return "", false
	}
	for i := 0; ; {
		j := strings.Index(p[i:], r.root)
		if j < 0 {
			return "", false
		}
		i += j
		install, rest := p[:i], p[i:]
		if (len(rest) == len(r.root) || rest[len(r.root)] == '/') &&
			r.exists(install+msysMarker) && !r.isDir(install+r.root) {
			return path.Clean(rest), true
		}
		i += len(r.root)
	}
}

func (r *Resolver) slash(p string) string {
	if r.windows() {
		return strings.ReplaceAll(p, `\`, "/")
	}
	return p
}

func (r *Resolver) isAbs(p string) bool {
	if r.windows() {
		return hasDrive(p) && len(p) > 2 && p[2] == '/' || strings.HasPrefix(p, "//")
	}
	return strings.HasPrefix(p, "/")
}

func hasDrive(p string) bool {
	return len(p) >= 2 && p[1] == ':' && ('a' <= p[0] && p[0] <= 'z' || 'A' <= p[0] && p[0] <= 'Z')
}

// clean is path.Clean, except that a drive root stays C:/ where path.Clean
// would make it the drive-relative C:.
func (r *Resolver) clean(p string) string {
	c := path.Clean(p)
	if r.windows() && len(c) == 2 && hasDrive(c) {
		c += "/"
	}
	return c
}

// nearestDir splits p into its nearest existing directory at or above it and
// the rest, so a file not written yet still canonicalizes. dir is "" when no
// directory on p's way to the root exists.
func (r *Resolver) nearestDir(p string) (dir, tail string) {
	for {
		if r.isDir(p) {
			return p, tail
		}
		parent := r.clean(path.Dir(p))
		if parent == p {
			return "", ""
		}
		tail = path.Join(path.Base(p), tail)
		p = parent
	}
}

// outside is the canonical form of a path in no clone with a remote: the path
// itself, with a Windows drive as a first component, C:/x/y as /c/x/y, since
// a canonical path is a slash path with no drive on every OS.
func (r *Resolver) outside(p string) string {
	if r.windows() && hasDrive(p) {
		return path.Join("/", strings.ToLower(p[:1]), p[2:])
	}
	return path.Clean("/" + strings.TrimLeft(p, "/"))
}

func (r *Resolver) windows() bool { return r.system == "windows" }

// fold is p as compared on this system: Windows paths ignore case.
func (r *Resolver) fold(p string) string {
	if r.windows() {
		return strings.ToLower(p)
	}
	return p
}
