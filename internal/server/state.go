package server

import (
	"context"
	"time"
)

const (
	msgState       = "state"
	msgStateFailed = "state count failed"
	stateEvery     = time.Minute
)

// LogState logs the state line at once and then every stateEvery until
// ctx ends; the call waits until then. A count that fails logs a WARN in
// that line's place.
func (s *Server) LogState(ctx context.Context) {
	tick := time.NewTicker(stateEvery)
	defer tick.Stop()
	for {
		s.logState(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Server) logState(ctx context.Context) {
	now := s.now()
	c, err := s.store.Census(ctx, s.onlineAt(now))
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn(msgStateFailed, "error", err.Error())
		}
		return
	}
	var oldest int64
	if !c.Oldest.IsZero() {
		oldest = now.Sub(c.Oldest).Milliseconds()
	}
	s.log.Info(msgState,
		"live", c.Live, "dormant", c.Dormant, "online", c.Online,
		"threads_announce", c.Threads.Announce, "threads_path", c.Threads.Path,
		"threads_direct", c.Threads.Direct, "threads_cards", c.Threads.Cards,
		"reservations", c.Held, "queued", c.Queued,
		"unread", c.Unread, "oldest_unread_ms", oldest,
		"file_bytes", c.Space.FileBytes, "free_bytes", c.Space.FreeBytes,
		"in_use_bytes", c.Space.InUseBytes, "max_bytes", c.MaxBytes)
}
