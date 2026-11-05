package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

// identityJSON is an identity as its own session sees it. It never carries
// a machine, environment or checkout value, only whether each is known.
type identityJSON struct {
	Expires          time.Time `json:"expires_at"`
	RegisteredAt     time.Time `json:"registered_at"`
	LastSeen         time.Time `json:"last_seen,omitzero"`
	DormantSince     time.Time `json:"dormant_since,omitzero"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	State            string    `json:"state"`
	Host             string    `json:"host"`
	OS               string    `json:"os"`
	Arch             string    `json:"arch"`
	Provider         string    `json:"provider"`
	MachineKind      string    `json:"machine_kind,omitempty"`
	TTL              string    `json:"ttl"`
	Notice           string    `json:"notice,omitempty"`
	Paths            []string  `json:"paths"`
	Follows          []string  `json:"follows"`
	EnvironmentKnown bool      `json:"environment_known"`
	Ended            bool      `json:"ended,omitempty"`
}

func identityOut(id *store.Identity) identityJSON {
	return identityJSON{
		Expires: id.Expires.UTC(), RegisteredAt: id.RegisteredAt.UTC(),
		LastSeen: id.LastSeen.UTC(), DormantSince: id.DormantSince.UTC(),
		Name: id.Name, Description: id.Description, State: id.State,
		Host: id.Host, OS: id.OS, Arch: id.Arch, Provider: id.Provider,
		MachineKind: id.MachineKind, TTL: id.TTL.String(), Notice: id.Notice,
		Paths: nonNil(id.Paths), Follows: nonNil(id.Follows),
		EnvironmentKnown: id.EnvironmentKnown, Ended: id.Ended,
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

type registerRequest struct {
	Where       *store.Where `json:"where"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Host        string       `json:"host"`
	OS          string       `json:"os"`
	Arch        string       `json:"arch"`
	Proc        string       `json:"proc"`
	Delegate    string       `json:"delegate"`
	Paths       []string     `json:"paths"`
	TTL         duration     `json:"ttl"`
	TakeOver    bool         `json:"take_over"`
}

type registerResponse struct {
	NoEnvironment string       `json:"no_environment,omitempty"`
	Identity      identityJSON `json:"identity"`
	Existing      bool         `json:"existing,omitempty"`
	Moved         bool         `json:"moved,omitempty"`
}

// noEnvironmentLine answers a register or revive that sent an empty
// environment; the client adds the path of the file it could not use.
const noEnvironmentLine = "this session has no environment id: another session cannot hand this identity over or revive it, and every agent sees your paths as unreadable"

// register is POST /v1/register: a new identity (201), a take-over (201),
// the session's live identity unchanged (200), or with delegate a
// delegate's registration from its link (201).
func (s *Server) register(r *http.Request, c store.Caller) (status int, body any, err error) {
	var req registerRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	got, err := s.store.Register(r.Context(), &store.Registration{
		Where: req.Where, Session: c.Session, Link: req.Delegate,
		Host: req.Host, OS: req.OS, Arch: req.Arch, Proc: req.Proc,
		Fields: store.Fields{Name: req.Name, Description: req.Description, Paths: req.Paths},
		TTL:    time.Duration(req.TTL), TakeOver: req.TakeOver, Self: c.Self,
	})
	if err != nil {
		return 0, nil, err
	}
	status = http.StatusCreated
	if got.Existing {
		status = http.StatusOK
	}
	out := registerResponse{Identity: identityOut(&got.Identity), Existing: got.Existing, Moved: got.Moved}
	if req.Delegate == "" && !got.Identity.EnvironmentKnown {
		out.NoEnvironment = noEnvironmentLine
	}
	return status, out, nil
}

type reviveRequest struct {
	Where *store.Where `json:"where"`
	Name  string       `json:"name"`
	Host  string       `json:"host"`
	OS    string       `json:"os"`
	Arch  string       `json:"arch"`
	Proc  string       `json:"proc"`
	TTL   duration     `json:"ttl"`
}

type reviveResponse struct {
	NoEnvironment string       `json:"no_environment,omitempty"`
	Identity      identityJSON `json:"identity"`
	Unread        int          `json:"unread"`
	Moved         bool         `json:"moved,omitempty"`
}

// revive is POST /v1/revive: this session's dormant identity, or with name
// a dormant one of this environment and provider moved to this session.
func (s *Server) revive(r *http.Request, c store.Caller) (status int, body any, err error) {
	var req reviveRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	got, err := s.store.Revive(r.Context(), &store.Revival{
		Where: req.Where, Session: c.Session, Name: req.Name,
		Host: req.Host, OS: req.OS, Arch: req.Arch, Proc: req.Proc, TTL: time.Duration(req.TTL),
	})
	if err != nil {
		return 0, nil, err
	}
	out := reviveResponse{Identity: identityOut(&got.Identity), Unread: got.Unread, Moved: got.Moved}
	if !got.Identity.EnvironmentKnown {
		out.NoEnvironment = noEnvironmentLine
	}
	return http.StatusOK, out, nil
}

// meResponse is GET /v1/me. State is live, dormant or none, which the
// hooks act on; a session with no identity gets only State, the hint when
// one applies, and the register command in its status line.
type meResponse struct {
	Identity     *identityJSON     `json:"identity,omitempty"`
	Unread       *countsJSON       `json:"unread,omitempty"`
	Hint         *hintJSON         `json:"hint,omitempty"`
	State        string            `json:"state"`
	Wake         string            `json:"wake,omitempty"`
	Push         string            `json:"push,omitempty"`
	Version      string            `json:"version"`
	PathRoot     string            `json:"path_root"`
	Reservations []reservationJSON `json:"reservations,omitempty"`
	Delegates    []delegateJSON    `json:"delegates,omitempty"`
	Links        []linkJSON        `json:"links,omitempty"`
	Online       bool              `json:"online"`
}

