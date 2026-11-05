package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

// headerStatus carries the status line on every identity response.
const headerStatus = "Grelot-Status"

// The wake states a status line names.
const (
	wakePush     = "host push"
	wakeWatcher  = "watcher"
	wakeDown     = "down"
	wakePullOnly = "pull only"
)

// statusLine is the one line the client prints before every answer to c:
// its identity, unread and wake counts, wake state and, under an hour, its
// remaining time; or the revive offer or the register command. It carries
// no peer text. It is empty when the status cannot be read.
func (s *Server) statusLine(ctx context.Context, c store.Caller) string {
	st, err := s.store.Status(ctx, c)
	var se *store.Error
	switch {
	case errors.As(err, &se) && se.Kind == store.KindGone:
		return "grelot: " + se.Msg
	case err != nil:
		return ""
	}
	now := s.now()
	line := fmt.Sprintf("grelot: @%s · %d unread", st.Identity.Name, st.Unread.Total())
	if n := st.Unread.Total(); n > 0 {
		line += fmt.Sprintf(" (%d wake)", st.Unread.Wake())
	}
	line += " · wake: " + s.wakeState(&st, now)
	if left := st.Identity.Expires.Sub(now); left < time.Hour {
		line += fmt.Sprintf(" · expires in %d min", max(1, int(left.Minutes())))
	}
	return line
}

// wakeState is how mail wakes st's identity now.
func (s *Server) wakeState(st *store.Status, now time.Time) string {
	switch {
	case st.Parent != 0:
		return wakePullOnly
	case s.push.State(st.Identity.ID) == pushOK:
		return wakePush
	case !st.Identity.Ended && s.presence.online(st.Identity.ID, now):
		return wakeWatcher
	}
	return wakeDown
}

// setStatus writes c's status line on w, before the body.
func (s *Server) setStatus(w http.ResponseWriter, r *http.Request, c store.Caller) {
	if line := s.statusLine(r.Context(), c); line != "" {
		w.Header().Set(headerStatus, line)
	}
}
