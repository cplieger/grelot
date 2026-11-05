package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/provider"
)

// httpFile is a provider file with one http door posting to base; extra is
// spliced into the door.
func httpFile(base, extra string) string {
	return "name: testhost\ndoors:\n  - name: notify\n    layer: 1\n    transport: http\n" +
		"    url: '" + base + "/notify/{{path .Session}}'\n" +
		"    body: '{\"line\":{{json .Line}},\"key\":{{json .DedupeKey}}}'\n" +
		"    dedupe_window: 2m\n" + extra
}

func loadDoor(t *testing.T, file string) *provider.Door {
	t.Helper()
	p, err := provider.Load("testhost", []byte(file))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return &p.Doors[0]
}

// statusServer answers every request with status and the Retry-After value,
// when one is given, and counts the requests.
func statusServer(t *testing.T, status int, retryAfter string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func nudgeData() *provider.Data {
	return &provider.Data{Line: SignalLine(7, 2), Seq: 7, Session: "a/b?c%", Identity: "test-agent", DedupeKey: "id1:3:1"}
}

func TestHTTPAccepted(t *testing.T) {
	type seen struct {
		method, path, auth, ctype, body string
	}
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b)}
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"error": "this body is not read`)
	}))
	t.Cleanup(srv.Close)
	door := loadDoor(t, httpFile(srv.URL, "    headers:\n      Authorization: 'Bearer {{secret \"test_token\"}}'\n      Content-Type: application/json\n"))
	if err := door.ResolveSecrets(func(string) (string, error) { return "test-token-value", nil }); err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}

	res, err := NewHTTP().Send(t.Context(), door, nudgeData())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Outcome != Accepted || res.Status != http.StatusAccepted {
		t.Errorf("result = %+v, want Accepted with status 202", res)
	}
	s := <-got
	if s.method != http.MethodPost {
		t.Errorf("method = %s, want POST", s.method)
	}
	if s.path != "/notify/a%2Fb%3Fc%25" {
		t.Errorf("path = %s, want the session escaped as one segment", s.path)
	}
	if s.auth != "Bearer test-token-value" || s.ctype != "application/json" {
		t.Errorf("headers = %q, %q; want the resolved secret and the door's content type", s.auth, s.ctype)
	}
	var body struct{ Line, Key string }
	if err := json.Unmarshal([]byte(s.body), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", s.body, err)
	}
	if body.Line != "[grelot:7] grelot (automated, not the user): 2 new; run grelot inbox" || body.Key != "id1:3:1" {
		t.Errorf("body = %+v, want the signal line and the dedupe key", body)
	}
}

func TestHTTPAnyTwoXXAccepted(t *testing.T) {
	for _, status := range []int{200, 201, 204, 299} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv, _ := statusServer(t, status, "")
			res, err := NewHTTP().Send(t.Context(), loadDoor(t, httpFile(srv.URL, "")), nudgeData())
			if err != nil || res.Outcome != Accepted || res.Status != status {
				t.Errorf("Send = %+v, %v; want Accepted with status %d", res, err, status)
			}
		})
	}
}

func TestHTTPRetryLater(t *testing.T) {
	cases := []struct {
		name       string
		retryAfter string
		status     int
		want       time.Duration
	}{
		{"429 honours Retry-After", "30", 429, 30 * time.Second},
		{"503 without Retry-After", "", 503, 15 * time.Second},
		{"500 below the floor", "1", 500, 5 * time.Second},
		{"502 above the ceiling", "120", 502, 60 * time.Second},
		{"501 is a 5xx", "", 501, 15 * time.Second},
		{"599 is a 5xx", "", 599, 15 * time.Second},
		{"malformed Retry-After", "soon", 503, 15 * time.Second},
		{"Retry-After zero", "0", 429, 5 * time.Second},
		{"Retry-After many zeros", "00000000000000000000", 429, 5 * time.Second},
		{"delta seconds past int64", "9999999999999999999", 503, 60 * time.Second},
		{"delta seconds past nineteen digits", "99999999999999999999", 503, 60 * time.Second},
		{"negative Retry-After", "-5", 503, 15 * time.Second},
		{"past date", "Mon, 02 Jan 2006 15:04:05 GMT", 503, 5 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := statusServer(t, c.status, c.retryAfter)
			res, err := NewHTTP().Send(t.Context(), loadDoor(t, httpFile(srv.URL, "")), nudgeData())
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if res.Outcome != RetryLater || res.Status != c.status || res.RetryAfter != c.want {
				t.Errorf("result = %+v, want RetryLater, status %d, after %s", res, c.status, c.want)
			}
		})
	}
}

func TestHTTPRetryAfterDate(t *testing.T) {
	srv, _ := statusServer(t, 503, time.Now().Add(40*time.Second).UTC().Format(http.TimeFormat))
	res, err := NewHTTP().Send(t.Context(), loadDoor(t, httpFile(srv.URL, "")), nudgeData())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Outcome != RetryLater || res.RetryAfter < 37*time.Second || res.RetryAfter > 40*time.Second {
		t.Errorf("result = %+v, want RetryLater about 40s out", res)
	}
}

