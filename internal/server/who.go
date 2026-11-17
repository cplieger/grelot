//go:build !windows

package server

import (
	"net/http"
	"time"

	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

// who is GET /v1/identities: every live identity, with all=1 the dormant
// ones too, narrowed by the filters, which combine with AND, by name. A
// session with no identity, or no Grelot-Session at all, sees every
// relation as unknown.
func (s *Server) who(r *http.Request, c store.Caller, _ store.Keyed) (status int, body any, err error) {
	q, err := queryParams[wire.IdentitiesQuery](r)
	if err != nil {
		return 0, nil, err
	}
	p := pagingOf(r)
	wq := store.WhoQuery{
		Path: q.Path, Repo: q.Repo, Holds: q.Holds, Queued: q.Queued, Text: q.Q,
		ReadAs: p.readAs, After: string(p.after), Limit: p.limit,
	}
	var online bool
	for _, f := range []struct {
		dst  *bool
		v    wire.Flag
		name string
	}{{&online, q.Online, "online"}, {&wq.Here, q.Here, "here"}, {&wq.All, q.All, "all"}} {
		if *f.dst, err = flag(f.v, f.name); err != nil {
			return 0, nil, err
		}
	}
	now := s.now()
	isOnline := s.onlineAt(now)
	if online {
		wq.Online = isOnline
	}
	got, err := s.store.Who(r.Context(), c, &wq)
	if err != nil {
		return 0, nil, err
	}
	out := wire.Peers{List: wire.List[wire.Peer]{Items: make([]wire.Peer, 0, len(got.Items)), Total: got.Total}}
	for i := range got.Items {
		out.Items = append(out.Items, whoOut(&got.Items[i], isOnline, now))
	}
	if n := len(got.Items); n > 0 {
		out.NextCursor, err = s.next(r, got.More, got.ReadAs, []byte(got.Items[n-1].Name))
	}
	return http.StatusOK, out, err
}

func whoOut(p *store.Peer, online func(*store.Identity) bool, now time.Time) wire.Peer {
	e := wire.Peer{
		Expires: p.Expires.UTC(), LastSeen: p.LastSeen.UTC(), Name: p.Name, Description: p.Description,
		State: p.State, Host: p.Host, OS: p.OS, Arch: p.Arch, Relation: p.Relation,
		Paths: nonNil(p.Paths), Follows: nonNil(p.Follows),
		Reservations: []wire.PeerReservation{}, Queued: []wire.PeerQueued{}, Delegates: []wire.PeerDelegate{},
		Online: p.State == stateLive && online(&p.Identity),
	}
	for _, v := range p.Reservations {
		e.Reservations = append(e.Reservations, wire.PeerReservation{Expires: v.Expires.UTC(), Path: v.Path, Queue: v.Queue})
	}
	for _, v := range p.Queued {
		e.Queued = append(e.Queued, wire.PeerQueued{Path: v.Path, Place: v.Place})
	}
	for i := range p.Delegates {
		d := &p.Delegates[i]
		e.Delegates = append(e.Delegates, wire.PeerDelegate{
			LastSeen: d.LastSeen.UTC(), Name: d.Name, Description: d.Description, Online: delegateOnline(&d.Identity, now),
		})
	}
	return e
}

// flag reads the query parameter name, a wire.Flag.
func flag(v wire.Flag, name string) (bool, error) {
	switch v {
	case "", wire.FlagOff:
		return false, nil
	case wire.FlagOn:
		return true, nil
	}
	return false, fieldRefusal(name+" must be 1 or 0",
		wire.FieldError{Field: name, Code: wire.FieldOneOf, Allowed: []string{string(wire.FlagOff), string(wire.FlagOn)}})
}
