//go:build !windows

package store

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/bolttable"
)

// WhoQuery filters Who; every set filter must hold. Path keeps identities
// with a work path or follow at, above or under it; Repo those with a work
// path at or under <path root>/<Repo>; Holds those holding a grant that
// overlaps it, and Queued those with a queue entry that does; Text those
// whose name, description, host label or a path holds it, or for which
// a nested delegate's name or description holds it, ignoring case;
// Online those it reports online; All adds dormant ones; Here keeps only
// those that share the caller's environment.
type WhoQuery struct {
	Online func(*Identity) bool
	Path   string
	Repo   string
	Holds  string
	Queued string
	Text   string
	All    bool
	Here   bool
}

// Peer is an identity as a caller sees it, with its relation to the
// caller. A dormant peer's Expires is its purge time. Only the caller's
// own entry carries MachineKind and EnvironmentKnown.
type Peer struct {
	Relation string
	// Delegates are a parent's live delegates, which Who lists only here.
	Delegates    []Peer
	Reservations []PeerReservation
	Queued       []PeerQueued
	Identity
}

// PeerReservation is a grant a peer holds, with how many queue entries of
// other identities overlap it.
type PeerReservation struct {
	Expires time.Time
	Path    string
	Queue   int
}

// PeerQueued is a queue entry a peer waits on; Place 1 is the oldest
// entry overlapping its path.
type PeerQueued struct {
	Path  string
	Place int
}

// Who lists live identities but @user, by name, as c's identity sees them, each
// live parent's delegates nested under it and nowhere else; q filters the
// parents. A session with no live identity sees every relation as unknown.
// Refusals: KindInvalid for a filter path that is not canonical or a repo
// that is not one path segment.
func (s *Store) Who(ctx context.Context, c Caller, query *WhoQuery) ([]Peer, error) {
	q := *query
	if err := s.checkWho(&q); err != nil {
		return nil, err
	}
	if _, err := s.Seen(ctx, c); err != nil && !isKind(err, KindGone) {
		return nil, err
	}
	var out []Peer
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		var caller party
		if me, found, err := s.callerFor(r, c, now); err != nil {
			return err
		} else if found && me.State == stateLive {
			caller = party{where: me.Where, id: me.ID}
		}
		held, queued, err := s.reservationsByHolder(r, now)
		if err != nil {
			return err
		}
		var shown []identityRow
		delegates := map[uint64][]identityRow{}
		err = s.t.identities.table.Scan(r, bolttable.Range{}, func(_ uint64, v *identityRow) (bool, error) {
			switch v = settled(v, now, s.dormantTTL); {
			case v == nil, isUser(v):
			case v.Parent != 0:
				delegates[v.Parent] = append(delegates[v.Parent], *v)
			case s.whoShows(&q, v, caller.where, held[v.ID], queued[v.ID]):
				shown = append(shown, *v)
			}
			return true, nil
		})
		out = s.whoPeers(caller, q.Text, shown, delegates, held, queued)
		return err
	})
	slices.SortFunc(out, func(a, b Peer) int { return strings.Compare(a.Name, b.Name) })
	return out, err
}

// whoPeers is each of shown that text matches as caller sees it, with
// its delegates and reservations.
func (s *Store) whoPeers(
	caller party, text string, shown []identityRow, delegates map[uint64][]identityRow,
	held map[uint64][]PeerReservation, queued map[uint64][]PeerQueued,
) []Peer {
	var out []Peer
	for i := range shown {
		id := shown[i].ID
		if !whoText(text, &shown[i], delegates[id]) {
			continue
		}
		p := s.parentPeer(caller, &shown[i], delegates[id])
		p.Reservations, p.Queued = held[id], queued[id]
		out = append(out, p)
	}
	return out
}

