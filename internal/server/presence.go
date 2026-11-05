package server

import (
	"maps"
	"slices"
	"sync"
	"time"
)

const (
	// onlineFor is how long after its last standing wait closed an identity
	// still counts as online: it covers an exit watcher's restart between
	// two wakes.
	onlineFor = 90 * time.Second
	// turnLapse bounds an open turn whose end no hook reports.
	turnLapse = 5 * time.Minute
)

// presence is what the server knows, in memory only, of each identity's
// session: its standing and in-turn waits, its open turn and the down
// notice. A restart forgets it all, so every identity then reads offline,
// its first turn is not deferred and a down episode can be told once more.
type presence struct {
	m  map[uint64]mark
	mu sync.Mutex
}

type mark struct {
	// closed is when the last standing wait closed.
	closed time.Time
	// turnAt is when the open turn was last set, zero when none is.
	turnAt time.Time
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

func newPresence() *presence { return &presence{m: map[uint64]mark{}} }

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
	p.update(id, func(m *mark) { *m = mark{closed: m.closed, open: m.open, inTurn: m.inTurn} })
}

// opened records a standing wait of id starting, which ends the down
// episode; the caller calls closed when it ends.
func (p *presence) opened(id uint64) {
	p.update(id, func(m *mark) { m.open, m.held, m.downTold = m.open+1, true, false })
}

func (p *presence) closed(id uint64, now time.Time) {
	p.update(id, func(m *mark) { m.open, m.closed = m.open-1, now })
}

// online reports whether id holds a standing wait, or closed one under
// onlineFor ago. A session the SessionEnd report closed is offline, which
// the caller checks.
func (p *presence) online(id uint64, now time.Time) bool {
	m := p.get(id)
	return m.open > 0 || (!m.closed.IsZero() && now.Sub(m.closed) < onlineFor)
}

// inTurnOpened records an in-turn wait of id starting; the caller calls
// inTurnClosed when it ends.
func (p *presence) inTurnOpened(id uint64) { p.update(id, func(m *mark) { m.inTurn++ }) }

func (p *presence) inTurnClosed(id uint64) { p.update(id, func(m *mark) { m.inTurn-- }) }

// inTurnOpen reports whether id holds an in-turn wait.
func (p *presence) inTurnOpen(id uint64) bool { return p.get(id).inTurn > 0 }

// sawStop records id's stop hook, without which no turn of id is kept open.
func (p *presence) sawStop(id uint64) { p.update(id, func(m *mark) { m.stopSeen = true }) }

// turnSet opens id's turn, or extends it, until turnLapse after now.
func (p *presence) turnSet(id uint64, now time.Time) { p.update(id, func(m *mark) { m.turnAt = now }) }

// turnClosed ends id's turn.
func (p *presence) turnClosed(id uint64) { p.update(id, func(m *mark) { m.turnAt = time.Time{} }) }

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
