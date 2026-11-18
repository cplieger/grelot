//go:build !windows

package accounts

import (
	"context"
	"slices"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/access/account"
	"github.com/cplieger/bolttable"
)

// ownedPasskey returns ref's passkey row when it belongs to ref.UserID.
func (r reader) ownedPasskey(ref access.PasskeyRef) (passkeyRow, bool, error) {
	if ref.ID <= 0 || ref.UserID == 0 {
		return passkeyRow{}, false, nil
	}
	v, found, err := r.t.passkeys.Get(r.r, ref.ID)
	if err != nil || !found || v.UserID != ref.UserID {
		return passkeyRow{}, false, err
	}
	return v, true, nil
}

// PasskeysByUserID implements account.PasskeyStore.
func (r reader) PasskeysByUserID(_ context.Context, userID int64) ([]access.PasskeyCredential, error) {
	return rows(r.r, r.t.passkeys, r.t.passkeyByUser, be64(userID), func(id int64, v *passkeyRow) access.PasskeyCredential { return v.passkey(id) })
}

// CreatePasskey implements account.PasskeyStore.
func (w *writer) CreatePasskey(_ context.Context, cred *access.PasskeyCredential) error {
	if len(cred.CredentialID) == 0 {
		return errNoCredentialID
	}
	id, err := nextID(w.tx, w.t.passkeys)
	if err != nil {
		return err
	}
	row := toPasskeyRow(cred)
	if err := w.t.passkeys.Put(w.tx, id, &row); err != nil {
		return conflict(err)
	}
	cred.ID = id
	return nil
}

// UpdatePasskeyAfterLogin implements account.PasskeyStore.
func (w *writer) UpdatePasskeyAfterLogin(_ context.Context, userID int64, credID []byte, upd access.PasskeyLoginUpdate) error {
	if len(credID) == 0 || userID == 0 {
		return account.ErrNotFound
	}
	id, _, found, err := w.t.passkeyByCred.Lookup(w.r, credID)
	if err != nil || !found {
		return notFound(err)
	}
	v, found, err := w.ownedPasskey(access.PasskeyRef{ID: id, UserID: userID})
	if err != nil || !found {
		return notFound(err)
	}
	v.LastUsedAt, v.SignCount, v.RawFlags, v.CloneWarning = upd.At, upd.SignCount, upd.RawFlags, upd.CloneWarning
	v.UserPresent, v.UserVerified, v.BackupEligible, v.BackupState = access.DecodePasskeyFlags(upd.RawFlags)
	return w.t.passkeys.Put(w.tx, id, &v)
}

// RenamePasskey implements account.PasskeyStore.
func (w *writer) RenamePasskey(_ context.Context, ref access.PasskeyRef, name string) error {
	v, found, err := w.ownedPasskey(ref)
	if err != nil || !found {
		return notFound(err)
	}
	v.Name = name
	return w.t.passkeys.Put(w.tx, ref.ID, &v)
}

// DeletePasskey implements account.PasskeyStore.
func (w *writer) DeletePasskey(_ context.Context, ref access.PasskeyRef) error {
	_, found, err := w.ownedPasskey(ref)
	if err != nil || !found {
		return notFound(err)
	}
	_, err = w.t.passkeys.Delete(w.tx, ref.ID)
	return err
}

