// Package wirespec is the build-time half of the wire contract: each
// wire.Routes row's types, summary and refusals, and the wiregen registry
// cmd/wire-codegen writes the typed client, docs/openapi.json and
// deadset-edges.json from. Only cmd/wire-codegen and tests import it,
// because wiregen loads golang.org/x/tools/go/packages, which must stay out
// of the binary (TestBinaryImportsNoCodegen).
package wirespec

import (
	"fmt"
	"reflect"
	"slices"

	"github.com/cplieger/access"
	"github.com/cplieger/access/gate"
	"github.com/cplieger/go/wiregen/v2"
	"github.com/cplieger/grelot/internal/wire"
	"github.com/cplieger/grelot/internal/wiregate"
)

// Spec documents one row. A nil type is none: no path parameters, no
// query, no body.
type Spec struct {
	Params   reflect.Type
	Query    reflect.Type
	Request  reflect.Type
	Response reflect.Type
	Summary  string
	// Errors are the row's own refusals; Registry adds the gate's, the
	// common ones and a list row's cursor refusals.
	Errors []wire.ErrorCode
	// Headers are the row's request headers beside Idempotency-Key and the
	// identity headers an agent row takes from its Access.
	Headers []wiregen.Header
	// AnySession is an agent row serving a request with no Grelot-Session
	// as a session with no identity.
	AnySession bool
}

// Lookup is the spec of the row named name, a copy the caller may change.
func Lookup(name string) (Spec, bool) {
	s, ok := specs[name]
	s.Errors = slices.Clone(s.Errors)
	return s, ok
}

func typeOf[T any]() reflect.Type { return reflect.TypeFor[T]() }

// asIdentity is what a request acting as its session's identity can be
// refused: a bad identity header, another key's identity, no live
// identity, an ended delegate, the delegate gate of a session with
// delegates.
var asIdentity = []wire.ErrorCode{wire.ErrInvalid, wire.ErrForbidden, wire.ErrNone, wire.ErrDormant, wire.ErrDelegateEnded, wire.ErrDelegateGate}

