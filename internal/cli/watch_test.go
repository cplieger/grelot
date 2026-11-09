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

// alphaDoors is alpha with three watcher doors: tray prints a line per
// nudge, once exits on the first, and poll holds 300 ms and prints JSON-free
// stand-ins for the idle and end outputs, exiting 0 after the end.
const alphaDoors = alphaFile + `doors:
  - {name: tray, layer: 2, transport: stdout, line: 'tray {{.Session}} {{.Line}}'}
  - {name: once, layer: 2, transport: exit}
  - {name: poll, layer: 2, transport: exit, hold: 300ms, idle: idle, end: 'end {{.Line}}', end_exit: 0}
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
		{name: "a 401 ends it", answers: []int{401}, wantCode: exitFailed},
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
