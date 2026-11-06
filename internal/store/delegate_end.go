//go:build !windows

package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Revoked is what a revoke ended: the live delegates of the label, by
// handle, and whether an unused link of the label went too.
type Revoked struct {
	Handles []string
	Link    bool
}

// Revoke ends c's live delegates of label and deletes its unused link of
// label. A handle keeps the parent's name it was granted under, so a
// delegate is matched by its label alone. Refusals: KindInvalid for a bad
// label, KindForbidden for a delegate caller, KindNotFound when nothing has
// the label, KindGone for a session with no live identity.
func (s *Store) Revoke(ctx context.Context, k Keyed, c Caller, label string) (Revoked, error) {
	if err := checkLabel(label); err != nil {
		return Revoked{}, err
	}
	return keyedResult(ctx, s, k, func(w *wtx) (Revoked, error) { return w.revoke(c, label) })
}

func (w *wtx) revoke(c Caller, label string) (Revoked, error) {
	me, err := w.liveAs(c)
	if err != nil {
		return Revoked{}, err
	}
	if me.Parent != 0 {
		return Revoked{}, refuse(KindForbidden, "a delegate has no delegates to revoke")
	}
	var out Revoked
	if out.Handles, err = w.revokeDelegates(me.ID, label); err != nil {
		return Revoked{}, err
	}
	if out.Link, err = w.revokeLink(&me, label); err != nil {
		return Revoked{}, err
	}
	if len(out.Handles) == 0 && !out.Link {
		return Revoked{}, refuse(KindNotFound, fmt.Sprintf("no live delegate or unused link is labelled %s: grelot who lists your delegates", label))
	}
	if w.seen(&me) {
		err = w.s.t.identities.table.Put(w.tx, me.ID, &me)
	}
	return out, err
}

// revokeDelegates ends parent's live delegates of label and returns their
// handles.
func (w *wtx) revokeDelegates(parent uint64, label string) ([]string, error) {
	ids, err := w.s.delegatesOf(w.r, parent)
	if err != nil {
		return nil, err
	}
	var handles []string
	for _, id := range ids {
		d, found, serr := w.s.t.identities.settle(w, id)
		if serr != nil {
			return nil, serr
		}
		if _, l, _ := strings.Cut(d.Name, "/"); !found || l != label {
			continue
		}
		if eerr := w.endDelegate(&d, endRevoke); eerr != nil {
			return nil, eerr
		}
		handles = append(handles, d.Name)
	}
	return handles, nil
}

// revokeLink deletes me's unused link of label, reporting whether it had
// one.
func (w *wtx) revokeLink(me *identityRow, label string) (bool, error) {
	links, err := w.unexpiredLinks(me.ID)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(links, func(l linkRow) bool { return l.Label == label })
	if i < 0 {
		return false, nil
	}
	if _, err := w.s.t.links.table.Delete(w.tx, pairKey(me.ID, links[i].ID)); err != nil {
		return false, err
	}
	w.logInfo("delegate link revoked", "name", me.Name, "label", label)
	return true, nil
}

// SubagentStop ends the session's own identity when that is a delegate
// registered from a session of its own. It sits outside the delegate gate
// and changes nothing for any other session, since hosts report their
// internal agents' stops too and a parent's session names no subagent.
func (s *Store) SubagentStop(ctx context.Context, session string) error {
	return s.update(ctx, func(w *wtx) error {
		own, found, err := w.identityBy(s.t.bySession, session)
		if err != nil || !found || own.State != stateLive || own.Parent == 0 {
			return err
		}
		return w.endDelegate(&own, endSubagent)
	})
}
