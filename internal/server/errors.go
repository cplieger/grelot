package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/store"
)

// apiError is a refusal the HTTP layer makes itself, before the store.
type apiError struct {
	code   httpserver.ErrorCode
	msg    string
	status int
}

func (e *apiError) Error() string { return e.msg }

func badRequest(msg string) *apiError {
	return &apiError{status: http.StatusBadRequest, code: "invalid", msg: msg}
}

// errorBody is every error response. PathRoot is the server's path root,
// which a client needs to name a path again; an unauthenticated caller
// never gets it.
type errorBody struct {
	Error     string               `json:"error"`
	Code      httpserver.ErrorCode `json:"code,omitempty"`
	RequestID string               `json:"request_id,omitempty"`
	PathRoot  string               `json:"path_root,omitempty"`
}

var kindStatus = map[store.Kind]struct {
	code   httpserver.ErrorCode
	status int
}{
	store.KindInvalid:   {"invalid", http.StatusBadRequest},
	store.KindNotFound:  {"not_found", http.StatusNotFound},
	store.KindConflict:  {"conflict", http.StatusConflict},
	store.KindGone:      {"gone", http.StatusGone},
	store.KindFull:      {"store_full", http.StatusInsufficientStorage},
	store.KindForbidden: {"forbidden", http.StatusForbidden},
}

// fail answers err: a refusal with its status and the fix, anything else
// with a 500 and one ERROR line. A request whose client went away gets
// nothing, since nobody reads it.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return
	}
	body := errorBody{RequestID: httpserver.RequestIDFromContext(r.Context()), PathRoot: s.cfg.PathRoot}
	status := http.StatusInternalServerError
	var se *store.Error
	var ae *apiError
	switch {
	case errors.As(err, &se):
		k := kindStatus[se.Kind]
		status, body.Code, body.Error = k.status, k.code, se.Msg
		if se.Why != "" {
			body.Code = httpserver.ErrorCode(se.Why)
		}
	case errors.As(err, &ae):
		status, body.Code, body.Error = ae.status, ae.code, ae.msg
	default:
		s.log.Error("request failed", "path", r.URL.Path, "error", err)
		body.Code, body.Error = "internal_error", "internal server error"
	}
	if status == http.StatusUnauthorized || status == http.StatusUpgradeRequired {
		body.PathRoot = ""
	}
	httpserver.WriteJSONStatus(w, status, body)
}
