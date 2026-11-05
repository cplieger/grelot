package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// captured is one request a fake server saw.
type captured struct {
	header http.Header
	method string
	uri    string
	body   string
}

// fakeServer answers every request with answer and keeps what it saw.
type fakeServer struct {
	srv  *httptest.Server
	seen []captured
	mu   sync.Mutex
}

func newFake(t *testing.T, answer http.HandlerFunc) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, captured{header: r.Header.Clone(), method: r.Method, uri: r.RequestURI, body: string(body)})
		f.mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) requests() []captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]captured(nil), f.seen...)
}

func newTestAPI(t *testing.T, server, version string) (*API, string) {
	t.Helper()
	dir := t.TempDir()
	a, err := NewAPI(&Config{Server: server, Token: "test-token"}, version, dir)
	if err != nil {
		t.Fatalf("NewAPI: %v", err)
	}
	return a, dir
}

func answerJSON(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestDo_sendsTheHeaders(t *testing.T) {
	f := newFake(t, answerJSON(http.StatusOK, `{"ok":true}`))
	a, _ := newTestAPI(t, f.srv.URL, "v1.1.0")
	a.Session, a.As, a.Self = "alpha:s-1", "tool-dev/review", true
	var out struct{ OK bool }
	_, err := a.Do(t.Context(), &Request{
		Method: http.MethodPost, Path: "/v1/posts",
		Query:  url.Values{"path": {"/workspace/a b"}},
		Header: http.Header{HeaderEnvironment: {"env-id"}},
		Body:   map[string]string{"subject": "hello"},
	}, &out)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !out.OK {
		t.Error("the 200 body was not decoded into out")
	}
	got := f.requests()
	if len(got) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(got))
	}
	r := got[0]
	want := map[string]string{
		"Authorization":      "Bearer test-token",
		"Grelot-Version":     "v1.1.0",
		"Grelot-Session":     "alpha:s-1",
		"Grelot-As":          "tool-dev/review",
		"Grelot-Self":        "1",
		"Grelot-Environment": "env-id",
		"Content-Type":       "application/json",
	}
	for k, v := range want {
		if r.header.Get(k) != v {
			t.Errorf("header %s = %q, want %q", k, r.header.Get(k), v)
		}
	}
	if r.method != http.MethodPost || r.uri != "/v1/posts?path=%2Fworkspace%2Fa+b" {
		t.Errorf("request = %s %s, want POST /v1/posts with the path query", r.method, r.uri)
	}
	if r.body != `{"subject":"hello"}` {
		t.Errorf("body = %q, want the JSON of Body", r.body)
	}
}

func TestDo_noIdentityHeadersWhenUnset(t *testing.T) {
	f := newFake(t, answerJSON(http.StatusOK, `{}`))
	a, _ := newTestAPI(t, f.srv.URL, "v1.1.0")
	if _, err := a.Do(t.Context(), &Request{Method: http.MethodGet, Path: "/v1/identities"}, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	h := f.requests()[0].header
	for _, k := range []string{"Grelot-Session", "Grelot-As", "Grelot-Self", "Content-Type"} {
		if _, ok := h[k]; ok {
			t.Errorf("header %s sent as %q, want it absent", k, h.Get(k))
		}
	}
}

func TestDo_retriesAGetAndSendsAPostOnce(t *testing.T) {
	for _, c := range []struct {
		method string
		want   int
	}{{http.MethodGet, 3}, {http.MethodPost, 1}, {http.MethodPatch, 1}, {http.MethodDelete, 1}} {
		t.Run(c.method, func(t *testing.T) {
			f := newFake(t, answerJSON(http.StatusServiceUnavailable, `{"error":"the server is shutting down; retry once it is back","code":"shutting_down"}`))
			a, _ := newTestAPI(t, f.srv.URL, "v1.1.0")
			_, err := a.Do(t.Context(), &Request{Method: c.method, Path: "/v1/me", Body: map[string]int{"n": 1}}, nil)
			var ae *APIError
			if !errors.As(err, &ae) || ae.HTTP != http.StatusServiceUnavailable {
				t.Errorf("err = %v, want the 503 as an *APIError", err)
			}
			if n := len(f.requests()); n != c.want {
				t.Errorf("server saw %d requests, want %d", n, c.want)
			}
		})
	}
}

func TestNewHookAPI_sendsAGetOnce(t *testing.T) {
	f := newFake(t, answerJSON(http.StatusServiceUnavailable, `{"error":"store unavailable","code":"unavailable"}`))
	a, err := NewHookAPI(&Config{Server: f.srv.URL, Token: "test-token"}, "v1.1.0", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Do(t.Context(), &Request{Method: http.MethodGet, Path: "/v1/me"}, nil); err == nil {
		t.Error("Do answered no error for a 503")
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("server saw %d requests, want 1", n)
	}
}

func TestDo_refusal(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Grelot-Status", "grelot: @a · 0 unread · wake: watcher")
		answerJSON(http.StatusConflict, `{"error":"/workspace/docs is another repository for docs-dev; give one of them another name with repos: in your client config","code":"conflict","path_root":"/srv/work"}`)(w, nil)
	})
	a, dir := newTestAPI(t, f.srv.URL, "v1.1.0")
	_, err := a.Do(t.Context(), &Request{Method: http.MethodPost, Path: "/v1/register"}, nil)
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if ae.HTTP != http.StatusConflict || ae.Code != "conflict" || ae.Status != "grelot: @a · 0 unread · wake: watcher" {
		t.Errorf("APIError = %+v, want the 409, its code and status line", ae)
	}
	if want := "grelot: /workspace/docs is another repository for docs-dev; give one of them another name with repos: in your client config"; ae.Error() != want {
		t.Errorf("Error() = %q, want %q", ae.Error(), want)
	}
	if got := LoadFacts(dir).PathRoot; got != "/srv/work" {
		t.Errorf("cached path root = %q, want the refusal's /srv/work", got)
	}
}

