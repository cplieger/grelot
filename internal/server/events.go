//go:build !windows

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/cplieger/go/sse/v2"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/webui/wire"
)

// The event stream's replay, sized for one user's tabs: a changed frame
// names a few thread ids, so eventReplay of them fit well under one
// sse.MaxFrameBytes, and eventReplayTTL covers a tab back from a short
// sleep. A tab away longer reconciles by refetching what it shows.
const (
	eventReplay    = 512
	eventReplayTTL = 10 * time.Minute
)

const msgChangeDropped = "web change not published"

// Events carries the store's web changes to the open /api/events streams
// as changed events, which a reconnecting tab resumes from its cursor.
type Events struct {
	hub *sse.Hub
	log *slog.Logger
}

// NewEvents returns a stream no tab reads yet; a nil log is slog.Default.
func NewEvents(log *slog.Logger) *Events { return newEvents(log) }

// newEvents applies opts after the production settings.
func newEvents(log *slog.Logger, opts ...sse.Option) *Events {
	if log == nil {
		log = slog.Default()
	}
	opts = append([]sse.Option{
		sse.WithReplay(eventReplay), sse.WithReplayTTL(eventReplayTTL),
		sse.WithReplayMaxBytes(sse.MaxFrameBytes), sse.WithLogger(log),
	}, opts...)
	return &Events{hub: sse.MustNew(opts...), log: log}
}

// Changed publishes c as one changed event, and nothing for an empty c.
// It never blocks, as store.Options.Changed requires.
func (e *Events) Changed(c store.WebChange) {
	if len(c.Conversations) == 0 && !c.Agents {
		return
	}
	data, err := json.Marshal(wire.Changed{Conversations: orEmpty(c.Conversations), Agents: c.Agents})
	if err == nil {
		_, err = e.hub.Publish(sse.Event{Name: wire.EventChanged, Data: data})
	}
	if err != nil {
		e.log.Warn(msgChangeDropped, "error", err.Error())
	}
}

// serve is GET /api/events.
func (e *Events) serve(w http.ResponseWriter, r *http.Request) { e.hub.Serve(w, r) }

// shutdown ends every stream and refuses new ones, waiting for the open
// ones to end until ctx does.
func (e *Events) shutdown(ctx context.Context) error { return e.hub.Shutdown(ctx) }
