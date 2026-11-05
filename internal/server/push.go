package server

// Pusher is how the server reaches an identity's host push. The server
// treats an identity it reports as not working like one with no door at
// all: its standing wait sends every due nudge itself.
type Pusher interface {
	// Working reports whether a door serves id and its push is ok now.
	Working(id uint64) bool
}

// noPush serves no identity, so every wake goes through a standing wait.
type noPush struct{}

var _ Pusher = noPush{}

func (noPush) Working(uint64) bool { return false }
