//go:build !windows

package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/auth"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/webui/wire"
)

// apiError is a refusal the HTTP layer makes itself, before the store.
type apiError struct {
	code   httpserver.ErrorCode
	msg    string
	status int
	// retryAfter, when set, goes out as Retry-After in whole seconds.
	retryAfter time.Duration
}

func (e *apiError) Error() string { return e.msg }

// refusal is an apiError with one of the refusals wire declares.
func refusal(c wire.ErrorCode, msg string) *apiError {
	return &apiError{status: c.Status, code: httpserver.ErrorCode(c.Code), msg: msg}
}

// errShuttingDown answers a request the server cannot serve because it is
// stopping: a wait at Release, or any route once the store is closed.
var errShuttingDown = refusal(wire.ErrShuttingDown, "the server is shutting down; retry once it is back")

// errTimeout answers a request whose route deadline passed. The deadline
// ends store calls only before they commit, so nothing changed.
var errTimeout = func() *apiError {
	e := refusal(wire.ErrTimeout, "the request ran out of time before anything was stored; nothing changed: send it again")
	e.retryAfter = time.Second
	return e
}()

func badRequest(msg string) *apiError {
	return refusal(wire.ErrInvalid, msg)
}

// whyDelegateGate is the store's Why on the delegate gate's 409.
const whyDelegateGate = "delegate_gate"

var kindStatus = map[store.Kind]wire.ErrorCode{
	store.KindInvalid:   wire.ErrInvalid,
	store.KindNotFound:  wire.ErrNotFound,
	store.KindConflict:  wire.ErrConflict,
	store.KindGone:      wire.ErrGone,
	store.KindFull:      wire.ErrStoreFull,
	store.KindForbidden: wire.ErrForbidden,
	store.KindKeyReused: wire.ErrKeyReused,
}

// fail answers err: a refusal with its status and the fix, anything else
// with a 500 and one ERROR line. A request whose client went away gets
// nothing, since nobody reads it.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return
	}
	switch {
	case errors.Is(err, store.ErrClosed):
		err = errShuttingDown
	case errors.Is(err, context.DeadlineExceeded) && r.Context().Err() != nil:
		err = errTimeout
	case errors.Is(err, store.ErrOtherKey):
		s.metrics.AuthFailures.Inc(obs.AuthWrongKeyIdentity)
	}
	body := wire.Error{RequestID: httpserver.RequestIDFromContext(r.Context()), PathRoot: s.cfg.PathRoot}
	var status int
	var se *store.Error
	var ae *apiError
	switch {
	case errors.As(err, &se):
		k := kindStatus[se.Kind]
		status, body.Code, body.Error = k.Status, k.Code, se.Msg
		if se.Why != "" {
			body.Code = se.Why
		}
		if se.Why == whyDelegateGate {
			s.log.Info("request refused", "method", r.Method, "path", r.URL.Path)
		}
	case errors.As(err, &ae):
		status, body.Code, body.Error = ae.status, string(ae.code), ae.msg
		if ae.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(ae.retryAfter.Seconds())))
		}
	default:
		s.log.Error("request failed", "path", r.URL.Path, "error", err)
		status, body.Code, body.Error = wire.ErrInternal.Status, wire.ErrInternal.Code, "internal server error"
	}
	if status == http.StatusUnauthorized || status == http.StatusUpgradeRequired {
		body.PathRoot = ""
	}
	httpserver.WriteJSONStatus(w, status, body)
}

// AuthErrors writes access's refusals, the sign-in routes' and the gate's,
// in the API's error envelope, logging to logger each one of status 500 or
// more and counting on m each attempt the failure limiter refused. A gate
// refusal says what grelot wants instead (gateWords); a sign-in route's keeps
// access's words.
func AuthErrors(logger *slog.Logger, m *obs.Metrics) access.ErrorWriter {
	return func(w http.ResponseWriter, r *http.Request, e access.Error) {
		if e.Status >= http.StatusInternalServerError {
			logger.Warn("auth request failed", "path", r.URL.Path, "code", string(e.Code), "error", fmt.Sprint(e.Err))
		}
		if e.Code == access.CodeRateLimited {
			m.AuthFailures.Inc(obs.AuthRateLimited)
		}
		msg := e.Detail
		if !strings.HasPrefix(r.URL.Path, auth.Prefix+"/") {
			msg = cmp.Or(gateWords(r, e.Code), msg)
		}
		httpserver.WriteJSONStatus(w, e.Status, wire.Error{
			Error: msg, Code: string(e.Code), Reason: e.Reason, RequestID: httpserver.RequestIDFromContext(r.Context()),
		})
	}
}

// gateWords is the fix for a gate refusal of r with code, "" for a code
// the gate does not answer.
func gateWords(r *http.Request, code access.Code) string {
	machine := strings.HasPrefix(r.URL.Path, "/v1/")
	switch code {
	case access.CodeAuthRequired, access.CodeSessionExpired, access.CodeInvalidCredentials, access.CodeKeyInvalid:
		if machine {
			return "a valid machine key is required: set token in the grelot client config to a key from grelot key create"
		}
		return "sign in to use the web UI"
	case access.CodeKeyRequired:
		return "the agent API takes a machine's API key, not a browser session"
	case access.CodeSessionRequired:
		return "the web API takes a signed-in browser session; an API key works on /v1 only"
	case access.CodeCrossOrigin:
		return "a cross-origin request cannot change anything: send it from grelot's own pages"
	case access.CodeUnavailable:
		return "the accounts file could not be read; try again shortly"
	case access.CodeNotFound, access.CodeMethodNotAllowed:
		return "no such route: " + r.Method + " " + r.URL.Path
	default:
		return ""
	}
}
