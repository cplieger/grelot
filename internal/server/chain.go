//go:build !windows

package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/cplieger/access/gate"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

// identityFunc serves one identity row as c and returns the status and
// body to answer with; k is the request's Idempotency-Key on a keyed row,
// zero otherwise.
type identityFunc func(r *http.Request, c store.Caller, k store.Keyed) (int, any, error)

// ownerFunc serves one JSON row as @user; k is as identityFunc's.
type ownerFunc func(r *http.Request, k store.Keyed) (any, error)

// binding is what one wire.Routes row runs: the function of the handler
// kind its class and kind need, and nothing else (checkBindings).
type binding struct {
	identity identityFunc
	owner    ownerFunc
	stream   http.Handler
	// anySession serves an identity row with no Grelot-Session as a
	// session with no identity, so an agent can look before it registers.
	anySession bool
	// held gives a row that holds for up to maxWait waitRouteDeadline.
	held bool
}

// bindings is every row's binding, by its wire.Routes Name.
func (s *Server) bindings() map[string]binding {
	id := func(fn identityFunc) binding { return binding{identity: fn} }
	owner := func(fn ownerFunc) binding { return binding{owner: fn} }
	return map[string]binding{
		"register":             id(s.register),
		"revive":               id(s.revive),
		"getMe":                id(s.me),
		"patchMe":              id(s.patchMe),
		"deleteMe":             id(s.deleteMe),
		"listIdentities":       {identity: s.who, anySession: true},
		"getVersion":           {identity: s.serverVersion, anySession: true},
		"createPost":           id(s.post),
		"listThreads":          id(s.threads),
		"getReceipts":          id(s.receipts),
		"getHistory":           id(s.history),
		"follow":               id(s.follow),
		"unfollow":             id(s.unfollow),
		"ackInbox":             id(s.ack),
		"reserve":              id(s.reserve),
		"listReservations":     id(s.reservations),
		"renewReservation":     id(s.renewReservation),
		"releaseReservation":   id(s.release),
		"grantDelegate":        id(s.grant),
		"revokeDelegate":       id(s.revoke),
		"lifecycle":            id(s.lifecycle),
		"holdEvent":            id(s.eventHold),
		"waitEvent":            {identity: s.standingWait, held: true},
		"readInbox":            {identity: s.inbox, held: true},
		"listConversations":    owner(s.listConversations),
		"getConversation":      owner(s.getConversation),
		"markConversationRead": owner(s.markConversationRead),
		"getUserReceipts":      owner(s.getUserReceipts),
		"createUserPost":       owner(s.createUserPost),
		"listAgents":           owner(s.listAgents),
		"getAgent":             owner(s.getAgent),
		"getAgentHistory":      owner(s.getAgentHistory),
		"getStats":             owner(s.getStats),
		"getSettings":          owner(s.getSettings),
		"putSettings":          owner(s.putSettings),
		"getSignIn":            owner(s.getSignIn),
		"events":               {stream: untilSessionEnds(http.HandlerFunc(s.events.serve))},
	}
}

// checkBindings is the startup check that routes and b agree: every row
// has a binding, every binding a row, and each row's binding holds the one
// function its class and kind run.
func checkBindings(routes []wire.Route, b map[string]binding) error {
	var errs []error
	rows := map[string]bool{}
	for i := range routes {
		r := &routes[i]
		rows[r.Name] = true
		got, ok := b[r.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("row %s has no binding", r.Name))
			continue
		}
		if !fits(r, &got) {
			errs = append(errs, fmt.Errorf("row %s (access %d, kind %d) is bound to the wrong handler kind", r.Name, r.Access, r.Kind))
		}
	}
	for name := range b {
		if !rows[name] {
			errs = append(errs, fmt.Errorf("binding %s names no row", name))
		}
	}
	return errors.Join(errs...)
}

// fits reports whether b holds the one function r's class and kind run.
func fits(r *wire.Route, b *binding) bool {
	identity, owner, stream := b.identity != nil, b.owner != nil, b.stream != nil
	switch {
	case r.Kind == wire.KindEventStream:
		return stream && !identity && !owner && r.Access == wire.AccessOwner
	case r.Access == wire.AccessAgent:
		return identity && !owner && !stream
	case r.Access == wire.AccessOwner, r.Access == wire.AccessPublic:
		return owner && !identity && !stream
	}
	return false
}

