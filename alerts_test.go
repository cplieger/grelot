package main

import (
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/store"
	"go.yaml.in/yaml/v3"
)

// shippedAlerts holds every alert name alerts/promql.yaml has carried: ""
// for one still in the file, else the alert now covering its condition or
// "removed: <why>". Deleting or renaming a rule fails the test until its
// entry here records the decision.
var shippedAlerts = map[string]string{
	"GrelotDown":              "",
	"GrelotSweepStalled":      "",
	"GrelotStoreStartedEmpty": "",
	"GrelotStoreNearlyFull":   "",
	"GrelotStoreFull":         "",
	"GrelotServerErrors":      "",
	"GrelotPushFailing":       "",
	"GrelotPushDoorOff":       "",
	"GrelotProviderInvalid":   "",
}

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert string `yaml:"alert"`
			Expr  string `yaml:"expr"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

func TestAlerts_everyRuleReadsASeriesTheServerExports(t *testing.T) {
	exported := servedSeries(t)
	rules := readRules(t)
	for alert, expr := range rules {
		t.Run(alert, func(t *testing.T) {
			names := metricNames(expr)
			if len(names) == 0 {
				t.Fatalf("no metric name found in %q", expr)
			}
			for _, name := range names {
				// up is the scraper's own series, never on the page.
				if name != "up" && !exported[name] {
					t.Errorf("reads %s, which /metrics does not serve", name)
				}
			}
		})
	}
}

func TestAlerts_aShippedAlertLeavesOnlyByARecordedDecision(t *testing.T) {
	rules := readRules(t)
	for alert := range rules {
		if _, ok := shippedAlerts[alert]; !ok {
			t.Errorf("%s is not in shippedAlerts", alert)
		}
	}
	for alert, now := range shippedAlerts {
		_, inFile := rules[alert]
		switch {
		case now == "" && !inFile:
			t.Errorf("%s left alerts/promql.yaml; record its successor or its removal in shippedAlerts", alert)
		case now != "" && inFile:
			t.Errorf("%s is in the file but recorded as %q", alert, now)
		case now != "" && !strings.HasPrefix(now, "removed: "):
			if _, ok := rules[now]; !ok {
				t.Errorf("%s's successor %s is not in the file", alert, now)
			}
		}
	}
}

// servedSeries builds the store and server as serve does, records one
// sample in every labelled family, since a family with none prints no
// series, and returns the names /metrics serves.
func servedSeries(t *testing.T) map[string]bool {
	t.Helper()
	m := obs.New()
	st, srv, err := build(testConfig(t), slog.New(slog.DiscardHandler), m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Release()
		st.Close()
	})
	const door = "provider/door"
	m.Posts.Inc("announce")
	m.PushDoorUp.Set(1, door)
	m.PushSends.Inc(door, obs.SendAccepted)
	m.Nudges.Inc(string(store.NudgeByWatcher), obs.AttemptFirst)
	m.NudgeDelay.Observe(1, string(store.NudgeByWatcher))
	m.RecordHTTP(httpserver.RequestMetric{Path: "/v1/me", Status: http.StatusOK, Latency: time.Millisecond})

	rec := httptest.NewRecorder()
	srv.HTTPServer().Handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: %d", rec.Code)
	}
	out := map[string]bool{}
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line[:strings.IndexAny(line, "{ ")]] = true
	}
	return out
}

// readRules returns alerts/promql.yaml's expressions by alert name.
func readRules(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("alerts/promql.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var f ruleFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, g := range f.Groups {
		for _, r := range g.Rules {
			if _, dup := out[r.Alert]; dup {
				t.Errorf("alert %s twice", r.Alert)
			}
			out[r.Alert] = r.Expr
		}
	}
	if len(out) == 0 {
		t.Fatal("no rules in alerts/promql.yaml")
	}
	return out
}

var (
	promString   = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	promMatchers = regexp.MustCompile(`\{[^}]*\}|\[[^\]]*\]`)
	promLabels   = regexp.MustCompile(`\b(?:by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`)
	promToken    = regexp.MustCompile(`[A-Za-z_:][A-Za-z0-9_:]*(\s*\()?|[0-9][0-9A-Za-z_.]*`)
	promKeywords = map[string]bool{
		"and": true, "or": true, "unless": true, "offset": true, "bool": true,
		"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true,
		"inf": true, "nan": true,
	}
)

// metricNames returns the metric names expr reads, sorted: every name
// that is not a function call, a keyword, a label or a number.
func metricNames(expr string) []string {
	expr = promString.ReplaceAllString(expr, `""`)
	expr = promMatchers.ReplaceAllString(expr, " ")
	expr = promLabels.ReplaceAllString(expr, " ")
	names := map[string]bool{}
	for _, m := range promToken.FindAllStringSubmatch(expr, -1) {
		tok := m[0]
		if m[1] != "" || tok[0] >= '0' && tok[0] <= '9' || promKeywords[strings.ToLower(tok)] {
			continue
		}
		names[tok] = true
	}
	return slices.Sorted(maps.Keys(names))
}
