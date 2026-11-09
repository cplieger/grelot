//go:build !windows

package server

import (
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

const (
	// onlineFor is how long after its last standing wait closed an identity
	// still counts as online: it covers an exit watcher's restart between
	// two wakes.
	onlineFor = 90 * time.Second
	// turnLapse bounds an open turn whose end no hook reports.
	turnLapse = 5 * time.Minute
	// ackGrace is how long mail an in-turn wait returned waits for the
	// client's ack, which follows the printed page at once, before a first
	// nudge covers it.
	ackGrace = 30 * time.Second
)

// presence is what the server knows, in memory only, of each identity's
// session: its standing and in-turn waits, its open turn and the down
// notice. A restart forgets it all, so every identity then reads offline,
// its first turn is not deferred and a down episode can be told once more.
type presence struct {
	m map[uint64]mark
	// sessions holds an entry only while one of its standing waits is open.
	sessions map[string]sessionWaits
	mu       sync.Mutex
}

// sessionWaits is one session's open standing waits. Only the newest
// serves the session: one nudge wakes it once, and the older waits end.
type sessionWaits struct {
	// newest is the newest wait's replaced channel.
	newest chan struct{}
	// handed is a nudge a replaced wait sent, which the newest returns.
	handed *nudgeJSON
	// id is the identity the newest wait holds for.
	id   uint64
	open int
}

// waitTicket is one standing wait's place among its session's waits.
type waitTicket struct {
	// replaced closes when a newer standing wait of the session opens.
	replaced chan struct{}
	session  string
	id       uint64
}

type mark struct {
	// closed is when the last standing wait closed.
	closed time.Time
	// turnAt is when the open turn was last set, zero when none is.
	turnAt time.Time
	// servedAt is when an in-turn wait last returned wake mail, up to seq
	// served, which its ack is still to mark read.
	servedAt time.Time
	served   uint64
	// open and inTurn count the standing and in-turn waits held now.
	open   int
	inTurn int
	// held and stopSeen count since the identity registered: a standing
	// wait held, a stop hook seen. downTold is the stop's down notice given
	// in this down episode, which the next standing wait ends.
	held     bool
	stopSeen bool
	downTold bool
}

func newPresence() *presence {
	return &presence{m: map[uint64]mark{}, sessions: map[string]sessionWaits{}}
}

// update runs fn on id's mark under the lock.
func (p *presence) update(id uint64, fn func(*mark)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.m[id]
	fn(&m)
	p.m[id] = m
}

func (p *presence) get(id uint64) mark {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.m[id]
}

// registered starts id's session marks afresh, for a registration, a
// take-over or a revival; the waits already open stay counted.
func (p *presence) registered(id uint64) {
	p.update(id, func(m *mark) {
		*m = mark{closed: m.closed, open: m.open, inTurn: m.inTurn}
	})
}

// opened records a standing wait of id starting in session, which ends
// the down episode and replaces the session's other open standing waits;
// the caller calls closed with the ticket when it ends.
func (p *presence) opened(id uint64, session string) waitTicket {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.m[id]
	m.open, m.held, m.downTold = m.open+1, true, false
	p.m[id] = m
	t := waitTicket{replaced: make(chan struct{}), session: session, id: id}
	sw := p.sessions[session]
	if sw.newest != nil {
		close(sw.newest)
	}
	if sw.id != id {
		sw.handed = nil
	}
	sw.newest, sw.id, sw.open = t.replaced, id, sw.open+1
	p.sessions[session] = sw
	return t
}

// next reports whether a newer standing wait of t's session replaced t
// and, when none has, takes the nudge a replaced wait handed it.
func (p *presence) next(t waitTicket) (handed *nudgeJSON, replaced bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sw := p.sessions[t.session]
	if sw.newest != t.replaced {
		return nil, true
	}
	handed, sw.handed = sw.handed, nil
	p.sessions[t.session] = sw
	return handed, false
}

// handOff reports whether t was replaced when it sent n and, when the
// session's newest wait holds for t's identity too, hands n to it.
func (p *presence) handOff(t waitTicket, n nudgeJSON) (handed, replaced bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sw := p.sessions[t.session]
	if sw.newest == t.replaced {
		return false, false
	}
	if sw.id != t.id {
		return false, true
	}
	sw.handed = &n
	p.sessions[t.session] = sw
	return true, true
}

func (p *presence) closed(t waitTicket, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.m[t.id]
	m.open, m.closed = m.open-1, now
	p.m[t.id] = m
	sw := p.sessions[t.session]
	sw.open--
	if sw.open == 0 {
		delete(p.sessions, t.session)
		return
	}
	p.sessions[t.session] = sw
}

// online reports whether id holds a standing wait, or closed one under
// onlineFor ago. A session the SessionEnd report closed is offline, which
// Server.onlineAt checks.
func (p *presence) online(id uint64, now time.Time) bool {
	m := p.get(id)
	return m.open > 0 || (!m.closed.IsZero() && now.Sub(m.closed) < onlineFor)
}

// onlineAt reports, at now, whether an identity is online.
func (s *Server) onlineAt(now time.Time) func(*store.Identity) bool {
	return func(id *store.Identity) bool { return !id.Ended && s.presence.online(id.ID, now) }
}

// delegateOnline reports, at now, whether a delegate is online: it made a
// request under onlineFor ago, since it never holds a standing wait, and
// its session has not ended.
func delegateOnline(d *store.Identity, now time.Time) bool {
	return !d.Ended && !d.LastSeen.IsZero() && now.Sub(d.LastSeen) < onlineFor
}

// inTurnOpened records an in-turn wait of id starting; the caller calls
// inTurnClosed when it ends.
func (p *presence) inTurnOpened(id uint64) { p.update(id, func(m *mark) { m.inTurn++ }) }

// inTurnClosed ends an in-turn wait of id; served is the newest wake seq
// it returned, 0 for none.
func (p *presence) inTurnClosed(id, served uint64, now time.Time) {
	p.update(id, func(m *mark) {
		m.inTurn--
		if served > 0 {
			m.served, m.servedAt = served, now
		}
	})
}

// inTurnHolds reports whether an in-turn wait of id covers a first nudge
// up to seq at now: one is open, or one returned that mail under ackGrace
// ago, until is then when that ends. One lock reads both, so a wait that
// closes in between is never missed.
func (p *presence) inTurnHolds(id, seq uint64, now time.Time) (until time.Time, ok bool) {
	m := p.get(id)
	if m.inTurn > 0 {
		return time.Time{}, true
	}
	until = m.servedAt.Add(ackGrace)
	return until, seq <= m.served && now.Before(until)
}

// sawStop records id's stop hook, without which no turn of id is kept open.
func (p *presence) sawStop(id uint64) { p.update(id, func(m *mark) { m.stopSeen = true }) }

// turnSet opens id's turn, or extends it, until turnLapse after now.
func (p *presence) turnSet(id uint64, now time.Time) { p.update(id, func(m *mark) { m.turnAt = now }) }

// turnClosed ends id's turn and the ack grace of mail an in-turn wait
// returned in it, so a first nudge for that mail is due at once.
func (p *presence) turnClosed(id uint64) {
	p.update(id, func(m *mark) { m.turnAt, m.served, m.servedAt = time.Time{}, 0, time.Time{} })
}

// turnOpen reports whether id's turn is open at now, and until when it
// lapses. A turn is open only for an identity whose stop hook was seen.
func (p *presence) turnOpen(id uint64, now time.Time) (until time.Time, open bool) {
	m := p.get(id)
	if !m.stopSeen || m.turnAt.IsZero() {
		return time.Time{}, false
	}
	until = m.turnAt.Add(turnLapse)
	return until, now.Before(until)
}

// tellDown reports whether the stop's down notice is owed id now, and
// marks it given: once per down episode, and only for an identity that
// has held a standing wait. The caller knows the wake is down.
func (p *presence) tellDown(id uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.m[id]
	if !m.held || m.downTold {
		return false
	}
	m.downTold = true
	p.m[id] = m
	return true
}

// ids lists every identity with a mark.
func (p *presence) ids() []uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Collect(maps.Keys(p.m))
}

// forget drops ids' marks, but for one that holds a wait: its closed call
// is still to come.
func (p *presence) forget(ids []uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range ids {
		if m := p.m[id]; m.open == 0 && m.inTurn == 0 {
			delete(p.m, id)
		}
	}
}