// DeleteUserPasskeys implements account.PasskeyStore.
func (w *writer) DeleteUserPasskeys(_ context.Context, userID int64) ([]int64, error) {
	keys, err := ids(w.r, w.t.passkeyByUser, be64(userID))
	if err != nil {
		return nil, err
	}
	if _, err := deleteAll(w.tx, w.t.passkeys, keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// ownedKey returns ref's key row when it belongs to ref.UserID.
func (r reader) ownedKey(ref access.KeyRef) (keyRow, bool, error) {
	if ref.ID <= 0 || ref.UserID == 0 {
		return keyRow{}, false, nil
	}
	v, found, err := r.t.keys.Get(r.r, ref.ID)
	if err != nil || !found || v.UserID != ref.UserID {
		return keyRow{}, false, err
	}
	return v, true, nil
}

// APIKeyByHash implements account.KeyStore.
func (r reader) APIKeyByHash(_ context.Context, hash access.Hash) (*access.Key, bool, error) {
	if hash.IsZero() {
		return nil, false, nil
	}
	id, _, found, err := r.t.keyByHash.Lookup(r.r, []byte(hash.Reveal()))
	if err != nil || !found {
		return nil, false, err
	}
	v, found, err := r.t.keys.Get(r.r, id)
	if err != nil || !found {
		return nil, false, err
	}
	k := v.key(id)
	return &k, true, nil
}

// ListAPIKeysByUserID implements account.KeyStore.
func (r reader) ListAPIKeysByUserID(_ context.Context, userID int64) ([]access.Key, error) {
	return rows(r.r, r.t.keys, r.t.keyByUser, be64(userID), func(id int64, v *keyRow) access.Key { return v.key(id) })
}

// ListAPIKeys implements account.KeyStore.
func (r reader) ListAPIKeys(context.Context) ([]access.Key, error) {
	return all(r.r, r.t.keys, func(id int64, v *keyRow) access.Key { return v.key(id) })
}

// CreateAPIKey implements account.KeyStore.
func (w *writer) CreateAPIKey(_ context.Context, key *access.Key) error {
	if key.KeyHash.IsZero() {
		return errNoHash
	}
	id, err := nextID(w.tx, w.t.keys)
	if err != nil {
		return err
	}
	row := toKeyRow(key)
	row.LineageIDs = append(slices.Clone(key.LineageIDs), id)
	if err := w.t.keys.Put(w.tx, id, &row); err != nil {
		return conflict(err)
	}
	key.ID, key.LineageIDs = id, row.LineageIDs
	return nil
}

// DeleteAPIKey implements account.KeyStore.
func (w *writer) DeleteAPIKey(_ context.Context, ref access.KeyRef) error {
	_, found, err := w.ownedKey(ref)
	if err != nil || !found {
		return notFound(err)
	}
	_, err = w.t.keys.Delete(w.tx, ref.ID)
	return err
}

// DeleteUserAPIKeys implements account.KeyStore.
func (w *writer) DeleteUserAPIKeys(_ context.Context, userID int64) ([]int64, error) {
	keys, err := ids(w.r, w.t.keyByUser, be64(userID))
	if err != nil {
		return nil, err
	}
	if _, err := deleteAll(w.tx, w.t.keys, keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// TouchAPIKey implements account.KeyStore.
func (w *writer) TouchAPIKey(_ context.Context, keyID int64, hash access.Hash, at time.Time) error {
	if keyID <= 0 || hash.IsZero() {
		return nil
	}
	v, found, err := w.t.keys.Get(w.r, keyID)
	if err != nil || !found || v.KeyHash != hash.Reveal() || !at.After(v.LastUsedAt) {
		return err
	}
	v.LastUsedAt = at
	return w.t.keys.Put(w.tx, keyID, &v)
}

// UpdateAPIKeySecret implements account.KeyStore.
func (w *writer) UpdateAPIKeySecret(_ context.Context, ref access.KeyRef, s access.KeySecret) error {
	v, found, err := w.ownedKey(ref)
	if err != nil || !found {
		return notFound(err)
	}
	v.KeyHash, v.PreviousHash = s.KeyHash.Reveal(), s.PreviousHash.Reveal()
	v.KeyPrefix, v.KeySuffix, v.PreviousUntil = s.KeyPrefix, s.KeySuffix, s.PreviousUntil
	v.LastUsedAt = time.Time{}
	if err := w.t.keys.Put(w.tx, ref.ID, &v); err != nil {
		return conflict(err)
	}
	return nil
}

// UpdateAPIKeyLabel implements account.KeyStore.
func (w *writer) UpdateAPIKeyLabel(_ context.Context, ref access.KeyRef, label string) error {
	v, found, err := w.ownedKey(ref)
	if err != nil || !found {
		return notFound(err)
	}
	v.Label = label
	return w.t.keys.Put(w.tx, ref.ID, &v)
}

// CreatePasskey implements account.PasskeyStore.
func (s *Store) CreatePasskey(ctx context.Context, cred *access.PasskeyCredential) error {
	return s.write(ctx, func(w *writer) error { return w.CreatePasskey(ctx, cred) })
}

// PasskeysByUserID implements account.PasskeyStore.
func (s *Store) PasskeysByUserID(ctx context.Context, userID int64) ([]access.PasskeyCredential, error) {
	return readValue(ctx, s, func(r reader) ([]access.PasskeyCredential, error) { return r.PasskeysByUserID(ctx, userID) })
}

// UpdatePasskeyAfterLogin implements account.PasskeyStore.
func (s *Store) UpdatePasskeyAfterLogin(ctx context.Context, userID int64, credID []byte, upd access.PasskeyLoginUpdate) error {
	return s.write(ctx, func(w *writer) error { return w.UpdatePasskeyAfterLogin(ctx, userID, credID, upd) })
}

// RenamePasskey implements account.PasskeyStore.
func (s *Store) RenamePasskey(ctx context.Context, ref access.PasskeyRef, name string) error {
	return s.write(ctx, func(w *writer) error { return w.RenamePasskey(ctx, ref, name) })
}

// DeletePasskey implements account.PasskeyStore.
func (s *Store) DeletePasskey(ctx context.Context, ref access.PasskeyRef) error {
	return s.write(ctx, func(w *writer) error { return w.DeletePasskey(ctx, ref) })
}

// DeleteUserPasskeys implements account.PasskeyStore.
func (s *Store) DeleteUserPasskeys(ctx context.Context, userID int64) ([]int64, error) {
	return writeResult(ctx, s, func(w *writer) ([]int64, error) { return w.DeleteUserPasskeys(ctx, userID) })
}

// CreateAPIKey implements account.KeyStore.
func (s *Store) CreateAPIKey(ctx context.Context, key *access.Key) error {
	return s.write(ctx, func(w *writer) error { return w.CreateAPIKey(ctx, key) })
}

// APIKeyByHash implements access.APIKeyReader and account.KeyStore.
func (s *Store) APIKeyByHash(ctx context.Context, hash access.Hash) (*access.Key, bool, error) {
	return readFound(ctx, s, func(r reader) (*access.Key, bool, error) { return r.APIKeyByHash(ctx, hash) })
}

// ListAPIKeysByUserID implements account.KeyStore.
func (s *Store) ListAPIKeysByUserID(ctx context.Context, userID int64) ([]access.Key, error) {
	return readValue(ctx, s, func(r reader) ([]access.Key, error) { return r.ListAPIKeysByUserID(ctx, userID) })
}

// ListAPIKeys implements account.KeyStore.
func (s *Store) ListAPIKeys(ctx context.Context) ([]access.Key, error) {
	return readValue(ctx, s, func(r reader) ([]access.Key, error) { return r.ListAPIKeys(ctx) })
}

// DeleteAPIKey implements account.KeyStore.
func (s *Store) DeleteAPIKey(ctx context.Context, ref access.KeyRef) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteAPIKey(ctx, ref) })
}

// DeleteUserAPIKeys implements account.KeyStore.
func (s *Store) DeleteUserAPIKeys(ctx context.Context, userID int64) ([]int64, error) {
	return writeResult(ctx, s, func(w *writer) ([]int64, error) { return w.DeleteUserAPIKeys(ctx, userID) })
}

// TouchAPIKey implements access.APIKeyToucher and account.KeyStore.
func (s *Store) TouchAPIKey(ctx context.Context, keyID int64, hash access.Hash, at time.Time) error {
	return s.write(ctx, func(w *writer) error { return w.TouchAPIKey(ctx, keyID, hash, at) })
}

// UpdateAPIKeySecret implements account.KeyStore.
func (s *Store) UpdateAPIKeySecret(ctx context.Context, ref access.KeyRef, sec access.KeySecret) error {
	return s.write(ctx, func(w *writer) error { return w.UpdateAPIKeySecret(ctx, ref, sec) })
}

// UpdateAPIKeyLabel implements account.KeyStore.
func (s *Store) UpdateAPIKeyLabel(ctx context.Context, ref access.KeyRef, label string) error {
	return s.write(ctx, func(w *writer) error { return w.UpdateAPIKeyLabel(ctx, ref, label) })
}

// ClearRetiredAPIKeySecrets implements account.Sweeper.
func (s *Store) ClearRetiredAPIKeySecrets(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit < 1 {
		return 0, errCleanupLimit
	}
	return writeResult(ctx, s, func(w *writer) (int, error) {
		retired, err := w.retiredSecrets(now, limit)
		if err != nil {
			return 0, err
		}
		for _, id := range retired {
			if err := w.clearPrevious(id); err != nil {
				return 0, err
			}
		}
		return len(retired), nil
	})
}

// retiredSecrets is up to limit keys whose previous secret no longer
// verifies at now: past its overlap, or the new secret used.
func (w *writer) retiredSecrets(now time.Time, limit int) ([]int64, error) {
	var retired []int64
	err := w.t.keys.Scan(w.r, bolttable.Range{}, func(id int64, v *keyRow) (bool, error) {
		if v.PreviousHash != "" && (!v.PreviousUntil.After(now) || !v.LastUsedAt.IsZero()) {
			retired = append(retired, id)
		}
		return len(retired) < limit, nil
	})
	return retired, err
}

// clearPrevious zeroes key id's previous secret and its overlap.
func (w *writer) clearPrevious(id int64) error {
	v, found, err := w.t.keys.Get(w.r, id)
	if err != nil || !found {
		return err
	}
	v.PreviousHash, v.PreviousUntil = "", time.Time{}
	return w.t.keys.Put(w.tx, id, &v)
}
