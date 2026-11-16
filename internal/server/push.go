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
	// failing is the first failed send with no accepted send since, zero
	// for none. A failing run outlives a retry a read or a standing wait
	// ended, since nothing reached the door since.
	failing time.Time
	// healthy is when push last turned healthy: the entry's start or the
	// accepted send that ended a failing run.
	healthy time.Time
	state   pushState
}

// wakes reports whether host push counts as the identity's wake at now:
// push is on and no failing run began pushGrace or more ago.
func (r pushReading) wakes(now time.Time) bool {
	switch {
	case r.state != pushOK && r.state != pushFailing:
		return false
	case r.failing.IsZero():
		return true
	}
	return now.Sub(r.failing) < pushGrace
}

// restoredAt is when push takes over from a watcher whose online stretch
// began at watched: pushGrace after the later of that and healthy. ok is
// false while push is not ok or a failing run is open.
func (r pushReading) restoredAt(watched time.Time) (at time.Time, ok bool) {
	if r.state != pushOK || !r.failing.IsZero() {
		return time.Time{}, false
	}
	if watched.After(r.healthy) {
		return watched.Add(pushGrace), true
	}
	return r.healthy.Add(pushGrace), true
}

// Pusher is how the server reaches an identity's host push. Only pushOK
// sends: an identity in any other state is woken by its standing waits,
// as one with no door is. An implementation calls Hub.Woke for an
// identity whose state leaves pushOK, or a standing wait already holding
// sends its due nudge only at its next wake.
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
