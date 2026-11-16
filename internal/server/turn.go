//go:build !windows

package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

const holdStop = "stop"

// eventHold is POST /v1/events/hold {for: stop, last?}, the stop hook's
// one request. A hold that blocks keeps the turn open, unless last says
// the host runs no second stop hook this turn; any other hold ends it.
func (s *Server) eventHold(r *http.Request, c store.Caller, _ store.Keyed) (status int, body any, err error) {
	var req wire.HoldRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	if req.For != holdStop {
		return 0, nil, badRequest(`for must be "stop"`)
	}
	ctx := r.Context()
	held, err := s.store.Hold(ctx, c)
	if err != nil {
		return 0, nil, err
	}
	id := held.Identity.ID
	s.presence.sawStop(id)
	// Hold may have committed: a deadline passing now must not answer an
	// error for it.
	st, err := s.store.Status(context.WithoutCancel(ctx), c)
	if err != nil {
		return 0, nil, err
	}
	out := wire.Held{Counts: countsOut(held.Counts), Shown: held.Shown, Wake: s.wakeState(&st, s.now())}
	switch {
	case held.Counts.Wake() > 0:
		out.Block = true
		s.sampler.heldTurn(id, held.Identity.Name)
		s.log.Info("hook shown", "via", holdStop, "name", held.Identity.Name, "count", held.Counts.Total())
	case out.Wake == wakeDown && s.presence.tellDown(id):
		out.Down = true
	}
	if out.Block && !req.Last {
		s.presence.turnSet(id, s.now())
	} else {
		s.endTurn(id)
	}
	return http.StatusOK, out, nil
}

// endTurn closes id's turn and wakes its standing waits, so a first nudge
// the turn deferred goes out at once.
func (s *Server) endTurn(id uint64) {
	s.presence.turnClosed(id)
	s.hub.Woke([]uint64{id})
}

// turnStart reads GET /v1/me's turn and signal parameters: start opens
// the turn, unless signal names a signal line whose seq read has reached.
type turnStart struct {
	signal uint64
	start  bool
}

func turnStartOf(q *wire.MeQuery) (turnStart, error) {
	var out turnStart
	switch q.Turn {
	case "":
	case wire.TurnStart:
		out.start = true
	default:
		return out, fieldRefusal(`turn must be "start" when sent`,
			wire.FieldError{Field: "turn", Code: wire.FieldOneOf, Allowed: []string{string(wire.TurnStart)}})
	}
	if q.Signal != 0 && !out.start {
		return out, badRequest("signal must be the seq of a signal line, sent with turn=start")
	}
	out.signal = uint64(q.Signal)
	return out, nil
}

// open opens id's turn for a prompt, unless the prompt is a stale signal
// line, which the client's stale-signal block ends.
func (t turnStart) open(s *Server, id *store.Identity) {
	if t.start && !t.stale(id) {
		s.presence.turnSet(id.ID, s.now())
	}
}

// stale reports whether the prompt is a signal line whose seq id's read
// has reached: GET /v1/me answers it as signal_read.
func (t turnStart) stale(id *store.Identity) bool {
	return t.signal != 0 && id.Read >= t.signal
}

// inTurnWait is GET /v1/inbox?wait_ms=: it holds until a wake delivery is
// past read, whatever the nudge state, then answers the page; at wait's
// end or on Release it answers the page as it stands; past a held-wait
// cap it holds nothing and answers 429. The wait is counted open before
// its first Inbox, whose Seen wakes an ended session's identity at its
// commit: a first nudge that wake finds uncovered goes out for the mail
// this wait returns.
func (s *Server) inTurnWait(ctx context.Context, c store.Caller, q store.InboxQuery, wait time.Duration) (page store.InboxPage, err error) {
	if wait == 0 {
		return s.store.Inbox(ctx, c, q)
	}
	me, err := s.store.Acting(ctx, c)
	if err != nil {
		// Inbox answers the refusal, so it reads as it does with no wait.
		return s.store.Inbox(ctx, c, q)
	}
	free, err := s.holdWait(c.Key)
	if err != nil {
		return store.InboxPage{}, err
	}
	defer free()
	id := me.ID
	s.presence.inTurnOpened(id)
	defer func() { s.inTurnEnded(id, newestWake(page.Items)) }()
	deadline := s.now().Add(wait)
	for {
		wake, release := s.hub.take(id)
		page, err = s.store.Inbox(ctx, c, q)
		if err != nil || page.Unread.Wake() > 0 || !s.now().Before(deadline) {
			release()
			return page, err
		}
		err = s.hold(ctx, wake, nil, deadline)
		release()
		switch {
		case errors.Is(err, errShuttingDown):
			return page, nil
		case err != nil:
			return page, err
		}
	}
}

// inTurnEnded records an in-turn wait of id ending, having returned wake
// mail up to served (0 for none), and wakes id's standing waits: one that
// looked while the wait was open deferred its first nudge with no end.
func (s *Server) inTurnEnded(id, served uint64) {
	s.presence.inTurnClosed(id, served, s.now())
	s.hub.Woke([]uint64{id})
}

// newestWake is the seq of the newest wake entry of es, 0 for none.
func newestWake(es []store.Entry) uint64 {
	var seq uint64
	for i := range es {
		if es[i].Wake {
			seq = max(seq, es[i].Seq)
		}
	}
	return seq
}
