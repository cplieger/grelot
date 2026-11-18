//go:build !windows

package accounts

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cplieger/access/account"
	"github.com/cplieger/bolttable"
)

// Index names are stored in the namespace's format digest, and conflict maps
// three of them to account's sentinels.
const (
	indexUserName      = "users_by_name"
	indexUserOIDC      = "users_by_oidc"
	indexUserHandle    = "users_by_handle"
	indexSessionHash   = "sessions_by_hash"
	indexSessionUser   = "sessions_by_user"
	indexPasskeyCred   = "passkeys_by_credential" //nolint:gosec // an index name, not a credential
	indexPasskeyUser   = "passkeys_by_user"
	indexKeyHash       = "keys_by_hash"
	indexKeyUser       = "keys_by_user"
	indexTokenSubject  = "tokens_by_subject" //nolint:gosec // an index name, not a credential
	indexTokenIssuer   = "tokens_by_issuer"  //nolint:gosec // an index name, not a credential
	indexAttribution   = "attributions_by_user"
	indexFailureUnread = "failures_unreported"
	indexLogClass      = "log_by_class"
	counterUsers       = "user_count"
	counterLogPrefix   = "log_count_"
)

// maxID is the largest ID a JSON number holds exactly (account's rule 17).
const maxID int64 = 1<<53 - 1

var (
	errSessionIDTaken   = errors.New("accounts: session ID already stored")
	errTokenTaken       = errors.New("accounts: token hash already stored")
	errNoHash           = errors.New("accounts: record has no hash")
	errNoSessionID      = errors.New("accounts: session has no ID")
	errNoCredentialID   = errors.New("accounts: passkey has no credential ID")
	errRotationIdentity = errors.New("accounts: rotation changes the session's ID or user")
	errIDsExhausted     = errors.New("accounts: IDs exhausted")
	errKeyLength        = errors.New("accounts: an ID key is not 8 bytes")
	errCleanupLimit     = errors.New("accounts: cleanup limit below 1")
	errCleanupTimeouts  = errors.New("accounts: cleanup timeouts not positive")
)

type accessTables struct {
	users             *bolttable.Table[int64, userRow]
	userCount         *bolttable.Counter[int64, userRow]
	userByName        *bolttable.Index[int64, userRow]
	userByOIDC        *bolttable.Index[int64, userRow]
	userByHandle      *bolttable.Index[int64, userRow]
	sessions          *bolttable.Table[[]byte, sessionRow]
	sessionByHash     *bolttable.Index[[]byte, sessionRow]
	sessionByUser     *bolttable.Index[[]byte, sessionRow]
	passkeys          *bolttable.Table[int64, passkeyRow]
	passkeyByCred     *bolttable.Index[int64, passkeyRow]
	passkeyByUser     *bolttable.Index[int64, passkeyRow]
	keys              *bolttable.Table[int64, keyRow]
	keyByHash         *bolttable.Index[int64, keyRow]
	keyByUser         *bolttable.Index[int64, keyRow]
	tokens            *bolttable.Table[[]byte, tokenRow]
	tokenBySubject    *bolttable.Index[[]byte, tokenRow]
	tokenByIssuer     *bolttable.Index[[]byte, tokenRow]
	attributions      *bolttable.Table[int64, attributionRow]
	attributionByUser *bolttable.Index[int64, attributionRow]
	failures          *bolttable.Table[[]byte, failureRow]
	failureUnreported *bolttable.Index[[]byte, failureRow]
	log               logTables
}

// declarations keeps the first error of a run of schema declarations.
type declarations struct{ err error }

// set returns a func storing a declaration's result in dst and its error in
// d, unless an earlier one is there.
func set[T any](d *declarations, dst *T) func(T, error) {
	return func(v T, err error) {
		*dst = v
		if d.err == nil {
			d.err = err
		}
	}
}

func entry(key []byte) []bolttable.Entry { return []bolttable.Entry{{Key: key}} }

