//go:build !windows

package server

import "context"

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

// Pusher is how the server reaches an identity's host push. Only pushOK
// counts as working: an identity in any other state is woken by its
// standing waits, as one with no door is. An implementation calls
// Hub.Woke for an identity whose state leaves pushOK, or a standing wait
// already holding sends its due nudge only at its next wake.
type Pusher interface {
	// State is id's push state now.
	State(id uint64) pushState
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

func (noPush) State(uint64) pushState { return pushNone }

func (noPush) registered(uint64, string, string) {}

func (noPush) beat(context.Context) {}

func (noPush) stop() {}
