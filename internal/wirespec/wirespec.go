// Package wirespec is the build-time half of the wire contract: each
// wire.Routes row's types, summary and refusals, and the wiregen registry
// cmd/wire-codegen writes the typed client, docs/openapi.json and
// deadset-edges.json from. Only cmd/wire-codegen and tests import it,
// because wiregen loads golang.org/x/tools/go/packages, which must stay out
// of the binary (TestBinaryImportsNoCodegen).
package wirespec

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"

	"github.com/cplieger/go/wiregen/v2"
	"github.com/cplieger/grelot/internal/wire"
)

// Spec documents one row. A nil type is none: no path parameters, no
// query, no body.
type Spec struct {
	Params   reflect.Type
	Query    reflect.Type
	Request  reflect.Type
	Response reflect.Type
	Summary  string
	Errors   []wire.ErrorCode
}

// Lookup is the spec of the row named name, a copy the caller may change.
func Lookup(name string) (Spec, bool) {
	s, ok := specs[name]
	s.Errors = slices.Clone(s.Errors)
	return s, ok
}

// The access gate's refusals, access/gate's Codes for a Session row with
// their statuses; the server's tests hold each route's list to it.
var (
	errMethodNotAllowed    = wire.ErrorCode{Code: "auth_method_not_allowed", Status: http.StatusMethodNotAllowed}
	errAuthRequired        = wire.ErrorCode{Code: "auth_required", Status: http.StatusUnauthorized}
	errSessionExpired      = wire.ErrorCode{Code: "auth_session_expired", Status: http.StatusUnauthorized}
	errInvalidCredentials  = wire.ErrorCode{Code: "auth_invalid_credentials", Status: http.StatusUnauthorized}
	errKeyInvalid          = wire.ErrorCode{Code: "auth_key_invalid", Status: http.StatusUnauthorized}
	errAuthSessionRequired = wire.ErrorCode{Code: "auth_session_required", Status: http.StatusForbidden}
	errAuthUnavailable     = wire.ErrorCode{Code: "auth_unavailable", Status: http.StatusServiceUnavailable}
	// errCrossOrigin refuses a write from another site.
	errCrossOrigin = wire.ErrorCode{Code: "auth_cross_origin", Status: http.StatusForbidden}
	// errStreamingUnsupported is the event stream's hub on a writer that
	// cannot flush.
	errStreamingUnsupported = wire.ErrorCode{Code: "streaming_unsupported", Status: http.StatusInternalServerError}
)

func typeOf[T any]() reflect.Type { return reflect.TypeFor[T]() }

// sessionGate is what the gate answers on a session's route.
var sessionGate = []wire.ErrorCode{
	errMethodNotAllowed, errAuthRequired, errSessionExpired, errInvalidCredentials, errKeyInvalid,
	errAuthSessionRequired, errAuthUnavailable,
}

// every route's refusals beyond its own: the gate's, a deadline passed,
// the server stopping, a failure of its own.
var common = append(slices.Clone(sessionGate), wire.ErrTimeout, wire.ErrShuttingDown, wire.ErrInternal)

func errs(own ...wire.ErrorCode) []wire.ErrorCode { return append(own, common...) }

// writes is errs for a route that changes something, which the gate also
// refuses from another site.
func writes(own ...wire.ErrorCode) []wire.ErrorCode { return errs(append(own, errCrossOrigin)...) }

// specs documents every wire.Routes row by its Name.
var specs = map[string]Spec{
	"listConversations": {
		Summary: "Conversations newest activity first, 50 a page",
		Query:   typeOf[wire.ConversationsQuery](), Response: typeOf[wire.ConversationsPage](), Errors: errs(wire.ErrInvalid),
	},
	"getConversation": {
		Summary: "A conversation's posts oldest first, 100 a page, and its context",
		Params:  typeOf[wire.ConversationParams](), Query: typeOf[wire.PageQuery](), Response: typeOf[wire.Conversation](),
		Errors: errs(wire.ErrInvalid, wire.ErrNotFound),
	},
	"markConversationRead": {
		Summary: "Mark the user's unread posts in a conversation read, up to the newest shown",
		Params:  typeOf[wire.ConversationParams](), Request: typeOf[wire.MarkReadRequest](), Response: typeOf[wire.MarkedRead](),
		Errors: writes(wire.ErrInvalid, wire.ErrForbidden, wire.ErrNotFound),
	},
	"getUserReceipts": {
		Summary: "Who has read a message",
		Params:  typeOf[wire.MessageParams](), Response: typeOf[wire.UserReceipts](), Errors: errs(wire.ErrInvalid, wire.ErrNotFound),
	},
	"createUserPost": {
		Summary: "Post as @user to announce, a path thread or an agent",
		Request: typeOf[wire.UserPostRequest](), Response: typeOf[wire.UserPosted](),
		Errors: writes(wire.ErrInvalid, wire.ErrForbidden, wire.ErrNotFound, wire.ErrConflict, wire.ErrGone, wire.ErrTooLarge,
			wire.ErrKeyReused, wire.ErrStoreFull),
	},
	"listAgents": {
		Summary:  "Every live and dormant agent, by name",
		Response: typeOf[wire.AgentList](), Errors: errs(),
	},
	"getAgent": {
		Summary: "An agent's card, reservations and delegates",
		Params:  typeOf[wire.AgentParams](), Response: typeOf[wire.AgentPage](), Errors: errs(wire.ErrNotFound),
	},
	"getAgentHistory": {
		Summary: "Every post an agent sent or received, newest first, 100 a page",
		Params:  typeOf[wire.AgentParams](), Query: typeOf[wire.PageQuery](), Response: typeOf[wire.HistoryPage](),
		Errors: errs(wire.ErrInvalid, wire.ErrNotFound),
	},
	"getStats": {
		Summary: "The Statistics tab over a range",
		Query:   typeOf[wire.StatsQuery](), Response: typeOf[wire.Stats](), Errors: errs(wire.ErrInvalid),
	},
	"getSettings": {
		Summary:  "The history retention and sign-in methods",
		Response: typeOf[wire.Settings](), Errors: errs(),
	},
	"putSettings": {
		Summary: "Replace the settings; a change of sign-in methods needs a recent sign-in",
		Request: typeOf[wire.SettingsRequest](), Response: typeOf[wire.Settings](),
		Errors: writes(wire.ErrInvalid, wire.ErrForbidden, wire.ErrRecentAuthRequired, wire.ErrWouldLockOut),
	},
	"getSignIn": {
		Summary:  "What the sign-in page shows beside the sign-in routes' state; needs no session",
		Response: typeOf[wire.SignIn](), Errors: []wire.ErrorCode{errMethodNotAllowed, wire.ErrInternal},
	},
	"events": {
		Summary: "A changed event after each commit the web UI shows, resumed from Last-Event-ID, between the sse hub's hello and keepalive events",
		Errors:  append(slices.Clone(sessionGate), wire.ErrSSEUnavailable, errStreamingUnsupported),
	},
}

