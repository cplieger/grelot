//go:build !windows

package server

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/keyenc"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

// maxBody caps every request body.
const maxBody = 128 << 10

// bearer is the key r sends, which its own logged text must not carry.
func bearer(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token
}

// callerMode says what an identity route reads from its request beyond
// the headers.
type callerMode int

const (
	// callerSession requires Grelot-Session.
	callerSession callerMode = iota
	// callerAnyone takes a request with no Grelot-Session as a session with
	// no identity, so an agent can look before it registers.
	callerAnyone
)

// callerOf reads who an identity request acts as from its headers.
func callerOf(r *http.Request, mode callerMode) (store.Caller, error) {
	c := store.Caller{Session: r.Header.Get(wire.HeaderSession), As: r.Header.Get(wire.HeaderAs), Key: keyID(r)}
	if c.Session == "" && (mode != callerAnyone || c.As != "") {
		return c, badRequest("Grelot-Session is required: send <provider>:<host session id>")
	}
	switch r.Header.Get(wire.HeaderSelf) {
	case "":
	case "1":
		c.Self = true
	default:
		return c, badRequest("Grelot-Self must be 1 when sent")
	}
	return c, nil
}

// keyOf reads a keyed write's Idempotency-Key and binds it to the request:
// its route, path, query, Grelot-Self and body, which it reads once under
// maxBody and puts back for decode.
func keyOf(r *http.Request, c store.Caller) (store.Keyed, error) {
	values := r.Header.Values(wire.HeaderIdempotencyKey)
	if len(values) == 0 {
		return store.Keyed{}, badRequest("this route requires an Idempotency-Key: send a new quoted random key with each request, and the same key when you send it again")
	}
	key, ok := parseKey(values)
	if !ok {
		return store.Keyed{}, badRequest(fmt.Sprintf("the Idempotency-Key must be one quoted string of %d to %d letters, digits, - or _",
			wire.IdempotencyKeyMin, wire.IdempotencyKeyMax))
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return store.Keyed{}, bodyError(err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	request := sha256.Sum256([]byte(keyenc.Join(r.Pattern, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get(wire.HeaderSelf), string(body))))
	return store.NewKeyed(c, key, request), nil
}

// parseKey reads the one Idempotency-Key value: an RFC 8941 String with no
// escapes, since its characters need none.
func parseKey(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	v := values[0]
	if len(v) < wire.IdempotencyKeyMin+2 || len(v) > wire.IdempotencyKeyMax+2 || v[0] != '"' || v[len(v)-1] != '"' {
		return "", false
	}
	key := v[1 : len(v)-1]
	if strings.ContainsFunc(key, notKeyChar) {
		return "", false
	}
	return key, true
}

func notKeyChar(ch rune) bool {
	return (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '-' && ch != '_'
}

// bodyError answers a request body that could not be read.
func bodyError(err error) error {
	if errors.As(err, new(*http.MaxBytesError)) {
		e := refusal(wire.ErrTooLarge, "the request body is over 128 KiB")
		e.reason = wire.ReasonRequestBody
		return e
	}
	return badRequest("the request body could not be read: " + err.Error())
}

// decode reads r's JSON body into v under maxBody.
func decode(r *http.Request, v any) error {
	err := httpserver.DecodeJSONInto(nil, r, v, maxBody)
	switch {
	case err == nil:
		return nil
	case errors.As(err, new(*http.MaxBytesError)):
		return bodyError(err)
	}
	return badRequest("the request body must be one JSON object: " + err.Error())
}

// durationMS is a request's _ms duration; 0 is none. A value past
// time.Duration's range saturates, so every later range check refuses it
// rather than reading a wrapped product.
func durationMS(ms int64) time.Duration {
	const limit = math.MaxInt64 / int64(time.Millisecond)
	switch {
	case ms > limit:
		return math.MaxInt64
	case ms < -limit:
		return math.MinInt64
	}
	return time.Duration(ms) * time.Millisecond
}
