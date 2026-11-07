package canon

import (
	"context"
	"path"
	"strings"
)

// maxCandidates bounds the paths one body's scan checks, and so its os.Stat
// and git calls, whatever the body holds.
const maxCandidates = 32

// Finding is a path in a post's body that other environments cannot read.
type Finding struct {
	Token     string // as the body wrote it
	Canonical string // its canonical form when it lies in a clone with a remote, else ""
}

// ScanLocal finds the first absolute path in body that is local to this
// host: under the home or temp dir, or existing on disk. A path in a clone
// whose local root is its canonical root <root>/<repo> is exempt, since
// every host names it the same way; so are the path root itself, a path
// with a placeholder component (..., …, <name>) and a ~/ path, which each
// reader takes as its own home. A path that neither exists nor lies under
// home or temp, such as an API route or another host's canonical path, is
// left alone.
func (r *Resolver) ScanLocal(ctx context.Context, body string) (Finding, bool) {
	trees := map[string]tree{}
	checked := 0
	for field := range strings.FieldsSeq(body) {
		lead := strings.TrimLeft(field, "\"'`([{<")
		if placeholder(lead) {
			continue
		}
		tok := strings.TrimRight(lead, "\"'`)]}>.,;:!?")
		p, ok := r.candidate(tok)
		if !ok {
			continue
		}
		if checked == maxCandidates {
			break
		}
		checked++
		if !r.under(p, r.home) && !r.under(p, r.temp) && !r.exists(p) {
			continue
		}
		dir, tail := r.nearestDir(p)
		t, seen := trees[dir]
		if !seen {
			t = r.treeAt(ctx, dir)
			trees[dir] = t
		}
		if t.clone.Remote == "" {
			return Finding{Token: tok}, true
		}
		canonRoot := path.Join(r.root, t.clone.Repo)
		if r.fold(t.clone.Toplevel) == r.fold(canonRoot) {
			continue
		}
		return Finding{Token: tok, Canonical: path.Join(canonRoot, t.prefix, tail)}, true
	}
	return Finding{}, false
}

type tree struct {
	prefix string
	clone  Clone
}

// treeAt is the clone holding dir, its zero value outside one; a git failure
// counts as none, so the path is flagged rather than exempted.
func (r *Resolver) treeAt(ctx context.Context, dir string) tree {
	if dir == "" {
		return tree{}
	}
	wt, ok, err := r.git.WorkTree(ctx, dir)
	if err != nil || !ok {
		return tree{}
	}
	return tree{prefix: wt.Prefix, clone: r.clone(wt)}
}

// candidate is tok as an absolute path in slash form, when tok is one that
// names at least one component below the path root: /x or a drive path C:\x
// or C:/x.
func (r *Resolver) candidate(tok string) (string, bool) {
	var p string
	switch {
	case strings.HasPrefix(tok, "/"):
		p = tok
	case hasDrive(tok) && len(tok) > 2 && (tok[2] == '\\' || tok[2] == '/'):
		p = strings.ReplaceAll(tok, `\`, "/")
	default:
		return "", false
	}
	p = r.clean(p)
	if strings.Trim(p, "/") == "" || len(p) == 3 && hasDrive(p) || r.fold(p) == r.fold(r.clean(r.root)) {
		return "", false
	}
	return p, true
}

// placeholder reports whether a component of tok stands for a path rather
// than naming one: ... or … (closing punctuation aside), or one opening <.
// A trailing ellipsis on a real name (/tmp/build.log...) is no placeholder.
func placeholder(tok string) bool {
	for c := range strings.FieldsFuncSeq(tok, func(c rune) bool { return c == '/' || c == '\\' }) {
		if strings.HasPrefix(c, "<") {
			return true
		}
		switch strings.TrimRight(c, "\"'`)]}>,;:!?") {
		case "...", "…":
			return true
		}
	}
	return false
}

// under reports whether p is dir or inside it; a root or empty dir holds
// nothing, so a home of / does not make every path local.
func (r *Resolver) under(p, dir string) bool {
	d := r.clean(r.slash(dir))
	if dir == "" || strings.Trim(d, "/") == "" {
		return false
	}
	p, d = r.fold(p), r.fold(d)
	return p == d || strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/")
}
