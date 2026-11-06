//go:build !windows

package server

import (
	"context"
	"net/http"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

type watchRequest struct {
	Wait duration `json:"wait"`
}

// nudgeJSON is a nudge as a standing wait returns it: the signal line's
// seq, the wake deliveries past read, and which send it is, 1 to 4.
type nudgeJSON struct {
	Seq     uint64 `json:"seq"`
	Count   int    `json:"count"`
	Attempt int    `json:"attempt"`
}

// errSessionEnded answers a standing wait while the session's end is
// reported; its code is the reason word the watcher's end line carries.
var errSessionEnded = &apiError{
	status: http.StatusGone, code: "ended",
	msg: "session ended: its end was reported, so its watcher stops; any other grelot command from this session reopens it",
}

// standingWait is POST /v1/events/wait: it holds until a nudge for the
// caller's identity goes out, sending a due one itself unless host push
// works, and returns it; 204 when wait passes with none.
func (s *Server) standingWait(r *http.Request, c store.Caller) (status int, body any, err error) {
	ctx := r.Context()
	id, wait, err := s.watchFor(r, c)
	if err != nil {
		return 0, nil, err
	}
	since := s.presence.opened(id)
	defer func() { s.presence.closed(id, s.now()) }()
	deadline := s.now().Add(wait)
	for {
		wake, release := s.hub.take(id)
		step, err := s.look(ctx, c, id, since)
		if err == nil && step.nudge == nil && !step.done {
			until := deadline
			if !step.until.IsZero() && step.until.Before(deadline) {
				until = step.until
			}
			err = s.hold(ctx, wake, until)
		}
		release()
		switch {
		case err != nil:
			return 0, nil, err
		case step.nudge != nil:
			return http.StatusOK, step.nudge, nil
		case step.done || !s.now().Before(deadline):
			return http.StatusNoContent, nil, nil
		}
	}
}

// watchFor reads a standing wait's request: the identity it holds for and
// how long. A refused wait holds nothing, so it never counts as held.
func (s *Server) watchFor(r *http.Request, c store.Caller) (id uint64, wait time.Duration, err error) {
	var req watchRequest
	if derr := decode(r, &req); derr != nil {
		return 0, 0, derr
	}
	wait = time.Duration(req.Wait)
	if wait <= 0 || wait > maxWait {
		return 0, 0, badRequest("wait must be a duration above 0 and at most 55s, such as 55s")
	}
	me, err := s.store.Watcher(r.Context(), c)
	switch {
	case err != nil:
		return 0, 0, err
	case me.Ended:
		return 0, 0, errSessionEnded
	}
	return me.ID, wait, nil
}

// watchStep is one look of a standing wait: the nudge to return, the wait
// ended with none, or the instant to look again by, zero for none; a wake
// of the hub's channel always brings the next look.
type watchStep struct {
	until time.Time
	nudge *nudgeJSON
	done  bool
}

// look reads id's nudge state and sends what is due through the standing
// wait; of two waits racing on one send, the one RecordNudge refuses reads
// again.
func (s *Server) look(ctx context.Context, c store.Caller, id uint64, since sendCount) (watchStep, error) {
	for {
		step, due, err := s.due(ctx, c, id, since)
		if err != nil || due == nil {
			return step, err
		}
		got, recorded, err := s.store.RecordNudge(ctx, due, store.NudgeByWatcher)
		if err != nil {
			return watchStep{}, err
		}
		if recorded {
			n := nudgeJSON{Seq: got.Seq, Count: got.Count, Attempt: got.Attempt}
			s.watcherSent(id, n)
			return watchStep{nudge: &n}, nil
		}
	}
}

// due is look's read: the nudge a standing wait of id is to send now, or
// the step to take when there is none. A wait whose id is gone, while its
// session holds another identity, ends with none: the next wait holds for
// that one.
func (s *Server) due(ctx context.Context, c store.Caller, id uint64, since sendCount) (watchStep, *store.Nudge, error) {
	me, err := s.store.Watcher(ctx, c)
	switch {
	case err != nil:
		return watchStep{}, nil, err
	case me.Ended:
		return watchStep{}, nil, errSessionEnded
	}
	if n, ok := s.presence.sentSince(id, since); ok {
		return watchStep{nudge: &n}, nil, nil
	}
	if s.push.State(id) == pushOK {
		return watchStep{}, nil, nil
	}
	st, found, err := s.store.Nudging(ctx, id)
	switch {
	case err != nil || !found:
		return watchStep{done: true}, nil, err
	case st.Due == nil:
		return watchStep{until: st.Next}, nil, nil
	}
	if until, deferred := s.deferred(id, st.Due); deferred {
		return watchStep{until: until}, nil, nil
	}
	return watchStep{}, st.Due, nil
}

// watcherSent records n, which a standing wait of id sent, for id's other
// waits. The send's own commit woke them before sentSince could show it,
// so they are woken again to look.
func (s *Server) watcherSent(id uint64, n nudgeJSON) {
	s.presence.watcherSent(id, n)
	s.hub.Woke([]uint64{id})
}

// deferred reports whether due, a first nudge, waits for the hooks: while
// id's turn is open, until it lapses, or while an in-turn wait covers it
// (presence.inTurnHolds). until is zero when the deferral ends on a wake
// rather than a clock (the in-turn wait closing, or a commit). A re-nudge
// never waits.
func (s *Server) deferred(id uint64, due *store.Nudge) (until time.Time, ok bool) {
	if due.Attempt > 1 {
		return time.Time{}, false
	}
	if until, open := s.presence.turnOpen(id, s.now()); open {
		return until, true
	}
	return s.presence.inTurnHolds(id, due.Seq, s.now())
}

// hold blocks until wake closes, the clock reaches until, ctx ends or
// Release is called, which it answers with errShuttingDown.
func (s *Server) hold(ctx context.Context, wake <-chan struct{}, until time.Time) error {
	t := time.NewTimer(until.Sub(s.now()))
	defer t.Stop()
	select {
	case <-wake:
	case <-t.C:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopping:
		return errShuttingDown
	}
	return nil
}

// rewait makes id's standing waits look again after a change no commit
// wakes them for: its session's end, its retirement or its move to another
// session, which they then answer with a 410.
func (s *Server) rewait(id uint64) { s.hub.Woke([]uint64{id}) }
