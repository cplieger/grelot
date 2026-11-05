package server

// pushState is an identity's host push as GET /v1/me reports it.
type pushState string

const (
	// pushNone is an identity no door serves.
	pushNone pushState = "none"
	// pushOK is a served identity whose last send was accepted, or that has
	// had none yet.
	pushOK pushState = "ok"
	// pushFailing is a served identity whose last send is to be retried;
	// its standing waits may send the nudge meanwhile.
	pushFailing pushState = "failing"
	// pushOff is a served identity whose door, or its own push, is off
	// until the server restarts or it registers again.
	pushOff pushState = "off"
)

// Pusher is how the server reaches an identity's host push. Only pushOK
// counts as working: an identity in any other state is woken by its
// standing waits, as one with no door is.
type Pusher interface {
	// State is id's push state now.
	State(id uint64) pushState
}

// noPush serves no identity, so every wake goes through a standing wait.
type noPush struct{}

var _ Pusher = noPush{}

func (noPush) State(uint64) pushState { return pushNone }
