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
	"time"

	"github.com/cplieger/access/gate"
	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/auth"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/webui"
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
	// Events must be the one whose Changed the store's Options.Changed
	// calls; nil gives a stream no commit reaches.
	Events *Events
	// Auth is the web UI's sign-in, whose routes Handler mounts under
	// /api/auth, and the machines' keys. Required.
	Auth *auth.Auth
	// Providers is the providers folder; nil reads Config.ProvidersDir.
	Providers fs.FS
	// UI is the built web UI that every path outside /v1 and /api answers
	// from; nil serves webui.Files.
	UI fs.FS
	// Metrics is the /metrics page the store records into too; nil gives
	// the server a page of its own.
	Metrics *obs.Metrics
	Version string
	Config  Config
	Limits  Limits
}

// Server serves the API over one open store.
type Server struct {
	store    *store.Store
	log      *slog.Logger
	now      func() time.Time
	presence *presence
	posts    *postLimit
	rates    *keyRates
	waits    *waitCaps
	hub      *Hub
	events   *Events
	auth     *auth.Auth
	ui       http.Handler
	started  time.Time
	push     Pusher
	metrics  *obs.Metrics
	health   *health.Status
	sampler  *sampler
	stopping chan struct{}
	// needles are every door secret, cut from logged text.
	needles  []string
	version  string
	cfg      Config
	limits   Limits
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
	events := o.Events
	if events == nil {
		events = NewEvents(o.Logger)
	}
	doors := loadDoors(o, o.Logger, m)
	limits := o.Limits.orDefault()
	ui := o.UI
	if ui == nil {
		ui = webui.Files()
	}
	s := &Server{
		ui:     webui.Handler(ui),
		limits: limits, rates: newKeyRates(limits), waits: newWaitCaps(limits),
		events: events, auth: o.Auth, started: now(),
		store: o.Store, log: o.Logger, now: now, version: o.Version, cfg: o.Config,
		presence: newPresence(), posts: newPostLimit(), hub: hub, push: o.Push, metrics: m,
		health:   health.NewStatus(health.WithLogger(o.Logger)),
		sampler:  newSampler(),
		needles:  doors.Secrets(),
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

// Release turns /healthz unhealthy for good, then answers every held
// standing and in-turn wait at once, and every one opened after it, so a
// graceful stop's drain need not outlast their deadlines. It does not wait
// for them: http.Server.Shutdown does. It ends every /api/events stream,
// waiting for them until ctx ends, and stops host push and waits for it,
// cancelling a send in flight. A second call does nothing.
func (s *Server) Release(ctx context.Context) {
	s.stopOnce.Do(func() {
		// BeginDrain first, so no /healthz answers 200 once a held wait is
		// told the server is stopping.
		s.health.BeginDrain()
		close(s.stopping)
		s.events.shutdown(ctx)
		s.push.stop()
	})
}

// Beat records a sweeper pass that committed, which /healthz reads, takes
// the minute's statistics sample once the minute has turned, and forgets
// the in-memory state of every identity no longer live and of every key
// idle long enough to have its whole burst back.
func (s *Server) Beat(ctx context.Context) {
	s.health.Set(true)
	s.sample(ctx)
	s.posts.prune(s.now())
	s.rates.prune(s.now())
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

// healthyFor is four sweep periods, so one slow pass does not mark the
// server down. reasonStale states it in words: change both together.
const (
	healthyFor     = 4 * store.SweepEvery
	reasonStale    = "the store sweep has not committed for over a minute"
	reasonStopping = "grelot is starting or shutting down"
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

// readTimeout bounds reading a request's headers and body. net/http clears
// the deadline once the body is read (connReader.startBackgroundRead), so
// it never ends a held wait or the event stream.
const readTimeout = waitRouteDeadline + 10*time.Second

// HTTPServer is the http.Server for Handler. It sets no write timeout, so
// a held wait can answer at maxWait.
func (s *Server) HTTPServer() *http.Server {
	srv := httpserver.New(s.Handler(), httpserver.WithSlogErrorLog(slog.LevelWarn))
	srv.ReadTimeout = readTimeout
	return srv
}

// Handler is the API: every /v1 route behind a machine key and the
// version check, access's sign-in routes under /api/auth, which check
// their own credentials, and every other /api route behind a signed-in
// session, all behind one access gate; any other path is the web UI's.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	t := newTable()
	s.routes(t)
	s.webRoutes(t)
	routes := s.auth.Routes()
	for i := range routes {
		t.add(routes[i].Method+" "+routes[i].Path, gate.Public, routes[i].Handler)
	}
	s.mount(mux, t, "/v1/", "/api/")
	mux.Handle("/", s.ui)
	return httpserver.Chain(mux,
		httpserver.Logging(httpserver.WithLogger(s.log), httpserver.WithRecordRouteMetric(s.recordHTTP),
			httpserver.WithClientIP(s.cfg.TrustedProxies...)),
		httpserver.Recoverer(httpserver.WithRecoverLogger(s.log)))
}

// OpsHTTPServer is the http.Server for OpsHandler.
func (s *Server) OpsHTTPServer() *http.Server {
	return httpserver.New(s.OpsHandler(), httpserver.WithSlogErrorLog(slog.LevelWarn))
}

// OpsHandler is the operations listener: /healthz and /metrics, with no
// headers. Its requests count on /metrics but not in the statistics
// sampler, whose requests are the API's.
func (s *Server) OpsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", health.CheckHandler(
		health.Check{Signal: s.health, Reason: reasonStopping},
		health.Check{Signal: s.health.Fresh(healthyFor), Reason: reasonStale},
	))
	mux.Handle("GET /metrics", s.metrics.Handler())
	return httpserver.Chain(mux,
		httpserver.Logging(httpserver.WithLogger(s.log), httpserver.WithProbeLogLevel("/healthz", "/metrics"),
			httpserver.WithRecordRouteMetric(s.metrics.RecordHTTP)),
		httpserver.Recoverer(httpserver.WithRecoverLogger(s.log)))
}

func (s *Server) recordHTTP(rm httpserver.RequestMetric) {
	s.metrics.RecordHTTP(rm)
	s.sampler.request(rm)
}

// routes adds every /v1 route to t as a key's row.
func (s *Server) routes(t *table) {
	handleFor := func(pattern string, h http.Handler, d time.Duration) {
		t.add(pattern, gate.Key, s.machine(httpserver.RouteDeadline(h, d)))
	}
	handle := func(pattern string, h http.Handler) { handleFor(pattern, h, routeDeadline) }
	handle("POST /v1/register", s.keyed(s.register))
	handle("POST /v1/revive", s.keyed(s.revive))
	handle("GET /v1/me", s.identity(s.me))
	handle("PATCH /v1/me", s.renewable(s.patchMe))
	handle("DELETE /v1/me", s.keyed(s.deleteMe))
	handle("GET /v1/identities", s.anyone(s.who))
	handle("GET /v1/version", s.anyone(s.serverVersion))
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
