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
// seconds, the binding and the identity the page was read as, then the
// position, sealed with AES-256-GCM under a fresh nonce. Every field but
// the position has a fixed size.
const (
	cursorFormat   = 1
	nonceBytes     = 12
	tagBytes       = 16
	issuedBytes    = 8
	bindingBytes   = sha256.Size
	readAsBytes    = 8
	cursorHeader   = 1 + issuedBytes + bindingBytes + readAsBytes
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

var errCursorInvalid = refusal(wire.ErrCursorInvalid,
	"the cursor is not one this server minted in the last 24 hours: read the first page again")

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

// cursorScope is what a cursor is bound to: the row, its canonical query,
// the credential and the store file whose ids the position names. The
// accounts file is bound by the cursor key, one of its own secrets. The
// identity a page was read as is no header text, which a retire and
// re-register reuses: it rides in the cursor, and the store checks it.
type cursorScope struct {
	row        string
	query      string
	credential string
	storeID    [16]byte
}

func (sc *cursorScope) binding() []byte {
	h := sha256.New()
	for _, part := range []string{sc.row, sc.query, sc.credential, string(sc.storeID[:])} {
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return h.Sum(nil)
}

func (c *cursors) seal(sc *cursorScope, readAs uint64, position []byte) (string, error) {
	if len(position) > maxPosition {
		return "", errPositionTooLong
	}
	plain := make([]byte, 0, cursorHeader+len(position))
	plain = append(plain, cursorFormat)
	plain = binary.BigEndian.AppendUint64(plain, uint64(c.now().Unix())) //nolint:gosec // an instant after 1970
	plain = append(plain, sc.binding()...)
	plain = binary.BigEndian.AppendUint64(plain, readAs)
	plain = append(plain, position...)
	nonce := make([]byte, nonceBytes, nonceBytes+len(plain)+tagBytes)
	_, _ = rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(c.aead.Seal(nonce, nonce, plain, nil)), nil
}

// open is the identity and the position cursor holds, or
// errCursorInvalid or store.ErrCursorMismatch.
func (c *cursors) open(sc *cursorScope, cursor string) (readAs uint64, position []byte, err error) {
	if len(cursor) > maxCursorBytes {
		return 0, nil, errCursorInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) < nonceBytes+tagBytes {
		return 0, nil, errCursorInvalid
	}
	plain, err := c.aead.Open(nil, raw[:nonceBytes], raw[nonceBytes:], nil)
	if err != nil || len(plain) < cursorHeader || plain[0] != cursorFormat {
		return 0, nil, errCursorInvalid
	}
	issued := time.Unix(int64(binary.BigEndian.Uint64(plain[1:])), 0) //nolint:gosec // a sealed Unix time
	if c.now().Sub(issued) > cursorLife {
		return 0, nil, errCursorInvalid
	}
	bound := plain[1+issuedBytes : 1+issuedBytes+bindingBytes]
	if !bytes.Equal(bound, sc.binding()) {
		return 0, nil, store.ErrCursorMismatch
	}
	return binary.BigEndian.Uint64(plain[cursorHeader-readAsBytes:]), plain[cursorHeader:], nil
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

// instantBytes is a sealed instant: its Unix seconds and nanoseconds,
// which span every instant RFC 3339 writes, where UnixNano spans only
// 1678 to 2262.
const instantBytes = 12

func appendInstant(p []byte, t time.Time) []byte {
	p = binary.BigEndian.AppendUint64(p, uint64(t.Unix()))          //nolint:gosec // read back as int64 by instantAt
	return binary.BigEndian.AppendUint32(p, uint32(t.Nanosecond())) //nolint:gosec // 0 to 999999999
}

func instantAt(p []byte) time.Time {
	return time.Unix(int64(binary.BigEndian.Uint64(p)), int64(binary.BigEndian.Uint32(p[8:]))).UTC() //nolint:gosec // written by appendInstant
}

// historyKeyBytes is an entry key's position: its written instant, its
// message and its seq.
const historyKeyBytes = instantBytes + 16

// historyPosition is the entry key a history page resumes before and the
// lower bound page 1 read, which a whole read (nil) leaves out.
func historyPosition(k store.EntryKey, since *time.Time) []byte {
	p := appendInstant(nil, k.Written)
	p = binary.BigEndian.AppendUint64(p, k.Message)
	p = binary.BigEndian.AppendUint64(p, k.Seq)
	if since != nil {
		p = appendInstant(p, *since)
	}
	return p
}

func afterHistory(p []byte) (store.EntryKey, *time.Time, error) {
	if len(p) != historyKeyBytes && len(p) != historyKeyBytes+instantBytes {
		return store.EntryKey{}, nil, errCursorInvalid
	}
	k := store.EntryKey{
		Written: instantAt(p),
		Message: binary.BigEndian.Uint64(p[instantBytes:]), Seq: binary.BigEndian.Uint64(p[instantBytes+8:]),
	}
	if len(p) == historyKeyBytes {
		return k, nil, nil
	}
	return k, new(instantAt(p[historyKeyBytes:])), nil
}

// paging is a Paged row's page request, which the chain reads before the
// handler runs: the limit, and the opened identity and position, both nil
// for the first page. The handler passes readAs to its store read.
type paging struct {
	readAs *uint64
	after  []byte
	scope  cursorScope
	limit  int
}

type pagingKey struct{}

// pagingOf is the paging the chain put on r.
func pagingOf(r *http.Request) *paging {
	p, _ := r.Context().Value(pagingKey{}).(*paging)
	return p
}

// readPaging reads r's limit and opens its cursor for route.
func (s *Server) readPaging(r *http.Request, route *wire.Route) (*paging, error) {
	page, err := queryParams[wire.Page](r)
	if err != nil {
		return nil, err
	}
	p := &paging{limit: wire.PageLimitDefault, scope: cursorScope{
		row: route.Name, credential: credentialOf(r), storeID: s.store.FileID(),
	}}
	if page.Limit != 0 {
		p.limit = int(page.Limit)
	}
	q := r.URL.Query()
	q.Del(wire.QueryCursor)
	q.Del(wire.QueryLimit)
	p.scope.query = q.Encode()
	if page.Cursor == "" {
		return p, nil
	}
	readAs, after, err := s.cursors.open(&p.scope, page.Cursor)
	if err != nil {
		return nil, err
	}
	p.readAs, p.after = &readAs, after
	return p, nil
}

// next is the next_cursor of a page read as readAs whose last item is at
// position, "" when no item follows.
func (s *Server) next(r *http.Request, more bool, readAs uint64, position []byte) (string, error) {
	if !more {
		return "", nil
	}
	p := pagingOf(r)
	c, err := s.cursors.seal(&p.scope, readAs, position)
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
