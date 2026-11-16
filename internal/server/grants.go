//go:build !windows

package server

import (
	"net/http"
	"strconv"

	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

func reservationOut(v *store.Reservation) wire.Reservation {
	return wire.Reservation{
		Expires: v.Expires.UTC(), Since: v.Since.UTC(), Path: v.Path, Reason: v.Reason, State: v.State,
		Holder: v.Holder, Host: v.Host, Relation: v.Relation, TTL: v.TTL.String(), ID: v.ID,
	}
}

// reserve is POST /v1/reservations: a grant, or with queue a queue entry
// when the path is taken.
func (s *Server) reserve(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	var req wire.ReserveRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	ttl, err := durationOf(req.TTL)
	if err != nil {
		return 0, nil, err
	}
	got, _, err := s.store.Reserve(r.Context(), k, &store.Reserving{
		Path: req.Path, Reason: req.Reason, Caller: c, TTL: ttl, Queue: req.Queue,
	})
	if err != nil {
		return 0, nil, err
	}
	out := wire.Reserved{Reservation: reservationOut(&got.Reservation)}
	if got.Behind != nil {
		b := reservationOut(got.Behind)
		out.Behind = &b
	}
	return http.StatusCreated, out, nil
}

// renewReservation is PATCH /v1/reservations/{id}.
func (s *Server) renewReservation(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	id, err := reservationID(r)
	if err != nil {
		return 0, nil, err
	}
	var req wire.RenewRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	ttl, err := durationOf(req.TTL)
	if err != nil {
		return 0, nil, err
	}
	got, err := s.store.RenewReservation(r.Context(), k, c, id, ttl)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, wire.ReservationAnswer{Reservation: reservationOut(&got)}, nil
}

// release is DELETE /v1/reservations/{id}: a grant freed or a queue entry
// cancelled.
func (s *Server) release(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	id, err := reservationID(r)
	if err != nil {
		return 0, nil, err
	}
	got, err := s.store.Release(r.Context(), k, c, id)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, wire.ReservationAnswer{Reservation: reservationOut(&got)}, nil
}

// reservations is GET /v1/reservations?path=: the grants and queue
// entries overlapping path, every one when it is absent.
func (s *Server) reservations(r *http.Request, c store.Caller, _ store.Keyed) (status int, body any, err error) {
	got, err := s.store.Reservations(r.Context(), c, store.ReservationQuery{Path: r.URL.Query().Get("path")})
	if err != nil {
		return 0, nil, err
	}
	out := make([]wire.Reservation, 0, len(got))
	for i := range got {
		out = append(out, reservationOut(&got[i]))
	}
	return http.StatusOK, wire.Reservations{Reservations: out}, nil
}

func reservationID(r *http.Request) (uint64, error) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		return 0, badRequest("the reservation id must be a whole number")
	}
	return id, nil
}

// grant is POST /v1/delegates: a single-use link a subagent registers
// with.
func (s *Server) grant(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	var req wire.GrantRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	ttl, err := durationOf(req.TTL)
	if err != nil {
		return 0, nil, err
	}
	got, _, err := s.store.Delegate(r.Context(), k, &store.Grant{
		Caller: c, Label: req.Label, AllowThreads: req.AllowThreads, AllowAgents: req.AllowAgents,
		DenyThreads: req.DenyThreads, DenyAgents: req.DenyAgents, TTL: ttl,
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, wire.Granted{Expires: got.Expires.UTC(), Link: got.Link, Handle: got.Handle}, nil
}

// revoke is DELETE /v1/delegates/{label}: the label's unused link revoked
// and its delegate ended.
func (s *Server) revoke(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	got, err := s.store.Revoke(r.Context(), k, c, r.PathValue("label"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, wire.Revoked{Ended: nonNil(got.Handles), Link: got.Link}, nil
}

// The lifecycle events a hook reports.
const (
	eventSessionEnd   = "session_end"
	eventSubagentStop = "subagent_stop"
	eventTurnEnd      = "turn_end"
)

// lifecycle is POST /v1/lifecycle. A session_end or turn_end with an
// agent_id came from inside a subagent and changes nothing; subagent_stop
// skips the delegate gate, since it only ever ends a delegate.
func (s *Server) lifecycle(r *http.Request, c store.Caller, _ store.Keyed) (status int, body any, err error) {
	var req wire.LifecycleRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	ctx := r.Context()
	switch req.Event {
	case eventSubagentStop:
		err = s.store.SubagentStop(ctx, c.Session, c.Key)
	case eventSessionEnd, eventTurnEnd:
		if req.AgentID != "" {
			break
		}
		var me store.Identity
		if me, err = s.store.Seen(ctx, c); err != nil {
			break
		}
		if req.Event == eventTurnEnd {
			s.endTurn(me.ID)
		} else if err = s.store.EndSession(ctx, c.Session, c.Key); err == nil {
			s.rewait(me.ID)
		}
	default:
		return 0, nil, badRequest("event must be session_end, subagent_stop or turn_end")
	}
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, wire.Lifecycle{Event: req.Event}, nil
}