// sparse is entry for a key that may be empty, which indexes nothing.
func sparse(key []byte) []bolttable.Entry {
	if len(key) == 0 {
		return nil
	}
	return entry(key)
}

// idKey lays an int64 out big-endian, so key order is ID order for the
// positive IDs the store assigns.
type idKey struct{}

// Version is 1.
func (idKey) Version() uint32 { return 1 }

// AppendKey appends k's 8 big-endian bytes.
func (idKey) AppendKey(dst []byte, k int64) []byte {
	return binary.BigEndian.AppendUint64(dst, uint64(k)) //nolint:gosec // the two's-complement bits, read back by ParseKey
}

// ParseKey refuses anything but 8 bytes.
func (idKey) ParseKey(b []byte) (int64, error) {
	if len(b) != 8 {
		return 0, errKeyLength
	}
	return int64(binary.BigEndian.Uint64(b)), nil //nolint:gosec // the bits AppendKey wrote
}

func be64(v int64) []byte { return idKey{}.AppendKey(nil, v) }

func defineAccess(ns *bolttable.Namespace) (accessTables, error) {
	var t accessTables
	var err declarations
	silent, unique, multi := bolttable.TableOptions{Silent: true},
		bolttable.IndexOptions{Unique: true, Version: 1}, bolttable.IndexOptions{Version: 1}
	set(&err, &t.users)(bolttable.Define(ns, prefixUsers, "users", idKey{}, bolttable.JSON[userRow](1), silent))
	set(&err, &t.userCount)(bolttable.DefineCounter(t.users, counterUsers, 1, func(int64, *userRow) int64 { return 1 }))
	set(&err, &t.userByName)(bolttable.DefineIndex(t.users, indexUserName, unique,
		func(_ int64, v *userRow) []bolttable.Entry { return sparse([]byte(v.Username)) }))
	set(&err, &t.userByOIDC)(bolttable.DefineIndex(t.users, indexUserOIDC, unique,
		func(_ int64, v *userRow) []bolttable.Entry { return sparse(oidcKey(v.OIDCIssuer, v.OIDCSubject)) }))
	set(&err, &t.userByHandle)(bolttable.DefineIndex(t.users, indexUserHandle, unique,
		func(_ int64, v *userRow) []bolttable.Entry { return sparse(v.WebAuthnHandle) }))

	set(&err, &t.sessions)(bolttable.Define(ns, prefixSessions, "sessions", bolttable.BytesKey{}, bolttable.JSON[sessionRow](1), silent))
	set(&err, &t.sessionByHash)(bolttable.DefineIndex(t.sessions, indexSessionHash, unique,
		func(_ []byte, v *sessionRow) []bolttable.Entry { return sparse([]byte(v.TokenHash)) }))
	set(&err, &t.sessionByUser)(bolttable.DefineIndex(t.sessions, indexSessionUser, multi,
		func(_ []byte, v *sessionRow) []bolttable.Entry { return entry(be64(v.UserID)) }))

	set(&err, &t.passkeys)(bolttable.Define(ns, prefixPasskeys, "passkeys", idKey{}, bolttable.JSON[passkeyRow](1), silent))
	set(&err, &t.passkeyByCred)(bolttable.DefineIndex(t.passkeys, indexPasskeyCred, unique,
		func(_ int64, v *passkeyRow) []bolttable.Entry { return sparse(v.CredentialID) }))
	set(&err, &t.passkeyByUser)(bolttable.DefineIndex(t.passkeys, indexPasskeyUser, multi,
		func(_ int64, v *passkeyRow) []bolttable.Entry { return entry(be64(v.UserID)) }))

	set(&err, &t.keys)(bolttable.Define(ns, prefixKeys, "keys", idKey{}, bolttable.JSON[keyRow](1), silent))
	set(&err, &t.keyByHash)(bolttable.DefineIndex(t.keys, indexKeyHash, unique,
		func(_ int64, v *keyRow) []bolttable.Entry {
			return append(sparse([]byte(v.KeyHash)), sparse([]byte(v.PreviousHash))...)
		}))
	set(&err, &t.keyByUser)(bolttable.DefineIndex(t.keys, indexKeyUser, multi,
		func(_ int64, v *keyRow) []bolttable.Entry { return entry(be64(v.UserID)) }))

	set(&err, &t.tokens)(bolttable.Define(ns, prefixTokens, "tokens", bolttable.BytesKey{}, bolttable.JSON[tokenRow](1), silent))
	set(&err, &t.tokenBySubject)(bolttable.DefineIndex(t.tokens, indexTokenSubject, multi,
		func(_ []byte, v *tokenRow) []bolttable.Entry { return entry(subjectKey(v.Subject, v.Purpose)) }))
	set(&err, &t.tokenByIssuer)(bolttable.DefineIndex(t.tokens, indexTokenIssuer, multi,
		func(_ []byte, v *tokenRow) []bolttable.Entry {
			if v.IssuedByKey == 0 {
				return nil
			}
			return entry(be64(v.IssuedByKey))
		}))

	set(&err, &t.attributions)(bolttable.Define(ns, prefixAttributions, "attributions",
		idKey{}, bolttable.JSON[attributionRow](1), silent))
	set(&err, &t.attributionByUser)(bolttable.DefineIndex(t.attributions, indexAttribution, multi,
		func(_ int64, v *attributionRow) []bolttable.Entry { return entry(be64(v.UserID)) }))

	set(&err, &t.failures)(bolttable.Define(ns, prefixFailures, "failures", bolttable.BytesKey{}, bolttable.JSON[failureRow](1), silent))
	set(&err, &t.failureUnreported)(bolttable.DefineIndex(t.failures, indexFailureUnread, multi,
		func(_ []byte, v *failureRow) []bolttable.Entry {
			if !v.Unreported {
				return nil
			}
			return entry([]byte{1})
		}))
	t.log = defineLog(ns, &err)
	if err.err != nil {
		return t, err.err
	}
	cascades := defineCascades(&t)
	return t, cascades
}

