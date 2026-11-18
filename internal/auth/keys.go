//go:build !windows

package auth

import (
	"context"
	"slices"

	"github.com/cplieger/access"
)

// KeyLive reports whether the key with ID id still authenticates: it
// exists, has not expired and its owner is enabled. It reads the accounts
// file, whose reads never wait on a writer.
func (a *Auth) KeyLive(id int64) (bool, error) {
	ctx := context.Background()
	keys, err := a.store.ListAPIKeys(ctx)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(keys, func(k access.Key) bool { return k.ID == id })
	if i < 0 {
		return false, nil
	}
	if k := &keys[i]; !k.ExpiresAt.IsZero() && !a.now().Before(k.ExpiresAt) {
		return false, nil
	}
	u, found, err := a.store.UserByID(ctx, keys[i].UserID)
	if err != nil {
		return false, err
	}
	return found && u.Enabled, nil
}
