package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

func reservationOut(v *store.Reservation) reservationJSON {
	return reservationJSON{
		Expires: v.Expires.UTC(), Since: v.Since.UTC(), Path: v.Path, Reason: v.Reason, State: v.State,
		Holder: v.Holder, Host: v.Host, Relation: v.Relation, TTL: v.TTL.String(), ID: v.ID,
	}
}

type reserveRequest struct {
	Path   string   `json:"path"`
	Reason string   `json:"reason"`
	TTL    duration `json:"ttl"`
	Queue  bool     `json:"queue"`
}

type reserveResponse struct {
	Behind      *reservationJSON `json:"behind,omitempty"`
	Reservation reservationJSON  `json:"reservation"`
}

// reserve is POST /v1/reservations: a grant, or with queue a queue entry
// when the path is taken.
func (s *Server) reserve(r *http.Request, c store.Caller) (status int, body any, err error) {
	var req reserveRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	got, err := s.store.Reserve(r.Context(), &store.Reserving{
		Path: req.Path, Reason: req.Reason, Caller: c, TTL: time.Duration(req.TTL), Queue: req.Queue,
	})
	if err != nil {
		return 0, nil, err
	}
	out := reserveResponse{Reservation: reservationOut(&got.Reservation)}
	if got.Behind != nil {
		b := reservationOut(got.Behind)
		out.Behind = &b
	}
	return http.StatusCreated, out, nil
}

type renewRequest struct {
	TTL duration `json:"ttl"`
}

type reservationResponse struct {
	Reservation reservationJSON `json:"reservation"`
}

// renewReservation is PATCH /v1/reservations/{id}.
func (s *Server) renewReservation(r *http.Request, c store.Caller) (status int, body any, err error) {
	id, err := reservationID(r)
	if err != nil {
		return 0, nil, err
	}
	var req renewRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	got, err := s.store.RenewReservation(r.Context(), c, id, time.Duration(req.TTL))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, reservationResponse{Reservation: reservationOut(&got)}, nil
}

// release is DELETE /v1/reservations/{id}: a grant freed or a queue entry
// cancelled.
func (s *Server) release(r *http.Request, c store.Caller) (status int, body any, err error) {
	id, err := reservationID(r)
	if err != nil {
		return 0, nil, err
	}
	got, err := s.store.Release(r.Context(), c, id)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, reservationResponse{Reservation: reservationOut(&got)}, nil
}

// reservations is GET /v1/reservations?path=: the grants and queue
// entries overlapping path, every one when it is absent.
func (s *Server) reservations(r *http.Request, c store.Caller) (status int, body any, err error) {
	got, err := s.store.Reservations(r.Context(), c, store.ReservationQuery{Path: r.URL.Query().Get("path")})
	if err != nil {
		return 0, nil, err
	}
	out := make([]reservationJSON, 0, len(got))
	for i := range got {
		out = append(out, reservationOut(&got[i]))
	}
	return http.StatusOK, map[string]any{"reservations": out}, nil
}

func reservationID(r *http.Request) (uint64, error) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		return 0, badRequest("the reservation id must be a whole number")
	}
	return id, nil
}

type delegateRequest struct {
	Label        string   `json:"label"`
	AllowThreads []string `json:"allow_threads"`
	AllowAgents  []string `json:"allow_agents"`
	DenyThreads  []string `json:"deny_threads"`
	DenyAgents   []string `json:"deny_agents"`
	TTL          duration `json:"ttl"`
}

type delegateResponse struct {
	Expires time.Time `json:"expires_at"`
	Link    string    `json:"link"`
	Handle  string    `json:"handle"`
}

// grant is POST /v1/delegates: a single-use link a subagent registers
// with.
func (s *Server) grant(r *http.Request, c store.Caller) (status int, body any, err error) {
	var req delegateRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	got, err := s.store.Delegate(r.Context(), &store.Grant{
		Caller: c, Label: req.Label, AllowThreads: req.AllowThreads, AllowAgents: req.AllowAgents,
		DenyThreads: req.DenyThreads, DenyAgents: req.DenyAgents, TTL: time.Duration(req.TTL),
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, delegateResponse{Expires: got.Expires.UTC(), Link: got.Link, Handle: got.Handle}, nil
}

type revokeResponse struct {
	Ended []string `json:"ended"`
	Link  bool     `json:"link"`
}

// revoke is DELETE /v1/delegates/{label}: the label's unused link revoked
// and its delegate ended.
func (s *Server) revoke(r *http.Request, c store.Caller) (status int, body any, err error) {
	got, err := s.store.Revoke(r.Context(), c, r.PathValue("label"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, revokeResponse{Ended: nonNil(got.Handles), Link: got.Link}, nil
}

type bindRequest struct {
	AgentID string `json:"agent_id"`
}

// bind is POST /v1/me/bind: the host's subagent id recorded on the
// delegate Grelot-As names.
func (s *Server) bind(r *http.Request, c store.Caller) (status int, body any, err error) {
	var req bindRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	if err := s.store.Bind(r.Context(), c, req.AgentID); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]bool{"bound": true}, nil
}

type lifecycleRequest struct {
	Event   string `json:"event"`
	AgentID string `json:"agent_id"`
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
func (s *Server) lifecycle(r *http.Request, c store.Caller) (status int, body any, err error) {
	var req lifecycleRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	ctx := r.Context()
	switch req.Event {
	case eventSubagentStop:
		err = s.store.SubagentStop(ctx, c.Session, req.AgentID)
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
		} else if err = s.store.EndSession(ctx, c.Session); err == nil {
			s.rewait(me.ID)
		}
	default:
		return 0, nil, badRequest("event must be session_end, subagent_stop or turn_end")
	}
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]string{"event": req.Event}, nil
}
