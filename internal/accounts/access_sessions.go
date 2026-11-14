//go:build !windows

package accounts

import (
	"context"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/access/account"
	"github.com/cplieger/bolttable"
)

// sessionByHash returns the ID and row of the session holding hash.
func (r reader) sessionByHash(hash access.Hash) (id []byte, v sessionRow, found bool, err error) {
	if hash.IsZero() {
		return nil, v, false, nil
	}
	id, _, found, err = r.t.sessionByHash.Lookup(r.r, []byte(hash.Reveal()))
	if err != nil || !found {
		return nil, v, false, err
	}
	v, found, err = r.t.sessions.Get(r.r, id)
	return id, v, found, err
}

// ownedSession returns ref's session row when it belongs to ref.UserID.
func (r reader) ownedSession(ref access.SessionRef) (sessionRow, bool, error) {
	if ref.ID == "" || ref.UserID == 0 {
		return sessionRow{}, false, nil
	}
	v, found, err := r.t.sessions.Get(r.r, []byte(ref.ID))
	if err != nil || !found || v.UserID != ref.UserID {
		return sessionRow{}, false, err
	}
	return v, true, nil
}

// SessionByHash implements account.SessionStore.
func (r reader) SessionByHash(_ context.Context, tokenHash access.Hash) (*access.Session, bool, error) {
	id, v, found, err := r.sessionByHash(tokenHash)
	if err != nil || !found {
		return nil, false, err
	}
	return v.session(id), true, nil
}

// SessionsByUserID implements account.SessionStore.
func (r reader) SessionsByUserID(_ context.Context, userID int64) ([]access.Session, error) {
	return rows(r.r, r.t.sessions, r.t.sessionByUser, be64(userID),
		func(id []byte, v *sessionRow) access.Session { return *v.session(id) })
}

// CreateSession implements account.SessionStore.
func (w *writer) CreateSession(_ context.Context, sess *access.Session) error {
	switch {
	case sess.TokenHash.IsZero():
		return errNoHash
	case sess.ID == "":
		return errNoSessionID
	}
	_, taken, err := w.t.sessions.Get(w.r, []byte(sess.ID))
	if err != nil {
		return err
	}
	if taken {
		return errSessionIDTaken
	}
	row := toSessionRow(sess)
	return conflict(w.t.sessions.Put(w.tx, []byte(sess.ID), &row))
}

// UpdateSessionActivity implements account.SessionStore.
func (w *writer) UpdateSessionActivity(_ context.Context, tokenHash access.Hash, act access.SessionActivity) error {
	id, v, found, err := w.sessionByHash(tokenHash)
	if err != nil || !found || !act.At.After(v.LastActivity) {
		return err
	}
	v.LastActivity, v.LastIP = act.At, act.IP
	return w.t.sessions.Put(w.tx, id, &v)
}

// RotateSession implements account.SessionStore.
func (w *writer) RotateSession(_ context.Context, rot account.Rotation) error { //nolint:gocritic // hugeParam: account.SessionStore's signature
	cur, found, err := w.ownedSession(access.SessionRef{ID: rot.ID, UserID: rot.UserID})
	if err != nil {
		return err
	}
	if !found || (!rot.ExpectHash.IsZero() && !rot.ExpectHash.Equal(access.NewHash(cur.TokenHash))) {
		return account.ErrNotFound
	}
	next := rot.Next
	switch {
	case next.ID != rot.ID || next.UserID != rot.UserID:
		return errRotationIdentity
	case next.TokenHash.IsZero():
		return errNoHash
	}
	if cur.LastActivity.After(next.LastActivity) {
		next.LastActivity, next.LastIP = cur.LastActivity, cur.LastIP
	}
	row := toSessionRow(&next)
	return conflict(w.t.sessions.Put(w.tx, []byte(rot.ID), &row))
}

// UpdateSessionLabel implements account.SessionStore.
func (w *writer) UpdateSessionLabel(_ context.Context, ref access.SessionRef, label string) error {
	v, found, err := w.ownedSession(ref)
	if err != nil || !found {
		return notFound(err)
	}
	v.Label = label
	return w.t.sessions.Put(w.tx, []byte(ref.ID), &v)
}

// DeleteSession implements account.SessionStore.
func (w *writer) DeleteSession(_ context.Context, ref access.SessionRef) error {
	_, found, err := w.ownedSession(ref)
	if err != nil || !found {
		return notFound(err)
	}
	_, err = w.t.sessions.Delete(w.tx, []byte(ref.ID))
	return err
}

