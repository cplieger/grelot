package canon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cplieger/grelot/internal/statefile"
	"github.com/cplieger/grelot/internal/where"
)

// ErrNoClone answers Local for a repository with no known clone here.
var ErrNoClone = errors.New("no clone of that repository on this machine")

// ErrNotRepoPath answers Local for a path outside the path root, which names
// a file local to its sender.
var ErrNotRepoPath = errors.New("not a repository path")

// maxClonesFile bounds the clones file read; it holds a few roots per repo.
const maxClonesFile = 1 << 20

// Clones is the clones file: each remote hash with the working-tree roots
// this machine has canonicalized paths in.
type Clones struct{ file string }

// NewClones is the clones file under the user config dir.
func NewClones(configDir string) *Clones {
	return &Clones{file: filepath.Join(configDir, "grelot", "clones")}
}

func (c *Clones) load() map[string][]string {
	m := map[string][]string{}
	f, err := os.Open(c.file)
	if err != nil {
		return m
	}
	defer f.Close()
	if err := json.NewDecoder(io.LimitReader(f, maxClonesFile)).Decode(&m); err != nil {
		return map[string][]string{}
	}
	return m
}

// record adds toplevel under remote's hash. It is best effort: a lost write
// only means a later Local cannot find that clone until it is canonicalized
// again.
func (c *Clones) record(remote, toplevel string) {
	if c == nil {
		return
	}
	m := c.load()
	h := where.Remote(remote)
	if slices.Contains(m[h], toplevel) {
		return
	}
	m[h] = append(m[h], toplevel)
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = statefile.Replace(c.file, data)
}

// Local is the local path, on this machine, of canonical, a path under the
// root: the same file in a recorded clone whose remote still names that repo,
// preferring a clone where the file exists. ErrNotRepoPath and ErrNoClone
// answer what it cannot map.
func (r *Resolver) Local(ctx context.Context, canonical string) (string, error) {
	repo, rel, ok := r.splitCanonical(canonical)
	if !ok {
		return "", ErrNotRepoPath
	}
	roots := r.clonesOf(ctx, repo)
	if len(roots) == 0 {
		return "", ErrNoClone
	}
	for _, root := range roots {
		if local := filepath.FromSlash(path.Join(root, rel)); r.exists(local) {
			return local, nil
		}
	}
	return filepath.FromSlash(path.Join(roots[0], rel)), nil
}

// clonesOf is the recorded roots that are still clones of repo, each remote
// hash's roots in the order they were recorded.
func (r *Resolver) clonesOf(ctx context.Context, repo string) []string {
	if r.clones == nil {
		return nil
	}
	m := r.clones.load()
	var roots []string
	for _, h := range slices.Sorted(maps.Keys(m)) {
		for _, root := range m[h] {
			if r.stillClone(ctx, root, h, repo) {
				roots = append(roots, root)
			}
		}
	}
	return roots
}

func (r *Resolver) splitCanonical(canonical string) (repo, rel string, ok bool) {
	p := path.Clean(canonical)
	rest, ok := strings.CutPrefix(p, strings.TrimSuffix(r.root, "/")+"/")
	if !ok || rest == "" {
		return "", "", false
	}
	repo, rel, _ = strings.Cut(rest, "/")
	return repo, rel, true
}

// stillClone reports whether root is still a working tree's top whose remote
// hashes to h and names repo: a clone moved, deleted or re-pointed since it
// was recorded answers nothing.
func (r *Resolver) stillClone(ctx context.Context, root, h, repo string) bool {
	wt, ok, err := r.git.WorkTree(ctx, root)
	if err != nil || !ok || r.fold(wt.Toplevel) != r.fold(root) {
		return false
	}
	c := r.clone(wt)
	return c.Remote != "" && where.Remote(c.Remote) == h && c.Repo == repo
}
