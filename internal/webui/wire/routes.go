package wire

import (
	"net/http"
	"reflect"
	"slices"
)

// Kind is how a route answers.
type Kind uint8

// The route kinds.
const (
	// KindJSON answers its Response as JSON.
	KindJSON Kind = iota
	// KindEventStream answers text/event-stream carrying Changed events.
	KindEventStream
)

// ErrorCode is one refusal a route can answer, with its status.
type ErrorCode struct {
	Code   string
	Status int
}

// The refusals of the web API.
var (
	ErrInvalid      = ErrorCode{"invalid", http.StatusBadRequest}
	ErrForbidden    = ErrorCode{"forbidden", http.StatusForbidden}
	ErrNotFound     = ErrorCode{"not_found", http.StatusNotFound}
	ErrConflict     = ErrorCode{"conflict", http.StatusConflict}
	ErrGone         = ErrorCode{"gone", http.StatusGone}
	ErrTooLarge     = ErrorCode{"too_large", http.StatusRequestEntityTooLarge}
	ErrKeyReused    = ErrorCode{"key_reused", http.StatusUnprocessableEntity}
	ErrInternal     = ErrorCode{"internal_error", http.StatusInternalServerError}
	ErrTimeout      = ErrorCode{"timeout", http.StatusServiceUnavailable}
	ErrShuttingDown = ErrorCode{"shutting_down", http.StatusServiceUnavailable}
	ErrStoreFull    = ErrorCode{"store_full", http.StatusInsufficientStorage}
	// ErrRecentAuthRequired asks for a step-up before the change.
	ErrRecentAuthRequired = ErrorCode{"recent_auth_required", http.StatusForbidden}
	// ErrWouldLockOut refuses a change leaving the account no way to sign in.
	ErrWouldLockOut = ErrorCode{"would_lock_out", http.StatusConflict}
	// The event stream's hub answers these two itself: once it is shut
	// down or at its client cap, and on a writer that cannot flush.
	ErrSSEUnavailable       = ErrorCode{"sse_unavailable", http.StatusServiceUnavailable}
	errStreamingUnsupported = ErrorCode{"streaming_unsupported", http.StatusInternalServerError}
)

// Route is one web API route. A nil type is none: no path parameters, no
// query, no body.
type Route struct {
	Params   reflect.Type
	Query    reflect.Type
	Request  reflect.Type
	Response reflect.Type
	// Name is the generated client function's name, and the key the
	// server binds the route's handler by.
	Name    string
	Method  string
	Path    string
	Summary string
	Errors  []ErrorCode
	// Status is the success status; zero is 200.
	Status int
	Kind   Kind
	// Keyed routes require an Idempotency-Key, which a re-send repeats.
	Keyed bool
	// Public routes answer without a session.
	Public bool
}

// The Idempotency-Key a keyed route requires is a quoted string of
// IdempotencyKeyMin to IdempotencyKeyMax characters.
const (
	IdempotencyKeyMin = 16
	IdempotencyKeyMax = 64
)

// Pattern is the route's net/http ServeMux pattern.
func (r *Route) Pattern() string { return r.Method + " " + r.Path }

// The access gate's refusals, access/gate's Codes for a Session row with
// their statuses; the server's tests hold each route's list to it.
var (
	errMethodNotAllowed    = ErrorCode{"auth_method_not_allowed", http.StatusMethodNotAllowed}
	errAuthRequired        = ErrorCode{"auth_required", http.StatusUnauthorized}
	errSessionExpired      = ErrorCode{"auth_session_expired", http.StatusUnauthorized}
	errInvalidCredentials  = ErrorCode{"auth_invalid_credentials", http.StatusUnauthorized}
	errKeyInvalid          = ErrorCode{"auth_key_invalid", http.StatusUnauthorized}
	errAuthSessionRequired = ErrorCode{"auth_session_required", http.StatusForbidden}
	errAuthUnavailable     = ErrorCode{"auth_unavailable", http.StatusServiceUnavailable}
	// errCrossOrigin refuses a write from another site.
	errCrossOrigin = ErrorCode{"auth_cross_origin", http.StatusForbidden}
)

func typeOf[T any]() reflect.Type { return reflect.TypeFor[T]() }

// sessionGate is what the gate answers on a session's route.
var sessionGate = []ErrorCode{
	errMethodNotAllowed, errAuthRequired, errSessionExpired, errInvalidCredentials, errKeyInvalid,
	errAuthSessionRequired, errAuthUnavailable,
}

// every route's refusals beyond its own: the gate's, a deadline passed,
// the server stopping, a failure of its own.
var common = append(slices.Clone(sessionGate), ErrTimeout, ErrShuttingDown, ErrInternal)

