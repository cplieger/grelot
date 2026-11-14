//go:build !windows

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/transport"
)

const (
	msgPushFailed       = "push failed"
	msgPushRefused      = "push refused"
	msgPushGone         = "push gone"
	msgPushLookupFailed = "push lookup failed"
)

// attrStatus is a push line's HTTP status, 0 when no answer came back.
const attrStatus = "status"

// lookupRetry is how soon a look the store failed is tried again.
const lookupRetry = 15 * time.Second

// hostPush sends each push-served identity's due nudge through the http
// door that serves it, from one goroutine. Its state is in memory only:
// a restart turns every door and identity back on.
type hostPush struct {
	s       *Server
	client  *transport.HTTP
	doors   *provider.ServerDoors
	cancel  context.CancelFunc
	done    chan struct{}
	kick    chan struct{}
	ids     map[uint64]*pushee
	doorOff map[*provider.Door]bool
	marks   map[uint64]struct{}
	mu      sync.Mutex
	once    sync.Once
}

var _ Pusher = (*hostPush)(nil)

// pushee is one push-served identity.
type pushee struct {
	// at is when to look again, zero for only at a wake.
	at time.Time
	// retryAt is when the failed send key is tried again, zero for none.
	retryAt time.Time
	door    provider.ServerDoor
	key     sendKey
	off     bool
}

// sendKey is what a send's dedupe key is made of besides the identity.
type sendKey struct {
	read    uint64
	attempt int
}

// startHostPush maps every live identity a door serves, from the store,
// and starts the sender, which looks at each once.
func startHostPush(s *Server, doors *provider.ServerDoors) *hostPush {
	ctx, cancel := context.WithCancel(context.Background())
	p := &hostPush{
		s: s, client: transport.NewHTTP(), doors: doors, cancel: cancel,
		done: make(chan struct{}), kick: make(chan struct{}, 1),
		ids: map[uint64]*pushee{}, doorOff: map[*provider.Door]bool{}, marks: map[uint64]struct{}{},
	}
	s.hub.listen(p.mark)
	all, err := s.store.Addressees(ctx)
	if err != nil {
		s.log.Error(msgPushLookupFailed, "error", err.Error())
	}
	for i := range all {
		p.registered(all[i].ID, all[i].Provider, all[i].Host)
	}
	for _, d := range doors.All() {
		s.metrics.PushDoorUp.Set(1, doorLabel(d))
		for _, o := range []string{obs.SendAccepted, obs.SendRetry, obs.SendGone, obs.SendGaveUp} {
			s.metrics.PushSends.Add(0, doorLabel(d), o)
		}
	}
	go p.run(ctx)
	return p
}

func (p *hostPush) State(id uint64) pushState {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.ids[id]
	switch {
	case e == nil:
		return pushNone
	case e.off || p.doorOff[e.door.Door]:
		return pushOff
	case !e.retryAt.IsZero():
		return pushFailing
	}
	return pushOK
}

// registered always installs a new entry, so a forget holding the old one
// leaves it, and on the same door it keeps a pending retry, so a repeated
// register never sends before the receiver's Retry-After.
func (p *hostPush) registered(id uint64, prov, host string) {
	d, ok := p.doors.For(prov, host)
	p.mu.Lock()
	if ok {
		fresh := &pushee{door: d}
		if e := p.ids[id]; e != nil && e.door.Door == d.Door {
			fresh.retryAt, fresh.key, fresh.at = e.retryAt, e.key, e.at
		}
		p.ids[id] = fresh
		p.marks[id] = struct{}{}
	} else {
		delete(p.ids, id)
	}
	p.mu.Unlock()
	p.wake()
}

func (p *hostPush) beat(ctx context.Context) {
	p.mu.Lock()
	read := maps.Clone(p.ids)
	p.mu.Unlock()
	live, err := p.s.store.Live(ctx, slices.Collect(maps.Keys(read)))
	if err != nil {
		return
	}
	for id, e := range read {
		if !slices.Contains(live, id) {
			p.forget(id, e)
		}
	}
}

func (p *hostPush) stop() {
	p.once.Do(func() {
		p.cancel()
		<-p.done
	})
}

// mark is the hub's listener: ids are looked at by the goroutine, since a
// commit notice may not send or write.
func (p *hostPush) mark(ids []uint64) {
	p.mu.Lock()
	for _, id := range ids {
		if p.ids[id] != nil {
			p.marks[id] = struct{}{}
		}
	}
	p.mu.Unlock()
	p.wake()
}

func (p *hostPush) wake() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *hostPush) run(ctx context.Context) {
	defer close(p.done)
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		for _, id := range p.due() {
			if !p.look(ctx, id) {
				return
			}
		}
		t.Stop()
		var tick <-chan time.Time
		if next, ok := p.next(); ok {
			t.Reset(next.Sub(p.s.now()))
			tick = t.C
		}
		select {
		case <-p.kick:
		case <-tick:
		case <-ctx.Done():
			return
		}
	}
}

