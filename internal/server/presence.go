package server

import (
	"sync"
	"time"
)

// onlineFor is how long after its last standing wait closed an identity
// still counts as online: it covers an exit watcher's restart between two
// wakes.
const onlineFor = 90 * time.Second

// presence is, in memory, when each identity last held a standing wait.
// A restart forgets it, and every identity then reads offline until its
// watcher polls again.
type presence struct {
	m  map[uint64]waitMark
	mu sync.Mutex
}

type waitMark struct {
	closed time.Time
	open   int
}

func newPresence() *presence { return &presence{m: map[uint64]waitMark{}} }

// opened records a standing wait of id starting; the caller calls closed
// when it ends.
func (p *presence) opened(id uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.m[id]
	m.open++
	p.m[id] = m
}

func (p *presence) closed(id uint64, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.m[id]
	m.open--
	m.closed = now
	p.m[id] = m
}

// online reports whether id holds a standing wait, or closed one under
// onlineFor ago. A session the SessionEnd report closed is offline, which
// the caller checks.
func (p *presence) online(id uint64, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.m[id]
	return ok && (m.open > 0 || now.Sub(m.closed) < onlineFor)
}