func errs(own ...ErrorCode) []ErrorCode { return append(own, common...) }

// writes is errs for a route that changes something, which the gate also
// refuses from another site.
func writes(own ...ErrorCode) []ErrorCode { return errs(append(own, errCrossOrigin)...) }

// Routes is the web API, every route under /api. The server binds exactly
// these, and the browser's client and docs/openapi.json are generated
// from them.
var Routes = []Route{
	{
		Name: "listConversations", Method: http.MethodGet, Path: "/api/conversations",
		Summary: "Conversations newest activity first, 50 a page",
		Query:   typeOf[ConversationsQuery](), Response: typeOf[ConversationsPage](), Errors: errs(ErrInvalid),
	},
	{
		Name: "getConversation", Method: http.MethodGet, Path: "/api/conversations/{id}",
		Summary: "A conversation's posts oldest first, 100 a page, and its context",
		Params:  typeOf[ConversationParams](), Query: typeOf[PageQuery](), Response: typeOf[Conversation](),
		Errors: errs(ErrInvalid, ErrNotFound),
	},
	{
		Name: "markConversationRead", Method: http.MethodPost, Path: "/api/conversations/{id}/read",
		Summary: "Mark the user's unread posts in a conversation read, up to the newest shown",
		Params:  typeOf[ConversationParams](), Request: typeOf[MarkReadRequest](), Response: typeOf[MarkedRead](), Errors: writes(ErrInvalid, ErrForbidden, ErrNotFound),
	},
	{
		Name: "getReceipts", Method: http.MethodGet, Path: "/api/messages/{id}/receipts",
		Summary: "Who has read a message",
		Params:  typeOf[MessageParams](), Response: typeOf[Receipts](), Errors: errs(ErrInvalid, ErrNotFound),
	},
	{
		Name: "createPost", Method: http.MethodPost, Path: "/api/posts",
		Summary: "Post as @user to announce, a path thread or an agent",
		Request: typeOf[PostRequest](), Response: typeOf[Posted](), Status: http.StatusCreated, Keyed: true,
		Errors: writes(ErrInvalid, ErrForbidden, ErrNotFound, ErrConflict, ErrGone, ErrTooLarge, ErrKeyReused, ErrStoreFull),
	},
	{
		Name: "listAgents", Method: http.MethodGet, Path: "/api/agents",
		Summary:  "Every live and dormant agent, by name",
		Response: typeOf[AgentList](), Errors: errs(),
	},
	{
		Name: "getAgent", Method: http.MethodGet, Path: "/api/agents/{name}",
		Summary: "An agent's card, reservations and delegates",
		Params:  typeOf[AgentParams](), Response: typeOf[AgentPage](), Errors: errs(ErrNotFound),
	},
	{
		Name: "getAgentHistory", Method: http.MethodGet, Path: "/api/agents/{name}/history",
		Summary: "Every post an agent sent or received, newest first, 100 a page",
		Params:  typeOf[AgentParams](), Query: typeOf[PageQuery](), Response: typeOf[HistoryPage](),
		Errors: errs(ErrInvalid, ErrNotFound),
	},
	{
		Name: "getStats", Method: http.MethodGet, Path: "/api/stats",
		Summary: "The Statistics tab over a range",
		Query:   typeOf[StatsQuery](), Response: typeOf[Stats](), Errors: errs(ErrInvalid),
	},
	{
		Name: "getSettings", Method: http.MethodGet, Path: "/api/settings",
		Summary:  "The history retention and sign-in methods",
		Response: typeOf[Settings](), Errors: errs(),
	},
	{
		Name: "putSettings", Method: http.MethodPut, Path: "/api/settings",
		Summary: "Replace the settings; a change of sign-in methods needs a recent sign-in",
		Request: typeOf[SettingsRequest](), Response: typeOf[Settings](),
		Errors: writes(ErrInvalid, ErrForbidden, ErrRecentAuthRequired, ErrWouldLockOut),
	},
	{
		Name: "getSignIn", Method: http.MethodGet, Path: "/api/sign-in",
		Summary:  "What the sign-in page shows beside the sign-in routes' state; needs no session",
		Response: typeOf[SignIn](), Public: true, Errors: []ErrorCode{errMethodNotAllowed, ErrInternal},
	},
	{
		Name: "events", Method: http.MethodGet, Path: "/api/events",
		Summary: "A changed event after each commit the web UI shows, resumed from Last-Event-ID, between the sse hub's hello and keepalive events",
		Kind:    KindEventStream, Errors: append(slices.Clone(sessionGate), ErrSSEUnavailable, errStreamingUnsupported),
	},
}
