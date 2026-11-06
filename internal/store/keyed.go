//go:build !windows

package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cplieger/bolttable"
	"github.com/cplieger/go/keyenc"
)

// requestTTL is how long a keyed write's first result answers its
// re-sends: 20 times the client's 15 s call budget, inside which every
// re-send happens.
const requestTTL = 5 * time.Minute

// Keyed is one write's Idempotency-Key, scoped to its caller and bound to
// the request it came with. Every write method that takes one also
// reports whether it replayed: a key whose first result is stored, for
// requestTTL, answers that result and changes nothing, ahead of any
// refusal the write would make now; a key sent with another request is
// KindKeyReused; a refusal records nothing. The zero Keyed is an unkeyed
// write, which runs every time and records nothing.
type Keyed struct {
	// seal is the key the first result is sealed under; only the client's
	// raw key derives it, and the row is stored under its hash (id).
	seal, request [sha256.Size]byte
}

// NewKeyed scopes key to c's session and handle, so a caller never replays
// another caller's answer, and binds it to request, the hash of the request
// the key came with.
func NewKeyed(c Caller, key string, request [sha256.Size]byte) Keyed {
	return Keyed{seal: sha256.Sum256([]byte(keyenc.Join(c.Session, c.As, key))), request: request}
}

// id is the key of k's request row.
func (k *Keyed) id() []byte {
	id := sha256.Sum256(k.seal[:])
	return id[:]
}

// requestRow is a keyed write's first result, keyed by Keyed.id. Sealed
// is the result's JSON under Keyed.seal: the file never holds a delegate
// link in the clear, as linkRow keeps only its hash.
type requestRow struct {
	Lease   lease  `json:"lease"`
	Request []byte `json:"request"`
	Sealed  []byte `json:"sealed"`
}

// errReplay rolls back the transaction of a write that found its key's
// first result, so a replay writes nothing.
var errReplay = errors.New("store: replayed")

// keyed runs fn as the write k names, as Keyed describes, with its record
// in fn's own transaction, so a crash never keeps one without the other.
// A refusal the transaction commits (wtx.refuseCommitting) records
// nothing either.
func keyed[T any](ctx context.Context, s *Store, k Keyed, fn func(*wtx) (T, error)) (out T, replayed bool, err error) {
	err = s.update(ctx, func(w *wtx) error {
		if k == (Keyed{}) {
			var ferr error
			out, ferr = fn(w)
			return ferr
		}
		return keyedRun(w, &k, &out, fn)
	})
	switch {
	case errors.Is(err, errReplay):
		return out, true, nil
	case err != nil:
		var zero T
		return zero, false, err
	}
	return out, false, nil
}

// keyedRun is keyed's transaction for a key: errReplay with out set to the
// first result when one is stored, else fn's result recorded.
func keyedRun[T any](w *wtx, k *Keyed, out *T, fn func(*wtx) (T, error)) error {
	row, found, err := w.s.t.requests.settle(w, k.id())
	if err != nil {
		return err
	}
	if found {
		if oerr := k.open(&row, out); oerr != nil {
			return oerr
		}
		return errReplay
	}
	if *out, err = fn(w); err != nil || w.refusal != nil {
		return err
	}
	return k.record(w, out)
}

// replay is the first result stored for k, read without taking the
// writer; a row past its expiry is gone, as the sweep will leave it.
func replay[T any](ctx context.Context, s *Store, k *Keyed) (out T, found bool, err error) {
	err = s.read(ctx, func(r *bolttable.ReadTx) error {
		row, ok, gerr := s.t.requests.table.Get(r, k.id())
		if gerr != nil || !ok || row.Lease.due(s.now()) {
			return gerr
		}
		found = true
		return k.open(&row, &out)
	})
	if err != nil || !found {
		var zero T
		return zero, false, err
	}
	return out, true, nil
}

// ReplayPost is the first result of the keyed post k when one is stored,
// read without taking the writer, so a re-send answers before the post
// rate limit counts it. KindKeyReused for k's key sent with another
// request; ErrClosed after Close.
func (s *Store) ReplayPost(ctx context.Context, k Keyed) (Posted, bool, error) {
	return replay[Posted](ctx, s, &k)
}

// record stores v as k's first result, for requestTTL.
func (k *Keyed) record(w *wtx, v any) error {
	plain, err := json.Marshal(v)
	if err != nil {
		return err
	}
	aead, err := k.aead()
	if err != nil {
		return err
	}
	id := k.id()
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	_, _ = rand.Read(nonce) // crypto/rand.Read never returns an error.
	row := requestRow{
		Lease:   serverLease(w.now, requestTTL),
		Request: k.request[:],
		Sealed:  aead.Seal(nonce, nonce, plain, id),
	}
	return w.s.t.requests.table.Put(w.tx, id, &row)
}

// open decodes row's result into v, refusing a row another request
// stored under the same key.
func (k *Keyed) open(row *requestRow, v any) error {
	if !bytes.Equal(row.Request, k.request[:]) {
		return refuse(KindKeyReused, "this Idempotency-Key was sent with a different request; send a new key")
	}
	aead, err := k.aead()
	if err != nil {
		return err
	}
	n := aead.NonceSize()
	if len(row.Sealed) < n {
		return fmt.Errorf("store: request row of %d bytes is shorter than its nonce", len(row.Sealed))
	}
	plain, err := aead.Open(nil, row.Sealed[:n], row.Sealed[n:], k.id())
	if err != nil {
		return fmt.Errorf("store: open request row: %w", err)
	}
	return json.Unmarshal(plain, v)
}

func (k *Keyed) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.seal[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
