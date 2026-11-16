//go:build !windows

package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

func identityOut(id *store.Identity) wire.Identity {
	return wire.Identity{
		Expires: id.Expires.UTC(), RegisteredAt: id.RegisteredAt.UTC(),
		LastSeen: id.LastSeen.UTC(), DormantSince: id.DormantSince.UTC(),
		Name: id.Name, Description: id.Description, State: id.State,
		Host: id.Host, OS: id.OS, Arch: id.Arch, Provider: id.Provider,
		MachineKind: id.MachineKind, TTLMS: id.TTL.Milliseconds(), Notice: id.Notice,
		Paths: nonNil(id.Paths), Follows: nonNil(id.Follows),
		EnvironmentKnown: id.EnvironmentKnown, Ended: id.Ended, Threaded: id.Threaded,
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// noEnvironmentLine answers a register or revive that sent an empty
// environment; the client adds the path of the file it could not use.
const noEnvironmentLine = "this session has no environment id: another session cannot hand this identity over or revive it, and every agent sees your paths as unreadable"

// register is POST /v1/register: a new identity (201), a take-over (201),
// the session's live identity unchanged (200), or with delegate a
// delegate's registration from its link (201).
func (s *Server) register(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	var req wire.RegisterRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	ttl := durationMS(req.TTLMS)
	got, replayed, err := s.store.Register(r.Context(), k, &store.Registration{
		Where: whereOf(req.Where), Session: c.Session, Key: c.Key, Link: req.Delegate,
		Host: req.Host, OS: req.OS, Arch: req.Arch, Proc: req.Proc,
		Fields: store.Fields{Name: req.Name, Description: req.Description, Paths: req.Paths},
		TTL:    ttl, TakeOver: req.TakeOver, Self: c.Self,
	})
	if err != nil {
		return 0, nil, err
	}
	status = http.StatusCreated
	if got.Existing {
		status = http.StatusOK
	}
	if !replayed {
		if !got.Existing {
			s.presence.registered(got.Identity.ID)
		}
		if req.Delegate == "" {
			s.push.registered(got.Identity.ID, got.Identity.Provider, got.Identity.Host)
		}
		if got.Moved {
			s.rewait(got.Identity.ID)
		}
	}
	out := wire.Registered{Identity: identityOut(&got.Identity), Existing: got.Existing, Moved: got.Moved}
	if req.Delegate == "" && !got.Identity.EnvironmentKnown {
		out.NoEnvironment = noEnvironmentLine
	}
	return status, out, nil
}

// revive is POST /v1/revive: this session's dormant identity, or with name
// a dormant one of this environment and provider moved to this session.
func (s *Server) revive(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	var req wire.ReviveRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	ttl := durationMS(req.TTLMS)
	got, replayed, err := s.store.Revive(r.Context(), k, &store.Revival{
		Where: whereOf(req.Where), Session: c.Session, Key: c.Key, Name: req.Name,
		Host: req.Host, OS: req.OS, Arch: req.Arch, Proc: req.Proc, TTL: ttl,
	})
	if err != nil {
		return 0, nil, err
	}
	if !replayed {
		s.presence.registered(got.Identity.ID)
		s.push.registered(got.Identity.ID, got.Identity.Provider, got.Identity.Host)
		if got.Moved {
			s.rewait(got.Identity.ID)
		}
	}
	out := wire.Registered{Identity: identityOut(&got.Identity), Unread: got.Unread, Moved: got.Moved}
	if !got.Identity.EnvironmentKnown {
		out.NoEnvironment = noEnvironmentLine
	}
	return http.StatusOK, out, nil
}

const (
	stateNone    = "none"
	stateDormant = "dormant"
)

// me is GET /v1/me, which every session may call, a dormant one included;
// with turn=start it is the prompt hook's, which opens the turn.
func (s *Server) me(r *http.Request, c store.Caller, _ store.Keyed) (status int, body any, err error) {
	q, err := queryParams[wire.MeQuery](r)
	if err != nil {
		return 0, nil, err
	}
	turn, err := turnStartOf(&q)
	if err != nil {
		return 0, nil, err
	}
	out := wire.Me{Version: s.version, PathRoot: s.cfg.PathRoot, State: stateNone}
	if oerr := s.openTurn(r, c, turn); oerr != nil {
		return 0, nil, oerr
	}
	id, err := s.store.Seen(r.Context(), c)
	var se *store.Error
	switch {
	case errors.As(err, &se) && se.Kind == store.KindGone && se.Why == stateNone:
		out.Hint, err = s.hint(r, c, q.Proc)
		return http.StatusOK, out, err
	case err != nil:
		return 0, nil, err
	}
	card := identityOut(&id)
	out.Identity, out.State = &card, id.State
	if id.State != stateLive {
		return http.StatusOK, out, nil
	}
	if err := s.meLive(r, c, &out, id.ID); err != nil {
		return 0, nil, err
	}
	out.SignalRead = turn.stale(&id)
	return http.StatusOK, out, nil
}

// openTurn opens the turn of c's live identity for a prompt before Seen
// writes: Seen clearing ended wakes the identity at its commit, and a
// first nudge that wake finds with the turn still shut goes out into it.
// A prompt the delegate gate refuses opens nothing and is refused here.
func (s *Server) openTurn(r *http.Request, c store.Caller, turn turnStart) error {
	if !turn.start {
		return nil
	}
	id, err := s.store.Acting(r.Context(), c)
	var se *store.Error
	switch {
	case errors.As(err, &se) && se.Kind == store.KindGone:
		return nil
	case err != nil:
		return err
	}
	turn.open(s, &id)
	return nil
}

// meLive fills out with what a live identity's status adds.
func (s *Server) meLive(r *http.Request, c store.Caller, out *wire.Me, id uint64) error {
	ctx := r.Context()
	st, err := s.store.Status(ctx, c)
	if err != nil {
		return err
	}
	resv, err := s.store.Reservations(ctx, c, store.ReservationQuery{Mine: true})
	if err != nil {
		return err
	}
	dl, err := s.store.Delegations(ctx, c)
	if err != nil {
		return err
	}
	now := s.now()
	counts := countsOut(st.Unread)
	out.Unread, out.Wake = &counts, s.wakeState(&st, now)
	out.Push = string(s.push.State(id))
	out.Online = s.onlineAt(now)(&st.Identity)
	for i := range resv.Items {
		out.Reservations = append(out.Reservations, reservationOut(&resv.Items[i]))
	}
	for _, d := range dl.Delegates {
		out.Delegates = append(out.Delegates, wire.Delegate{
			Expires: d.Expires.UTC(), LastSeen: d.LastSeen.UTC(), Handle: d.Handle,
			Description: d.Description, Bound: d.Bound,
		})
	}
	for _, l := range dl.Links {
		out.Links = append(out.Links, wire.Link{Expires: l.Expires.UTC(), Handle: l.Handle})
	}
	return nil
}

// hint is the take-over hint for a session with no identity that names
// its CLI process in proc.
func (s *Server) hint(r *http.Request, c store.Caller, proc string) (*wire.Hint, error) {
	if proc == "" {
		return nil, nil
	}
	caller := store.Where{Machine: r.Header.Get(wire.HeaderMachine), Environment: r.Header.Get(wire.HeaderEnvironment)}
	id, found, err := s.store.TakeOverHint(r.Context(), c.Session, c.Key, proc, caller)
	if err != nil || !found {
		return nil, err
	}
	h := &wire.Hint{Name: id.Name, State: id.State}
	h.Line = fmt.Sprintf("you were @%s before this session started; to go on as it, run `grelot register %s --take-over --ttl=<d>`",
		id.Name, id.Name)
	if id.State != stateLive {
		h.Line = fmt.Sprintf("you were @%s before this session started, and it is dormant since %s; to go on as it, run `grelot revive %s --ttl=<d>`",
			id.Name, id.DormantSince.UTC().Format("2006-01-02 15:04 UTC"), id.Name)
	}
	return h, nil
}

// patchMe is PATCH /v1/me: new fields, with where when paths change, or
// with ttl and where a renewal of the lease.
func (s *Server) patchMe(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	var req wire.PatchMeRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	ttl := durationMS(req.TTLMS)
	got, err := s.store.Set(r.Context(), k, c, store.Change{
		Where: whereOf(req.Where), TTL: ttl,
		Fields: store.Fields{Name: req.Name, Description: req.Description, Paths: req.Paths},
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, wire.IdentityAnswer{Notices: []string{}, Identity: identityOut(&got)}, nil
}

// deleteMe is DELETE /v1/me, grelot bye.
func (s *Server) deleteMe(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	id, replayed, err := s.store.Retire(r.Context(), k, c)
	if err != nil {
		return 0, nil, err
	}
	if !replayed {
		s.rewait(id)
	}
	return http.StatusOK, wire.Retired{Retired: true}, nil
}

const stateLive = "live"

func countsOut(c store.Counts) wire.Counts {
	return wire.Counts{
		Direct: c.Direct, Path: c.Path, Announce: c.Announce, Identity: c.Identity,
		Reservation: c.Reservation, Receipts: c.Receipts, Cards: c.Cards, Copies: c.Copies,
		QuietPath: c.QuietPath, Wake: c.Wake(), Total: c.Total(),
	}
}

// whereOf is the store's where report of w; nil when the request sent none.
func whereOf(w *wire.Where) *store.Where {
	if w == nil {
		return nil
	}
	out := &store.Where{Machine: w.Machine, Environment: w.Environment}
	for _, c := range w.Checkouts {
		out.Checkouts = append(out.Checkouts, store.Checkout(c))
	}
	if w.Checkouts != nil && out.Checkouts == nil {
		out.Checkouts = []store.Checkout{}
	}
	return out
}