// DeleteSessionByHash implements account.SessionStore.
func (w *writer) DeleteSessionByHash(_ context.Context, tokenHash access.Hash) error {
	id, _, found, err := w.sessionByHash(tokenHash)
	if err != nil || !found {
		return err
	}
	_, err = w.t.sessions.Delete(w.tx, id)
	return err
}

// DeleteUserSessions implements account.SessionStore.
func (w *writer) DeleteUserSessions(_ context.Context, userID int64, exceptID string) ([]string, error) {
	keys, err := ids(w.r, w.t.sessionByUser, be64(userID))
	if err != nil {
		return nil, err
	}
	removed := []string{}
	for _, id := range keys {
		if string(id) == exceptID {
			continue
		}
		if _, err := w.t.sessions.Delete(w.tx, id); err != nil {
			return nil, err
		}
		removed = append(removed, string(id))
	}
	return removed, nil
}

// CleanupExpiredSessions implements account.Sweeper: it deletes, in session
// ID order, at most limit of the sessions access.ValidateSession refuses.
func (s *Store) CleanupExpiredSessions(ctx context.Context, now time.Time, timeouts access.SessionTimeouts, limit int) ([]access.SessionRef, error) {
	switch {
	case limit < 1:
		return nil, errCleanupLimit
	case timeouts.Idle <= 0 || timeouts.Absolute <= 0:
		return nil, errCleanupTimeouts
	}
	return writeResult(ctx, s, func(w *writer) ([]access.SessionRef, error) {
		var expired []access.SessionRef
		err := w.t.sessions.Scan(w.r, bolttable.Range{}, func(id []byte, v *sessionRow) (bool, error) {
			if sess := v.session(id); access.ValidateSession(sess, timeouts, now) != nil {
				expired = append(expired, access.SessionRef{ID: sess.ID, UserID: sess.UserID})
			}
			return len(expired) < limit, nil
		})
		if err != nil {
			return nil, err
		}
		for _, ref := range expired {
			if _, err := w.t.sessions.Delete(w.tx, []byte(ref.ID)); err != nil {
				return nil, err
			}
		}
		return expired, nil
	})
}

// CreateSession implements account.SessionStore.
func (s *Store) CreateSession(ctx context.Context, sess *access.Session) error {
	return s.write(ctx, func(w *writer) error { return w.CreateSession(ctx, sess) })
}

// SessionByHash implements access.SessionReader and account.SessionStore.
func (s *Store) SessionByHash(ctx context.Context, tokenHash access.Hash) (*access.Session, bool, error) {
	return readFound(ctx, s, func(r reader) (*access.Session, bool, error) { return r.SessionByHash(ctx, tokenHash) })
}

// SessionsByUserID implements account.SessionStore.
func (s *Store) SessionsByUserID(ctx context.Context, userID int64) ([]access.Session, error) {
	return readValue(ctx, s, func(r reader) ([]access.Session, error) { return r.SessionsByUserID(ctx, userID) })
}

// UpdateSessionActivity implements access.SessionActivityUpdater and
// account.SessionStore.
func (s *Store) UpdateSessionActivity(ctx context.Context, tokenHash access.Hash, act access.SessionActivity) error {
	return s.write(ctx, func(w *writer) error { return w.UpdateSessionActivity(ctx, tokenHash, act) })
}

// RotateSession implements account.SessionStore.
func (s *Store) RotateSession(ctx context.Context, rot account.Rotation) error { //nolint:gocritic // hugeParam: account.SessionStore's signature
	return s.write(ctx, func(w *writer) error { return w.RotateSession(ctx, rot) })
}

// UpdateSessionLabel implements account.SessionStore.
func (s *Store) UpdateSessionLabel(ctx context.Context, ref access.SessionRef, label string) error {
	return s.write(ctx, func(w *writer) error { return w.UpdateSessionLabel(ctx, ref, label) })
}

// DeleteSession implements account.SessionStore.
func (s *Store) DeleteSession(ctx context.Context, ref access.SessionRef) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteSession(ctx, ref) })
}

// DeleteSessionByHash implements account.SessionStore.
func (s *Store) DeleteSessionByHash(ctx context.Context, tokenHash access.Hash) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteSessionByHash(ctx, tokenHash) })
}

// DeleteUserSessions implements account.SessionStore.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64, exceptID string) ([]string, error) {
	return writeResult(ctx, s, func(w *writer) ([]string, error) { return w.DeleteUserSessions(ctx, userID, exceptID) })
}
