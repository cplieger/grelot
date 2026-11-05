// Command grelot is the grelot server, which coordinates coding agents
// through one HTTP API, its healthcheck and its provider-file check.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/cplieger/go/health"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/slogx"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/store"
)

// version is set at build time with -ldflags "-X main.version=<tag>".
var version = "dev"

const listenAddr = ":8080"

// markerAge is the oldest health marker the probe accepts: four sweep
// periods, since every pass that commits refreshes it.
var markerAge = health.Lease{Interval: store.SweepEvery, Cycles: 4}.Duration()

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run dispatches one subcommand and returns the process exit code: 0 on
// success, 1 on a runtime failure, 2 on a usage error.
func run(args []string, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
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
	case "health":
		return health.ProbeCheck(health.DefaultPath, health.WithMaxAge(markerAge))
	case "provider":
		return providerCommand(args[1:], stderr)
	default:
		fmt.Fprintf(stderr, "grelot: unknown command %q; want serve, health or provider check\n", cmd)
		return 2
	}
}

// serve opens the store, sweeps it and serves the API on addr until ctx
// ends, then waits for the sweeper and closes the store. The health marker
// at markerPath is fresh while the sweeper commits.
func serve(ctx context.Context, cfg *server.Config, addr, markerPath string, logger *slog.Logger) error {
	st, err := store.Open(cfg.StorePath, store.Options{
		Logger: logger, MaxBytes: cfg.MaxBytes, ThreadTTL: cfg.ThreadTTL,
		DormantTTL: cfg.DormantTTL, ReservationMaxTTL: cfg.ReservationMaxTTL, PathRoot: cfg.PathRoot,
	})
	if err != nil {
		return err
	}
	defer st.Close()
	marker := health.NewMarker(markerPath)
	marker.Set(true)
	defer marker.Cleanup()
	srv := server.New(&server.Options{Store: st, Logger: logger, Version: version, Config: *cfg})
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
			srv.Beat()
		})
	}()
	logger.Info("listening", "addr", ln.Addr().String(), "version", version)
	err = httpserver.Run(ctx, srv.HTTPServer(), ln, nil)
	cancel()
	if serr := <-swept; err == nil {
		err = serr
	}
	if err == nil {
		logger.Info("stopped")
	}
	return err
}
