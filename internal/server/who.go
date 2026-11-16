//go:build !windows

package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

// whoFields are the names fields= takes, in wire.Peer's JSON names.
var whoFields = []string{
	"arch", "delegates", "description", "expires_at", "follows", "host", "last_seen",
	"name", "online", "os", "paths", "queued", "relation", "reservations", "state",
}

// who is GET /v1/identities: every live identity, with all=1 the dormant
// ones too, narrowed by the filters, which combine with AND. A session
// with no identity, or no Grelot-Session at all, sees every relation as
// unknown.
func (s *Server) who(r *http.Request, c store.Caller, _ store.Keyed) (status int, body any, err error) {
	q := r.URL.Query()
	fields, err := whoFieldSet(q.Get("fields"))
	if err != nil {
		return 0, nil, err
	}
	wq := store.WhoQuery{
		Path: q.Get("path"), Repo: q.Get("repo"), Holds: q.Get("holds"), Queued: q.Get("queued"), Text: q.Get("q"),
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
	peers, err := s.store.Who(r.Context(), c, &wq)
	if err != nil {
		return 0, nil, err
	}
	entries := make([]any, 0, len(peers))
	for i := range peers {
		e := whoOut(&peers[i], isOnline, now)
		if fields == nil {
			entries = append(entries, e)
			continue
		}
		picked, perr := pick(&e, fields)
		if perr != nil {
			return 0, nil, perr
		}
		entries = append(entries, picked)
	}
	return http.StatusOK, map[string]any{"identities": entries}, nil
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

// whoFieldSet parses fields=, which always keeps name; nil means every
// field.
func whoFieldSet(v string) ([]string, error) {
	if v == "" {
		return nil, nil
	}
	out := []string{"name"}
	for f := range strings.SplitSeq(v, ",") {
		if !slices.Contains(whoFields, f) {
			return nil, badRequest("unknown field " + strconv.Quote(f) + " in fields: valid fields are " + strings.Join(whoFields, ", "))
		}
		out = append(out, f)
	}
	return out, nil
}

// pick is e with only the named fields.
func pick(e *wire.Peer, fields []string) (map[string]json.RawMessage, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(fields))
	for _, f := range fields {
		if v, ok := all[f]; ok {
			out[f] = v
		}
	}
	return out, nil
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
