//go:build !windows

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/cplieger/grelot/internal/admin"
	"github.com/cplieger/grelot/internal/auth"
	"github.com/cplieger/grelot/internal/server"
)

const keyUsage = "grelot key create <label> | grelot key list | grelot key rotate <label> | grelot key revoke <label>"

// adminCommand sends one command to the running server's admin socket and
// prints its answer: exit 0 on success, 1 on a refusal or no server, 2 on a
// usage error.
func adminCommand(cmd string, args []string, stdout, stderr io.Writer) int {
	req, usage := adminRequest(cmd, args)
	if usage != "" {
		fmt.Fprintf(stderr, "usage: %s\n", usage)
		return 2
	}
	path, err := server.AdminSocket()
	if err != nil {
		fmt.Fprintf(stderr, "grelot %s: %v\n", cmd, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	resp, err := admin.Call(ctx, path, req)
	if err != nil {
		fmt.Fprintf(stderr, "grelot %s: %v\n", cmd, err)
		return 1
	}
	printAnswer(stdout, req, &resp)
	return 0
}

// adminRequest is the request args ask for, or the usage line they break.
func adminRequest(cmd string, args []string) (req admin.Request, usage string) {
	switch cmd {
	case cmdSetupToken:
		if len(args) == 0 {
			return admin.Request{Verb: admin.VerbSetupToken}, ""
		}
		return admin.Request{}, "grelot setup-token"
	case cmdRecovery:
		if len(args) == 1 {
			return admin.Request{Verb: admin.VerbRecovery, Arg: args[0]}, ""
		}
		return admin.Request{}, "grelot recovery <username>"
	}
	switch {
	case len(args) == 2 && args[0] == "create":
		return admin.Request{Verb: admin.VerbKeyCreate, Arg: args[1]}, ""
	case len(args) == 1 && args[0] == "list":
		return admin.Request{Verb: admin.VerbKeyList}, ""
	case len(args) == 2 && args[0] == "revoke":
		return admin.Request{Verb: admin.VerbKeyRevoke, Arg: args[1]}, ""
	case len(args) == 2 && args[0] == "rotate":
		return admin.Request{Verb: admin.VerbKeyRotate, Arg: args[1]}, ""
	}
	return admin.Request{}, keyUsage
}

// printAnswer prints a secret alone on the first line, so a script can
// take it with head -n 1.
func printAnswer(w io.Writer, req admin.Request, resp *admin.Response) {
	expires := resp.Expires.UTC().Format(time.RFC3339)
	switch req.Verb {
	case admin.VerbSetupToken:
		fmt.Fprintf(w, "%s\nOpen %s before %s to create the first account, or enter the token on grelot's setup page.\n", resp.Secret, resp.Link, expires)
	case admin.VerbRecovery:
		fmt.Fprintf(w, "%s\nOpen %s before %s to set a new password or passkey for %s, or enter the token on grelot's recovery page.\n", resp.Secret, resp.Link, expires, req.Arg)
	case admin.VerbKeyCreate:
		fmt.Fprintf(w, "%s\nThis is %s's key, shown only once: put it in that machine's client config as token.\n", resp.Secret, req.Arg)
	case admin.VerbKeyRotate:
		fmt.Fprintf(w, "%s\nThis is %s's new key, shown only once: put it in that machine's client config as token. The old key works until the new one's first use, or for an hour.\n", resp.Secret, req.Arg)
	case admin.VerbKeyList:
		printKeys(w, resp.Keys)
	case admin.VerbKeyRevoke:
		fmt.Fprintf(w, "Revoked the key labelled %s.\n", req.Arg)
	}
}

func printKeys(w io.Writer, keys []admin.Key) {
	if len(keys) == 0 {
		fmt.Fprintln(w, "No keys: grelot key create <label> makes one.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LABEL\tCREATED\tLAST USED")
	for _, k := range keys {
		used := "never"
		if !k.LastUsed.IsZero() {
			used = k.LastUsed.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", k.Label, k.Created.UTC().Format(time.RFC3339), used)
	}
	_ = tw.Flush()
}

// serveAdmin answers the admin socket at path for a until ctx ends or the
// returned stop, which closes the socket and waits for the answer in hand.
func serveAdmin(ctx context.Context, path string, a *auth.Auth, logger *slog.Logger) (stop func(), err error) {
	ln, err := admin.Listen(ctx, path)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := admin.NewServer(a, logger).Serve(ctx, ln); err != nil {
			logger.Error("admin socket stopped", "error", err)
		}
	}()
	return func() { cancel(); <-done }, nil
}
