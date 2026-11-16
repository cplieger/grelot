//go:build !windows

package server

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cplieger/access/gate"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

// The cursor's plaintext layout: a format byte, the issue time in Unix
// seconds and the binding, then the position, sealed with AES-256-GCM
// under a fresh nonce. Every field but the position has a fixed size.
const (
	cursorFormat   = 1
	nonceBytes     = 12
	tagBytes       = 16
	issuedBytes    = 8
	bindingBytes   = sha256.Size
	cursorHeader   = 1 + issuedBytes + bindingBytes
	cursorKeyBytes = 32
	// maxPosition is the longest position a Paged row resumes after: a
	// thread path, which the store bounds.
	maxPosition = store.MaxPathBytes
	// cursorLife is how long a cursor opens after it was minted.
	cursorLife = 24 * time.Hour
)

// maxCursorBytes is the longest cursor seal mints, which open refuses past
// before it decodes anything.
var maxCursorBytes = base64.RawURLEncoding.EncodedLen(nonceBytes + tagBytes + cursorHeader + maxPosition)

// errPositionTooLong is seal's refusal of a position over maxPosition, a
// wiring bug the chain answers 500.
var errPositionTooLong = errors.New("server: a cursor position is over maxPosition")

var (
	errCursorInvalid = refusal(wire.ErrCursorInvalid,
		"the cursor is not one this server minted in the last 24 hours: read the first page again")
	errCursorMismatch = refusal(wire.ErrCursorMismatch,
		"the cursor belongs to another list, query or credential: read the first page again")
)

// cursors mints and opens the API's list cursors.
type cursors struct {
	aead cipher.AEAD
	now  func() time.Time
}

// newCursors panics on a key that is not 32 bytes, which aes.NewCipher
// would take as AES-128 or AES-192 without a word.
func newCursors(key []byte, now func() time.Time) *cursors {
	if len(key) != cursorKeyBytes {
		panic("server: the cursor key must be 32 bytes, got " + strconv.Itoa(len(key)))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		panic("server: " + err.Error())
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic("server: " + err.Error())
	}
	return &cursors{aead: aead, now: now}
}

// cursorScope is what a cursor is bound to: the row, its canonical query
// and the caller's credential.
type cursorScope struct {
	row        string
	query      string
	credential string
}