func TestHTTPGiveUp(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 418, 499} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv, _ := statusServer(t, status, "30")
			res, err := NewHTTP().Send(t.Context(), loadDoor(t, httpFile(srv.URL, "")), nudgeData())
			if err != nil || res.Outcome != GiveUp || res.Status != status || res.RetryAfter != 0 {
				t.Errorf("Send = %+v, %v; want GiveUp with status %d", res, err, status)
			}
		})
	}
}

func TestHTTPGone(t *testing.T) {
	cases := []struct {
		status int
		want   Outcome
	}{{404, Gone}, {410, Gone}, {400, GiveUp}}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.status), func(t *testing.T) {
			srv, _ := statusServer(t, c.status, "")
			door := loadDoor(t, httpFile(srv.URL, "    gone: [404, 410]\n"))
			res, err := NewHTTP().Send(t.Context(), door, nudgeData())
			if err != nil || res.Outcome != c.want || res.Status != c.status {
				t.Errorf("Send = %+v, %v; want %s with status %d", res, err, c.want, c.status)
			}
		})
	}
}

// A redirect would carry the door's credential headers to wherever the
// receiver points, so the door's literal host is the only one reached.
func TestHTTPRedirectNotFollowed(t *testing.T) {
	other, otherHits := statusServer(t, 200, "")
	srv := httptest.NewServer(http.RedirectHandler(other.URL+"/elsewhere", http.StatusFound))
	t.Cleanup(srv.Close)
	door := loadDoor(t, httpFile(srv.URL, "    headers:\n      Authorization: 'Bearer {{secret \"test_token\"}}'\n"))
	if err := door.ResolveSecrets(func(string) (string, error) { return "test-token-value", nil }); err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	res, err := NewHTTP().Send(t.Context(), door, nudgeData())
	if err != nil || res.Outcome != GiveUp || res.Status != http.StatusFound {
		t.Errorf("Send = %+v, %v; want GiveUp with status 302", res, err)
	}
	if n := otherHits.Load(); n != 0 {
		t.Errorf("the redirect target got %d requests, want 0", n)
	}
}

func TestHTTPNetworkError(t *testing.T) {
	srv, _ := statusServer(t, 200, "")
	door := loadDoor(t, httpFile(srv.URL, ""))
	srv.Close()
	res, err := NewHTTP().Send(t.Context(), door, nudgeData())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Outcome != RetryLater || res.Status != 0 || res.RetryAfter != 15*time.Second || res.Err == nil {
		t.Errorf("result = %+v, want RetryLater after 15s with the cause", res)
	}
}

// Not parallel: it shortens the shipped request timeout.
func TestHTTPTimeout(t *testing.T) {
	setHTTPTimeout(t, 100*time.Millisecond)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	res, err := NewHTTP().Send(t.Context(), loadDoor(t, httpFile(srv.URL, "")), nudgeData())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Outcome != RetryLater || res.Status != 0 || res.RetryAfter != 15*time.Second || res.Err == nil {
		t.Errorf("result = %+v, want RetryLater after 15s with the cause", res)
	}
}

// Not parallel: it shortens the shipped request timeout. The receiver sends
// its status and then never finishes the body, so only a transport that reads
// no body returns before the timeout.
func TestHTTPReadsNoBody(t *testing.T) {
	setHTTPTimeout(t, 2*time.Second)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"partial":`)
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	start := time.Now()
	res, err := NewHTTP().Send(t.Context(), loadDoor(t, httpFile(srv.URL, "")), nudgeData())
	if err != nil || res.Outcome != Accepted {
		t.Errorf("Send = %+v, %v; want Accepted", res, err)
	}
	if took := time.Since(start); took >= 2*time.Second {
		t.Errorf("Send took %s, so it waited on the body", took)
	}
}

func TestHTTPCallerContextEnded(t *testing.T) {
	srv, hits := statusServer(t, 200, "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if res, err := NewHTTP().Send(ctx, loadDoor(t, httpFile(srv.URL, "")), nudgeData()); err == nil {
		t.Errorf("Send = %+v with no error, want the context's error", res)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the receiver got %d requests, want 0", n)
	}
}

func TestHTTPRefusesUnsendable(t *testing.T) {
	srv, hits := statusServer(t, 200, "")
	t.Run("unresolved secret", func(t *testing.T) {
		door := loadDoor(t, httpFile(srv.URL, "    headers:\n      Authorization: 'Bearer {{secret \"test_token\"}}'\n"))
		if res, err := NewHTTP().Send(t.Context(), door, nudgeData()); err == nil {
			t.Errorf("Send = %+v with no error, want the render error", res)
		}
	})
	t.Run("not an http door", func(t *testing.T) {
		door := loadDoor(t, "name: testhost\ndoors:\n  - name: watch\n    layer: 2\n    transport: stdout\n")
		if res, err := NewHTTP().Send(t.Context(), door, nudgeData()); err == nil {
			t.Errorf("Send = %+v with no error, want a refusal", res)
		}
	})
	if n := hits.Load(); n != 0 {
		t.Errorf("the receiver got %d requests, want 0", n)
	}
}

func setHTTPTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := httpTimeout
	httpTimeout = d
	t.Cleanup(func() { httpTimeout = old })
}
