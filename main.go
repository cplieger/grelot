// Command grelot will coordinate coding agents. This release runs the server's
// health endpoint only; the API and the agent client arrive in later releases.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	listenAddr   = ":8080"
	healthURL    = "http://127.0.0.1:8080/healthz"
	probeTimeout = 3 * time.Second
	stopTimeout  = 10 * time.Second
)

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
		logger := slog.New(slog.NewJSONHandler(stderr, nil))
		if err := serve(ctx, listenAddr, logger); err != nil {
			logger.Error("server stopped", "error", err)
			return 1
		}
		return 0
	case "health":
		if err := probe(healthURL, probeTimeout); err != nil {
			fmt.Fprintf(stderr, "grelot health: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "grelot: unknown command %q; want serve or health\n", cmd)
		return 2
	}
}

// newMux serves GET /healthz, which answers 200 while the process serves.
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	return mux
}

// serve listens on addr until ctx ends, then drains for at most stopTimeout.
func serve(ctx context.Context, addr string, logger *slog.Logger) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: newMux(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	logger.Info("listening", "addr", ln.Addr().String())
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("stopped")
	return nil
}

// probe returns nil when url answers 200 within timeout.
func probe(url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: timeout + time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
