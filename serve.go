//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/signal"
	"syscall"
	"time"

	"github.com/cplieger/go/health/v2/probe"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/slogx"
	"github.com/cplieger/grelot/internal/accounts"
	"github.com/cplieger/grelot/internal/auth"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/store"
	buildver "github.com/cplieger/grelot/internal/version"
)

// serverCommand runs serve, health or one of the admin socket's commands.
func serverCommand(cmd string, args []string, stdout, stderr io.Writer) int {
	switch cmd {
	case cmdServe:
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		h, _ := slogx.NewHandler(slogx.Options{Output: stderr, Format: slogx.JSON})
		logger := slog.New(h)
		slog.SetDefault(logger)
		cfg, err := server.LoadConfig()
		sock, sockErr := server.AdminSocket()
		addrs := listeners{api: server.ListenAddr(), ops: server.OpsListenAddr(), admin: sock}
		if err = errors.Join(err, sockErr, server.CheckListenAddrs(addrs.api, addrs.ops)); err != nil {
			logger.Error("config invalid", "error", err)
			return 1
		}
		if err := serve(ctx, &cfg, addrs, logger); err != nil {
			logger.Error("server stopped", "error", err)
			return 1
		}
		return 0
	case cmdHealth:
		return probeHealth(stderr, server.OpsListenAddr(), probe.DefaultTimeout)
	}
	return adminCommand(cmd, args, stdout, stderr)
}

// listeners are where serve listens.
type listeners struct {
	// api serves /v1 and /api.
	api string
	// ops serves /healthz and /metrics.
	ops string
	// admin is the admin socket's path.
	admin string
}

// probeHealth is the image's healthcheck: a GET of /healthz at the local URL
// of addr, the operations address, so a wedged listener fails it as well as
// a stalled sweeper. It returns 0 for a 2xx within timeout.
func probeHealth(stderr io.Writer, addr string, timeout time.Duration) int {
	u, err := probe.LocalURL(addr, "/healthz")
	if err != nil {
		fmt.Fprintf(stderr, "unhealthy: OPS_LISTEN_ADDR %q: %v\n", addr, err)
		return 1
	}
	return probe.Check(stderr, timeout, u)
}

// build opens the store and builds the server on it with sign-in au, both
// recording into metrics, which gains grelot_build_info. Each store write
// reads its history retention from retention, nil being
// store.DefaultRetention. The caller closes the store and releases the
// server.
func build(cfg *server.Config, retention func() time.Duration, au *auth.Auth, logger *slog.Logger, metrics *obs.Metrics) (*store.Store, *server.Server, error) {
	metrics.ServeBuildInfo(buildVersion())
	hub, events := server.NewHub(), server.NewEvents(logger)
	st, err := store.Open(cfg.StorePath, &store.Options{
		Logger: logger, Metrics: metrics, MaxBytes: cfg.MaxBytes, Retention: retention,
		DormantTTL: cfg.DormantTTL, ReservationMaxTTL: cfg.ReservationMaxTTL, PathRoot: cfg.PathRoot,
		Woke: hub.Woke, Changed: events.Changed, KeyLive: au.KeyLive,
	})
	if err != nil {
		return nil, nil, err
	}
	return st, server.New(&server.Options{
		Store: st, Logger: logger, Hub: hub, Events: events, Metrics: metrics, Auth: au,
		Version: buildver.Wire(buildVersion()), Config: *cfg,
	}), nil
}

// serve opens the accounts file, its account service and the store, sweeps
// the store and serves on addrs until ctx ends, then waits for the sweeper
// and the admin socket and closes them all. It logs a WARN for each retired
// setting the environment still sets.
func serve(ctx context.Context, cfg *server.Config, addrs listeners, logger *slog.Logger) error {
	for _, line := range server.RetiredSettings() {
		logger.Warn(line)
	}
	acc, err := accounts.Open(cfg.AccountsPath)
	if err != nil {
		return err
	}
	defer acc.Close()
	metrics := obs.New()
	au, err := auth.New(ctx, acc, &auth.Options{
		Logger: logger, PublicURL: cfg.PublicURL, Errors: server.AuthErrors(logger, metrics),
		OIDC: cfg.OIDC, TrustedProxies: cfg.TrustedProxies,
	})
	if err != nil {
		return err
	}
	defer closeAuth(ctx, au, logger)
	stopAdmin, err := serveAdmin(ctx, addrs.admin, au, logger)
	if err != nil {
		return err
	}
	defer stopAdmin()
	st, srv, err := build(cfg, acc.Retention, au, logger, metrics)
	if err != nil {
		return err
	}
	defer st.Close()
	lc := &net.ListenConfig{}
	api, err := lc.Listen(ctx, "tcp", addrs.api)
	if err != nil {
		return err
	}
	ops, err := lc.Listen(ctx, "tcp", addrs.ops)
	if err != nil {
		_ = api.Close()
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	swept := make(chan error, 1)
	go func() {
		swept <- st.Run(ctx, func() { srv.Beat(ctx) })
	}()
	logger.Info("listening", "addr", api.Addr().String(), "ops_addr", ops.Addr().String(), "admin_socket", addrs.admin, "version", buildVersion())
	err = runListeners(ctx, cancel, srv, api, ops)
	cancel()
	if serr := <-swept; err == nil {
		err = serr
	}
	if err == nil {
		logger.Info("stopped")
	}
	return err
}

// authStopTimeout bounds the wait for the account service's sweep at stop.
const authStopTimeout = 5 * time.Second

func closeAuth(ctx context.Context, a *auth.Auth, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), authStopTimeout)
	defer cancel()
	if err := a.Close(ctx); err != nil {
		logger.Warn("account service did not stop cleanly", "error", err)
	}
}

// runListeners serves the API on api and the operations routes on ops
// until ctx ends or either listener fails. An operations failure calls
// stop, so the API drains; an API failure releases the server and returns.
// The operations listener stops only once the API's Run has returned, so
// /healthz answers 503 through the drain instead of refusing connections.
func runListeners(ctx context.Context, stop context.CancelFunc, srv *server.Server, api, ops net.Listener) error {
	opsCtx, stopOps := context.WithCancel(context.WithoutCancel(ctx))
	defer stopOps()
	opsDone := make(chan error, 1)
	go func() {
		opsDone <- httpserver.Run(opsCtx, srv.OpsHTTPServer(), ops,
			httpserver.WithServeExit(func(context.Context) { stop() }))
	}()
	err := httpserver.Run(ctx, srv.HTTPServer(), api, httpserver.WithPreDrain(srv.Release), httpserver.WithServeExit(srv.Release))
	stopOps()
	return errors.Join(err, <-opsDone)
}
