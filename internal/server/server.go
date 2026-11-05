// Package server is grelot's HTTP API: the routes of every client verb and
// hook, the standing and in-turn waits, and the in-memory state behind
// them (presence, open turns, push state).
package server

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/store"
)

// Options are what New needs.
type Options struct {
	Store  *store.Store
	Logger *slog.Logger
	Now    func() time.Time
	// Push defaults to one that serves no identity.
	Push Pusher
	// Hub must be the one whose Woke the store's Options.Woke calls; nil
	// gives a hub no commit reaches, whose waits wake only at their timeout.
	Hub     *Hub
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
	version  string
	cfg      Config
	beat     atomic.Int64
	bearer   httpserver.StaticTokenVerifier
}

// New builds the server. Options.Now defaults to time.Now and must be the
// store's clock.
func New(o *Options) *Server {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	push := o.Push
	if push == nil {
		push = noPush{}
	}
	hub := o.Hub
	if hub == nil {
		hub = NewHub()
	}
	return &Server{
		store: o.Store, log: o.Logger, now: now, version: o.Version, cfg: o.Config,
		presence: newPresence(), posts: newPostLimit(), hub: hub, push: push,
		bearer: httpserver.NewStaticTokenVerifier(o.Config.Token),
	}
}

// Beat records a sweeper pass that committed, which /healthz answers down
// once the last one is older than healthyFor, and forgets the in-memory
// state of every identity no longer live.
func (s *Server) Beat(ctx context.Context) {
	s.beat.Store(s.now().UnixNano())
	s.posts.prune(s.now())
	ids := s.presence.ids()
	live, err := s.store.Live(ctx, ids)
	if err != nil {
		s.log.Warn(msgPruneFailed, "error", err.Error())
		return
	}
	s.presence.forget(slices.DeleteFunc(ids, func(id uint64) bool { return slices.Contains(live, id) }))
}

const msgPruneFailed = "memory prune failed"

// healthyFor is four sweep periods, the same age the image's file probe
// allows.
const healthyFor = 4 * store.SweepEvery

// Route timeouts. waitRouteTimeout sits above maxWait, so a held wait
// answers itself before the timeout cuts it.
const (
	routeTimeout     = 15 * time.Second
	waitRouteTimeout = maxWait + 10*time.Second
	// maxWait is the longest a standing or in-turn wait holds.
	maxWait = 55 * time.Second
)

// readTimeout bounds a slow request body. net/http keeps this deadline on
// the connection while the handler runs and cancels the request's context
// when its background read hits it (net/http connReader.backgroundRead),
// so it must outlast the longest route, a held wait.
const readTimeout = waitRouteTimeout + 10*time.Second

// HTTPServer is the http.Server for Handler. It sets no write timeout, so
// a held wait can answer at maxWait.
func (s *Server) HTTPServer() *http.Server {
	return httpserver.New(s.Handler(), httpserver.WithReadTimeout(readTimeout),
		httpserver.WithSlogErrorLog(slog.LevelWarn))
}

// Handler is the whole API: /healthz with no headers, and every /v1 route
// behind the bearer and the version check.
func (s *Server) Handler() http.Handler {
	v1 := http.NewServeMux()
	s.routes(v1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("/v1/", s.authorize(v1))
	return httpserver.Chain(mux,
		httpserver.Logging(httpserver.WithLogger(s.log), httpserver.ProbeLogLevel("/healthz")),
		httpserver.Recoverer(httpserver.WithRecoverLogger(s.log)))
}

func (s *Server) routes(mux *http.ServeMux) {
	handle := func(pattern string, h http.Handler) {
		mux.Handle(pattern, httpserver.RouteTimeout(h, routeTimeout, ""))
	}
	handle("POST /v1/register", s.identity(s.register))
	handle("POST /v1/revive", s.identity(s.revive))
	handle("GET /v1/me", s.identityAgent(s.me))
	handle("PATCH /v1/me", s.identity(s.patchMe))
	handle("DELETE /v1/me", s.identity(s.deleteMe))
	handle("GET /v1/identities", s.anyone(s.who))
	handle("POST /v1/posts", s.identity(s.post))
	handle("GET /v1/threads", s.identity(s.threads))
	handle("GET /v1/messages/{id}/receipts", s.identity(s.receipts))
	handle("GET /v1/history", s.identity(s.history))
	handle("PUT /v1/follows", s.identity(s.follow))
	handle("DELETE /v1/follows", s.identity(s.unfollow))
	handle("GET /v1/inbox", s.identity(s.inbox))
	handle("POST /v1/inbox/ack", s.identity(s.ack))
	handle("POST /v1/reservations", s.identity(s.reserve))
	handle("GET /v1/reservations", s.identity(s.reservations))
	handle("PATCH /v1/reservations/{id}", s.identity(s.renewReservation))
	handle("DELETE /v1/reservations/{id}", s.identity(s.release))
	handle("POST /v1/delegates", s.identity(s.grant))
	handle("DELETE /v1/delegates/{label}", s.identity(s.revoke))
	handle("POST /v1/me/bind", s.identity(s.bind))
	handle("POST /v1/lifecycle", s.identity(s.lifecycle))
	mux.Handle("POST /v1/events/wait", httpserver.RouteTimeout(s.identity(s.standingWait), waitRouteTimeout, ""))
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, r, &apiError{status: http.StatusNotFound, code: "not_found", msg: "no such route: " + r.Method + " " + r.URL.Path})
	})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	if s.now().Sub(time.Unix(0, s.beat.Load())) > healthyFor {
		httpserver.WriteJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "down"})
		return
	}
	httpserver.WriteJSONStatus(w, http.StatusOK, map[string]string{"status": "up"})
}
