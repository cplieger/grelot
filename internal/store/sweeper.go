//go:build !windows

package store

import (
	"context"
	"time"
)

// SweepEvery is how often Run sweeps. A health probe's maximum age is a
// multiple of it.
const SweepEvery = 15 * time.Second

const msgSweepFailed = "sweep failed"

// Run sweeps at once and then every SweepEvery until ctx is done, and
// returns nil then. After each pass whose last transaction committed it
// calls beat, which must not be nil, so a wedged store or a stopped
// sweeper leaves the beat stale. A failed pass logs an ERROR and calls no
// beat; the next tick tries again.
func (s *Store) Run(ctx context.Context, beat func()) error {
	tick := time.NewTicker(SweepEvery)
	defer tick.Stop()
	for {
		if _, err := s.Sweep(ctx); err == nil {
			beat()
		} else if ctx.Err() == nil {
			s.log.Error(msgSweepFailed, "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