// gateClass is the gate class of r's Access.
func gateClass(r *wire.Route) gate.Class {
	switch r.Access {
	case wire.AccessAgent:
		return gate.Key
	case wire.AccessPublic:
		return gate.Public
	default:
		return gate.Session
	}
}

// chains adds every wire.Routes row to t behind its chain, outermost first:
// the server's version, the key's bucket, the client's version on an
// identity row, the route deadline and the handler kind. It panics when
// the rows and the bindings disagree.
func (s *Server) chains(t *table) {
	b := s.bindings()
	if err := checkBindings(wire.Routes, b); err != nil {
		panic("server: " + err.Error())
	}
	for i := range wire.Routes {
		r := &wire.Routes[i]
		got := b[r.Name]
		deadline := routeDeadline
		if got.held {
			deadline = waitRouteDeadline
		}
		var h http.Handler
		switch {
		case got.stream != nil:
			h = got.stream
		case got.identity != nil:
			h = s.clientVersion(httpserver.RouteDeadline(s.identityFor(got.identity, r, got.anySession), deadline))
		default:
			h = httpserver.RouteDeadline(s.ownerFor(got.owner, r), deadline)
		}
		t.add(r.Pattern(), gateClass(r), s.versionAnswer(s.keyBucket(h)))
	}
}

// versionAnswer names the server's version on every answer the gate
// admitted, so a client can tell a refusal it cannot read from a newer
// server.
func (s *Server) versionAnswer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(wire.HeaderVersion, s.version)
		next.ServeHTTP(w, r)
	})
}

// keyBucket names a key's label on the access line and spends one of the
// key's tokens, answering 429 past its rate. A session or no credential
// takes no token.
func (s *Server) keyBucket(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := gate.Principal(r.Context())
		if !ok || !p.IsAPIKey() {
			next.ServeHTTP(w, r)
			return
		}
		httpserver.AddLogAttrs(r.Context(), slog.String("key", p.KeyLabel))
		if retry, ok := s.rates.take(p.KeyID, s.now()); !ok {
			s.fail(w, r, s.rateLimited(retry))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), machineKey{}, p.KeyID)))
	})
}

// clientVersion answers 426, naming the client to install, a request whose
// Grelot-Version this server does not speak, before its body is read.
func (s *Server) clientVersion(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get(wire.HeaderVersion); !speaks(v) {
			s.fail(w, r, refusal(wire.ErrUpgradeRequired, upgradeLine(v, s.version)))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type machineKey struct{}

// keyID is the ID of the machine key the gate admitted r with, which
// keyBucket puts on its context.
func keyID(r *http.Request) int64 {
	id, _ := r.Context().Value(machineKey{}).(int64)
	return id
}

// keyRule is route's keyed rule: its Idempotency-Key bound to the request
// on a keyed row, or a refusal of a key sent to a row that takes none, so
// a client that would re-send a request the row never dedups fails at once.
func keyRule(r *http.Request, route *wire.Route, c store.Caller) (store.Keyed, error) {
	if route.Keyed {
		return keyOf(r, c)
	}
	if _, sent := r.Header[wire.HeaderIdempotencyKey]; sent {
		return store.Keyed{}, badRequest("this route takes no Idempotency-Key")
	}
	return store.Keyed{}, nil
}

// identityFor serves fn as the caller r's headers name, and puts the
// caller's status line on the answer, read after fn so it shows what fn
// changed.
func (s *Server) identityFor(fn identityFunc, route *wire.Route, anySession bool) http.Handler {
	mode := callerSession
	if anySession {
		mode = callerAnyone
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := callerOf(r, mode)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		httpserver.LimitBody(w, r, maxBody)
		status, body := 0, any(nil)
		k, err := keyRule(r, route, c)
		if err == nil {
			status, body, err = fn(r, c, k)
		}
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

// ownerUser is who every owner row acts as.
var ownerUser = store.Caller{User: true}

// ownerFor serves fn as @user with route's success status. It reads no
// Grelot-* header.
func (s *Server) ownerFor(fn ownerFunc, route *wire.Route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpserver.LimitBody(w, r, maxBody)
		k, err := keyRule(r, route, ownerUser)
		var body any
		if err == nil {
			body, err = fn(r, k)
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		httpserver.WriteJSONStatus(w, cmp.Or(route.Status, http.StatusOK), body)
	})
}
