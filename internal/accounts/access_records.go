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

// OneTimeTokenByHash implements account.TokenStore.
func (r reader) OneTimeTokenByHash(_ context.Context, hash access.Hash) (access.OneTimeToken, bool, error) {
	if hash.IsZero() {
		return access.OneTimeToken{}, false, nil
	}
	key := []byte(hash.Reveal())
	v, found, err := r.t.tokens.Get(r.r, key)
	if err != nil || !found {
		return access.OneTimeToken{}, false, err
	}
	return v.token(key), true, nil
}

// LiveOneTimeTokens implements account.TokenStore.
func (r reader) LiveOneTimeTokens(_ context.Context, purposes []access.TokenPurpose, now time.Time) ([]access.OneTimeToken, error) {
	out := []access.OneTimeToken{}
	err := r.t.tokens.Scan(r.r, bolttable.Range{}, func(k []byte, v *tokenRow) (bool, error) {
		if slices.Contains(purposes, v.Purpose) && v.ExpiresAt.After(now) {
			out = append(out, v.token(k))
		}
		return true, nil
	})
	return out, err
}

// CreateOneTimeToken implements account.TokenStore.
func (w *writer) CreateOneTimeToken(_ context.Context, tok access.OneTimeToken) error { //nolint:gocritic // hugeParam: account.TokenStore's signature
	if tok.Hash.IsZero() {
		return errNoHash
	}
	key := []byte(tok.Hash.Reveal())
	_, taken, err := w.t.tokens.Get(w.r, key)
	if err != nil {
		return err
	}
	if taken {
		return errTokenTaken
	}
	row := toTokenRow(&tok)
	return w.t.tokens.Put(w.tx, key, &row)
}

// ConsumeOneTimeToken implements account.TokenStore.
func (w *writer) ConsumeOneTimeToken(ctx context.Context, hash access.Hash, now time.Time) (access.OneTimeToken, bool, error) {
	tok, found, err := w.OneTimeTokenByHash(ctx, hash)
	if err != nil || !found {
		return access.OneTimeToken{}, false, err
	}
	if _, err := w.t.tokens.Delete(w.tx, []byte(hash.Reveal())); err != nil {
		return access.OneTimeToken{}, false, err
	}
	if !tok.ExpiresAt.After(now) {
		return access.OneTimeToken{}, false, nil
	}
	return tok, true, nil
}

// DeleteOneTimeTokens implements account.TokenStore.
func (w *writer) DeleteOneTimeTokens(_ context.Context, purpose access.TokenPurpose, subject int64) (int, error) {
	keys, err := ids(w.r, w.t.tokenBySubject, subjectKey(subject, purpose))
	if err != nil {
		return 0, err
	}
	return deleteAll(w.tx, w.t.tokens, keys)
}

