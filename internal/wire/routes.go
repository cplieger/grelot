package wire

import "net/http"

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

// The refusals the server's own code answers.
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
	// ErrSSEUnavailable is the event stream's hub refusing a subscriber once
	// it is shut down or at its client cap.
	ErrSSEUnavailable = ErrorCode{"sse_unavailable", http.StatusServiceUnavailable}
)

// Route is one row the server binds: what it reads at run time. Each row's
// types, summary and refusals are internal/wirespec's, under the same Name.
type Route struct {
	// Name is the generated client function's name, and the key the
	// server binds the route's handler by.
	Name   string
	Method string
	Path   string
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

// Routes is every row the server binds, by Name.
var Routes = []Route{
	{Name: "listConversations", Method: http.MethodGet, Path: "/api/conversations"},
	{Name: "getConversation", Method: http.MethodGet, Path: "/api/conversations/{id}"},
	{Name: "markConversationRead", Method: http.MethodPost, Path: "/api/conversations/{id}/read"},
	{Name: "getUserReceipts", Method: http.MethodGet, Path: "/api/messages/{id}/receipts"},
	{Name: "createUserPost", Method: http.MethodPost, Path: "/api/posts", Status: http.StatusCreated, Keyed: true},
	{Name: "listAgents", Method: http.MethodGet, Path: "/api/agents"},
	{Name: "getAgent", Method: http.MethodGet, Path: "/api/agents/{name}"},
	{Name: "getAgentHistory", Method: http.MethodGet, Path: "/api/agents/{name}/history"},
	{Name: "getStats", Method: http.MethodGet, Path: "/api/stats"},
	{Name: "getSettings", Method: http.MethodGet, Path: "/api/settings"},
	{Name: "putSettings", Method: http.MethodPut, Path: "/api/settings"},
	{Name: "getSignIn", Method: http.MethodGet, Path: "/api/sign-in", Public: true},
	{Name: "events", Method: http.MethodGet, Path: "/api/events", Kind: KindEventStream},
}
