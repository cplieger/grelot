//go:build !windows

// Package server is grelot's HTTP API: the routes of every client verb and
// hook, the standing and in-turn waits, and the in-memory state behind
// them (presence, open turns, push state). It does not build on Windows,
// where its store does not run.
package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/go/health"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/store"
)

// Options are what New needs.
type Options struct {
	Store  *store.Store
	Logger *slog.Logger
	Now    func() time.Time
	// Push defaults to host push through the doors of the providers folder,
	// started here and stopped by Release, or to none when it has none.
	Push Pusher
	// Hub must be the one whose Woke the store's Options.Woke calls; nil
	// gives a hub no commit reaches, whose waits wake only at their timeout.
	Hub *Hub
	// Providers is the providers folder; nil reads Config.ProvidersDir.
	Providers fs.FS
	// Metrics is the /metrics page the store records into too; nil gives
	// the server a page of its own.
	Metrics *obs.Metrics
	Version string
	Config  Config
}

// Server serves the API over one open store.
type Server struct {
	store    *store.Store
	log      *slog.Logger
	now      func() time.Time
	presence *presence
	posts    *postLimit
	hub      *Hub
	push     Pusher
	metrics  *obs.Metrics
	stopping chan struct{}
	// needles are the bearer and every door secret, cut from logged text.
	needles  []string
	version  string
	cfg      Config
	beat     atomic.Int64
	bearer   httpserver.StaticTokenVerifier
	stopOnce sync.Once
}

// New builds the server. Options.Now defaults to time.Now and must be the
// store's clock.
func New(o *Options) *Server {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	hub := o.Hub
	if hub == nil {
		hub = NewHub()
	}
	m := o.Metrics
	if m == nil {
		m = obs.New()
	}
	doors := loadDoors(o, o.Logger, m)
	s := &Server{
		store: o.Store, log: o.Logger, now: now, version: o.Version, cfg: o.Config,
		presence: newPresence(), posts: newPostLimit(), hub: hub, push: o.Push, metrics: m,
		needles:  append([]string{o.Config.Token}, doors.Secrets()...),
		bearer:   httpserver.NewStaticTokenVerifier(o.Config.Token),
		stopping: make(chan struct{}),
	}
	m.ServeState(s.state, now)
	switch {
	case s.push != nil:
	case len(doors.All()) > 0:
		s.push = startHostPush(s, doors)
	default:
		s.push = noPush{}
	}
	return s
}

// Release answers every held standing and in-turn wait at once, and every
// one opened after it, so a graceful stop's drain need not outlast their
// deadlines. It does not wait for them: http.Server.Shutdown does. It
// stops host push and waits for it, cancelling a send in flight.
func (s *Server) Release() {
	s.stopOnce.Do(func() {
		close(s.stopping)
		s.push.stop()
	})
}

// Beat records a sweeper pass that committed, which Healthy reads, and
// forgets the in-memory state of every identity no longer live.
func (s *Server) Beat(ctx context.Context) {
	s.beat.Store(s.now().UnixNano())
	s.posts.prune(s.now())
	s.push.beat(ctx)
	ids := s.presence.ids()
	live, err := s.store.Live(ctx, ids)
	if err != nil {
		s.log.Warn(msgPruneFailed, "error", err.Error())
		return
	}
	s.presence.forget(slices.DeleteFunc(ids, func(id uint64) bool { return slices.Contains(live, id) }))
}

const msgPruneFailed = "memory prune failed"

// Healthy is the /healthz check: false before the first committed sweeper
// pass and once the last is older than healthyFor.
func (s *Server) Healthy() bool {
	return s.now().Sub(time.Unix(0, s.beat.Load())) <= healthyFor
}

// healthyFor is four sweep periods, so one slow pass does not mark the
// server down. reasonStale states it in words: change both together.
const (
	healthyFor  = 4 * store.SweepEvery
	reasonStale = "the store sweep has not committed for over a minute"
)

// Route deadlines, which end a request's store calls before they commit,
// never after (fail answers a passed one with errTimeout).
const (
	// routeDeadline sits below the client's 15 s call budget, so the
	// server's answer, success or timeout, reaches the client before it
	// gives up.
	routeDeadline = 10 * time.Second
	// waitRouteDeadline sits above maxWait, so a held wait answers itself
	// and the deadline only ends one whose store calls stall.
	waitRouteDeadline = maxWait + 10*time.Second
	// maxWait is the longest a standing or in-turn wait holds.
	maxWait = 55 * time.Second
)

