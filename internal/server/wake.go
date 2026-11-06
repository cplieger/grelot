//go:build !windows

package server

import "sync"

// Hub wakes the waits of each identity. Woke, the store's commit notice,
// closes the channel every wait of those identities holds and replaces it.
type Hub struct {
	m map[uint64]*waiters
	// listener is told every Woke's ids after the waits are woken.
	listener func(ids []uint64)
	mu       sync.Mutex
}

// waiters is one identity's channel and how many waits hold it; the entry
// goes when the last lets go.
type waiters struct {
	ch chan struct{}
	n  int
}

// NewHub returns a hub no wait holds yet.
func NewHub() *Hub { return &Hub{m: map[uint64]*waiters{}} }

// Woke wakes every wait of each of ids. It only closes channels and tells
// the listener, as store.Options.Woke requires.
func (h *Hub) Woke(ids []uint64) {
	h.mu.Lock()
	for _, id := range ids {
		if e := h.m[id]; e != nil {
			close(e.ch)
			e.ch = make(chan struct{})
		}
	}
	tell := h.listener
	h.mu.Unlock()
	if tell != nil {
		tell(ids)
	}
}

// listen has fn told the ids of every later Woke. fn runs inside a
// store's commit notice, so it must not block or start a store write.
func (h *Hub) listen(fn func(ids []uint64)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listener = fn
}

// take returns id's channel, which the next Woke naming id closes, and the
// release the caller calls once when it stops waiting. A wait takes it
// before it reads the store, so a commit landing between that read and the
// block has already closed the channel it holds.
func (h *Hub) take(id uint64) (wake <-chan struct{}, release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.m[id]
	if e == nil {
		e = &waiters{ch: make(chan struct{})}
		h.m[id] = e
	}
	e.n++
	return e.ch, sync.OnceFunc(func() { h.release(id) })
}

func (h *Hub) release(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.m[id]; e != nil {
		e.n--
		if e.n == 0 {
			delete(h.m, id)
		}
	}
}
