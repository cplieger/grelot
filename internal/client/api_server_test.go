package client

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/store"
)

// serverVersion is the in-process server's version in these tests.
const serverVersion = "v1.1.0-dev.4"

// realServer runs grelot's server over a fresh store and returns its URL.
func realServer(t *testing.T) string {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := server.NewHub()
	st, err := store.Open(filepath.Join(t.TempDir(), "grelot.db"), &store.Options{
		Logger: logger, PathRoot: store.DefaultPathRoot, Woke: hub.Woke,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(&server.Options{
		Store: st, Logger: logger, Hub: hub, Version: serverVersion,
		Config: server.Config{Token: "test-token", PathRoot: store.DefaultPathRoot},
	})
	h := httptest.NewServer(srv.Handler())
	t.Cleanup(h.Close)
	return h.URL
}

// registerBody is a registration with no where values.
func registerBody(name string) map[string]any {
	return map[string]any{
		"name": name, "paths": []string{"/workspace/tool"}, "description": "testing the client",
		"host": "box", "os": "linux", "arch": "amd64", "ttl": "2h",
		"where": map[string]any{"machine": "", "environment": "", "checkouts": []any{}},
	}
}

func TestMe_againstTheServer(t *testing.T) {
	a, dir := newTestAPI(t, realServer(t), "v1.1.0")
	a.Session = "alpha:s-1"
	if _, err := a.Do(t.Context(), &Request{Method: http.MethodPost, Path: "/v1/register", Body: registerBody("tool-dev")}, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	me, resp, err := a.Me(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if me.State != StateLive || me.Identity == nil || me.Identity.Name != "tool-dev" || me.Identity.Provider != "alpha" {
		t.Errorf("Me = %+v, want the live identity tool-dev of provider alpha", me)
	}
	if !strings.HasPrefix(resp.Status, "grelot: @tool-dev · 0 unread") {
		t.Errorf("status line = %q, want the identity and its unread count", resp.Status)
	}
	if got := LoadFacts(dir); got != (Facts{PathRoot: "/workspace", Version: serverVersion}) {
		t.Errorf("cached facts = %+v, want the server's root and version", got)
	}
}

func TestMe_aSessionWithNoIdentity(t *testing.T) {
	a, _ := newTestAPI(t, realServer(t), "v1.1.0")
	a.Session = "alpha:s-new"
	me, resp, err := a.Me(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if me.State != StateNone || me.Identity != nil {
		t.Errorf("Me = %+v, want state none and no identity", me)
	}
	if !strings.Contains(resp.Status, "grelot register") {
		t.Errorf("status line = %q, want the register command", resp.Status)
	}
}

func TestMe_cachedRootCorrectedByTheNextMe(t *testing.T) {
	a, dir := newTestAPI(t, realServer(t), "v1.1.0")
	a.Session = "alpha:s-1"
	SaveFacts(dir, Facts{PathRoot: "/home/u/src"})
	if _, _, err := a.Me(t.Context(), nil, nil); err != nil {
		t.Fatalf("Me: %v", err)
	}
	if got := LoadFacts(dir).PathRoot; got != "/workspace" {
		t.Errorf("cached root = %q, want the server's /workspace", got)
	}
}

func TestDo_oldClientGetsThe426(t *testing.T) {
	a, _ := newTestAPI(t, realServer(t), "v0.9.0")
	a.Session = "alpha:s-1"
	_, _, err := a.Me(t.Context(), nil, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.HTTP != http.StatusUpgradeRequired {
		t.Fatalf("err = %v, want a 426 *APIError", err)
	}
	if !strings.Contains(ae.Msg, "install grelot v1.1.0") {
		t.Errorf("426 line = %q, want it to name the version to install", ae.Msg)
	}
}

func TestDo_wrongTokenIsRefused(t *testing.T) {
	a, err := NewAPI(&Config{Server: realServer(t), Token: "wrong"}, "v1.1.0", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = a.Me(t.Context(), nil, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.HTTP != http.StatusUnauthorized || !strings.Contains(ae.Msg, "token") {
		t.Errorf("err = %v, want the 401 naming the token", err)
	}
}
