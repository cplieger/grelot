//go:build !windows

package store

import (
	"fmt"
	"slices"
	"strings"

	"github.com/cplieger/bolttable"
)

// allowsPath reports whether sc covers the path thread at p, to post in
// and to receive: an allowed thread at or above p and no denied one.
func (sc *scope) allowsPath(p string) bool {
	in := func(root string) bool { return under(p, root) }
	return !slices.ContainsFunc(sc.DenyThreads, in) && slices.ContainsFunc(sc.AllowThreads, in)
}

func (sc *scope) allowsAnnounce() bool { return sc.Announce && !sc.DenyAnnounce }

// allowsAgent reports whether delegate d's scope covers its direct thread
// with other: its parent always, which no deny can name; a sibling or an
// allowed agent unless denied.
func allowsAgent(d, other *identityRow) bool {
	switch {
	case other.ID == d.Parent:
		return true
	case slices.Contains(d.Scope.DenyAgents, other.ID):
		return false
	}
	return other.Parent == d.Parent || slices.Contains(d.Scope.AllowAgents, other.ID)
}

// scopeRefusal is the 403 for a post outside delegate d's scope, naming
// what it allows and denies.
func (s *Store) scopeRefusal(r *bolttable.ReadTx, d *identityRow) error {
	name := func(id uint64) (string, bool, error) {
		v, found, err := s.t.identities.table.Get(r, id)
		return "@" + v.Name, found, err
	}
	parent, _, err := name(d.Parent)
	if err != nil {
		return err
	}
	allowed := append([]string{parent + " and its delegates"}, d.Scope.AllowThreads...)
	denied := slices.Clone(d.Scope.DenyThreads)
	if d.Scope.Announce {
		allowed = append(allowed, "announcements")
	}
	if d.Scope.DenyAnnounce {
		denied = append(denied, "announcements")
	}
	names := func(out []string, ids []uint64) ([]string, error) {
		for _, id := range ids {
			n, found, nerr := name(id)
			if nerr != nil {
				return nil, nerr
			}
			if found {
				out = append(out, n)
			}
		}
		return out, nil
	}
	if allowed, err = names(allowed, d.Scope.AllowAgents); err != nil {
		return err
	}
	if denied, err = names(denied, d.Scope.DenyAgents); err != nil {
		return err
	}
	msg := fmt.Sprintf("outside @%s's scope: allowed %s", d.Name, strings.Join(allowed, ", "))
	if len(denied) > 0 {
		msg += "; denied " + strings.Join(denied, ", ")
	}
	return refuse(KindForbidden, msg)
}

// whereOf is the where v is judged by. A delegate's row stores none: it
// runs in its parent's environment, so its parent's current one is read.
func (s *Store) whereOf(r *bolttable.ReadTx, v *identityRow) (Where, error) {
	if v.Parent == 0 {
		return v.Where, nil
	}
	p, _, err := s.t.identities.table.Get(r, v.Parent)
	return p.Where, err
}

// viewOf is row as its own session sees it, a delegate with its parent's
// where.
func (w *wtx) viewOf(row *identityRow) (Identity, error) {
	where, err := w.s.whereOf(w.r, row)
	v := *row
	v.Where = where
	return w.s.view(&v), err
}

// applyDelegateFields applies a delegate's post or set: its name is its
// handle and its scope replaces paths, so only its description changes.
func applyDelegateFields(row *identityRow, f Fields, mode fieldMode) error {
	keep := func(v string) bool { return v == "" || v == NoChange }
	if !keep(f.Name) || len(f.Paths) > 1 || len(f.Paths) == 1 && !keep(f.Paths[0]) {
		return refuse(KindInvalid, fmt.Sprintf(
			"a delegate's name is its handle and its scope replaces paths: pass only --desc, or %q", NoChange,
		))
	}
	d, err := resolveText(row.Description, f.Description, mode,
		"description", "--desc <what you are doing and why>", checkDescription)
	if err == nil {
		row.Description = d
	}
	return err
}
