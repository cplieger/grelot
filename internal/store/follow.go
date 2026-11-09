//go:build !windows

package store

import (
	"context"
	"fmt"
	"slices"
)

// Followed is what a follow answers: the identity, and lines for the
// caller, such as no thread living at the path yet.
type Followed struct {
	Notices  []string
	Identity Identity
}

// Follow adds path to c's identity's follows, so every post in a thread
// at or under path reaches it, the first post included when no thread
// lives there yet; a path its follows already cover is not added. A
// follow lasts until an unfollow or the identity's end, across the
// threads' expiries.
// Refusals: KindInvalid for a bad path; KindGone for a session with no
// live identity; KindForbidden for a delegate; KindConflict at the
// follow cap.
func (s *Store) Follow(ctx context.Context, k Keyed, c Caller, path string) (Followed, error) {
	if _, err := checkPaths([]string{path}); err != nil {
		return Followed{}, err
	}
	return keyedResult(ctx, s, k, func(w *wtx) (Followed, error) {
		_, live, err := w.thread(path)
		if err != nil {
			return Followed{}, err
		}
		me, err := w.liveAs(c)
		if err != nil {
			return Followed{}, err
		}
		var notices []string
		switch f := followCovering(&me, path); {
		case me.Parent != 0:
			return Followed{}, refuse(KindForbidden, "a delegate follows nothing: its scope decides what it receives")
		case f == path:
		case f != "":
			notices = append(notices, fmt.Sprintf("your follow of %s already reaches %s, so nothing was added", f, path))
		case len(me.Follows) >= maxFollows:
			return Followed{}, refuse(KindConflict, fmt.Sprintf("you follow %d threads, the most allowed: grelot unfollow one first", maxFollows))
		default:
			me.Follows = append(me.Follows, path)
		}
		me.Threaded = true
		w.touch(&me)
		if err := s.t.identities.table.Put(w.tx, me.ID, &me); err != nil {
			return Followed{}, err
		}
		if !live {
			notices = append(notices, fmt.Sprintf("no thread lives at %s now: its next post reaches you (grelot threads lists the live ones)", path))
		}
		return Followed{Identity: s.view(&me), Notices: notices}, nil
	})
}

// Unfollow takes path out of c's identity's follows and returns the
// identity; a path it does not follow changes nothing. A thread at or
// under a work path, a held reservation or another follow cannot be
// muted, since that delivers it. Refusals: KindInvalid for a bad path;
// KindGone for a session with no live identity; KindConflict under a work
// path, a held reservation or another follow.
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
		if i := slices.IndexFunc(me.Follows, func(f string) bool { return f != path && under(path, f) }); i >= 0 {
			return Identity{}, refuse(KindConflict, fmt.Sprintf(
				"%s is under your follow of %s, which delivers it whatever else you follow: unfollow that one to stop hearing both", path, me.Follows[i],
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