// defineCascades makes DeleteUser's cascade (account.UserStore) one Delete:
// every child index's key starts with the user's ID.
func defineCascades(t *accessTables) error {
	return errors.Join(
		bolttable.CascadeIndex(t.users, t.sessionByUser, 1, be64),
		bolttable.CascadeIndex(t.users, t.passkeyByUser, 1, be64),
		bolttable.CascadeIndex(t.users, t.keyByUser, 1, be64),
		bolttable.CascadeIndex(t.users, t.tokenBySubject, 1, be64),
		bolttable.CascadeIndex(t.users, t.attributionByUser, 1, be64),
	)
}

// Store is the accounts file's access namespace as access's account.Store:
// users, sessions, passkeys, API keys, one-time tokens, key attributions,
// failure records and the security log. Update groups concurrent calls
// into one commit and may run its callback more than once; every other
// write is one transaction of its own, and every read one snapshot.
type Store struct {
	db *bolttable.DB
	t  *accessTables
}

var _ account.Store = (*Store)(nil)

// reader answers account.Tx's reads from one snapshot.
type reader struct {
	t *accessTables
	r *bolttable.ReadTx
}

// writer is account.Tx inside one write transaction; its reads see its own
// writes.
type writer struct {
	reader
	tx *bolttable.Tx
}

var _ account.Tx = (*writer)(nil)

func (s *Store) writer(tx *bolttable.Tx) *writer {
	return &writer{t: s.t, r: tx.Read(), tx: tx}
}

// Update implements account.Store through bolttable's grouped commit.
func (s *Store) Update(ctx context.Context, fn func(tx account.Tx) error) error {
	return s.db.Batch(ctx, func(tx *bolttable.Tx) error { return fn(s.writer(tx)) })
}

func (s *Store) view(ctx context.Context, fn func(reader) error) error {
	return s.db.View(ctx, func(r *bolttable.ReadTx) error { return fn(reader{t: s.t, r: r}) })
}

