// Command grelot is the grelot server, which coordinates coding agents
// through one HTTP API, its healthcheck, and the client every agent runs.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/cplieger/go/health"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/slogx"
	"github.com/cplieger/grelot/internal/cli"
	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/proctree"
	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/where"
	"github.com/cplieger/grelot/providers"
)

// version is set at build time with -ldflags "-X main.version=<tag>".
var version = "dev"

const listenAddr = ":8080"

// markerAge is the oldest health marker the probe accepts: four sweep
// periods, since every pass that commits refreshes it.
var markerAge = health.Lease{Interval: store.SweepEvery, Cycles: 4}.Duration()

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one subcommand and returns the process exit code: 0 on
// success, 1 on a runtime failure, 2 on a usage error.
func run(args []string, stdout, stderr io.Writer) int {
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
		return clientCommand(args, stdout, stderr)
	}
}

// clientCommand runs one of the client's verbs in this process's
// environment.
func clientCommand(args []string, stdout, stderr io.Writer) int {
	configDir, err := os.UserConfigDir()
	if err != nil {
		fmt.Fprintf(stderr, "grelot cannot find the user config dir: %v; set HOME, or XDG_CONFIG_HOME on Linux\n", err)
		return 1
	}
	set, err := provider.LoadSet(providers.FS, os.DirFS(filepath.Join(client.Dir(configDir), "providers")))
	if err != nil {
		fmt.Fprintf(stderr, "grelot: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return cli.Run(ctx, &cli.Env{
		Stdin: os.Stdin, Stdout: stdout, Stderr: stderr,
		Lookup: os.LookupEnv, Hostname: os.Hostname, Getwd: os.Getwd, Home: os.UserHomeDir,
		Machine: where.Machine, InContainer: where.InContainer,
		Providers: set, Processes: proctree.System(),
		ConfigDir: configDir, Version: version, PID: os.Getpid(),
	}, args)
}

// serve opens the store, sweeps it and serves the API on addr until ctx
// ends, then waits for the sweeper and closes the store. The health marker at markerPath is fresh while the
// sweeper commits.
func serve(ctx context.Context, cfg *server.Config, addr, markerPath string, logger *slog.Logger) error {
	hub := server.NewHub()
	metrics := obs.New()
	st, err := store.Open(cfg.StorePath, &store.Options{
		Logger: logger, Metrics: metrics, MaxBytes: cfg.MaxBytes, ThreadTTL: cfg.ThreadTTL,
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
	srv := server.New(&server.Options{Store: st, Logger: logger, Hub: hub, Metrics: metrics, Version: version, Config: *cfg})
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
	logger.Info("listening", "addr", ln.Addr().String(), "version", version)
	err = httpserver.Run(ctx, srv.HTTPServer(), ln, nil,
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
