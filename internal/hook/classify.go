package hook

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/statefile"
)

// Kind is whose hook this is.
type Kind int

const (
	// Parent runs for the environment session's identity, sending
	// Grelot-Self.
	Parent Kind = iota
	// SameAgent is the same agent going on in a new session: it runs as
	// Parent does.
	SameAgent
	// Child is a subagent, a workflow step or a fork beside a parent that
	// goes on: it never acts for the parent.
	Child
	// Replaced is a session a same-agent fork has replaced: it prints
	// nothing.
	Replaced
)

// Class is a hook's classification.
type Class struct {
	// Stdin is the hook input's session id, "" when the provider names
	// no session.stdin or the input holds none.
	Stdin string
	Kind  Kind
	// Fork marks a Child whose record names a parent: a second chat beside
	// it, which gets the fork's one line.
	Fork bool
}

// Shell is what classification reads of the hook's process.
type Shell struct {
	Lookup func(name string) (string, bool)
	Home   func() (string, error)
	// Session is the environment's host session id, without the provider.
	Session string
}

// maxRecord caps a host session record read.
const maxRecord = 1 << 20

// Classify decides whose hook in is. agentID is the role's stdin path of
// the subagent id. A SameAgent result records its parent as replaced in
// marks, so the old session's hooks go quiet from then on.
func Classify(p *provider.Provider, in Input, agentID string, sh Shell, marks *Marks) Class {
	c := Class{Stdin: in.String(p.Session.Stdin), Kind: Parent}
	if c.Stdin != "" && marks.Has(markReplaced, p.Name+":"+c.Stdin) {
		c.Kind = Replaced
		return c
	}
	if in.String(agentID) != "" {
		c.Kind = Child
		return c
	}
	rec := p.Session.Record
	if rec == nil || c.Stdin == "" || c.Stdin == sh.Session {
		return c
	}
	parent, reason, err := readRecord(rec, c.Stdin, sh)
	switch {
	case err != nil, parent == "":
		c.Kind = Child
	case slices.Contains(rec.SameAgent, reason):
		c.Kind = SameAgent
		marks.Set(markReplaced, p.Name+":"+parent)
	default:
		c.Kind, c.Fork = Child, true
	}
	return c
}

// readRecord reads the parent and reason fields of session's own record
// in the host's state directory.
func readRecord(rec *provider.Record, session string, sh Shell) (parent, reason string, err error) {
	dir, err := recordDir(rec, sh)
	if err != nil {
		return "", "", err
	}
	glob, err := rec.RenderGlob(session)
	if err != nil {
		return "", "", err
	}
	fsys := os.DirFS(dir)
	matches, err := fs.Glob(fsys, glob)
	if err != nil {
		return "", "", err
	}
	if len(matches) == 0 {
		return "", "", fs.ErrNotExist
	}
	data, err := statefile.Read(filepath.Join(dir, filepath.FromSlash(matches[0])), maxRecord)
	if err != nil {
		return "", "", err
	}
	in, err := ParseInput(data)
	if err != nil {
		return "", "", err
	}
	return in.String(rec.Parent), in.String(rec.Reason), nil
}

// recordDir is dir_env's value when set, else dir_default, with a leading
// ~ read as the home directory. It must be absolute.
func recordDir(rec *provider.Record, sh Shell) (string, error) {
	dir := rec.DirDefault
	if rec.DirEnv != "" {
		if v, ok := sh.Lookup(rec.DirEnv); ok && v != "" {
			dir = v
		}
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := sh.Home()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(dir[1:], "/")))
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("the session record directory is not absolute")
	}
	return dir, nil
}
