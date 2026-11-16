//go:build !windows

package server

import (
	"context"
	"time"
)

// pushState is an identity's host push as GET /v1/me reports it.
type pushState string

const (
	// pushNone is an identity no door serves.
	pushNone pushState = "none"
	// pushOK is a served identity with push on and no retry pending.
	pushOK pushState = "ok"
	// pushFailing is a served identity whose last send is to be retried;
	// its standing waits may send the nudge meanwhile.
	pushFailing pushState = "failing"
	// pushOff is a served identity whose door is off until the server
	// restarts, or whose own push is off until it registers again, is
	// revived or the server restarts.
	pushOff pushState = "off"
)

// pushGrace is how long a failing run of sends still counts as the wake, so
// one timeout that its retry gets past never asks for a watcher, and how
// long push must have worked before it takes over from a watcher again.
const pushGrace = 2 * time.Minute

// pushReading is an identity's push state and the instants its runs began.
type pushReading struct {
	// failing is the first failed send of the retry run now open, zero for
	// none. The run ends at an accepted send, and at a retry a read or a
	// standing wait made obsolete, so the next failure gets its own grace.
	failing time.Time
	// healthy is when push last turned healthy: the entry's start or the
	// accepted send that followed a failure.
	healthy time.Time
	state   pushState
	// unproven is a failed send with no accepted send since. An obsoleted
	// retry leaves it set: a door that never accepts must not end the
	// watcher that is the only wake that works.
	unproven bool
}

// wakes reports whether host push counts as the identity's wake at now.
func (r pushReading) wakes(now time.Time) bool {
	return r.state == pushOK || r.state == pushFailing && now.Sub(r.failing) < pushGrace
}

// restoredAt is when push takes over from a watcher whose online stretch
// began at watched: pushGrace after the later of that and healthy. ok is
// false while push is not ok or unproven.
func (r pushReading) restoredAt(watched time.Time) (at time.Time, ok bool) {
	if r.state != pushOK || r.unproven {
		return time.Time{}, false
	}
	if watched.After(r.healthy) {
		return watched.Add(pushGrace), true
	}
	return r.healthy.Add(pushGrace), true
}

// Pusher is how the server reaches an identity's host push. Standing waits
// send an identity's due nudge whenever its state is not pushOK, as for one
// with no door. An implementation calls Hub.Woke for an identity whose
// state leaves pushOK, or a standing wait already holding sends its due
// nudge only at its next wake.
type Pusher interface {
	// Reading is id's push state now.
	Reading(id uint64) pushReading
	// registered maps id, which registered, took over or was revived on
	// provider and host, to its door afresh, clearing its own off.
	registered(id uint64, provider, host string)
	// beat forgets every identity no longer live.
	beat(ctx context.Context)
	// stop ends host push and waits for it, cancelling a send in flight.
	stop()
}

// noPush serves no identity, so every wake goes through a standing wait.
type noPush struct{}

var _ Pusher = noPush{}

func (noPush) Reading(uint64) pushReading { return pushReading{state: pushNone} }

func (noPush) registered(uint64, string, string) {}

func (noPush) beat(context.Context) {}

func (noPush) stop() {}
