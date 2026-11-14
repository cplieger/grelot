//go:build !windows

// Package admin carries the operator's commands to the running server over
// a Unix socket that only the server's own user may use: one JSON request
// per connection, answered with one JSON response. bbolt locks the accounts
// file, so a second process cannot open it; the socket reaches the process
// that holds it.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/cplieger/grelot/internal/auth"
)

// The verbs a Request names.
const (
	VerbSetupToken = "setup-token"
	VerbRecovery   = "recovery"
	VerbKeyCreate  = "key-create"
	VerbKeyList    = "key-list"
	VerbKeyRevoke  = "key-revoke"
	VerbKeyRotate  = "key-rotate"
)

// Request is one command: its verb and the username or label it names.
type Request struct {
	Verb string `json:"verb"`
	Arg  string `json:"arg,omitempty"`
}

// Response is a command's answer: Error set on a refusal, else the fields
// its verb fills.
type Response struct {
	Expires time.Time `json:"expires,omitzero"`
	Error   string    `json:"error,omitempty"`
	Secret  string    `json:"secret,omitempty"`
	// Link is the page that takes Secret, with Secret in its fragment.
	Link string `json:"link,omitempty"`
	Keys []Key  `json:"keys,omitempty"`
}

// Key is one row of the key list.
type Key struct {
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitzero"`
	Label    string    `json:"label"`
}

const (
	maxRequest  = 4 << 10
	maxResponse = 1 << 20
	// connTimeout bounds one connection, so a stalled client holds the
	// one-at-a-time socket no longer.
	connTimeout = 10 * time.Second
	dialTimeout = 2 * time.Second
)

// Listen listens on path with mode 0600. A socket there that nothing
// answers, left by a server that died, is replaced; a live one, or a file
// that is not a socket, is refused and left alone. Closing the listener
// removes the socket.
func Listen(ctx context.Context, path string) (*net.UnixListener, error) {
	if err := clearStale(ctx, path); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func clearStale(ctx context.Context, path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.Mode().Type() != fs.ModeSocket:
		return fmt.Errorf("admin socket %s: a file that is not a socket is there; move it away or pick another path", path)
	}
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "unix", path)
	switch {
	case err == nil:
		_ = conn.Close()
		return fmt.Errorf("admin socket %s: another running grelot server answers there", path)
	case errors.Is(err, syscall.ECONNREFUSED):
		return os.Remove(path)
	}
	return fmt.Errorf("admin socket %s: %w", path, err)
}

// Server answers the commands on a's accounts.
type Server struct {
	auth   *auth.Auth
	logger *slog.Logger
	uid    int
}

// NewServer returns a server for a that answers only this process's user.
func NewServer(a *auth.Auth, logger *slog.Logger) *Server {
	return &Server{auth: a, logger: logger, uid: os.Getuid()}
}

// Serve answers ln's connections one at a time until ctx ends, then closes
// ln, which removes the socket, and returns once the connection in hand is
// answered. It logs each command's verb and argument, never its secret.
func (s *Server) Serve(ctx context.Context, ln *net.UnixListener) error {
	defer ln.Close()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn *net.UnixConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(connTimeout))
	_ = json.NewEncoder(conn).Encode(s.answer(ctx, conn)) //nolint:gosec // G117: the minted secret goes to the operator who asked, as intended
}

func (s *Server) answer(ctx context.Context, conn *net.UnixConn) Response {
	if uid, err := peerUID(conn); err != nil || uid != s.uid {
		s.logger.Warn("admin connection refused", "peer_uid", uid, "error", err)
		return Response{Error: "the admin socket answers only the grelot server's own user"}
	}
	var req Request
	if err := json.NewDecoder(io.LimitReader(conn, maxRequest)).Decode(&req); err != nil {
		return Response{Error: "the admin socket could not read the request: " + err.Error()}
	}
	resp, err := s.run(ctx, req)
	if err != nil {
		s.logger.Info("admin command refused", "verb", req.Verb, "arg", req.Arg, "error", err)
		return Response{Error: err.Error()}
	}
	s.logger.Info("admin command", "verb", req.Verb, "arg", req.Arg)
	return resp
}

func (s *Server) run(ctx context.Context, req Request) (Response, error) {
	switch req.Verb {
	case VerbSetupToken:
		return token(s.auth.SetupToken(ctx))
	case VerbRecovery:
		return token(s.auth.Recovery(ctx, req.Arg))
	case VerbKeyCreate:
		key, err := s.auth.CreateKey(ctx, req.Arg)
		return Response{Secret: key}, err
	case VerbKeyList:
		keys, err := s.auth.Keys(ctx)
		resp := Response{Keys: make([]Key, len(keys))}
		for i, k := range keys {
			resp.Keys[i] = Key{Label: k.Label, Created: k.Created, LastUsed: k.LastUsed}
		}
		return resp, err
	case VerbKeyRevoke:
		return Response{}, s.auth.RevokeKey(ctx, req.Arg)
	case VerbKeyRotate:
		key, err := s.auth.RotateKey(ctx, req.Arg)
		return Response{Secret: key}, err
	}
	return Response{}, fmt.Errorf("unknown admin command %q", req.Verb)
}

func token(t auth.Token, err error) (Response, error) {
	return Response{Secret: t.Value, Link: t.Link, Expires: t.Expires}, err
}

// Call sends req to the server at path and returns its response; a
// refusal is an error carrying the server's words.
func Call(ctx context.Context, path string, req Request) (Response, error) {
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "unix", path)
	if err != nil {
		return Response{}, fmt.Errorf("cannot reach the grelot server at %s: it is not running, or ADMIN_SOCKET differs (%w)", path, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(connTimeout))
	// A refused peer's answer comes before the server reads anything, so a
	// failed send may still have one to read.
	sendErr := json.NewEncoder(conn).Encode(req)
	var resp Response
	if err := json.NewDecoder(io.LimitReader(conn, maxResponse)).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("talking to the grelot server at %s: %w", path, errors.Join(sendErr, err))
	}
	if resp.Error != "" {
		return Response{}, errors.New(resp.Error)
	}
	return resp, nil
}
