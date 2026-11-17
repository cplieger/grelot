package wire

import (
	"net/http"
	"net/url"
	"strings"
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
	// ErrStaleRevision refuses a settings save over another revision than
	// the stored one; the client reads the settings again.
	ErrStaleRevision = ErrorCode{"stale_revision", http.StatusConflict}
	// ErrSSEUnavailable is the event stream's hub refusing a subscriber once
	// it is shut down or at its client cap.
	ErrSSEUnavailable = ErrorCode{"sse_unavailable", http.StatusServiceUnavailable}
	// ErrStreamingUnsupported is the event stream's hub on a writer that
	// cannot flush.
	ErrStreamingUnsupported = ErrorCode{"streaming_unsupported", http.StatusInternalServerError}
	ErrRateLimited          = ErrorCode{"rate_limited", http.StatusTooManyRequests}
	// ErrUpgradeRequired refuses a client below the server's floor; its
	// error is the install line.
	ErrUpgradeRequired = ErrorCode{"upgrade_required", http.StatusUpgradeRequired}
	ErrTooManyWaits    = ErrorCode{"too_many_waits", http.StatusTooManyRequests}
	// ErrCursorInvalid and ErrCursorMismatch refuse a list read's cursor:
	// not one the server minted in the last 24 hours, or minted for
	// another row, query or credential. The client reads page one again.
	ErrCursorInvalid  = ErrorCode{"cursor_invalid", http.StatusBadRequest}
	ErrCursorMismatch = ErrorCode{"cursor_mismatch", http.StatusBadRequest}
)

// The reasons that refine a code: the resource a not_found names, and a
// too_large past the request body's cap rather than a field's bound.
const (
	ReasonAgent        = "agent"
	ReasonConversation = "conversation"
	ReasonMessage      = "message"
	ReasonIdentity     = "identity"
	ReasonDelegate     = "delegate"
	ReasonReservation  = "reservation"
	ReasonRequestBody  = "request_body"
)

// Common is what every row can answer beyond its own codes and the gate's:
// a deadline passed, the server stopping, a failure of its own. A row the
// gate authenticates can also answer ErrRateLimited, from a key's bucket.
var Common = []ErrorCode{ErrTimeout, ErrShuttingDown, ErrInternal}

// The refusals of a request acting as an agent identity, named by the
// store or the server.
var (
	// ErrNone and ErrDormant refuse a session with no live identity.
	ErrNone          = ErrorCode{"none", http.StatusGone}
	ErrDormant       = ErrorCode{"dormant", http.StatusGone}
	ErrDelegateEnded = ErrorCode{"delegate_ended", http.StatusGone}
	ErrLinkGone      = ErrorCode{"link_gone", http.StatusGone}
	ErrDelegateGate  = ErrorCode{"delegate_gate", http.StatusConflict}
	ErrReserved      = ErrorCode{"reserved", http.StatusConflict}
	ErrQueued        = ErrorCode{"queued", http.StatusConflict}
	ErrOtherProvider = ErrorCode{"other_provider", http.StatusConflict}
	ErrNoEnvironment = ErrorCode{"no_environment", http.StatusConflict}
	ErrOtherRepo     = ErrorCode{"other_repository", http.StatusConflict}
	ErrOtherEnv      = ErrorCode{"other_environment", http.StatusConflict}
	// ErrEnded, ErrReplaced and ErrRestored end a standing wait: its
	// session's end was reported, a newer wait of the session took over, or
	// host push took over from the watcher.
	ErrEnded    = ErrorCode{"ended", http.StatusGone}
	ErrReplaced = ErrorCode{"replaced", http.StatusGone}
	ErrRestored = ErrorCode{"restored", http.StatusGone}
)

// Access is who a row admits; the zero value admits no one, which the
// server refuses to bind.
type Access uint8

// The access classes.
const (
	// AccessAgent is a machine key acting as its own identities.
	AccessAgent Access = iota + 1
	// AccessOwner is the person: a signed-in session.
	AccessOwner
	// AccessPublic needs no credential.
	AccessPublic
)

// The key scopes: a key holds ScopeAgent or ScopeOwner. ScopeAgentOrOwner
// is a row's rule, never a key's scope, so no key is minted with it.
const (
	ScopeAgent        = "agent"
	ScopeOwner        = "owner"
	ScopeAgentOrOwner = "agent-or-owner"
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
	Access Access
	// Keyed routes require an Idempotency-Key, which a re-send repeats.
	Keyed bool
	// Paged routes are list reads: they take cursor and limit and answer
	// a type embedding List.
	Paged bool
}

// The Idempotency-Key a keyed route requires is a quoted string of
// IdempotencyKeyMin to IdempotencyKeyMax characters.
const (
	IdempotencyKeyMin = 16
	IdempotencyKeyMax = 64
)

// Pattern is the route's net/http ServeMux pattern.
func (r *Route) Pattern() string { return r.Method + " " + r.Path }

// Path is the path of the row called name with params filling its
// wildcards, each escaped as one segment. It panics on a name no row has
// or a wildcard params does not fill, which is a caller's wiring bug.
func Path(name string, params map[string]string) string {
	for i := range Routes {
		if Routes[i].Name != name {
			continue
		}
		segs := strings.Split(Routes[i].Path, "/")
		for j, seg := range segs {
			if !strings.HasPrefix(seg, "{") {
				continue
			}
			v, ok := params[strings.Trim(seg, "{}")]
			if !ok {
				panic("wire: row " + name + " needs path parameter " + seg)
			}
			segs[j] = url.PathEscape(v)
		}
		return strings.Join(segs, "/")
	}
	panic("wire: no row " + name)
}

