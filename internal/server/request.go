//go:build !windows

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cplieger/access/gate"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/keyenc"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/webui/wire"
)

// The request headers every /v1 route reads.
const (
	headerVersion = "Grelot-Version"
	headerSession = "Grelot-Session"
	headerAs      = "Grelot-As"
	headerSelf    = "Grelot-Self"
)

// maxBody caps every request body.
const maxBody = 128 << 10

// machine runs a /v1 route for the key the gate admitted: past the key's
// request rate it answers 429, then 426 naming the client to install for a
// version this server does not speak. Every answer names the server's
// version, so a client can tell a refusal it cannot read from a newer
// server, and its access line names the key's label.
func (s *Server) machine(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := gate.Principal(r.Context())
		httpserver.AddLogAttrs(r.Context(), slog.String("key", p.KeyLabel))
		w.Header().Set(headerVersion, s.version)
		if retry, ok := s.rates.take(p.KeyID, s.now()); !ok {
			s.fail(w, r, s.rateLimited(retry))
			return
		}
		if v := r.Header.Get(headerVersion); !speaks(v) {
			s.fail(w, r, &apiError{status: http.StatusUpgradeRequired, code: "upgrade_required", msg: upgradeLine(v, s.version)})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), machineKey{}, p.KeyID)))
	})
}

type machineKey struct{}

// keyID is the ID of the machine key the gate admitted r with, which machine puts on its context.
func keyID(r *http.Request) int64 {
	id, _ := r.Context().Value(machineKey{}).(int64)
	return id
}

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
	c := store.Caller{Session: r.Header.Get(headerSession), As: r.Header.Get(headerAs), Key: keyID(r)}
	if c.Session == "" && (mode != callerAnyone || c.As != "") {
		return c, badRequest("Grelot-Session is required: send <provider>:<host session id>")
	}
	switch r.Header.Get(headerSelf) {
	case "":
	case "1":
		c.Self = true
	default:
		return c, badRequest("Grelot-Self must be 1 when sent")
	}
	return c, nil
}

// identityFunc serves one identity route as c and returns the status and
// body to answer with.
type identityFunc func(r *http.Request, c store.Caller) (int, any, error)

// keyedFunc is identityFunc for a keyed write, which passes k to the
// store write it makes and answers a replayed result as it answered it
// first.
type keyedFunc func(r *http.Request, c store.Caller, k store.Keyed) (int, any, error)

// identity serves fn with the caller its headers name, and puts the
// caller's status line on the answer, read after fn so it shows what fn
// changed. The route takes no Idempotency-Key.
func (s *Server) identity(fn identityFunc) http.Handler {
	return s.identityFor(unkeyed(fn), callerSession)
}

// anyone is identity for a read any caller may make, with or without a
// session.
func (s *Server) anyone(fn identityFunc) http.Handler {
	return s.identityFor(unkeyed(fn), callerAnyone)
}

// keyed is identity for a write that requires an Idempotency-Key.
func (s *Server) keyed(fn keyedFunc) http.Handler {
	return s.identityFor(func(r *http.Request, c store.Caller) (int, any, error) {
		k, err := keyOf(r, c)
		if err != nil {
			return 0, nil, err
		}
		return fn(r, c, k)
	}, callerSession)
}

// renewable is keyed for PATCH /v1/me, which also takes a renewal alone,
// {ttl, where}, with no key: a re-sent renewal only moves the expiry to
// ttl after the later send, so it needs no dedup, and a client older than
// the key rule still renews and its identity never goes dormant over the
// client's upgrade.
func (s *Server) renewable(fn keyedFunc) http.Handler {
	return s.identityFor(func(r *http.Request, c store.Caller) (int, any, error) {
		if _, sent := r.Header[headerKey]; !sent {
			renewal, err := renewalOnly(r)
			if err != nil {
				return 0, nil, err
			}
			if renewal {
				return fn(r, c, store.Keyed{})
			}
		}
		k, err := keyOf(r, c)
		if err != nil {
			return 0, nil, err
		}
		return fn(r, c, k)
	}, callerSession)
}

// renewalOnly reports whether r's body names a ttl and nothing beside it
// but where, putting the body back for decode.
func renewalOnly(r *http.Request) (bool, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false, bodyError(err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return false, nil
	}
	_, ttl := fields["ttl"]
	delete(fields, "ttl")
	delete(fields, "where")
	return ttl && len(fields) == 0, nil
}

// unkeyed refuses a key on a route that takes none, so a client that keys
// a request the route never dedups, and would re-send it, fails at once.
func unkeyed(fn identityFunc) identityFunc {
	return func(r *http.Request, c store.Caller) (int, any, error) {
		if _, sent := r.Header[headerKey]; sent {
			return 0, nil, badRequest("this route takes no Idempotency-Key")
		}
		return fn(r, c)
	}
}

func (s *Server) identityFor(fn identityFunc, mode callerMode) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := callerOf(r, mode)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		httpserver.LimitBody(w, r, maxBody)
		status, body, err := fn(r, c)
		if c.Session != "" {
			s.setStatus(w, r, c)
		}
		switch {
		case err != nil:
			s.fail(w, r, err)
		case status == http.StatusNoContent:
			w.WriteHeader(status)
		default:
			httpserver.WriteJSONStatus(w, status, body)
		}
	})
}

// headerKey is the draft-ietf-httpapi-idempotency-key-header request
// header, a quoted RFC 8941 String of wire.IdempotencyKeyMin to
// wire.IdempotencyKeyMax characters drawn from [A-Za-z0-9_-].
const headerKey = "Idempotency-Key"

// keyOf reads a keyed write's Idempotency-Key and binds it to the request:
// its route, path, query, Grelot-Self and body, which it reads once under
// maxBody and puts back for decode.
func keyOf(r *http.Request, c store.Caller) (store.Keyed, error) {
	values := r.Header.Values(headerKey)
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
	request := sha256.Sum256([]byte(keyenc.Join(r.Pattern, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get(headerSelf), string(body))))
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
		return refusal(wire.ErrTooLarge, "the request body is over 128 KiB")
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
	case errors.As(err, new(*durationError)):
		return badRequest(err.Error())
	}
	return badRequest("the request body must be one JSON object: " + err.Error())
}

// duration is a JSON duration string such as "2h".
type duration time.Duration

type durationError struct{ value string }

func (e *durationError) Error() string {
	return `a duration must be a string such as "90m" or "2h", got ` + e.value
}

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return &durationError{value: string(b)}
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return &durationError{value: s}
	}
	*d = duration(v)
	return nil
}