// Registry is the contract wiregen generates from: every wire.Routes row
// joined with its spec, in the rows' order. It panics on a row with no
// spec, which TestSpecCoversEveryRow keeps from shipping.
func Registry() *wiregen.Registry {
	return &wiregen.Registry{
		ErrorEnvelope: wiregen.TypeRef[wire.Error](),
		OpenAPI: wiregen.OpenAPIInfo{
			Title:       "grelot web API",
			Version:     "2",
			Description: "The routes the web UI calls, under /api, for a signed-in session.",
		},
		Enums: []wiregen.EnumRef{
			wiregen.StringEnum[wire.Show](),
			wiregen.StringEnum[wire.ThreadKind](),
			wiregen.StringEnum[wire.SenderKind](),
			wiregen.StringEnum[wire.ReservationState](),
			wiregen.StringEnum[wire.AgentState](),
			wiregen.StringEnum[wire.StatsRange](),
			wiregen.StringEnum[wire.EventKind](),
		},
		// The types a route's own types reach; a route's own are registered
		// by naming them on its endpoint.
		Types: []wiregen.Type{
			wiregen.TypeRef[wire.ConversationThread](), wiregen.TypeRef[wire.LastPost](), wiregen.TypeRef[wire.ConversationRow](),
			wiregen.TypeRef[wire.ConversationCounts](),
			wiregen.TypeRef[wire.ReadCount](), wiregen.TypeRef[wire.Post](), wiregen.TypeRef[wire.ConversationReservation](),
			wiregen.TypeRef[wire.Agent](), wiregen.TypeRef[wire.UserReceipt](), wiregen.TypeRef[wire.PostTarget](),
			wiregen.TypeRef[wire.HistoryPost](), wiregen.TypeRef[wire.PostBucket](), wiregen.TypeRef[wire.AgentStats](),
			wiregen.TypeRef[wire.ThreadStats](), wiregen.TypeRef[wire.DelayHistogram](), wiregen.TypeRef[wire.Sample](),
			wiregen.TypeRef[wire.Event](), wiregen.TypeRef[wire.EventCounts](), wiregen.TypeRef[wire.HeldTurns](),
			wiregen.TypeRef[wire.Methods](),
		},
		Endpoints: endpoints(wire.Routes),
		Events:    []wiregen.Event{{Name: wire.EventChanged, Data: wiregen.TypeRef[wire.Changed]()}},
	}
}

// idempotencyKey is a keyed route's header, whose byte bounds count the
// key's two quotes.
var idempotencyKey = wiregen.Header{
	Name: "Idempotency-Key", Arg: "idempotencyKey", Required: true,
	MinBytes: wire.IdempotencyKeyMin + 2, MaxBytes: wire.IdempotencyKeyMax + 2,
	Description: fmt.Sprintf("A new quoted random key of %d to %d letters, digits, - or _ per post, the same key when it is sent again.",
		wire.IdempotencyKeyMin, wire.IdempotencyKeyMax),
}

func endpoints(routes []wire.Route) []wiregen.Endpoint {
	out := make([]wiregen.Endpoint, 0, len(routes))
	for i := range routes {
		r := &routes[i]
		s, ok := Lookup(r.Name)
		if !ok {
			panic("wirespec: row " + r.Name + " has no spec")
		}
		e := wiregen.Endpoint{
			Name: r.Name, Method: r.Method, Path: r.Path, Summary: s.Summary, Status: r.Status,
			Params: wiregen.TypeOf(s.Params), Query: wiregen.TypeOf(s.Query),
			Request: wiregen.TypeOf(s.Request), Response: wiregen.TypeOf(s.Response),
			Kind: wiregen.KindJSON,
		}
		if r.Kind == wire.KindEventStream {
			e.Kind = wiregen.KindEventStream
		}
		if r.Keyed {
			e.Headers = []wiregen.Header{idempotencyKey}
		}
		for _, c := range s.Errors {
			e.Errors = append(e.Errors, wiregen.ErrorRef{Code: c.Code, Status: c.Status})
		}
		out = append(out, e)
	}
	return out
}
