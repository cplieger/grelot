package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cplieger/grelot/internal/client"
)

// versionTimeout bounds grelot version's one call, so a server that is
// down costs a person a short wait for the client's own line.
const versionTimeout = 5 * time.Second

// version prints this client's build, then the server's version or why it
// is unknown, then the line naming the client to install when the status
// line's skew rule names one. It exits 0 once the client's line is out:
// the server's part is informational.
func (r *run) version(ctx context.Context, args []string) error {
	rest, err := parse(flags(r.verb), args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usagef(r.verb, "grelot version takes no arguments")
	}
	fmt.Fprintln(r.Stdout, "grelot client "+safe(r.Build))
	server, err := r.serverVersion(ctx)
	if err != nil {
		fmt.Fprintln(r.Stdout, "grelot server unknown: "+strings.TrimPrefix(safe(err.Error()), "grelot: "))
		return nil
	}
	fmt.Fprintln(r.Stdout, "grelot server "+server)
	if line := (&client.Status{Client: r.Version, ServerVersion: server}).Line(); line != "" {
		fmt.Fprintln(r.Stdout, line)
	}
	return nil
}

// serverVersion is the version the server names in its answer to GET
// /v1/version, sent as no session. A server older than the route names it
// on its 404, and one that refuses this client on its 426.
func (r *run) serverVersion(ctx context.Context) (string, error) {
	cfg, err := r.config()
	if err != nil {
		return "", err
	}
	a, err := client.NewOnceAPI(cfg, r.Version, r.ConfigDir)
	if err != nil {
		return "", err
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = versionTimeout
	}
	resp, err := a.Do(ctx, &client.Request{Method: http.MethodGet, Path: "/v1/version", Timeout: timeout}, nil)
	switch {
	case resp.Version != "":
		return resp.Version, nil
	case err != nil:
		return "", err
	}
	return "", errors.New("the server's answer names no version")
}
