//go:build !windows

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/cplieger/bolttable"
)

// Revival is a revive request from Session, "<provider>:<id>". An empty
// Name revives the session's own dormant identity; a Name revives that
// dormant identity, moving it to Session as a take-over moves a live one.
type Revival struct {
	Where   *Where
	Session string
	Name    string
	Host    string
	OS      string
	Arch    string
	Proc    string
	TTL     time.Duration
}

// Revived is what a revive answers.
type Revived struct {
	Identity Identity
	// Unread counts the deliveries past the identity's read cursor.
	Unread int
	// Moved reports a revive by name that moved the identity to this
	// session.
	Moved bool
}

// Revive makes a dormant identity live again under a new lease, keeping
// its id, follows, cursors and mail, and resetting its nudge state.
// Refusals: KindInvalid for a bad ttl, label or where; KindConflict for a
// session with a live identity, an empty environment on a revive by name,
// or another provider; KindNotFound for no dormant identity to revive, in
// this environment or at all.
func (s *Store) Revive(ctx context.Context, r *Revival) (Revived, error) {
	provider, err := checkSession(r.Session)
	if err == nil {
		err = checkLabels(r.Host, r.OS, r.Arch, r.Proc)
	}
	if err == nil && r.Where == nil {
		err = checkWhere(nil, nil)
	}
	if err != nil {
		return Revived{}, err
	}
	var out Revived
	err = s.update(ctx, func(w *wtx) error {
		var uerr error
		out, uerr = w.revive(r, provider)
		return uerr
	})
	return out, err
}

func (w *wtx) revive(r *Revival, provider string) (Revived, error) {
	ls, err := identityPolicy.grant(w.now, r.TTL, time.Time{})
	if err != nil {
		return Revived{}, err
	}
	own, ownFound, err := w.identityBy(w.s.t.bySession, r.Session)
	if err != nil {
		return Revived{}, err
	}
	if ownFound && own.State == stateLive {
		return Revived{}, refuse(KindConflict,
			fmt.Sprintf("this session already has the live identity %s; revive from a session with none", own.Name))
	}
	row, moved, err := w.reviveTarget(r, provider, &own, ownFound)
	if err != nil {
		return Revived{}, err
	}
	if werr := checkWhere(r.Where, row.Paths); werr != nil {
		return Revived{}, werr
	}
	if moved && ownFound {
		if derr := w.detach(&own); derr != nil {
			return Revived{}, derr
		}
	}
	row.Session, row.Host, row.OS, row.Arch, row.Proc = r.Session, r.Host, r.OS, r.Arch, r.Proc
	row.Where = Where{
		Machine: r.Where.Machine, Environment: r.Where.Environment,
		Checkouts: mergeCheckouts(row.Where.Checkouts, r.Where.Checkouts, row.Paths),
	}
	if cerr := w.repoConflict(&row); cerr != nil {
		return Revived{}, cerr
	}
	row.TTL, row.Lease = r.TTL, ls
	row.State, row.DormantSince = stateLive, time.Time{}
	w.touch(&row)
	row.Nudged, row.Nudges, row.NudgedAt = 0, 0, time.Time{}
	if perr := w.s.t.identities.table.Put(w.tx, row.ID, &row); perr != nil {
		return Revived{}, perr
	}
	// The left card told the others it had gone.
	w.queueCards(kindJoin, &row, row.Paths)
	unread, err := w.s.unread(w.r, &row, w.now)
	if err != nil {
		return Revived{}, err
	}
	w.s.log.Info("identity revived", "name", row.Name, "id", row.ID, "provider", provider, "moved", moved)
	return Revived{Identity: w.s.view(&row), Unread: unread, Moved: moved}, nil
}

// reviveTarget finds the dormant row r revives, checking a revive by name
// in the order of a take-over, and reports whether it moves sessions.
func (w *wtx) reviveTarget(r *Revival, provider string, own *identityRow, ownFound bool) (identityRow, bool, error) {
	if r.Name == "" || ownFound && own.Name == r.Name {
		if !ownFound {
			return identityRow{}, false, refuse(KindNotFound,
				"this session has no dormant identity: run grelot revive <name> --ttl <d> to revive one by name from its own environment")
		}
		return *own, false, nil
	}
	if r.Where.Environment == "" {
		return identityRow{}, false, w.s.refuseMove(verbRevive, r.Name, noEnvironment())
	}
	row, found, err := w.identityBy(w.s.t.byName, r.Name)
	if err != nil {
		return identityRow{}, false, err
	}
	if !found || row.State != stateDormant {
		return identityRow{}, false, refuse(KindNotFound, "no dormant identity "+r.Name)
	}
	if err := w.s.moveRefusal(verbRevive, &row, *r.Where, provider); err != nil {
		return identityRow{}, false, err
	}
	return row, true, nil
}

// TakeOverHint finds the identity a new session may be the continuation
// of: one registered by another session under proc, live or dormant, that
// shares the caller's environment and has the session's provider, ranked
// by hintBeats. It never moves one. Refusals: KindInvalid for a bad
// session, machine or environment.
func (s *Store) TakeOverHint(ctx context.Context, session, proc string, caller Where) (Identity, bool, error) {
	provider, err := checkSession(session)
	if err == nil {
		err = checkWhere(&caller, nil)
	}
	if err != nil || proc == "" {
		return Identity{}, false, err
	}
	var best *identityRow
	err = s.read(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		return scanExact(r, s.t.byProc, proc, func(id uint64) (bool, error) {
			row, found, gerr := s.t.identities.table.Get(r, id)
			if gerr != nil || !found {
				return gerr == nil, gerr
			}
			v := settled(&row, now, s.dormantTTL)
			if v != nil && v.Session != session && v.Provider == provider && shareEnvironment(caller, v.Where) && hintBeats(v, best) {
				best = v
			}
			return true, nil
		})
	})
	if err != nil || best == nil {
		return Identity{}, false, err
	}
	return s.view(best), true, nil
}

// hintBeats reports whether v is a better take-over hint than best: live
// before dormant, then the newest registration.
func hintBeats(v, best *identityRow) bool {
	if best == nil || v.State != best.State {
		return best == nil || v.State == stateLive
	}
	return v.ID > best.ID
}