// DeleteOneTimeTokensIssuedBy implements account.TokenStore.
func (w *writer) DeleteOneTimeTokensIssuedBy(_ context.Context, keyIDs []int64) (int, error) {
	n := 0
	for _, id := range keyIDs {
		if id == 0 {
			continue
		}
		keys, err := ids(w.r, w.t.tokenByIssuer, be64(id))
		if err != nil {
			return n, err
		}
		deleted, err := deleteAll(w.tx, w.t.tokens, keys)
		n += deleted
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// CleanupExpiredOneTimeTokens implements account.Sweeper.
func (s *Store) CleanupExpiredOneTimeTokens(ctx context.Context, now time.Time) (int64, error) {
	n, err := writeResult(ctx, s, func(w *writer) (int, error) {
		var expired [][]byte
		err := w.t.tokens.Scan(w.r, bolttable.Range{}, func(k []byte, v *tokenRow) (bool, error) {
			if !v.ExpiresAt.After(now) {
				expired = append(expired, k)
			}
			return true, nil
		})
		if err != nil {
			return 0, err
		}
		return deleteAll(w.tx, w.t.tokens, expired)
	})
	return int64(n), err
}

// ListKeyAttributions implements account.AttributionStore.
func (r reader) ListKeyAttributions(context.Context) ([]access.KeyAttribution, error) {
	return all(r.r, r.t.attributions, func(id int64, v *attributionRow) access.KeyAttribution { return v.attribution(id) })
}

// CreateKeyAttribution implements account.AttributionStore.
func (w *writer) CreateKeyAttribution(_ context.Context, a *access.KeyAttribution) error {
	id, err := nextID(w.tx, w.t.attributions)
	if err != nil {
		return err
	}
	row := attributionRow{At: a.At, Action: a.Action, KeyLineage: a.KeyLineage, UserID: a.UserID}
	if err := w.t.attributions.Put(w.tx, id, &row); err != nil {
		return err
	}
	a.ID = id
	return nil
}

// AuthFailure implements account.FailureStore.
func (r reader) AuthFailure(_ context.Context, ref account.FailureRef) (account.FailureCount, bool, error) {
	v, found, err := r.t.failures.Get(r.r, failureKey(ref))
	if err != nil || !found {
		return account.FailureCount{}, false, err
	}
	return v.failure(), true, nil
}

// UnreportedLocks implements account.FailureStore.
func (r reader) UnreportedLocks(context.Context) ([]account.FailureCount, error) {
	return rows(r.r, r.t.failures, r.t.failureUnreported, []byte{1},
		func(_ []byte, v *failureRow) account.FailureCount { return v.failure() })
}

// PutAuthFailure implements account.FailureStore.
func (w *writer) PutAuthFailure(_ context.Context, f account.FailureCount) error { //nolint:gocritic // hugeParam: account.FailureStore's signature
	row := toFailureRow(&f)
	return w.t.failures.Put(w.tx, failureKey(f.FailureRef), &row)
}

// DeleteAuthFailures implements account.FailureStore.
func (w *writer) DeleteAuthFailures(_ context.Context, name string, methods []access.Method) error {
	for _, m := range methods {
		for _, trusted := range []bool{false, true} {
			if _, err := w.t.failures.Delete(w.tx, failureKey(account.FailureRef{Name: name, Method: m, Trusted: trusted})); err != nil {
				return err
			}
		}
	}
	return nil
}

// PruneAuthFailures implements account.FailureStore.
func (w *writer) PruneAuthFailures(_ context.Context, keep []string) (int64, error) {
	kept := make(map[string]struct{}, len(keep))
	for _, name := range keep {
		kept[name] = struct{}{}
	}
	var pruned [][]byte
	err := w.t.failures.Scan(w.r, bolttable.Range{}, func(k []byte, v *failureRow) (bool, error) {
		if _, ok := kept[v.Name]; !ok {
			pruned = append(pruned, k)
		}
		return true, nil
	})
	if err != nil {
		return 0, err
	}
	n, err := deleteAll(w.tx, w.t.failures, pruned)
	return int64(n), err
}

// CreateOneTimeToken implements account.TokenStore.
func (s *Store) CreateOneTimeToken(ctx context.Context, tok access.OneTimeToken) error { //nolint:gocritic // hugeParam: account.TokenStore's signature
	return s.write(ctx, func(w *writer) error { return w.CreateOneTimeToken(ctx, tok) })
}

// OneTimeTokenByHash implements account.TokenStore.
func (s *Store) OneTimeTokenByHash(ctx context.Context, hash access.Hash) (access.OneTimeToken, bool, error) {
	return readFound(ctx, s, func(r reader) (access.OneTimeToken, bool, error) { return r.OneTimeTokenByHash(ctx, hash) })
}

// ConsumeOneTimeToken implements account.TokenStore.
func (s *Store) ConsumeOneTimeToken(ctx context.Context, hash access.Hash, now time.Time) (access.OneTimeToken, bool, error) {
	var tok access.OneTimeToken
	var found bool
	err := s.write(ctx, func(w *writer) error {
		var err error
		tok, found, err = w.ConsumeOneTimeToken(ctx, hash, now)
		return err
	})
	if err != nil {
		return access.OneTimeToken{}, false, err
	}
	return tok, found, nil
}

// DeleteOneTimeTokens implements account.TokenStore.
func (s *Store) DeleteOneTimeTokens(ctx context.Context, purpose access.TokenPurpose, subject int64) (int, error) {
	return writeResult(ctx, s, func(w *writer) (int, error) { return w.DeleteOneTimeTokens(ctx, purpose, subject) })
}

// LiveOneTimeTokens implements account.TokenStore.
func (s *Store) LiveOneTimeTokens(ctx context.Context, purposes []access.TokenPurpose, now time.Time) ([]access.OneTimeToken, error) {
	return readValue(ctx, s, func(r reader) ([]access.OneTimeToken, error) { return r.LiveOneTimeTokens(ctx, purposes, now) })
}

// DeleteOneTimeTokensIssuedBy implements account.TokenStore.
func (s *Store) DeleteOneTimeTokensIssuedBy(ctx context.Context, keyIDs []int64) (int, error) {
	return writeResult(ctx, s, func(w *writer) (int, error) { return w.DeleteOneTimeTokensIssuedBy(ctx, keyIDs) })
}

// CreateKeyAttribution implements account.AttributionStore.
func (s *Store) CreateKeyAttribution(ctx context.Context, a *access.KeyAttribution) error {
	return s.write(ctx, func(w *writer) error { return w.CreateKeyAttribution(ctx, a) })
}

// ListKeyAttributions implements account.AttributionStore.
func (s *Store) ListKeyAttributions(ctx context.Context) ([]access.KeyAttribution, error) {
	return readValue(ctx, s, func(r reader) ([]access.KeyAttribution, error) { return r.ListKeyAttributions(ctx) })
}

// AuthFailure implements account.FailureStore.
func (s *Store) AuthFailure(ctx context.Context, ref account.FailureRef) (account.FailureCount, bool, error) {
	return readFound(ctx, s, func(r reader) (account.FailureCount, bool, error) { return r.AuthFailure(ctx, ref) })
}

// PutAuthFailure implements account.FailureStore.
func (s *Store) PutAuthFailure(ctx context.Context, f account.FailureCount) error { //nolint:gocritic // hugeParam: account.FailureStore's signature
	return s.write(ctx, func(w *writer) error { return w.PutAuthFailure(ctx, f) })
}

// UnreportedLocks implements account.FailureStore.
func (s *Store) UnreportedLocks(ctx context.Context) ([]account.FailureCount, error) {
	return readValue(ctx, s, func(r reader) ([]account.FailureCount, error) { return r.UnreportedLocks(ctx) })
}

// DeleteAuthFailures implements account.FailureStore.
func (s *Store) DeleteAuthFailures(ctx context.Context, name string, methods []access.Method) error {
	return s.write(ctx, func(w *writer) error { return w.DeleteAuthFailures(ctx, name, methods) })
}

// PruneAuthFailures implements account.FailureStore.
func (s *Store) PruneAuthFailures(ctx context.Context, keep []string) (int64, error) {
	return writeResult(ctx, s, func(w *writer) (int64, error) { return w.PruneAuthFailures(ctx, keep) })
}