func TestDo_refusalWithoutAnErrorBody(t *testing.T) {
	for name, body := range map[string]string{"not JSON": "<html>bad gateway</html>", "no error field": `{"code":"x"}`} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, body)
			})
			a, _ := newTestAPI(t, f.srv.URL, "v1.1.0")
			_, err := a.Do(t.Context(), &Request{Method: http.MethodPost, Path: "/v1/posts"}, nil)
			if err == nil || err.Error() != "grelot: the server answered 502 Bad Gateway" {
				t.Errorf("err = %v, want the status named", err)
			}
		})
	}
}

func TestDo_refusalTextIsOneSafeLine(t *testing.T) {
	f := newFake(t, answerJSON(http.StatusBadRequest, `{"error":"bad\u001b[2J\nline\u202e","code":"in\nvalid"}`))
	a, _ := newTestAPI(t, f.srv.URL, "v1.1.0")
	_, err := a.Do(t.Context(), &Request{Method: http.MethodPost, Path: "/v1/posts"}, nil)
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if strings.ContainsAny(ae.Msg+ae.Code, "\x1b\n\u202e") {
		t.Errorf("APIError = %q %q, want no escape, line break or bidi control", ae.Msg, ae.Code)
	}
}

func TestDo_answerThatIsNotJSON(t *testing.T) {
	f := newFake(t, answerJSON(http.StatusOK, `not json`))
	a, _ := newTestAPI(t, f.srv.URL, "v1.1.0")
	var out struct{}
	_, err := a.Do(t.Context(), &Request{Method: http.MethodGet, Path: "/v1/me"}, &out)
	if err == nil || !strings.Contains(err.Error(), "grelot me") {
		t.Errorf("err = %v, want a line naming grelot me", err)
	}
}

func TestDo_answerOverTheCap(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, maxResponse+1))
	})
	a, _ := newTestAPI(t, f.srv.URL, "v1.1.0")
	_, err := a.Do(t.Context(), &Request{Method: http.MethodPost, Path: "/v1/inbox/ack"}, nil)
	if _, ok := errors.AsType[*UnreachableError](err); !ok {
		t.Errorf("err = %v, want an *UnreachableError", err)
	}
}

func TestDo_unreachable(t *testing.T) {
	f := newFake(t, answerJSON(http.StatusOK, `{}`))
	addr := f.srv.URL
	f.srv.Close()
	a, _ := newTestAPI(t, addr, "v1.1.0")
	_, err := a.Do(t.Context(), &Request{Method: http.MethodPost, Path: "/v1/posts"}, nil)
	var ue *UnreachableError
	if !errors.As(err, &ue) || ue.Timeout {
		t.Fatalf("err = %v, want an *UnreachableError that is no timeout", err)
	}
	if !strings.Contains(err.Error(), "cannot reach the server "+addr) {
		t.Errorf("err = %q, want it to name the server", err)
	}
}

func TestDo_timeout(t *testing.T) {
	release := make(chan struct{})
	f := newFake(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	a, _ := newTestAPI(t, f.srv.URL, "v1.1.0")
	_, err := a.Do(t.Context(), &Request{Method: http.MethodPost, Path: "/v1/posts", Timeout: 50 * time.Millisecond}, nil)
	var ue *UnreachableError
	if !errors.As(err, &ue) || !ue.Timeout {
		t.Fatalf("err = %v, want an *UnreachableError with Timeout", err)
	}
	if !strings.Contains(err.Error(), "did not answer in time") {
		t.Errorf("err = %q, want the timeout named", err)
	}
}

func TestDo_callerCancelIsTheContextError(t *testing.T) {
	release := make(chan struct{})
	f := newFake(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	a, _ := newTestAPI(t, f.srv.URL, "v1.1.0")
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := a.Do(ctx, &Request{Method: http.MethodPost, Path: "/v1/posts"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if _, ok := errors.AsType[*UnreachableError](err); ok {
		t.Errorf("err = %v, want the caller's end, not a server never answering", err)
	}
}

func TestDo_noRedirectFollowed(t *testing.T) {
	var hit atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	t.Cleanup(other.Close)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	})
	a, _ := newTestAPI(t, f.srv.URL, "v1.1.0")
	_, err := a.Do(t.Context(), &Request{Method: http.MethodGet, Path: "/v1/me"}, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.HTTP != http.StatusFound {
		t.Errorf("err = %v, want the 302 as an *APIError", err)
	}
	if hit.Load() {
		t.Error("the redirect target was called, carrying the bearer")
	}
}
