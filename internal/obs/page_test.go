package obs

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/httpserver"
)

const goldenPage = "testdata/page.golden"

// TestHandler_servesTheGoldenPage pins grelot's own families on /metrics
// byte for byte: alerts/promql.yaml and the dashboards select them by name,
// label and bucket.
func TestHandler_servesTheGoldenPage(t *testing.T) {
	m := New()
	m.ServeBuildInfo("v1.2.3")
	m.ServeState(func(context.Context) (State, bool) {
		return State{
			Oldest: testNow.Add(-90 * time.Second), Live: 3, Dormant: 2, Online: 1,
			Announce: 1, Path: 4, Direct: 5, Cards: 6, Held: 7, Queued: 8, Unread: 9,
			FileBytes: 4096, FreeBytes: 1024, InUse: 3072, MaxBytes: 512 << 20,
		}, true
	}, func() time.Time { return testNow })
	m.SweepLastSuccess.Set(float64(testNow.Unix()))
	m.SweepFailures.Inc()
	m.StoreStartedEmpty.Inc(StartedEmptyCorrupt)
	m.Posts.Inc("announce")
	m.Posts.Add(2, "path")
	m.PostsRefused.Inc(RefusedScope)
	m.WritesReplayed.Inc()
	m.PushDoorUp.Set(1, "provider/door")
	m.PushDoorUp.Set(0, "provider/off")
	m.PushSends.Add(0, "provider/door", SendRetry)
	m.PushSends.Inc("provider/door", SendAccepted)
	m.ProvidersInvalid.Set(2)
	m.Nudges.Inc("watcher", AttemptFirst)
	m.Nudges.Inc("push", AttemptRepeat)
	m.NudgeDelay.Observe(0.2, "watcher")
	m.NudgeDelay.Observe(42, "push")
	m.RecordHTTP(httpserver.RequestMetric{Method: "GET", Path: "/v1/me", Status: 200, Latency: 30 * time.Millisecond})
	m.RecordHTTP(httpserver.RequestMetric{Method: "POST", Path: "/v1/posts", Status: 507, Latency: 2500 * time.Millisecond})

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	got := ownLines(rec.Body.String())

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPage), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPage, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPage)
	if err != nil {
		t.Fatalf("%v; write it with UPDATE_GOLDEN=1 go test ./internal/obs/ -run TestHandler_servesTheGoldenPage", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("/metrics differs from %s; review the change, then regenerate with UPDATE_GOLDEN=1 go test ./internal/obs/ -run TestHandler_servesTheGoldenPage\ngot:\n%s", goldenPage, got)
	}
}

// ownLines keeps the page's grelot_ lines, dropping the Go and process
// families, whose values change from run to run.
func ownLines(page string) []byte {
	var b bytes.Buffer
	for line := range strings.SplitSeq(page, "\n") {
		name := line
		if rest, ok := strings.CutPrefix(line, "# HELP "); ok {
			name = rest
		} else if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name = rest
		}
		if strings.HasPrefix(name, "grelot_") {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}
