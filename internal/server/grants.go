//go:build !windows

package server

import (
	"net/http"

	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

func reservationOut(v *store.Reservation) wire.Reservation {
	return wire.Reservation{
		Expires: v.Expires.UTC(), Since: v.Since.UTC(), Path: v.Path, Reason: v.Reason, State: v.State,
		Holder: v.Holder, Host: v.Host, Relation: v.Relation, TTLMS: v.TTL.Milliseconds(), ID: v.ID,
	}
}

// reserve is POST /v1/reservations: a grant, or with queue a queue entry
// when the path is taken.
func (s *Server) reserve(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	var req wire.ReserveRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	ttl := durationMS(req.TTLMS)
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
	p, err := pathParams[wire.ReservationParams](r)
	if err != nil {
		return 0, nil, err
	}
	var req wire.RenewRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	ttl := durationMS(req.TTLMS)
	got, err := s.store.RenewReservation(r.Context(), k, c, uint64(p.ID), ttl)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, wire.ReservationAnswer{Reservation: reservationOut(&got)}, nil
}

// release is DELETE /v1/reservations/{id}: a grant freed or a queue entry
// cancelled.
func (s *Server) release(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	p, err := pathParams[wire.ReservationParams](r)
	if err != nil {
		return 0, nil, err
	}
	got, err := s.store.Release(r.Context(), k, c, uint64(p.ID))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, wire.ReservationAnswer{Reservation: reservationOut(&got)}, nil
}

// reservations is GET /v1/reservations?path=: the grants and queue
// entries overlapping path, every one when it is absent, by id.
func (s *Server) reservations(r *http.Request, c store.Caller, _ store.Keyed) (status int, body any, err error) {
	rq, err := queryParams[wire.ReservationsQuery](r)
	if err != nil {
		return 0, nil, err
	}
	p := pagingOf(r)
	q := store.ReservationQuery{ReadAs: p.readAs, Path: rq.Path, Limit: p.limit}
	if p.after != nil {
		if q.After, err = afterSeq(p.after); err != nil {
			return 0, nil, err
		}
	}
	got, err := s.store.Reservations(r.Context(), c, q)
	if err != nil {
		return 0, nil, err
	}
	out := wire.Reservations{List: wire.List[wire.Reservation]{Items: make([]wire.Reservation, 0, len(got.Items)), Total: got.Total}}
	for i := range got.Items {
		out.Items = append(out.Items, reservationOut(&got.Items[i]))
	}
	if n := len(got.Items); n > 0 {
		out.NextCursor, err = s.next(r, got.More, got.ReadAs, seqPosition(got.Items[n-1].ID))
	}
	return http.StatusOK, out, err
}

// holders is GET /v1/holders?path=&path=: each grant another agent holds
// over a queried path, once, for the file reservation guard.
func (s *Server) holders(r *http.Request, c store.Caller, _ store.Keyed) (status int, body any, err error) {
	got, err := s.store.Holders(r.Context(), c, r.URL.Query()["path"])
	if err != nil {
		return 0, nil, err
	}
	out := wire.Holders{Holders: make([]wire.Holding, 0, len(got))}
	for i := range got {
		h := wire.Holding{Held: reservationOut(&got[i].Held)}
		if got[i].Queued != nil {
			q := reservationOut(got[i].Queued)
			h.Queued = &q
		}
		out.Holders = append(out.Holders, h)
	}
	return http.StatusOK, out, nil
}

// grant is POST /v1/delegates: a single-use link a subagent registers
// with.
func (s *Server) grant(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	var req wire.GrantRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	ttl := durationMS(req.TTLMS)
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
	p, err := pathParams[wire.DelegateParams](r)
	if err != nil {
		return 0, nil, err
	}
	got, err := s.store.Revoke(r.Context(), k, c, p.Label)
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