func (s *Store) write(ctx context.Context, fn func(*writer) error) error {
	return s.db.Update(ctx, func(tx *bolttable.Tx) error { return fn(s.writer(tx)) })
}

// readValue runs one read that answers a value.
func readValue[T any](ctx context.Context, s *Store, fn func(reader) (T, error)) (T, error) {
	var out T
	err := s.view(ctx, func(r reader) error {
		var err error
		out, err = fn(r)
		return err
	})
	return out, err
}

// readFound runs one by-key read; an error answers the zero value, not found.
func readFound[T any](ctx context.Context, s *Store, fn func(reader) (T, bool, error)) (out T, found bool, err error) {
	err = s.view(ctx, func(r reader) error {
		var readErr error
		out, found, readErr = fn(r)
		return readErr
	})
	if err != nil {
		var zero T
		return zero, false, err
	}
	return out, found, nil
}

// writeResult runs one single-call write that answers a value.
func writeResult[T any](ctx context.Context, s *Store, fn func(*writer) (T, error)) (T, error) {
	var out T
	err := s.write(ctx, func(w *writer) error {
		var err error
		out, err = fn(w)
		return err
	})
	return out, err
}

// nextID keeps account's rule 17: IDs fit a JSON number, and bbolt's
// sequence, which a delete never lowers, never hands one out twice.
func nextID[K, V any](tx *bolttable.Tx, t *bolttable.Table[K, V]) (int64, error) {
	id, err := t.NextID(tx)
	if err != nil {
		return 0, err
	}
	if id > uint64(maxID) {
		return 0, errIDsExhausted
	}
	return int64(id), nil
}

// notFound is err, or account.ErrNotFound for a lookup that found nothing.
func notFound(err error) error {
	if err != nil {
		return err
	}
	return account.ErrNotFound
}

// conflict maps a unique-index refusal to account's sentinel for it; the
// other unique keys are store invariants, reported as backend errors.
func conflict(err error) error {
	ce, ok := errors.AsType[*bolttable.ConflictError](err)
	if !ok {
		return err
	}
	switch ce.Index {
	case indexUserName:
		return account.ErrUsernameTaken
	case indexUserOIDC:
		return account.ErrOIDCIdentityTaken
	case indexPasskeyCred:
		return account.ErrDuplicateCredential
	}
	return fmt.Errorf("accounts: %w", err)
}

// ids scans an index's entries under key and returns their row keys.
func ids[K, V any](r *bolttable.ReadTx, ix *bolttable.Index[K, V], key []byte) ([]K, error) {
	out := []K{}
	err := ix.Scan(r, bolttable.Exact(key), func(_ []byte, k K, _ []byte) (bool, error) {
		out = append(out, k)
		return true, nil
	})
	return out, err
}

// rows reads the rows of an index's entries under key, converted by conv.
func rows[K, V, T any](r *bolttable.ReadTx, t *bolttable.Table[K, V], ix *bolttable.Index[K, V], key []byte, conv func(K, *V) T) ([]T, error) {
	keys, err := ids(r, ix, key)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(keys))
	for _, k := range keys {
		v, found, err := t.Get(r, k)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, conv(k, &v))
		}
	}
	return out, nil
}

// all reads every row of t, converted by conv, in key order.
func all[K, V, T any](r *bolttable.ReadTx, t *bolttable.Table[K, V], conv func(K, *V) T) ([]T, error) {
	out := []T{}
	err := t.Scan(r, bolttable.Range{}, func(k K, v *V) (bool, error) {
		out = append(out, conv(k, v))
		return true, nil
	})
	return out, err
}

// deleteAll deletes every row of t under keys and reports how many there were.
func deleteAll[K, V any](tx *bolttable.Tx, t *bolttable.Table[K, V], keys []K) (int, error) {
	n := 0
	for _, k := range keys {
		deleted, err := t.Delete(tx, k)
		if err != nil {
			return n, err
		}
		if deleted {
			n++
		}
	}
	return n, nil
}