// readTimeout bounds a slow request body. net/http keeps this deadline on
// the connection while the handler runs and cancels the request's context
// when its background read hits it (net/http connReader.backgroundRead),
// so it must outlast the longest route, a held wait.
const readTimeout = waitRouteDeadline + 10*time.Second

// HTTPServer is the http.Server for Handler. It sets no write timeout, so
// a held wait can answer at maxWait.
func (s *Server) HTTPServer() *http.Server {
	return httpserver.New(s.Handler(), httpserver.WithReadTimeout(readTimeout),
		httpserver.WithSlogErrorLog(slog.LevelWarn))
}

// Handler is the whole API: /healthz and /metrics with no headers, and
// every /v1 route behind the bearer and the version check.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", health.CheckHandler(health.Check{Signal: s, Reason: reasonStale}))
	mux.Handle("GET /metrics", s.metrics.Handler())
	s.routes(mux)
	return httpserver.Chain(mux,
		httpserver.Logging(httpserver.WithLogger(s.log), httpserver.ProbeLogLevel("/healthz", "/metrics"),
			httpserver.WithRecordRouteMetric(s.metrics.RecordHTTP)),
		httpserver.Recoverer(httpserver.WithRecoverLogger(s.log)))
}

// routes registers every /v1 route on the one mux the access log reads
// its route label from: a nested mux would label every route /v1/.
func (s *Server) routes(mux *http.ServeMux) {
	handleFor := func(pattern string, h http.Handler, d time.Duration) {
		mux.Handle(pattern, s.authorize(httpserver.RouteDeadline(h, d)))
	}
	handle := func(pattern string, h http.Handler) { handleFor(pattern, h, routeDeadline) }
	handle("POST /v1/register", s.keyed(s.register))
	handle("POST /v1/revive", s.keyed(s.revive))
	handle("GET /v1/me", s.identity(s.me))
	handle("PATCH /v1/me", s.renewable(s.patchMe))
	handle("DELETE /v1/me", s.keyed(s.deleteMe))
	handle("GET /v1/identities", s.anyone(s.who))
	handle("POST /v1/posts", s.keyed(s.post))
	handle("GET /v1/threads", s.identity(s.threads))
	handle("GET /v1/messages/{id}/receipts", s.identity(s.receipts))
	handle("GET /v1/history", s.identity(s.history))
	handle("PUT /v1/follows", s.keyed(s.follow))
	handle("DELETE /v1/follows", s.keyed(s.unfollow))
	handle("POST /v1/inbox/ack", s.keyed(s.ack))
	handle("POST /v1/reservations", s.keyed(s.reserve))
	handle("GET /v1/reservations", s.identity(s.reservations))
	handle("PATCH /v1/reservations/{id}", s.keyed(s.renewReservation))
	handle("DELETE /v1/reservations/{id}", s.keyed(s.release))
	handle("POST /v1/delegates", s.keyed(s.grant))
	handle("DELETE /v1/delegates/{label}", s.keyed(s.revoke))
	handle("POST /v1/lifecycle", s.identity(s.lifecycle))
	handle("POST /v1/events/hold", s.identity(s.eventHold))
	handleFor("POST /v1/events/wait", s.identity(s.standingWait), waitRouteDeadline)
	handleFor("GET /v1/inbox", s.identity(s.inbox), waitRouteDeadline)
	mux.Handle("/v1/", s.authorize(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, r, &apiError{status: http.StatusNotFound, code: "not_found", msg: "no such route: " + r.Method + " " + r.URL.Path})
	})))
}

// state is the census the /metrics gauges read; false when the store
// cannot count, which leaves them off that scrape.
func (s *Server) state(ctx context.Context) (obs.State, bool) {
	c, err := s.store.Census(ctx, s.onlineAt(s.now()))
	if err != nil {
		return obs.State{}, false
	}
	return obs.State{
		Oldest: c.Oldest, Live: c.Live, Dormant: c.Dormant, Online: c.Online,
		Announce: c.Threads.Announce, Path: c.Threads.Path, Direct: c.Threads.Direct, Cards: c.Threads.Cards,
		Held: c.Held, Queued: c.Queued, Unread: c.Unread,
		FileBytes: c.Space.FileBytes, FreeBytes: c.Space.FreeBytes, InUse: c.Space.InUseBytes, MaxBytes: c.MaxBytes,
	}, true
}