// The paths access's sign-in, account and key routes are mounted under.
const (
	AuthPrefix = "/api/auth"
	OIDCPrefix = AuthPrefix + "/oidc"
)

// mePath is the session's own identity.
const mePath = "/v1/me"

// Routes is every row the server binds, by Name.
var Routes = []Route{
	{Name: "register", Method: http.MethodPost, Path: "/v1/register", Status: http.StatusCreated, Access: AccessAgent, Keyed: true},
	{Name: "revive", Method: http.MethodPost, Path: "/v1/revive", Access: AccessAgent, Keyed: true},
	{Name: "getMe", Method: http.MethodGet, Path: mePath, Access: AccessAgent},
	{Name: "patchMe", Method: http.MethodPatch, Path: mePath, Access: AccessAgent, Keyed: true},
	{Name: "deleteMe", Method: http.MethodDelete, Path: mePath, Access: AccessAgent, Keyed: true},
	{Name: "listIdentities", Method: http.MethodGet, Path: "/v1/identities", Access: AccessAgent, Paged: true},
	{Name: "getVersion", Method: http.MethodGet, Path: "/v1/version", Access: AccessAgent},
	{Name: "createPost", Method: http.MethodPost, Path: "/v1/posts", Status: http.StatusCreated, Access: AccessAgent, Keyed: true},
	{Name: "listThreads", Method: http.MethodGet, Path: "/v1/threads", Access: AccessAgent, Paged: true},
	{Name: "getReceipts", Method: http.MethodGet, Path: "/v1/messages/{id}/receipts", Access: AccessAgent},
	{Name: "getHistory", Method: http.MethodGet, Path: "/v1/history", Access: AccessAgent, Paged: true},
	{Name: "follow", Method: http.MethodPut, Path: "/v1/follows", Access: AccessAgent, Keyed: true},
	{Name: "unfollow", Method: http.MethodDelete, Path: "/v1/follows", Access: AccessAgent, Keyed: true},
	{Name: "ackInbox", Method: http.MethodPost, Path: "/v1/inbox/ack", Access: AccessAgent, Keyed: true},
	{Name: "reserve", Method: http.MethodPost, Path: "/v1/reservations", Status: http.StatusCreated, Access: AccessAgent, Keyed: true},
	{Name: "listReservations", Method: http.MethodGet, Path: "/v1/reservations", Access: AccessAgent, Paged: true},
	{Name: "renewReservation", Method: http.MethodPatch, Path: "/v1/reservations/{id}", Access: AccessAgent, Keyed: true},
	{Name: "releaseReservation", Method: http.MethodDelete, Path: "/v1/reservations/{id}", Access: AccessAgent, Keyed: true},
	{Name: "grantDelegate", Method: http.MethodPost, Path: "/v1/delegates", Status: http.StatusCreated, Access: AccessAgent, Keyed: true},
	{Name: "revokeDelegate", Method: http.MethodDelete, Path: "/v1/delegates/{label}", Access: AccessAgent, Keyed: true},
	{Name: "lifecycle", Method: http.MethodPost, Path: "/v1/lifecycle", Access: AccessAgent},
	{Name: "holdEvent", Method: http.MethodPost, Path: "/v1/events/hold", Access: AccessAgent},
	{Name: "waitEvent", Method: http.MethodPost, Path: "/v1/events/wait", Access: AccessAgent},
	{Name: "readInbox", Method: http.MethodGet, Path: "/v1/inbox", Access: AccessAgent, Paged: true},
	{Name: "listConversations", Method: http.MethodGet, Path: "/api/conversations", Access: AccessOwner},
	{Name: "getConversation", Method: http.MethodGet, Path: "/api/conversations/{id}", Access: AccessOwner},
	{Name: "markConversationRead", Method: http.MethodPost, Path: "/api/conversations/{id}/read", Access: AccessOwner},
	{Name: "getUserReceipts", Method: http.MethodGet, Path: "/api/messages/{id}/receipts", Access: AccessOwner},
	{Name: "createUserPost", Method: http.MethodPost, Path: "/api/posts", Status: http.StatusCreated, Access: AccessOwner, Keyed: true},
	{Name: "listAgents", Method: http.MethodGet, Path: "/api/agents", Access: AccessOwner},
	{Name: "getAgent", Method: http.MethodGet, Path: "/api/agents/{name}", Access: AccessOwner},
	{Name: "getAgentHistory", Method: http.MethodGet, Path: "/api/agents/{name}/history", Access: AccessOwner},
	{Name: "getStats", Method: http.MethodGet, Path: "/api/stats", Access: AccessOwner},
	{Name: "getSettings", Method: http.MethodGet, Path: "/api/settings", Access: AccessOwner},
	{Name: "putSettings", Method: http.MethodPut, Path: "/api/settings", Access: AccessOwner},
	{Name: "getSignIn", Method: http.MethodGet, Path: "/api/sign-in", Access: AccessPublic},
	{Name: "events", Method: http.MethodGet, Path: "/api/events", Kind: KindEventStream, Access: AccessOwner},
}
