//go:build !windows

package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/transport"
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
	status: http.StatusGone, code: transport.EndEnded,
	msg: "session ended: its end was reported, so its watcher stops; any other grelot command from this session reopens it",
}

// errReplaced answers a standing wait once a newer one of its session
// opened, so that each nudge wakes the session once.
var errReplaced = &apiError{
	status: http.StatusGone, code: transport.EndReplaced,
	msg: "replaced: another watcher of this session took over, so this one stops",
}

// standingWait is POST /v1/events/wait: it holds until a nudge for the
// caller's identity goes out, sending a due one itself unless host push
// works, and returns it; 204 when wait passes with none, and 410 replaced
// once a newer standing wait of the caller's session opens.
func (s *Server) standingWait(r *http.Request, c store.Caller) (status int, body any, err error) {
	ctx := r.Context()
	id, wait, err := s.watchFor(r, c)
	if err != nil {
		return 0, nil, err
	}
	t := s.presence.opened(id, c.Session)
	defer func() { s.presence.closed(t, s.now()) }()
	deadline := s.now().Add(wait)
	for {
		wake, release := s.hub.take(id)
		step, err := s.look(ctx, c, t)
		if err == nil && step.nudge == nil && !step.done {
			until := deadline
			if !step.until.IsZero() && step.until.Before(deadline) {
				until = step.until
			}
			err = s.hold(ctx, wake, t.replaced, until)
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

// endReasons maps the store's refusal of a session with no live identity
// to the end reason a watcher's end line carries, which transport owns.
var endReasons = map[string]httpserver.ErrorCode{
	stateNone:    transport.EndNone,
	stateDormant: transport.EndDormant,
}

// waitEnd answers a standing wait the store refused for want of a live
// identity with its end reason; any other error passes through.
func waitEnd(err error) error {
	var se *store.Error
	if !errors.As(err, &se) || se.Kind != store.KindGone {
		return err
	}
	code, ok := endReasons[se.Why]
	if !ok {
		return err
	}
	return &apiError{status: http.StatusGone, code: code, msg: se.Msg}
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
		return 0, 0, badRequest(`wait must be a duration above 0 and at most 55s, such as "55s"`)
	}
	me, err := s.store.Watcher(r.Context(), c)
	switch {
	case err != nil:
		return 0, 0, waitEnd(err)
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

// look reads t's nudge state and sends what is due through the standing
// wait; of two waits racing on one send, the one RecordNudge refuses reads
// again.
func (s *Server) look(ctx context.Context, c store.Caller, t waitTicket) (watchStep, error) {
	for {
		step, due, err := s.due(ctx, c, t)
		if err != nil || due == nil {
			return step, err
		}
		got, recorded, err := s.store.RecordNudge(ctx, due, store.NudgeByWatcher)
		if err != nil {
			return watchStep{}, err
		}
		if recorded {
			n := nudgeJSON{Seq: got.Seq, Count: got.Count, Attempt: got.Attempt}
			if s.handOff(t, n) {
				return watchStep{}, errReplaced
			}
			return watchStep{nudge: &n}, nil
		}
	}
}

// due is look's read: the nudge a standing wait t is to send or return
// now, or the step to take when there is none. A wait whose identity is
// gone, while its session holds another identity, ends with none: the next
// wait holds for that one.
func (s *Server) due(ctx context.Context, c store.Caller, t waitTicket) (watchStep, *store.Nudge, error) {
	me, err := s.store.Watcher(ctx, c)
	switch {
	case err != nil:
		return watchStep{}, nil, waitEnd(err)
	case me.Ended:
		return watchStep{}, nil, errSessionEnded
	}
	handed, replaced := s.presence.next(t)
	switch {
	case replaced:
		return watchStep{}, nil, errReplaced
	case handed != nil:
		return watchStep{nudge: handed}, nil, nil
	}
	id := t.id
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

// handOff reports whether t was replaced when it sent n, which its
// session's newest wait then returns instead. The send's own commit woke
// that wait before it could take n, so it is woken again to look.
func (s *Server) handOff(t waitTicket, n nudgeJSON) (replaced bool) {
	handed, replaced := s.presence.handOff(t, n)
	if handed {
		s.hub.Woke([]uint64{t.id})
	}
	return replaced
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

// hold blocks until wake or replaced closes (a nil channel never does),
// the clock reaches until, ctx ends or Release is called, which it answers
// with errShuttingDown.
func (s *Server) hold(ctx context.Context, wake, replaced <-chan struct{}, until time.Time) error {
	t := time.NewTimer(until.Sub(s.now()))
	defer t.Stop()
	select {
	case <-wake:
	case <-replaced:
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