// agent is an agent row's own refusals: own, asIdentity and the client's
// version, each code once.
func agent(own ...wire.ErrorCode) []wire.ErrorCode {
	var out []wire.ErrorCode
	for _, c := range slices.Concat(own, asIdentity, []wire.ErrorCode{wire.ErrUpgradeRequired}) {
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// anySession is agent's for a row taking a request with no session: only
// a bad identity header and another key's identity of asIdentity.
func anySession(own ...wire.ErrorCode) []wire.ErrorCode {
	return append(own, wire.ErrInvalid, wire.ErrForbidden, wire.ErrUpgradeRequired)
}

// specs documents every wire.Routes row by its Name.
var specs = map[string]Spec{
	"register": {
		Summary: "Register this session as a new identity, take one over, or register a delegate from its link",
		Request: typeOf[wire.RegisterRequest](), Response: typeOf[wire.Registered](),
		Errors: agent(wire.ErrConflict, wire.ErrOtherEnv, wire.ErrOtherProvider, wire.ErrNoEnvironment, wire.ErrOtherRepo,
			wire.ErrLinkGone, wire.ErrKeyReused, wire.ErrStoreFull, wire.ErrTooLarge),
	},
	"revive": {
		Summary: "Make this session's dormant identity, or one of this environment by name, live again",
		Request: typeOf[wire.ReviveRequest](), Response: typeOf[wire.Registered](),
		Errors: agent(wire.ErrNotFound, wire.ErrGone, wire.ErrConflict, wire.ErrOtherProvider, wire.ErrNoEnvironment,
			wire.ErrKeyReused, wire.ErrStoreFull, wire.ErrTooLarge),
	},
	"getMe": {
		Summary: "This session's identity and status; with turn=start, the prompt hook's opening of the turn",
		Query:   typeOf[wire.MeQuery](), Response: typeOf[wire.Me](), Errors: agent(wire.ErrDelegateGate),
		Headers: []wiregen.Header{
			{
				Name: wire.HeaderEnvironment, Arg: "grelotEnvironment", MinBytes: 32, MaxBytes: 32,
				Description: "The session's environment id, for the take-over hint.",
			},
			{
				Name: wire.HeaderMachine, Arg: "grelotMachine", MinBytes: 1, MaxBytes: 43,
				Description: "The session's machine id, for the take-over hint.",
			},
		},
	},
	"patchMe": {
		Summary: "Change this session's identity, or renew its lease",
		Request: typeOf[wire.PatchMeRequest](), Response: typeOf[wire.IdentityAnswer](),
		Errors: agent(wire.ErrConflict, wire.ErrOtherRepo, wire.ErrKeyReused, wire.ErrStoreFull, wire.ErrTooLarge),
	},
	"deleteMe": {
		Summary:  "Retire this session's identity",
		Response: typeOf[wire.Retired](), Errors: agent(wire.ErrKeyReused),
	},
	"listIdentities": {
		Summary: "Every live identity, or with all=1 the dormant ones too, narrowed by the filters, by name",
		Query:   typeOf[wire.IdentitiesQuery](), Response: typeOf[wire.Peers](), Errors: anySession(), AnySession: true,
	},
	"getVersion": {
		Summary:  "The server's version and path root",
		Response: typeOf[wire.Version](), Errors: anySession(), AnySession: true,
	},
	"createPost": {
		Summary: "Post to announce, a path thread or an agent",
		Request: typeOf[wire.PostRequest](), Response: typeOf[wire.Posted](),
		Errors: agent(wire.ErrNotFound, wire.ErrConflict, wire.ErrDelegateGate, wire.ErrGone, wire.ErrOtherRepo,
			wire.ErrTooLarge, wire.ErrKeyReused, wire.ErrStoreFull),
	},
	"listThreads": {
		Summary: "The live path threads under a path, by path",
		Query:   typeOf[wire.ThreadsQuery](), Response: typeOf[wire.Threads](), Errors: agent(),
	},
	"getReceipts": {
		Summary: "Who has read a message this identity sent or received",
		Params:  typeOf[wire.MessageParams](), Response: typeOf[wire.Receipts](), Errors: agent(wire.ErrNotFound),
	},
	"getHistory": {
		Summary: "This identity's mailbox, read and unread, moving nothing; pages go back in time, each oldest first",
		Query:   typeOf[wire.HistoryQuery](), Response: typeOf[wire.History](), Errors: agent(),
	},
	"follow": {
		Summary: "Follow a path's threads",
		Query:   typeOf[wire.PathQuery](), Response: typeOf[wire.IdentityAnswer](),
		Errors: agent(wire.ErrKeyReused, wire.ErrStoreFull),
	},
	"unfollow": {
		Summary: "Stop following a path's threads",
		Query:   typeOf[wire.PathQuery](), Response: typeOf[wire.IdentityAnswer](),
		Errors: agent(wire.ErrKeyReused, wire.ErrStoreFull),
	},
	"ackInbox": {
		Summary: "Mark read every inbox entry up to one",
		Request: typeOf[wire.AckRequest](), Response: typeOf[wire.Acked](),
		Errors: agent(wire.ErrKeyReused, wire.ErrTooLarge),
	},
	"reserve": {
		Summary: "Reserve a path, or queue for it when it is taken",
		Request: typeOf[wire.ReserveRequest](), Response: typeOf[wire.Reserved](),
		Errors: agent(wire.ErrConflict, wire.ErrReserved, wire.ErrQueued, wire.ErrKeyReused, wire.ErrStoreFull, wire.ErrTooLarge),
	},
	"listReservations": {
		Summary: "The grants and queue entries overlapping a path, every one when it is absent, by id",
		Query:   typeOf[wire.ReservationsQuery](), Response: typeOf[wire.Reservations](), Errors: agent(),
	},
	"renewReservation": {
		Summary: "Renew one of this identity's reservations",
		Params:  typeOf[wire.ReservationParams](), Request: typeOf[wire.RenewRequest](), Response: typeOf[wire.ReservationAnswer](),
		Errors: agent(wire.ErrNotFound, wire.ErrKeyReused, wire.ErrTooLarge),
	},
	"releaseReservation": {
		Summary: "Free a grant or cancel a queue entry of this identity",
		Params:  typeOf[wire.ReservationParams](), Response: typeOf[wire.ReservationAnswer](),
		Errors: agent(wire.ErrNotFound, wire.ErrKeyReused),
	},
	"grantDelegate": {
		Summary: "A single-use link a subagent registers with",
		Request: typeOf[wire.GrantRequest](), Response: typeOf[wire.Granted](),
		Errors: agent(wire.ErrConflict, wire.ErrKeyReused, wire.ErrStoreFull, wire.ErrTooLarge),
	},
	"revokeDelegate": {
		Summary: "Revoke a label's unused link and end its delegate",
		Params:  typeOf[wire.DelegateParams](), Response: typeOf[wire.Revoked](),
		Errors: agent(wire.ErrNotFound, wire.ErrKeyReused),
	},
	"lifecycle": {
		Summary: "A hook's report of a session's end, a turn's end or a subagent's stop",
		Request: typeOf[wire.LifecycleRequest](), Response: typeOf[wire.Lifecycle](), Errors: agent(wire.ErrTooLarge),
	},
	"holdEvent": {
		Summary: "The stop hook's hold: whether wake mail keeps the turn open",
		Request: typeOf[wire.HoldRequest](), Response: typeOf[wire.Held](), Errors: agent(wire.ErrTooLarge),
	},
	"waitEvent": {
		Summary: "A standing wait, held up to 55s, that returns this identity's next nudge",
		Request: typeOf[wire.WaitRequest](), Response: typeOf[wire.Nudge](),
		Errors: agent(wire.ErrTooManyWaits, wire.ErrEnded, wire.ErrReplaced, wire.ErrTooLarge),
	},
	"readInbox": {
		Summary: "The unread entries oldest first, marking nothing; with wait_ms, held until wake mail arrives",
		Query:   typeOf[wire.InboxQuery](), Response: typeOf[wire.Inbox](), Errors: agent(wire.ErrTooManyWaits),
	},
	"listConversations": {
		Summary: "Conversations newest activity first, 50 a page",
		Query:   typeOf[wire.ConversationsQuery](), Response: typeOf[wire.ConversationsPage](), Errors: []wire.ErrorCode{wire.ErrInvalid},
	},
	"getConversation": {
		Summary: "A conversation's posts oldest first, 100 a page, and its context",
		Params:  typeOf[wire.ConversationParams](), Query: typeOf[wire.PageQuery](), Response: typeOf[wire.Conversation](),
		Errors: []wire.ErrorCode{wire.ErrInvalid, wire.ErrNotFound},
	},
	"markConversationRead": {
		Summary: "Mark the user's unread posts in a conversation read, up to the newest shown",
		Params:  typeOf[wire.ConversationParams](), Request: typeOf[wire.MarkReadRequest](), Response: typeOf[wire.MarkedRead](),
		Errors: []wire.ErrorCode{wire.ErrInvalid, wire.ErrForbidden, wire.ErrNotFound},
	},
	"getUserReceipts": {
		Summary: "Who has read a message",
		Params:  typeOf[wire.MessageParams](), Response: typeOf[wire.UserReceipts](), Errors: []wire.ErrorCode{wire.ErrInvalid, wire.ErrNotFound},
	},
	"createUserPost": {
		Summary: "Post as @user to announce, a path thread or an agent",
		Request: typeOf[wire.UserPostRequest](), Response: typeOf[wire.UserPosted](),
		Errors: []wire.ErrorCode{
			wire.ErrInvalid, wire.ErrForbidden, wire.ErrNotFound, wire.ErrConflict, wire.ErrGone, wire.ErrTooLarge,
			wire.ErrKeyReused, wire.ErrStoreFull,
		},
	},
	"listAgents": {
		Summary:  "Every live and dormant agent, by name",
		Response: typeOf[wire.AgentList](),
	},
	"getAgent": {
		Summary: "An agent's card, reservations and delegates",
		Params:  typeOf[wire.AgentParams](), Response: typeOf[wire.AgentPage](), Errors: []wire.ErrorCode{wire.ErrNotFound},
	},
	"getAgentHistory": {
		Summary: "Every post an agent sent or received, newest first, 100 a page",
		Params:  typeOf[wire.AgentParams](), Query: typeOf[wire.PageQuery](), Response: typeOf[wire.HistoryPage](),
		Errors: []wire.ErrorCode{wire.ErrInvalid, wire.ErrNotFound},
	},
	"getStats": {
		Summary: "The Statistics tab over a range",
		Query:   typeOf[wire.StatsQuery](), Response: typeOf[wire.Stats](), Errors: []wire.ErrorCode{wire.ErrInvalid},
	},
	"getSettings": {
		Summary:  "The history retention and sign-in methods",
		Response: typeOf[wire.Settings](),
	},
	"putSettings": {
		Summary: "Replace the settings; a change of sign-in methods needs a recent sign-in",
		Request: typeOf[wire.SettingsRequest](), Response: typeOf[wire.Settings](),
		Errors: []wire.ErrorCode{wire.ErrInvalid, wire.ErrForbidden, wire.ErrRecentAuthRequired, wire.ErrWouldLockOut},
	},
	"getSignIn": {
		Summary:  "What the sign-in page shows beside the sign-in routes' state; needs no session",
		Response: typeOf[wire.SignIn](),
	},
	"events": {
		Summary: "A changed event after each commit the web UI shows, resumed from Last-Event-ID, between the sse hub's hello and keepalive events",
		Errors:  []wire.ErrorCode{wire.ErrSSEUnavailable, wire.ErrStreamingUnsupported},
	},
}

// Registry is the contract wiregen generates from: every wire.Routes row
// joined with its spec, in the rows' order. It panics on a row with no
// spec, which TestSpecCoversEveryRow keeps from shipping.
func Registry() *wiregen.Registry {
	return &wiregen.Registry{
		ErrorEnvelope: wiregen.TypeRef[wire.Error](),
		OpenAPI: wiregen.OpenAPIInfo{
			Title:       "grelot API",
			Version:     "2",
			Description: "The agents' routes under /v1, for a machine key, and the web UI's under /api, for a signed-in session.",
			SecuritySchemes: map[string]wiregen.SecurityScheme{
				bearerKey: {Type: "http", Scheme: "bearer", Description: "A machine key, minted by grelot key create or the web UI."},
				sessionCookie: {
					Type: "apiKey", In: "cookie", Name: access.DefaultCookieConfig().Name,
					Description: "A signed-in browser session.",
				},
			},
		},
		Enums: []wiregen.EnumRef{
			wiregen.StringEnum[wire.Show](),
			wiregen.StringEnum[wire.ThreadKind](),
			wiregen.StringEnum[wire.SenderKind](),
			wiregen.StringEnum[wire.ReservationState](),
			wiregen.StringEnum[wire.AgentState](),
			wiregen.StringEnum[wire.StatsRange](),
			wiregen.StringEnum[wire.EventKind](),
			wiregen.StringEnum[wire.FieldCode](),
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
			wiregen.TypeRef[wire.Where](), wiregen.TypeRef[wire.Checkout](), wiregen.TypeRef[wire.Identity](),
			wiregen.TypeRef[wire.Counts](), wiregen.TypeRef[wire.Hint](), wiregen.TypeRef[wire.Reservation](),
			wiregen.TypeRef[wire.Delegate](), wiregen.TypeRef[wire.Link](), wiregen.TypeRef[wire.Peer](),
			wiregen.TypeRef[wire.PeerReservation](), wiregen.TypeRef[wire.PeerQueued](), wiregen.TypeRef[wire.PeerDelegate](),
			wiregen.TypeRef[wire.IdentityFields](), wiregen.TypeRef[wire.Recipients](), wiregen.TypeRef[wire.Thread](),
			wiregen.TypeRef[wire.Receipt](), wiregen.TypeRef[wire.Entry](), wiregen.TypeRef[wire.FieldError](),
		},
		Endpoints: endpoints(wire.Routes),
		Events:    []wiregen.Event{{Name: wire.EventChanged, Data: wiregen.TypeRef[wire.Changed]()}},
	}
}

// idempotencyKey is a keyed route's header, whose byte bounds count the
// key's two quotes.
var idempotencyKey = wiregen.Header{
	Name: wire.HeaderIdempotencyKey, Arg: "idempotencyKey", Required: true,
	MinBytes: wire.IdempotencyKeyMin + 2, MaxBytes: wire.IdempotencyKeyMax + 2,
	Description: fmt.Sprintf("A new quoted random key of %d to %d letters, digits, - or _ per post, the same key when it is sent again.",
		wire.IdempotencyKeyMin, wire.IdempotencyKeyMax),
}

// identityHeaders are the headers an agent row reads to act as its
// session's identity; sessionRequired is false on an AnySession row.
func identityHeaders(sessionRequired bool) []wiregen.Header {
	return []wiregen.Header{
		{
			Name: wire.HeaderVersion, Arg: "grelotVersion", Required: true, MinBytes: 1, MaxBytes: 64,
			Description: "The client's version, which the server answers 426 below its floor.",
		},
		{
			Name: wire.HeaderSession, Arg: "grelotSession", Required: sessionRequired, MinBytes: 3, MaxBytes: 305,
			Description: "<provider>:<host session id>, the session the request acts as.",
		},
		{
			Name: wire.HeaderAs, Arg: "grelotAs", MinBytes: 1, MaxBytes: 97,
			Description: "A delegate's handle, to act as that delegate of the session's identity.",
		},
		{
			Name: wire.HeaderSelf, Arg: "grelotSelf", MinBytes: 1, MaxBytes: 1,
			Description: "1 when the agent's own command sent the request.",
		},
	}
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
		if r.Access == wire.AccessAgent {
			e.Headers = identityHeaders(!s.AnySession)
		}
		e.Headers = append(e.Headers, s.Headers...)
		if r.Keyed {
			e.Headers = append(e.Headers, idempotencyKey)
		}
		gr := wiregate.Route(r)
		e.Security = security(&gr)
		e.Errors = errorRefs(r, &gr, s.Errors)
		out = append(out, e)
	}
	return out
}

// The security schemes a row's credential is one of.
const (
	bearerKey     = "bearerKey"
	sessionCookie = "sessionCookie"
)

// security is the credential g's class admits, with its scope as the
// scheme's one role; a public row needs none.
func security(g *gate.Route) []wiregen.SecurityRequirement {
	var roles []string
	if g.Scope != "" {
		roles = []string{g.Scope}
	}
	switch g.Class {
	case gate.Key:
		return []wiregen.SecurityRequirement{{bearerKey: roles}}
	case gate.Session:
		return []wiregen.SecurityRequirement{{sessionCookie: nil}}
	case gate.Public:
		return nil
	}
	panic("wirespec: no security scheme for the gate class of " + g.Name)
}

// errorRefs is r's union: its own codes, wire.Common, a key bucket's 429
// on a row the gate authenticates, the gate's codes for g and, on a list
// row, the cursor's refusals.
func errorRefs(r *wire.Route, g *gate.Route, own []wire.ErrorCode) []wiregen.ErrorRef {
	codes := slices.Concat(own, wire.Common)
	if g.Class != gate.Public {
		codes = append(codes, wire.ErrRateLimited)
	}
	for _, c := range wiregate.Codes(r) {
		codes = append(codes, wire.ErrorCode{Code: string(c), Status: access.NewError(c, "", nil).Status})
	}
	if r.Paged {
		codes = append(codes, wire.ErrCursorInvalid, wire.ErrCursorMismatch)
	}
	out := make([]wiregen.ErrorRef, 0, len(codes))
	for _, c := range codes {
		out = append(out, wiregen.ErrorRef{Code: c.Code, Status: c.Status})
	}
	return out
}
