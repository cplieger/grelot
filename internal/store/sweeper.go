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
// records the pass's time and calls beat, which must not be nil, so a
// wedged store or a stopped sweeper leaves both stale. A failed pass logs
// an ERROR, is counted and calls no beat; the next tick tries again.
func (s *Store) Run(ctx context.Context, beat func()) error {
	tick := time.NewTicker(SweepEvery)
	defer tick.Stop()
	for {
		if _, err := s.Sweep(ctx); err == nil {
			s.m.SweepLastSuccess.Set(float64(s.now().UnixNano()) / 1e9)
			beat()
		} else if ctx.Err() == nil {
			s.log.Error(msgSweepFailed, "error", err.Error())
			s.m.SweepFailures.Inc()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
