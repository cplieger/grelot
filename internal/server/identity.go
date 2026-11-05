package server

import (
	"errors"
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
	Identity identityJSON `json:"identity"`
	Existing bool         `json:"existing,omitempty"`
	Moved    bool         `json:"moved,omitempty"`
}

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
	return status, registerResponse{Identity: identityOut(&got.Identity), Existing: got.Existing, Moved: got.Moved}, nil
}

// meResponse is GET /v1/me. State is live, dormant or none, which the
// hooks act on; a session with no identity gets only State and the
// register command in its status line.
type meResponse struct {
	Identity *identityJSON `json:"identity,omitempty"`
	Unread   *countsJSON   `json:"unread,omitempty"`
	State    string        `json:"state"`
	Wake     string        `json:"wake,omitempty"`
	Version  string        `json:"version"`
	PathRoot string        `json:"path_root"`
	Online   bool          `json:"online"`
}

const stateNone = "none"

// me is GET /v1/me, which every session may call, a dormant one included.
func (s *Server) me(r *http.Request, c store.Caller) (status int, body any, err error) {
	out := meResponse{Version: s.version, PathRoot: s.cfg.PathRoot, State: stateNone}
	id, err := s.store.Seen(r.Context(), c)
	var se *store.Error
	switch {
	case errors.As(err, &se) && se.Kind == store.KindGone && se.Why == stateNone:
		return http.StatusOK, out, nil
	case err != nil:
		return 0, nil, err
	}
	card := identityOut(&id)
	out.Identity, out.State = &card, id.State
	if id.State != stateLive {
		return http.StatusOK, out, nil
	}
	st, err := s.store.Status(r.Context(), c)
	if err != nil {
		return 0, nil, err
	}
	now := s.now()
	counts := countsOut(st.Unread)
	out.Unread, out.Wake = &counts, s.wakeState(&st, now)
	out.Online = !id.Ended && s.presence.online(id.ID, now)
	return http.StatusOK, out, nil
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