// hintJSON is the take-over hint: the identity a new session may go on
// as, and the command that does it, which only the agent runs.
type hintJSON struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Line  string `json:"line"`
}

// reservationJSON is a grant or queue entry; Relation is its holder's
// relation to the caller at Path.
type reservationJSON struct {
	Expires  time.Time `json:"expires_at,omitzero"`
	Since    time.Time `json:"since"`
	Path     string    `json:"path"`
	Reason   string    `json:"reason"`
	State    string    `json:"state"`
	Holder   string    `json:"holder"`
	Host     string    `json:"host"`
	Relation string    `json:"relation"`
	TTL      string    `json:"ttl"`
	ID       uint64    `json:"id"`
}

type delegateJSON struct {
	Expires     time.Time `json:"expires_at"`
	LastSeen    time.Time `json:"last_seen,omitzero"`
	Handle      string    `json:"handle"`
	Description string    `json:"description"`
	Bound       bool      `json:"bound"`
}

type linkJSON struct {
	Expires time.Time `json:"expires_at"`
	Handle  string    `json:"handle"`
}

const stateNone = "none"

// The headers the session start hook sends its machine and environment
// in, for the take-over hint; never the query, which access logs keep.
const (
	headerEnvironment = "Grelot-Environment"
	headerMachine     = "Grelot-Machine"
)

// me is GET /v1/me, which every session may call, a dormant one included.
func (s *Server) me(r *http.Request, c store.Caller) (status int, body any, err error) {
	out := meResponse{Version: s.version, PathRoot: s.cfg.PathRoot, State: stateNone}
	id, err := s.store.Seen(r.Context(), c)
	var se *store.Error
	switch {
	case errors.As(err, &se) && se.Kind == store.KindGone && se.Why == stateNone:
		out.Hint, err = s.hint(r, c)
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
	return http.StatusOK, out, nil
}

// meLive fills out with what a live identity's status adds.
func (s *Server) meLive(r *http.Request, c store.Caller, out *meResponse, id uint64) error {
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
	out.Online = !st.Identity.Ended && s.presence.online(id, now)
	for i := range resv {
		out.Reservations = append(out.Reservations, reservationOut(&resv[i]))
	}
	for _, d := range dl.Delegates {
		out.Delegates = append(out.Delegates, delegateJSON{
			Expires: d.Expires.UTC(), LastSeen: d.LastSeen.UTC(), Handle: d.Handle,
			Description: d.Description, Bound: d.Bound,
		})
	}
	for _, l := range dl.Links {
		out.Links = append(out.Links, linkJSON{Expires: l.Expires.UTC(), Handle: l.Handle})
	}
	return nil
}

// hint is the take-over hint for a session with no identity that names
// its CLI process in proc.
func (s *Server) hint(r *http.Request, c store.Caller) (*hintJSON, error) {
	proc := r.URL.Query().Get("proc")
	if proc == "" {
		return nil, nil
	}
	caller := store.Where{Machine: r.Header.Get(headerMachine), Environment: r.Header.Get(headerEnvironment)}
	id, found, err := s.store.TakeOverHint(r.Context(), c.Session, proc, caller)
	if err != nil || !found {
		return nil, err
	}
	h := &hintJSON{Name: id.Name, State: id.State}
	h.Line = fmt.Sprintf("you were %s before this session started; to go on as it, run `grelot register %s --take-over --ttl <d>`",
		id.Name, id.Name)
	if id.State != stateLive {
		h.Line = fmt.Sprintf("you were %s before this session started, and it is dormant since %s; to go on as it, run `grelot revive %s --ttl <d>`",
			id.Name, id.DormantSince.UTC().Format("2006-01-02 15:04 UTC"), id.Name)
	}
	return h, nil
}

type patchRequest struct {
	Where       *store.Where `json:"where"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Paths       []string     `json:"paths"`
	TTL         duration     `json:"ttl"`
}

type identityResponse struct {
	Identity identityJSON `json:"identity"`
}

// patchMe is PATCH /v1/me: new fields, with where when paths change, or
// with {ttl, where} a renewal of the lease.
func (s *Server) patchMe(r *http.Request, c store.Caller) (status int, body any, err error) {
	var req patchRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	got, err := s.store.Set(r.Context(), c, store.Change{
		Where: req.Where, TTL: time.Duration(req.TTL),
		Fields: store.Fields{Name: req.Name, Description: req.Description, Paths: req.Paths},
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, identityResponse{Identity: identityOut(&got)}, nil
}

// deleteMe is DELETE /v1/me, grelot bye.
func (s *Server) deleteMe(r *http.Request, c store.Caller) (status int, body any, err error) {
	if err := s.store.Retire(r.Context(), c); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]bool{"retired": true}, nil
}

const stateLive = "live"

// countsJSON is deliveries by kind.
type countsJSON struct {
	Direct      int `json:"direct"`
	Path        int `json:"path"`
	Announce    int `json:"announce"`
	Identity    int `json:"identity"`
	Reservation int `json:"reservation"`
	Receipts    int `json:"receipts"`
	Cards       int `json:"cards"`
	Copies      int `json:"copies"`
	Wake        int `json:"wake"`
	Total       int `json:"total"`
}

func countsOut(c store.Counts) countsJSON {
	return countsJSON{
		Direct: c.Direct, Path: c.Path, Announce: c.Announce, Identity: c.Identity,
		Reservation: c.Reservation, Receipts: c.Receipts, Cards: c.Cards, Copies: c.Copies,
		Wake: c.Wake(), Total: c.Total(),
	}
}
