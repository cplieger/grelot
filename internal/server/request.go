package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/store"
)

// The request headers of section "HTTP API".
const (
	headerVersion = "Grelot-Version"
	headerSession = "Grelot-Session"
	headerAs      = "Grelot-As"
	headerSelf    = "Grelot-Self"
)

// maxBody caps every request body.
const maxBody = 128 << 10

// authorize admits a request with the bearer token and a client version
// this server speaks: 401 otherwise, then 426 naming the client to install.
func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !s.bearer.Verify(token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="grelot"`)
			s.fail(w, r, &apiError{
				status: http.StatusUnauthorized, code: "unauthorized",
				msg: "a valid bearer token is required: set token in the grelot client config",
			})
			return
		}
		if v := r.Header.Get(headerVersion); !speaks(v) {
			s.fail(w, r, &apiError{status: http.StatusUpgradeRequired, code: "upgrade_required", msg: upgradeLine(v, s.version)})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// callerOf reads who an identity request acts as from its headers and,
// with agent, the subagent id its agent parameter names.
func callerOf(r *http.Request, agent bool) (store.Caller, error) {
	c := store.Caller{Session: r.Header.Get(headerSession), As: r.Header.Get(headerAs)}
	if agent {
		c.Agent = r.URL.Query().Get("agent")
	}
	if c.Session == "" {
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

// identity serves fn with the caller its headers name, and puts the
// caller's status line on the answer, read after fn so it shows what fn
// changed.
func (s *Server) identity(fn identityFunc) http.Handler { return s.identityFor(fn, false) }

// identityAgent is identity for a route a subagent's hook calls, which
// acts as the delegate bound to the subagent id in its agent parameter.
func (s *Server) identityAgent(fn identityFunc) http.Handler { return s.identityFor(fn, true) }

func (s *Server) identityFor(fn identityFunc, agent bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := callerOf(r, agent)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		httpserver.LimitBody(w, r, maxBody)
		status, body, err := fn(r, c)
		s.setStatus(w, r, c)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		httpserver.WriteJSONStatus(w, status, body)
	})
}

// decode reads r's JSON body into v under maxBody.
func decode(r *http.Request, v any) error {
	err := httpserver.DecodeJSONInto(nil, r, v, maxBody)
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &tooLarge):
		return &apiError{status: http.StatusRequestEntityTooLarge, code: "too_large", msg: "the request body is over 128 KiB"}
	case errors.As(err, new(*durationError)):
		return badRequest(err.Error())
	}
	return badRequest("the request body must be one JSON object: " + err.Error())
}

// duration is a JSON duration string such as "2h".
type duration time.Duration

type durationError struct{ value string }

func (e *durationError) Error() string {
	return "a duration must be a string such as 90m or 2h, got " + e.value
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
