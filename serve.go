//go:build !windows

package main

import (
	"context"
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
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/store"
	buildver "github.com/cplieger/grelot/internal/version"
)

// serverCommand runs serve or health, the image's two server commands.
func serverCommand(cmd string, stderr io.Writer) int {
	switch cmd {
	case cmdServe:
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		h, _ := slogx.NewHandler(slogx.Options{Output: stderr, Format: slogx.JSON})
		logger := slog.New(h)
		slog.SetDefault(logger)
		cfg, err := server.LoadConfig()
		if err != nil {
			logger.Error("config invalid", "error", err)
			return 1
		}
		if err := serve(ctx, &cfg, server.ListenAddr(), logger); err != nil {
			logger.Error("server stopped", "error", err)
			return 1
		}
		return 0
	case cmdHealth:
		return probeHealth(stderr, server.ListenAddr(), probe.DefaultTimeout)
	}
	return 0
}

// probeHealth is the image's healthcheck: a GET of /healthz at the local URL
// of addr, the listen address, so a wedged listener fails it as well as a
// stalled sweeper. It returns 0 for a 2xx within timeout.
func probeHealth(stderr io.Writer, addr string, timeout time.Duration) int {
	u, err := probe.LocalURL(addr, "/healthz")
	if err != nil {
		fmt.Fprintf(stderr, "unhealthy: LISTEN_ADDR %q: %v\n", addr, err)
		return 1
	}
	return probe.Check(stderr, timeout, u)
}

// build opens the store and builds the server on it, both recording into
// metrics, which gains grelot_build_info. The caller closes the store and
// releases the server.
func build(cfg *server.Config, logger *slog.Logger, metrics *obs.Metrics) (*store.Store, *server.Server, error) {
	metrics.ServeBuildInfo(buildVersion())
	hub := server.NewHub()
	st, err := store.Open(cfg.StorePath, &store.Options{
		Logger: logger, Metrics: metrics, MaxBytes: cfg.MaxBytes, ThreadTTL: cfg.ThreadTTL,
		DormantTTL: cfg.DormantTTL, ReservationMaxTTL: cfg.ReservationMaxTTL, PathRoot: cfg.PathRoot,
		Woke: hub.Woke,
	})
	if err != nil {
		return nil, nil, err
	}
	return st, server.New(&server.Options{
		Store: st, Logger: logger, Hub: hub, Metrics: metrics, Version: buildver.Wire(buildVersion()), Config: *cfg,
	}), nil
}

// serve opens the store, sweeps it and serves the API on addr until ctx
// ends, then waits for the sweeper and closes the store.
func serve(ctx context.Context, cfg *server.Config, addr string, logger *slog.Logger) error {
	st, srv, err := build(cfg, logger, obs.New())
	if err != nil {
		return err
	}
	defer st.Close()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	swept := make(chan error, 1)
	go func() {
		swept <- st.Run(ctx, func() { srv.Beat(ctx) })
	}()
	logger.Info("listening", "addr", ln.Addr().String(), "version", buildVersion())
	err = httpserver.Run(ctx, srv.HTTPServer(), ln,
		httpserver.WithPreDrain(func(context.Context) { srv.Release() }))
	cancel()
	if serr := <-swept; err == nil {
		err = serr
	}
	if err == nil {
		logger.Info("stopped")
	}
	return err
}