// due takes, in id order, the identities marked since the last call and
// those whose look time has come.
func (p *hostPush) due() []uint64 {
	now := p.s.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, e := range p.ids {
		if !e.at.IsZero() && !now.Before(e.at) {
			e.at = time.Time{}
			p.marks[id] = struct{}{}
		}
	}
	ids := slices.Sorted(maps.Keys(p.marks))
	clear(p.marks)
	return ids
}

// next is the soonest look time, if any is set.
func (p *hostPush) next() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var next time.Time
	for _, e := range p.ids {
		if !e.at.IsZero() && (next.IsZero() || e.at.Before(next)) {
			next = e.at
		}
	}
	return next, !next.IsZero()
}

// target is id's entry, and a copy of it, while push is on for it.
func (p *hostPush) target(id uint64) (cur *pushee, snap pushee, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.ids[id]
	if e == nil || e.off || p.doorOff[e.door.Door] {
		return nil, pushee{}, false
	}
	return e, *e, true
}

// edit runs fn on id's entry, if it still has one, under the lock.
func (p *hostPush) edit(id uint64, fn func(*pushee)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.ids[id]; e != nil {
		fn(e)
	}
}

// look sends id's due nudge, or sets when to look again. A send accepted
// whose record a standing wait beat reads again, so the loop is bounded
// by the few sends that can race. It reports false once the sender stops.
func (p *hostPush) look(ctx context.Context, id uint64) bool {
	for range 3 {
		cur, e, ok := p.target(id)
		if !ok {
			return true
		}
		st, found, err := p.s.store.Nudging(ctx, id)
		switch {
		case err != nil:
			return p.lookupFailed(ctx, id, err)
		case !found:
			p.forget(id, cur)
			return true
		case st.Due == nil:
			p.edit(id, func(e *pushee) { e.retryAt, e.at = time.Time{}, st.Next })
			return true
		}
		key := sendKey{read: st.Due.Read, attempt: st.Due.Attempt}
		if p.waits(id, &e, st.Due, key) {
			return true
		}
		if sent, ok := p.send(ctx, id, cur, st.Due, key); !ok || !sent {
			return ok
		}
	}
	return true
}

// waits reports whether due, whose send is key, is not to go out yet, and
// sets when to look again: a failed send waits for its retry time, and a
// first nudge for the hooks (Server.deferred). Any other send due ends a
// pending retry, since a standing wait or a read moved the nudge on.
func (p *hostPush) waits(id uint64, e *pushee, due *store.Nudge, key sendKey) bool {
	if !e.retryAt.IsZero() {
		if key == e.key && p.s.now().Before(e.retryAt) {
			p.edit(id, func(e *pushee) { e.at = e.retryAt })
			return true
		}
		p.edit(id, func(e *pushee) { e.retryAt = time.Time{} })
	}
	until, deferred := p.s.deferred(id, due)
	if deferred {
		p.edit(id, func(e *pushee) { e.at = until })
	}
	return deferred
}

// send pushes due to id through cur's door. sent is true when the door
// accepted it and the nudge is recorded or was beaten to it; ok is false
// once the sender stops.
func (p *hostPush) send(ctx context.Context, id uint64, cur *pushee, due *store.Nudge, key sendKey) (sent, ok bool) {
	to, found, err := p.s.store.Addressee(ctx, id)
	switch {
	case err != nil:
		return false, p.lookupFailed(ctx, id, err)
	case !found:
		p.forget(id, cur)
		return false, true
	}
	d := cur.door
	res, err := p.client.Send(ctx, d.Door, &provider.Data{
		Line: transport.SignalLine(due.Seq, due.Count), Seq: due.Seq,
		Session: to.Session, Identity: to.Name,
		DedupeKey: fmt.Sprintf("%d:%d:%d", id, key.read, key.attempt),
	})
	if err != nil {
		if ctx.Err() != nil {
			return false, false
		}
		p.counted(d, obs.SendGaveUp, to.Name)
		p.identityOff(ctx, id, slog.LevelError, msgPushFailed, p.lineAttrs(d, to.Name, 0, err))
		return false, true
	}
	p.counted(d, sendOutcome(res.Outcome), to.Name)
	switch res.Outcome {
	case transport.Accepted:
		if _, _, err := p.s.store.RecordNudge(ctx, due, store.NudgeByPush); err != nil {
			return false, p.lookupFailed(ctx, id, err)
		}
		return true, true
	case transport.RetryLater:
		p.retryLater(ctx, id, key, res, p.lineAttrs(d, to.Name, res.Status, res.Err))
	case transport.Gone:
		p.identityOff(ctx, id, slog.LevelInfo, msgPushGone, p.lineAttrs(d, to.Name, res.Status, nil))
	default:
		if res.Status == http.StatusUnauthorized || res.Status == http.StatusForbidden {
			p.doorRefused(d, res.Status)
		} else {
			p.identityOff(ctx, id, slog.LevelError, msgPushFailed, p.lineAttrs(d, to.Name, res.Status, nil))
		}
	}
	return false, true
}