// checkWho validates q's paths and repo, and lowercases its text.
func (s *Store) checkWho(q *WhoQuery) error {
	for _, f := range []struct{ name, v string }{{"path", q.Path}, {"holds", q.Holds}, {"queued", q.Queued}} {
		if f.v != "" && (!isCanonicalPath(f.v) || len(f.v) > maxPathBytes) {
			return refuse(KindInvalid, fmt.Sprintf(
				"%s %q is not a canonical path: send it as %s/<repo>/<file>, which grelot path prints", f.name, f.v, s.pathRoot,
			))
		}
	}
	if q.Repo != "" && (strings.ContainsAny(q.Repo, `/\`) || q.Repo == "." || q.Repo == ".." || strings.ContainsFunc(q.Repo, isControl)) {
		return refuse(KindInvalid, fmt.Sprintf("repo %q must be one repository name, the <repo> of %s/<repo>", q.Repo, s.pathRoot))
	}
	q.Text = strings.ToLower(q.Text)
	return nil
}

// whoShows reports whether q's filters other than Text list v, a parent,
// for a caller at where; held and queued are v's reservations.
func (s *Store) whoShows(q *WhoQuery, v *identityRow, where Where, held []PeerReservation, queued []PeerQueued) bool {
	switch {
	case v.State == stateDormant && !q.All, q.Here && !shareEnvironment(where, v.Where):
		return false
	case q.Path != "" && !slices.ContainsFunc(slices.Concat(v.Paths, v.Follows), func(p string) bool { return overlaps(p, q.Path) }):
		return false
	case q.Repo != "" && !slices.ContainsFunc(v.Paths, func(p string) bool { return under(p, path.Join(s.pathRoot, q.Repo)) }):
		return false
	case q.Holds != "" && !slices.ContainsFunc(held, func(h PeerReservation) bool { return overlaps(h.Path, q.Holds) }):
		return false
	case q.Queued != "" && !slices.ContainsFunc(queued, func(e PeerQueued) bool { return overlaps(e.Path, q.Queued) }):
		return false
	case q.Online != nil && !s.online(q.Online, v):
		return false
	}
	return true
}

// whoText reports whether text, lowercased, is in v's name, description,
// host label or a work path, or in a delegate's name or description; ""
// matches every parent.
func whoText(text string, v *identityRow, delegates []identityRow) bool {
	if text == "" {
		return true
	}
	hay := append([]string{v.Name, v.Description, v.Host}, v.Paths...)
	for i := range delegates {
		hay = append(hay, delegates[i].Name, delegates[i].Description)
	}
	return slices.ContainsFunc(hay, func(h string) bool { return strings.Contains(strings.ToLower(h), text) })
}

func (s *Store) online(pred func(*Identity) bool, v *identityRow) bool {
	id := s.view(v)
	return pred(&id)
}

// reservationsByHolder groups the unexpired grants and the queue entries
// by holder, in id order. A grant's Queue counts the other identities'
// entries overlapping it; an entry's Place counts the entries queued
// before it that overlap it, plus one.
func (s *Store) reservationsByHolder(r *bolttable.ReadTx, now time.Time) (held map[uint64][]PeerReservation, queued map[uint64][]PeerQueued, err error) {
	var grants, entries []reservationRow
	err = s.t.resv.table.Scan(r, bolttable.Range{}, func(_ uint64, v *reservationRow) (bool, error) {
		switch {
		case v.State == reservationQueued:
			entries = append(entries, *v)
		case !v.Lease.due(now):
			grants = append(grants, *v)
		}
		return true, nil
	})
	held = map[uint64][]PeerReservation{}
	for i := range grants {
		g := &grants[i]
		n := 0
		for j := range entries {
			if entries[j].Holder != g.Holder && overlaps(entries[j].Path, g.Path) {
				n++
			}
		}
		held[g.Holder] = append(held[g.Holder], PeerReservation{Expires: g.Lease.Expires, Path: g.Path, Queue: n})
	}
	queued = map[uint64][]PeerQueued{}
	for i := range entries {
		e := &entries[i]
		place := 1
		for j := range entries[:i] {
			if overlaps(entries[j].Path, e.Path) {
				place++
			}
		}
		queued[e.Holder] = append(queued[e.Holder], PeerQueued{Path: e.Path, Place: place})
	}
	return held, queued, err
}

// parentPeer is v as caller sees it, with its delegates, which a dormant
// v no longer has, judged by v's where.
func (s *Store) parentPeer(caller party, v *identityRow, delegates []identityRow) Peer {
	p := s.peer(caller, v)
	if v.State != stateLive {
		return p
	}
	for i := range delegates {
		delegates[i].Where = v.Where
		p.Delegates = append(p.Delegates, s.peer(caller, &delegates[i]))
	}
	slices.SortFunc(p.Delegates, func(a, b Peer) int { return strings.Compare(a.Name, b.Name) })
	return p
}

// peer is v as caller sees it.
func (s *Store) peer(caller party, v *identityRow) Peer {
	p := Peer{Identity: s.view(v), Relation: caller.relationTo(party{where: v.Where, id: v.ID}, "")}
	if v.ID != caller.id {
		p.MachineKind, p.EnvironmentKnown = "", false
	}
	return p
}
