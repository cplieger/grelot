//go:build !windows

package store

import (
	"context"
	"fmt"
	"slices"
)

// Follow adds the live path thread at path to c's identity's
// follows and returns the identity; following it again changes nothing.
// Refusals: KindInvalid for a bad path; KindGone for a session with no
// live identity; KindForbidden for a delegate; KindNotFound for no live
// thread there; KindConflict at the follow cap.
func (s *Store) Follow(ctx context.Context, k Keyed, c Caller, path string) (Identity, error) {
	if _, err := checkPaths([]string{path}); err != nil {
		return Identity{}, err
	}
	return keyedResult(ctx, s, k, func(w *wtx) (Identity, error) {
		_, live, err := w.thread(path)
		if err != nil {
			return Identity{}, err
		}
		me, err := w.liveAs(c)
		if err != nil {
			return Identity{}, err
		}
		switch {
		case me.Parent != 0:
			return Identity{}, refuse(KindForbidden, "a delegate follows nothing: its scope decides what it receives")
		case !live:
			return Identity{}, refuse(KindNotFound, fmt.Sprintf("no thread at %s; post there to start one", path))
		case slices.Contains(me.Follows, path):
		case len(me.Follows) >= maxFollows:
			return Identity{}, refuse(KindConflict, fmt.Sprintf("you follow %d threads, the most allowed: grelot unfollow one first", maxFollows))
		default:
			me.Follows = append(me.Follows, path)
		}
		w.touch(&me)
		if err := s.t.identities.table.Put(w.tx, me.ID, &me); err != nil {
			return Identity{}, err
		}
		return s.view(&me), nil
	})
}

// Unfollow takes path out of c's identity's follows and returns the
// identity; a path it does not follow changes nothing. A thread at or
// under a work path or a held reservation cannot be muted, since that
// delivers it. Refusals: KindInvalid for a bad path; KindGone for a
// session with no live identity; KindConflict under a work path or a held
// reservation.
func (s *Store) Unfollow(ctx context.Context, k Keyed, c Caller, path string) (Identity, error) {
	if _, err := checkPaths([]string{path}); err != nil {
		return Identity{}, err
	}
	return keyedResult(ctx, s, k, func(w *wtx) (Identity, error) {
		held, err := w.heldOverlapping(path)
		if err != nil {
			return Identity{}, err
		}
		me, err := w.liveAs(c)
		if err != nil {
			return Identity{}, err
		}
		if i := slices.IndexFunc(me.Paths, func(p string) bool { return under(path, p) }); i >= 0 {
			return Identity{}, refuse(KindConflict, fmt.Sprintf(
				"%s is under your work path %s, which delivers it whatever you follow: narrow your paths with grelot set --paths to stop hearing it", path, me.Paths[i],
			))
		}
		if i := slices.IndexFunc(held, func(r reservationRow) bool { return r.Holder == me.ID && under(path, r.Path) }); i >= 0 {
			return Identity{}, refuse(KindConflict, fmt.Sprintf(
				"%s is under your reservation %d on %s, which delivers it whatever you follow: release %d first", path, held[i].ID, held[i].Path, held[i].ID,
			))
		}
		me.Follows = slices.DeleteFunc(me.Follows, func(f string) bool { return f == path })
		w.touch(&me)
		if err := s.t.identities.table.Put(w.tx, me.ID, &me); err != nil {
			return Identity{}, err
		}
		return w.viewOf(&me)
	})
}