// sendOutcome is o's push_sends_total outcome label; like send, it takes
// every other outcome as a give up.
func sendOutcome(o transport.Outcome) string {
	switch o {
	case transport.Accepted:
		return obs.SendAccepted
	case transport.RetryLater:
		return obs.SendRetry
	case transport.Gone:
		return obs.SendGone
	}
	return obs.SendGaveUp
}

// counted counts one push try through d to the identity called name, and
// records a retry or give up as an event.
func (p *hostPush) counted(d provider.ServerDoor, outcome, name string) {
	p.s.metrics.PushSends.Inc(doorLabel(d), outcome)
	kind := obs.EventPushRetry
	switch outcome {
	case obs.SendRetry:
	case obs.SendGaveUp:
		kind = obs.EventPushGaveUp
	default:
		return
	}
	p.s.metrics.Events.Add(obs.Event{At: p.s.now(), Kind: kind, Detail: doorLabel(d), Agent: name})
}

// doorLabel is d's door label value: the provider and door names the
// operator wrote, never its URL.
func doorLabel(d provider.ServerDoor) string { return d.File + "/" + d.Door.Name }

// lineAttrs are a push line's attributes: the door and the identity's
// name, never the URL, which may hold a session id, or a header.
func (p *hostPush) lineAttrs(d provider.ServerDoor, name string, status int, err error) []any {
	out := append(doorAttrs(d, status), "name", name)
	if err != nil {
		out = append(out, "error", p.s.redact(err.Error()))
	}
	return out
}

func doorAttrs(d provider.ServerDoor, status int) []any {
	return []any{"provider", d.File, "door", d.Door.Name, attrStatus, status}
}

// retryLater tries key again after the receiver's Retry-After. Meanwhile
// push is failing, so id's standing waits may send the nudge, which ends
// the retry.
func (p *hostPush) retryLater(ctx context.Context, id uint64, key sendKey, res transport.HTTPResult, attrs []any) {
	level := slog.LevelWarn
	if res.Status != 0 && res.Status != http.StatusTooManyRequests && res.Status != http.StatusServiceUnavailable {
		level = slog.LevelError
	}
	p.s.log.Log(ctx, level, msgPushFailed, attrs...)
	at := p.s.now().Add(res.RetryAfter)
	p.edit(id, func(e *pushee) { e.retryAt, e.key, e.at = at, key, at })
	p.s.hub.Woke([]uint64{id})
}

// identityOff turns push off for id alone, until it registers again or is
// revived, and hands its nudge to its standing waits.
func (p *hostPush) identityOff(ctx context.Context, id uint64, level slog.Level, msg string, attrs []any) {
	p.s.log.Log(ctx, level, msg, attrs...)
	p.edit(id, func(e *pushee) { e.off, e.retryAt, e.at = true, time.Time{}, time.Time{} })
	p.s.hub.Woke([]uint64{id})
}

// doorRefused turns d off for every identity it serves until the server
// restarts, with one line for them all: the credential they share failed.
func (p *hostPush) doorRefused(d provider.ServerDoor, status int) {
	p.s.log.Error(msgPushRefused, doorAttrs(d, status)...)
	p.s.metrics.PushDoorUp.Set(0, doorLabel(d))
	p.mu.Lock()
	p.doorOff[d.Door] = true
	var served []uint64
	for id, e := range p.ids {
		if e.door.Door == d.Door {
			served = append(served, id)
		}
	}
	p.mu.Unlock()
	p.s.hub.Woke(served)
}

// forget drops id's entry e, read before a store call; an entry a register
// or revive installed meanwhile is newer than that read and stays.
func (p *hostPush) forget(id uint64, e *pushee) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ids[id] == e {
		delete(p.ids, id)
	}
}

// lookupFailed logs a store read that failed and looks again later; a
// closed store or a stop ends the sender.
func (p *hostPush) lookupFailed(ctx context.Context, id uint64, err error) bool {
	if ctx.Err() != nil || errors.Is(err, store.ErrClosed) {
		return false
	}
	p.s.log.Warn(msgPushLookupFailed, "error", err.Error())
	p.edit(id, func(e *pushee) { e.at = p.s.now().Add(lookupRetry) })
	return true
}
