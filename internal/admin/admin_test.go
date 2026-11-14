//go:build !windows

package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cplieger/access"
	"github.com/cplieger/grelot/internal/accounts"
	"github.com/cplieger/grelot/internal/admin"
	"github.com/cplieger/grelot/internal/auth"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// socketPath is a path in a folder of its own, short enough for a socket
// address wherever the temporary folder is.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "grelot")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "admin.sock")
}

// running is a server answering on path until stop.
type running struct {
	srv  *admin.Server
	logs *syncBuffer
	path string
	stop func() error
}

func start(t *testing.T, configure func(*admin.Server)) *running {
	t.Helper()
	db, err := accounts.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a, err := auth.New(t.Context(), db, testAuthOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	r := &running{logs: &syncBuffer{}, path: socketPath(t)}
	r.srv = admin.NewServer(a, slog.New(slog.NewJSONHandler(r.logs, nil)))
	if configure != nil {
		configure(r.srv)
	}
	ln, err := admin.Listen(t.Context(), r.path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.srv.Serve(ctx, ln) }()
	var once sync.Once
	var result error
	r.stop = func() error {
		once.Do(func() { cancel(); result = <-done })
		return result
	}
	t.Cleanup(func() { _ = r.stop() })
	return r
}

func TestListen_makesASocketOnlyItsOwnerReaches(t *testing.T) {
	r := start(t, nil)
	fi, err := os.Lstat(r.path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Type() != fs.ModeSocket || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v, want a socket with mode 0600", fi.Mode())
	}
}

func TestListen_replacesASocketNothingAnswers(t *testing.T) {
	path := socketPath(t)
	old, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = old.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the dead server's socket is not there: %v", err)
	}
	ln, err := admin.Listen(t.Context(), path)
	if err != nil {
		t.Fatalf("Listen over a stale socket = %v, want it replaced", err)
	}
	_ = ln.Close()
}

func TestListen_refusesASocketAServerAnswers(t *testing.T) {
	r := start(t, nil)
	if ln, err := admin.Listen(t.Context(), r.path); err == nil || !strings.Contains(err.Error(), "another running grelot server") {
		if ln != nil {
			_ = ln.Close()
		}
		t.Fatalf("Listen on a live socket = %v, want refused", err)
	}
	if _, err := admin.Call(t.Context(), r.path, admin.Request{Verb: admin.VerbKeyList}); err != nil {
		t.Errorf("the first server no longer answers: %v", err)
	}
}

func TestListen_leavesAFileThatIsNotASocket(t *testing.T) {
	path := socketPath(t)
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ln, err := admin.Listen(t.Context(), path); err == nil || !strings.Contains(err.Error(), "not a socket") {
		if ln != nil {
			_ = ln.Close()
		}
		t.Fatalf("Listen over a regular file = %v, want refused", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "keep me" {
		t.Errorf("the file reads %q, %v after the refusal; want it untouched", got, err)
	}
}

func TestServe_removesTheSocketWhenItStops(t *testing.T) {
	r := start(t, nil)
	if err := r.stop(); err != nil {
		t.Fatalf("Serve returned %v at stop, want nil", err)
	}
	if _, err := os.Lstat(r.path); !os.IsNotExist(err) {
		t.Errorf("Lstat of the socket after stop = %v, want it removed", err)
	}
}

func TestServe_answersOnlyItsOwnUser(t *testing.T) {
	r := start(t, func(s *admin.Server) { s.AnswerOnly(os.Getuid() + 1) })
	resp, err := admin.Call(t.Context(), r.path, admin.Request{Verb: admin.VerbSetupToken})
	if err == nil || !strings.Contains(err.Error(), "only the grelot server's own user") || resp.Secret != "" {
		t.Errorf("Call from another user = %+v, %v; want refused with no token", resp, err)
	}
	if !strings.Contains(r.logs.String(), "admin connection refused") {
		t.Errorf("no refusal line; logs: %s", r.logs)
	}
}

func TestServe_answersItsOwnUser(t *testing.T) {
	r := start(t, nil)
	resp, err := admin.Call(t.Context(), r.path, admin.Request{Verb: admin.VerbSetupToken})
	if err != nil || resp.Secret == "" || resp.Expires.IsZero() {
		t.Errorf("Call from this process's user = %+v, %v; want a token and its expiry", resp, err)
	}
}

func TestServe_neverLogsTheSecret(t *testing.T) {
	r := start(t, nil)
	resp, err := admin.Call(t.Context(), r.path, admin.Request{Verb: admin.VerbSetupToken})
	if err != nil {
		t.Fatal(err)
	}
	logs := r.logs.String()
	if strings.Contains(logs, resp.Secret) {
		t.Errorf("the logs hold the setup token: %s", logs)
	}
	if !strings.Contains(logs, `"verb":"setup-token"`) {
		t.Errorf("no line naming the command; logs: %s", logs)
	}
}

func TestServe_refusesWhatItCannotRun(t *testing.T) {
	r := start(t, nil)
	t.Run("an unknown verb", func(t *testing.T) {
		if _, err := admin.Call(t.Context(), r.path, admin.Request{Verb: "drop-everything"}); err == nil ||
			!strings.Contains(err.Error(), `unknown admin command "drop-everything"`) {
			t.Errorf("Call(drop-everything) = %v, want unknown admin command", err)
		}
	})
	t.Run("a request that is not JSON, and the next one still answered", func(t *testing.T) {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", r.path)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("setup-token\n")); err != nil {
			t.Fatal(err)
		}
		var resp admin.Response
		if err := json.NewDecoder(conn).Decode(&resp); err != nil || !strings.Contains(resp.Error, "could not read the request") {
			t.Errorf("answer %+v, %v; want could not read the request", resp, err)
		}
		if _, err := admin.Call(t.Context(), r.path, admin.Request{Verb: admin.VerbKeyList}); err != nil {
			t.Errorf("the next request: %v", err)
		}
	})
}

// testAuthOptions serves the sign-in at an https origin, its refusals
// written nowhere.
func testAuthOptions() *auth.Options {
	return &auth.Options{
		Logger: slog.New(slog.DiscardHandler), PublicURL: &url.URL{Scheme: "https", Host: "grelot.example.com"},
		Errors: func(http.ResponseWriter, *http.Request, access.Error) {},
	}
}