func (sc *cursorScope) binding() []byte {
	h := sha256.New()
	for _, part := range []string{sc.row, sc.query, sc.credential} {
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return h.Sum(nil)
}

func (c *cursors) seal(sc cursorScope, position []byte) (string, error) {
	if len(position) > maxPosition {
		return "", errPositionTooLong
	}
	plain := make([]byte, 0, cursorHeader+len(position))
	plain = append(plain, cursorFormat)
	plain = binary.BigEndian.AppendUint64(plain, uint64(c.now().Unix())) //nolint:gosec // an instant after 1970
	plain = append(plain, sc.binding()...)
	plain = append(plain, position...)
	nonce := make([]byte, nonceBytes, nonceBytes+len(plain)+tagBytes)
	_, _ = rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(c.aead.Seal(nonce, nonce, plain, nil)), nil
}

// open is the position cursor holds, or errCursorInvalid or
// errCursorMismatch.
func (c *cursors) open(sc cursorScope, cursor string) ([]byte, error) {
	if len(cursor) > maxCursorBytes {
		return nil, errCursorInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) < nonceBytes+tagBytes {
		return nil, errCursorInvalid
	}
	plain, err := c.aead.Open(nil, raw[:nonceBytes], raw[nonceBytes:], nil)
	if err != nil || len(plain) < cursorHeader || plain[0] != cursorFormat {
		return nil, errCursorInvalid
	}
	issued := time.Unix(int64(binary.BigEndian.Uint64(plain[1:])), 0) //nolint:gosec // a sealed Unix time
	if c.now().Sub(issued) > cursorLife {
		return nil, errCursorInvalid
	}
	if !bytes.Equal(plain[1+issuedBytes:cursorHeader], sc.binding()) {
		return nil, errCursorMismatch
	}
	return plain[cursorHeader:], nil
}

// The positions each Paged row resumes after, encoded only here.

func seqPosition(seq uint64) []byte { return binary.BigEndian.AppendUint64(nil, seq) }

// afterSeq is a seq or id position; another shape is a cursor of no row
// this server pages.
func afterSeq(p []byte) (uint64, error) {
	if len(p) != 8 {
		return 0, errCursorInvalid
	}
	return binary.BigEndian.Uint64(p), nil
}

// historyPosition is the entry key a history page resumes before and the
// lower bound page 1 read, a zero bound sealed as 0.
func historyPosition(k store.EntryKey, since time.Time) []byte {
	p := binary.BigEndian.AppendUint64(nil, uint64(k.Written.UnixNano()))
	p = binary.BigEndian.AppendUint64(p, k.Message)
	p = binary.BigEndian.AppendUint64(p, k.Seq)
	var bound int64
	if !since.IsZero() {
		bound = since.UnixNano()
	}
	return binary.BigEndian.AppendUint64(p, uint64(bound))
}

func afterHistory(p []byte) (store.EntryKey, time.Time, error) {
	if len(p) != 32 {
		return store.EntryKey{}, time.Time{}, errCursorInvalid
	}
	k := store.EntryKey{
		Written: time.Unix(0, int64(binary.BigEndian.Uint64(p))).UTC(), //nolint:gosec // sealed by historyPosition
		Message: binary.BigEndian.Uint64(p[8:]), Seq: binary.BigEndian.Uint64(p[16:]),
	}
	var since time.Time
	if bound := int64(binary.BigEndian.Uint64(p[24:])); bound != 0 { //nolint:gosec // sealed by historyPosition
		since = time.Unix(0, bound).UTC()
	}
	return k, since, nil
}

// paging is a Paged row's page request, which the chain reads before the
// handler runs: the limit and the opened position, nil for the first page.
type paging struct {
	scope cursorScope
	after []byte
	limit int
}

type pagingKey struct{}

// pagingOf is the paging the chain put on r.
func pagingOf(r *http.Request) *paging {
	p, _ := r.Context().Value(pagingKey{}).(*paging)
	return p
}

// readPaging reads r's limit and opens its cursor for route.
func (s *Server) readPaging(r *http.Request, route *wire.Route) (*paging, error) {
	q := r.URL.Query()
	p := &paging{limit: wire.PageLimitDefault, scope: cursorScope{row: route.Name, credential: credentialOf(r)}}
	if v := q.Get(wire.QueryLimit); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > wire.PageLimitMax {
			return nil, fieldRefusal("limit must be 1 to 200", wire.FieldError{
				Field: wire.QueryLimit, Code: wire.FieldOutOfRange, Min: new(int64(1)), Max: new(int64(wire.PageLimitMax)),
			})
		}
		p.limit = n
	}
	cursor := q.Get(wire.QueryCursor)
	q.Del(wire.QueryCursor)
	q.Del(wire.QueryLimit)
	p.scope.query = q.Encode()
	if cursor == "" {
		return p, nil
	}
	after, err := s.cursors.open(p.scope, cursor)
	if err != nil {
		return nil, err
	}
	p.after = after
	return p, nil
}

// next is the next_cursor of a page whose last item is at position, ""
// when no item follows.
func (s *Server) next(r *http.Request, more bool, position []byte) (string, error) {
	if !more {
		return "", nil
	}
	p := pagingOf(r)
	c, err := s.cursors.seal(p.scope, position)
	if err != nil {
		err = fmt.Errorf("%w: row %s, %d bytes", err, p.scope.row, len(position))
	}
	return c, err
}

// credentialOf names the credential r was admitted with.
func credentialOf(r *http.Request) string {
	p, ok := gate.Principal(r.Context())
	switch {
	case !ok:
		return ""
	case p.IsAPIKey():
		return "key:" + strconv.FormatInt(p.KeyID, 10)
	}
	return "session:" + url.QueryEscape(p.SessionID)
}
