//go:build !windows

package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Followed is what a follow or an unfollow answers: the identity, and
// lines for the caller, such as no thread living at the path yet.
type Followed struct {
	Notices  []string
	Identity Identity
}

// Follow adds path to c's identity's follows, so every post in a thread
// at or under path reaches it, the first post included when no thread
// lives there yet; a path its work paths or follows already reach is not
// added, and the follow replaces those under it. It lasts until an
// unfollow or the identity's end, across the threads' expiries.
// Refusals: KindInvalid for a bad path; KindGone for a session with no
// live identity; KindForbidden for a delegate; KindFull for a follow it
// would add past 90 % of Options.MaxBytes.
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
		if me.Parent != 0 {
			return Followed{}, refuse(KindForbidden, "a delegate follows nothing: its scope decides what it receives")
		}
		notices, err := w.followPath(&me, path)
		if err != nil {
			return Followed{}, err
		}
		if len(me.Follows) > 0 {
			me.Threaded = true
		}
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

// Unfollow takes path out of c's identity's follows. Its answer names
// what still delivers path's threads, a work path, a held reservation or
// a follow above path, since an unfollow mutes none of them. A path it
// does not follow changes nothing.
// Refusals: KindInvalid for a bad path; KindGone for a session with no
// live identity; KindConflict for a path it does not follow that one of
// those delivers, which no unfollow can mute.
func (s *Store) Unfollow(ctx context.Context, k Keyed, c Caller, path string) (Followed, error) {
	if _, err := checkPaths([]string{path}); err != nil {
		return Followed{}, err
	}
	return keyedResult(ctx, s, k, func(w *wtx) (Followed, error) {
		held, err := w.heldOverlapping(path)
		if err != nil {
			return Followed{}, err
		}
		me, err := w.liveAs(c)
		if err != nil {
			return Followed{}, err
		}
		still := deliveredBy(&me, held, path)
		if still != "" && !slices.Contains(me.Follows, path) {
			return Followed{}, refuse(KindConflict, still)
		}
		me.Follows = slices.DeleteFunc(me.Follows, func(f string) bool { return f == path })
		w.touch(&me)
		if err := s.t.identities.table.Put(w.tx, me.ID, &me); err != nil {
			return Followed{}, err
		}
		var notices []string
		if still != "" {
			notices = []string{still}
		}
		v, verr := w.viewOf(&me)
		return Followed{Identity: v, Notices: notices}, verr
	})
}

// deliveredBy says which of me's work paths, held reservations or other
// follows delivers path's threads whatever me follows, and how to stop
// it; "" when none does.
func deliveredBy(me *identityRow, held []reservationRow, path string) string {
	if i := slices.IndexFunc(me.Paths, func(p string) bool { return under(path, p) }); i >= 0 {
		return fmt.Sprintf("%s is under your work path %s, which delivers it whatever you follow: narrow your paths with grelot set --paths to stop hearing it", path, me.Paths[i])
	}
	if i := slices.IndexFunc(held, func(r reservationRow) bool { return r.Holder == me.ID && under(path, r.Path) }); i >= 0 {
		return fmt.Sprintf("%s is under your reservation %d on %s, which delivers it whatever you follow: release %d first", path, held[i].ID, held[i].Path, held[i].ID)
	}
	if i := slices.IndexFunc(me.Follows, func(f string) bool { return f != path && under(path, f) }); i >= 0 {
		return fmt.Sprintf("%s is under your follow of %s, which delivers it whatever else you follow: unfollow that one to stop hearing it", path, me.Follows[i])
	}
	return ""
}

// followPath is Follow's change to me, and the notice saying why nothing
// was added or which follows the new one replaced.
func (w *wtx) followPath(me *identityRow, path string) ([]string, error) {
	if slices.Contains(me.Follows, path) {
		return nil, nil
	}
	if by := reachedBy(me, path); by != "" {
		return []string{fmt.Sprintf("%s already reaches %s, so nothing was added", by, path)}, nil
	}
	if err := w.grow(); err != nil {
		return nil, err
	}
	replaced := addFollow(me, path)
	if len(replaced) == 0 {
		return nil, nil
	}
	return []string{"this follow replaces your follows under it: " + strings.Join(replaced, ", ")}, nil
}

// addFollow makes me follow p in place of the follows under p, and
// returns those.
func addFollow(me *identityRow, p string) (replaced []string) {
	var kept []string
	for _, f := range me.Follows {
		if under(f, p) {
			replaced = append(replaced, f)
		} else {
			kept = append(kept, f)
		}
	}
	kept = append(kept, p)
	me.Follows = kept
	return replaced
}
