//go:build !windows

package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os/signal"
	"syscall"

	"github.com/cplieger/go/health"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/slogx"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/store"
)

const listenAddr = ":8080"

// markerAge is the oldest health marker the probe accepts: four sweep
// periods, since every pass that commits refreshes it.
var markerAge = health.Lease{Interval: store.SweepEvery, Cycles: 4}.Duration()

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
		if err := serve(ctx, &cfg, listenAddr, health.DefaultPath, logger); err != nil {
			logger.Error("server stopped", "error", err)
			return 1
		}
		return 0
	case cmdHealth:
		return health.ProbeCheck(health.DefaultPath, health.WithMaxAge(markerAge))
	}
	return 0
}

// serve opens the store, sweeps it, logs the state line and serves the API
// on addr until ctx ends, then waits for the sweeper and the state line and
// closes the store. The health marker at markerPath is fresh while the
// sweeper commits.
func serve(ctx context.Context, cfg *server.Config, addr, markerPath string, logger *slog.Logger) error {
	hub := server.NewHub()
	st, err := store.Open(cfg.StorePath, store.Options{
		Logger: logger, MaxBytes: cfg.MaxBytes, ThreadTTL: cfg.ThreadTTL,
		DormantTTL: cfg.DormantTTL, ReservationMaxTTL: cfg.ReservationMaxTTL, PathRoot: cfg.PathRoot,
		Woke: hub.Woke,
	})
	if err != nil {
		return err
	}
	defer st.Close()
	marker := health.NewMarker(markerPath)
	marker.Set(true)
	defer marker.Cleanup()
	srv := server.New(&server.Options{Store: st, Logger: logger, Hub: hub, Version: version, Config: *cfg})
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	swept := make(chan error, 1)
	go func() {
		swept <- st.Run(ctx, func() {
			marker.Set(true)
			srv.Beat(ctx)
		})
	}()
	stated := make(chan struct{})
	go func() {
		srv.LogState(ctx)
		close(stated)
	}()
	logger.Info("listening", "addr", ln.Addr().String(), "version", version)
	err = httpserver.Run(ctx, srv.HTTPServer(), ln, nil,
		httpserver.WithPreDrain(func(context.Context) { srv.Release() }))
	cancel()
	<-stated
	if serr := <-swept; err == nil {
		err = serr
	}
	if err == nil {
		logger.Info("stopped")
	}
	return err
}
