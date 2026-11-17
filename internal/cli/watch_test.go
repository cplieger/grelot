package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// alphaDoors is alpha with four watcher doors: tray prints a line per
// nudge, once exits on the first, poll holds 300 ms and prints JSON-free
// stand-ins for the idle and end outputs, exiting 0 after the end, and long
// is poll with a hold no test outlasts.
const alphaDoors = alphaFile + `doors:
  - {name: tray, layer: 2, transport: stdout, line: 'tray {{.Session}} {{.Line}}'}
  - {name: once, layer: 2, transport: exit}
  - {name: poll, layer: 2, transport: exit, hold: 300ms, idle: idle, end: 'end {{.Line}}', end_exit: 0}
  - {name: long, layer: 2, transport: exit, hold: 1h, idle: idle, end: 'end {{.Line}}', end_exit: 0}
`

// syncBuffer is an output a watcher writes while the test reads it.
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

// closeRecorder is a stdin that records being closed.
type closeRecorder struct {
	mu     sync.Mutex
	closed bool
}

func (c *closeRecorder) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func signal(seq, n int) string {
	return "[grelot:" + strconv.Itoa(seq) + "] grelot (automated, not the user): " + strconv.Itoa(n) + " new; run grelot inbox"
}

func TestWatch_triesAgainWhileTheServerFails(t *testing.T) {
	defer func(d time.Duration) { watchRetry = d }(watchRetry)
	watchRetry = time.Millisecond
	cases := []struct {
		name     string
		answers  []int
		wantCode int
		wantOut  string
		wantWarn int
	}{
		{name: "two 503s then a nudge", answers: []int{503, 503, 200}, wantCode: 0, wantOut: signal(4, 1) + "\n", wantWarn: 1},
		{name: "a 204 between two outages", answers: []int{503, 204, 502, 200}, wantCode: 0, wantOut: signal(4, 1) + "\n", wantWarn: 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			n := 0
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				code := c.answers[min(n, len(c.answers)-1)]
				n++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				switch code {
				case http.StatusOK:
					_, _ = w.Write([]byte(`{"seq":4,"count":1,"attempt":1}`))
				case http.StatusNoContent:
				default:
					_, _ = w.Write([]byte(`{"error":"failing","code":"x"}`))
				}
			}))
			defer fake.Close()
			a := newAgent(t, &testServer{URL: fake.URL}, "s-1", "")
			a.env.Providers = providerSet(t, "alpha", alphaDoors)
			code, out, errOut := a.run("watch", "--door", "once")
			if code != c.wantCode || out != c.wantOut {
				t.Errorf("exit %d, stdout %q, want %d, %q; stderr %q", code, out, c.wantCode, c.wantOut, errOut)
			}
			if got := strings.Count(errOut, "level=WARN"); c.wantWarn > 0 && got != c.wantWarn {
				t.Errorf("%d WARN lines, want one per outage, %d: %q", got, c.wantWarn, errOut)
			}
		})
	}
}

// An error that waiting again would not pass ends the run with the door's
// end output, the end line naming the error, and its end_exit.
func TestWatch_aRunEndingErrorDeliversTheEnd(t *testing.T) {
	const end = "[grelot:end] grelot (automated, not the user): this watcher stopped (error); run grelot me"
	cases := []struct {
		name      string
		door      string
		wantOut   string
		wantErr   string
		wantCode  int
		noSession bool
	}{
		{name: "a refused token on an exit door", door: "once", wantOut: end + "\n", wantErr: "refused", wantCode: exitFailed},
		{name: "a refused token on a held exit door", door: "poll", wantOut: "end " + end + "\n", wantErr: "refused", wantCode: 0},
		{name: "a refused token on a stdout door", door: "tray", wantOut: "tray s-1 " + end + "\n", wantErr: "refused", wantCode: exitFailed},
		{name: "no session id", door: "poll", noSession: true, wantOut: "end " + end + "\n", wantErr: "ALPHA_ID", wantCode: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"refused","code":"unauthorized"}`))
			}))
			defer fake.Close()
			a := newAgent(t, &testServer{URL: fake.URL}, "s-1", "")
			a.env.Providers = providerSet(t, "alpha", alphaDoors)
			if c.noSession {
				delete(a.vars, "ALPHA_ID")
			}
			code, out, errOut := a.run("--provider", "alpha", "watch", "--door", c.door)
			if code != c.wantCode || out != c.wantOut {
				t.Errorf("exit %d, stdout %q, want %d, %q; stderr %q", code, out, c.wantCode, c.wantOut, errOut)
			}
			if !strings.Contains(errOut, c.wantErr) {
				t.Errorf("stderr %q, want the error naming %q", errOut, c.wantErr)
			}
		})
	}
}

// Host push working again ends the watcher with nothing failed: exit 0
// whatever the door's end_exit.
func TestWatch_aRestoredEndSaysHostPushWorksAndExits0(t *testing.T) {
	const end = "[grelot:end] grelot (automated, not the user): this watcher stopped (host push working again); run grelot me"
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":"host push works again, so this watcher stops","code":"restored"}`))
	}))
	defer fake.Close()
	for _, c := range []struct{ door, wantOut string }{
		{door: "once", wantOut: end + "\n"},
		{door: "tray", wantOut: "tray s-1 " + end + "\n"},
		{door: "poll", wantOut: "end " + end + "\n"},
	} {
		t.Run(c.door, func(t *testing.T) {
			a := newAgent(t, &testServer{URL: fake.URL}, "s-1", "")
			a.env.Providers = providerSet(t, "alpha", alphaDoors)
			code, out, errOut := a.run("--provider", "alpha", "watch", "--door", c.door)
			if code != 0 || out != c.wantOut {
				t.Errorf("exit %d, stdout %q, want 0, %q; stderr %q", code, out, c.wantOut, errOut)
			}
		})
	}
}
