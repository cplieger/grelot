//go:build !windows

package client

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cplieger/grelot/internal/protopath"
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
		Logger: logger, PathRoot: protopath.DefaultRoot, Woke: hub.Woke,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(&server.Options{
		Store: st, Logger: logger, Hub: hub, Version: serverVersion,
		Config: server.Config{Token: "test-token", PathRoot: protopath.DefaultRoot},
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
	if !strings.Contains(ae.Msg, "install grelot "+serverVersion+" with go install github.com/cplieger/grelot@"+serverVersion) {
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

// dropFirstPost fronts server and answers its first POST /v1/posts by
// letting the server commit it and then closing the connection unanswered,
// keeping each post's Idempotency-Key.
func dropFirstPost(t *testing.T, server string) (front string, keys func() []string) {
	t.Helper()
	target, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mu sync.Mutex
	var seen []string
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One request per connection: net/http re-sends a keyed request
		// itself when a reused connection drops, which would leave the
		// client's own retry untested.
		w.Header().Set("Connection", "close")
		if r.Method != http.MethodPost || r.URL.Path != "/v1/posts" {
			proxy.ServeHTTP(w, r)
			return
		}
		mu.Lock()
		seen = append(seen, r.Header.Get("Idempotency-Key"))
		first := len(seen) == 1
		mu.Unlock()
		if !first {
			proxy.ServeHTTP(w, r)
			return
		}
		proxy.ServeHTTP(httptest.NewRecorder(), r)
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(hs.Close)
	return hs.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestDo_aPostCommittedButUnansweredIsReSentWithItsKeyAndAnswersItsFirstResult(t *testing.T) {
	front, keys := dropFirstPost(t, realServer(t))
	sender, _ := newTestAPI(t, front, "v1.1.0")
	sender.Session = "alpha:s-1"
	reader, _ := newTestAPI(t, front, "v1.1.0")
	reader.Session = "alpha:s-2"
	for _, r := range []struct {
		api  *API
		name string
	}{{sender, "sender-dev"}, {reader, "reader-dev"}} {
		if _, err := r.api.Do(t.Context(), &Request{Method: http.MethodPost, Path: "/v1/register", Body: registerBody(r.name)}, nil); err != nil {
			t.Fatalf("register %s: %v", r.name, err)
		}
	}
	noChange := map[string]any{"name": store.NoChange, "description": store.NoChange, "paths": []string{store.NoChange}}

	var posted struct {
		Message uint64 `json:"message"`
	}
	_, err := sender.Do(t.Context(), &Request{Method: http.MethodPost, Path: "/v1/posts", Body: map[string]any{
		"to": map[string]string{"agent": "reader-dev"}, "subject": "once", "body": "a body", "identity": noChange,
	}}, &posted)
	if err != nil {
		t.Fatalf("post: %v, want the first result", err)
	}
	if k := keys(); len(k) != 2 || k[0] == "" || k[1] != k[0] {
		t.Errorf("the post's attempts sent keys %q, want two attempts with one key", k)
	}
	var history struct {
		Entries []struct {
			Subject string `json:"subject"`
			Message uint64 `json:"message"`
		} `json:"entries"`
	}
	if _, err := reader.Do(t.Context(), &Request{Method: http.MethodGet, Path: "/v1/history"}, &history); err != nil {
		t.Fatalf("history: %v", err)
	}
	var stored []uint64
	for _, e := range history.Entries {
		if e.Subject == "once" {
			stored = append(stored, e.Message)
		}
	}
	if len(stored) != 1 || stored[0] != posted.Message || posted.Message == 0 {
		t.Errorf("reader's history holds messages %v of the post, want only the answered %d", stored, posted.Message)
	}
}
