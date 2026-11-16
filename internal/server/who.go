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
	q := r.URL.Query()
	p := pagingOf(r)
	wq := store.WhoQuery{
		Path: q.Get("path"), Repo: q.Get("repo"), Holds: q.Get("holds"), Queued: q.Get("queued"), Text: q.Get("q"),
		After: string(p.after), Limit: p.limit,
	}
	var online bool
	for _, f := range []struct {
		dst  *bool
		name string
	}{{&online, "online"}, {&wq.Here, "here"}, {&wq.All, "all"}} {
		if *f.dst, err = flag(q.Get(f.name), f.name); err != nil {
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
		out.NextCursor, err = s.next(r, got.More, []byte(got.Items[n-1].Name))
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

// flag reads a 0-or-1 query parameter.
func flag(v, name string) (bool, error) {
	switch v {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, badRequest(name + " must be 1 or 0")
}
