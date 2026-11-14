//go:build !windows

package accounts

import (
	"context"

	"github.com/cplieger/access"
	"github.com/cplieger/access/account"
)

func (r reader) userBy(k int64, found bool, err error) (*access.User, bool, error) {
	if err != nil || !found {
		return nil, false, err
	}
	v, found, err := r.t.users.Get(r.r, k)
	if err != nil || !found {
		return nil, false, err
	}
	return v.user(k), true, nil
}

// UserByID implements account.UserStore.
func (r reader) UserByID(_ context.Context, id int64) (*access.User, bool, error) {
	return r.userBy(id, id > 0, nil)
}

// UserByUsername implements account.UserStore.
func (r reader) UserByUsername(_ context.Context, canonical string) (*access.User, bool, error) {
	if canonical == "" {
		return nil, false, nil
	}
	k, _, found, err := r.t.userByName.Lookup(r.r, []byte(canonical))
	return r.userBy(k, found, err)
}

// UserByOIDC implements account.UserStore.
func (r reader) UserByOIDC(_ context.Context, id access.OIDCIdentity) (*access.User, bool, error) {
	key := oidcKey(id.Issuer, id.Subject)
	if key == nil {
		return nil, false, nil
	}
	k, _, found, err := r.t.userByOIDC.Lookup(r.r, key)
	return r.userBy(k, found, err)
}

// UserByWebAuthnHandle implements account.UserStore.
func (r reader) UserByWebAuthnHandle(_ context.Context, handle []byte) (*access.User, bool, error) {
	if len(handle) == 0 {
		return nil, false, nil
	}
	k, _, found, err := r.t.userByHandle.Lookup(r.r, handle)
	return r.userBy(k, found, err)
}

// ListUsers implements account.UserStore.
func (r reader) ListUsers(context.Context) ([]access.User, error) {
	return all(r.r, r.t.users, func(k int64, v *userRow) access.User { return *v.user(k) })
}

// UserCount implements account.UserStore.
func (r reader) UserCount(context.Context) (int, error) {
	n, err := r.t.userCount.Value(r.r)
	return int(n), err
}

// CreateUser implements account.UserStore.
func (w *writer) CreateUser(_ context.Context, user *access.User) error {
	id, err := nextID(w.tx, w.t.users)
	if err != nil {
		return err
	}
	row := toUserRow(user)
	if err := w.t.users.Put(w.tx, id, &row); err != nil {
		return conflict(err)
	}
	user.ID = id
	return nil
}

// CreateInitialUser implements account.UserStore.
func (w *writer) CreateInitialUser(ctx context.Context, user *access.User) error {
	n, err := w.UserCount(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return account.ErrSetupComplete
	}
	return w.CreateUser(ctx, user)
}

// UpdateUser implements account.UserStore.
func (w *writer) UpdateUser(_ context.Context, user *access.User) error {
	if user.ID <= 0 {
		return account.ErrNotFound
	}
	_, found, err := w.t.users.Get(w.r, user.ID)
	if err != nil || !found {
		return notFound(err)
	}
	row := toUserRow(user)
	return conflict(w.t.users.Put(w.tx, user.ID, &row))
}

// DeleteUser implements account.UserStore; the schema's cascades remove the
// user's sessions, passkeys, keys, tokens and attributions with it.
func (w *writer) DeleteUser(_ context.Context, id int64) error {
	if id <= 0 {
		return account.ErrNotFound
	}
	deleted, err := w.t.users.Delete(w.tx, id)
	if err != nil || !deleted {
		return notFound(err)
	}
	return nil
}

// CreateUser implements account.UserStore.
func (s *Store) CreateUser(ctx context.Context, user *access.User) error {
	return s.write(ctx, func(w *writer) error { return w.CreateUser(ctx, user) })
}

// CreateInitialUser implements account.UserStore.
func (s *Store) CreateInitialUser(ctx context.Context, user *access.User) error {
	return s.write(ctx, func(w *writer) error { return w.CreateInitialUser(ctx, user) })
}

// UserByID implements account.UserStore.
func (s *Store) UserByID(ctx context.Context, id int64) (*access.User, bool, error) {
	return readFound(ctx, s, func(r reader) (*access.User, bool, error) { return r.UserByID(ctx, id) })
}

// UserByUsername implements account.UserStore.
func (s *Store) UserByUsername(ctx context.Context, canonical string) (*access.User, bool, error) {
	return readFound(ctx, s, func(r reader) (*access.User, bool, error) { return r.UserByUsername(ctx, canonical) })
}

// UserByOIDC implements account.UserStore.
func (s *Store) UserByOIDC(ctx context.Context, id access.OIDCIdentity) (*access.User, bool, error) {
	return readFound(ctx, s, func(r reader) (*access.User, bool, error) { return r.UserByOIDC(ctx, id) })
}

// UserByWebAuthnHandle implements account.UserStore.
func (s *Store) UserByWebAuthnHandle(ctx context.Context, handle []byte) (*access.User, bool, error) {
	return readFound(ctx, s, func(r reader) (*access.User, bool, error) { return r.UserByWebAuthnHandle(ctx, handle) })
}

// ListUsers implements account.UserStore.
func (s *Store) ListUsers(ctx context.Context) ([]access.User, error) {
	return readValue(ctx, s, func(r reader) ([]access.User, error) { return r.ListUsers(ctx) })
}

// UpdateUser implements account.UserStore.
func (s *Store) UpdateUser(ctx context.Context, user *access.User) error {
	return s.write(ctx, func(w *writer) error { return w.UpdateUser(ctx, user) })
}

// DeleteUser implements account.UserStore.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteUser(ctx, id) })
}

// UserCount implements account.UserStore.
func (s *Store) UserCount(ctx context.Context) (int, error) {
	return readValue(ctx, s, func(r reader) (int, error) { return r.UserCount(ctx) })
}
