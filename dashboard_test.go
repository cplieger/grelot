//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/go/slogx"
	"github.com/cplieger/grelot/internal/obs"
)

// dashQuery is one query of grafana-dashboard.json: a panel's or a
// variable's.
type dashQuery struct {
	where, group, expr string
}

type dashboard struct {
	panels    map[string]map[string]any
	queries   []dashQuery
	attention []any
	preload   bool
}

func readDashboard(t *testing.T) dashboard {
	t.Helper()
	raw, err := os.ReadFile("grafana-dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Spec struct {
			Elements map[string]struct {
				Spec map[string]any `json:"spec"`
			} `json:"elements"`
			Variables []struct {
				Kind string         `json:"kind"`
				Spec map[string]any `json:"spec"`
			} `json:"variables"`
			Layout  map[string]any `json:"layout"`
			Preload bool           `json:"preload"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	d := dashboard{panels: map[string]map[string]any{}, preload: doc.Spec.Preload}
	for name, el := range doc.Spec.Elements {
		d.panels[name] = el.Spec
		for _, q := range list(dig(el.Spec, "data", "spec", "queries")) {
			query := dig(q, "spec", "query")
			d.queries = append(d.queries, dashQuery{
				where: name + " " + str(el.Spec["title"]),
				group: str(dig(query, "group")),
				expr:  str(dig(query, "spec", "expr")),
			})
		}
	}
	for _, v := range doc.Spec.Variables {
		if v.Kind == "QueryVariable" {
			d.queries = append(d.queries, dashQuery{
				where: "variable " + str(v.Spec["name"]),
				group: str(dig(v.Spec, "query", "group")),
				expr:  str(dig(v.Spec, "query", "spec", "query")),
			})
		}
	}
	for _, tab := range list(dig(doc.Spec.Layout, "spec", "tabs")) {
		for _, row := range list(dig(tab, "spec", "layout", "spec", "rows")) {
			if str(dig(row, "spec", "title")) == "Needs attention" {
				d.attention = list(dig(row, "spec", "layout", "spec", "items"))
			}
		}
	}
	return d
}

func TestDashboard_everyMetricsQueryReadsASeriesTheServerExports(t *testing.T) {
	exported := servedSeries(t)
	d := readDashboard(t)
	var prom int
	for _, q := range d.queries {
		if q.group != "prometheus" {
			continue
		}
		prom++
		names := metricNames(q.expr)
		if len(names) == 0 {
			t.Errorf("%s: no metric name in %q", q.where, q.expr)
		}
		for _, name := range names {
			// ALERTS is the ruler's own series for the shipped rules.
			if name != "ALERTS" && !deployedPageServes(exported, name) {
				t.Errorf("%s reads %s, which /metrics does not serve", q.where, name)
			}
		}
	}
	if prom < 20 {
		t.Fatalf("%d metrics queries found, want every panel's", prom)
	}
}

var (
	alertSelector = regexp.MustCompile(`ALERTS\{([^}]*)\}`)
	alertName     = regexp.MustCompile(`alertname="([^"]+)"`)
)

// firingAlerts returns the alert names expr reads, failing when a
// selector is not the firing state of one named alert.
func firingAlerts(t *testing.T, where, expr string) []string {
	t.Helper()
	var out []string
	for _, m := range alertSelector.FindAllStringSubmatch(expr, -1) {
		name := alertName.FindStringSubmatch(m[1])
		if name == nil || !strings.Contains(m[1], `alertstate="firing"`) {
			t.Errorf("%s: %s is not one alert's firing series", where, m[0])
			continue
		}
		out = append(out, name[1])
	}
	return out
}

func TestDashboard_attentionTilesMirrorEveryShippedAlert(t *testing.T) {
	rules := readRules(t)
	want := slices.Sorted(maps.Keys(rules))
	d := readDashboard(t)

	var tiles []string
	for _, item := range d.attention {
		name := str(dig(item, "spec", "element", "name"))
		p := d.panels[name]
		queries := list(dig(p, "data", "spec", "queries"))
		if len(queries) != 1 {
			t.Errorf("tile %s has %d queries, want 1", name, len(queries))
			continue
		}
		alerts := firingAlerts(t, name, str(dig(queries[0], "spec", "query", "spec", "expr")))
		if len(alerts) != 1 {
			t.Errorf("tile %s reads alerts %v, want exactly one", name, alerts)
		}
		tiles = append(tiles, alerts...)
	}
	slices.Sort(tiles)
	if !slices.Equal(tiles, want) {
		t.Errorf("attention tiles mirror %v, want one per rule in alerts/promql.yaml: %v", tiles, want)
	}

	for _, q := range d.queries {
		if q.where == "variable attention" {
			got := firingAlerts(t, q.where, q.expr)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("the attention count reads %v, want %v, so the row shows exactly when a tile does", got, want)
			}
			return
		}
	}
	t.Error("no attention variable")
}

// A has-data rule is undecided while its query loads, and an undecided
// group shows its item, so each tile also needs an always-false rule, a
// pinned maxDataPoints and the dashboard's preload (grafana/grafana#134713).
func TestDashboard_attentionTilesStartHidden(t *testing.T) {
	d := readDashboard(t)
	if !d.preload {
		t.Error("preload is off, so a tile below the fold never queries and never hides")
	}
	if len(d.attention) == 0 {
		t.Fatal("no Needs attention tiles")
	}
	for _, item := range d.attention {
		name := str(dig(item, "spec", "element", "name"))
		group := dig(item, "spec", "conditionalRendering", "spec")
		var data, never bool
		for _, rule := range list(dig(group, "items")) {
			switch str(dig(rule, "kind")) {
			case "ConditionalRenderingData":
				data = dig(rule, "spec", "value") == true
			case "ConditionalRenderingTimeRangeSize":
				never = str(dig(rule, "spec", "value")) == "0s"
			}
		}
		if str(dig(group, "condition")) != "or" || !data || !never {
			t.Errorf("tile %s: rules %v, want has-data or a 0s time range", name, group)
		}
		if dig(d.panels[name], "data", "spec", "queryOptions", "maxDataPoints") == nil {
			t.Errorf("tile %s pins no maxDataPoints", name)
		}
	}
}

var (
	lokiMsg   = regexp.MustCompile(`\| json \| msg="([^"]+)"`)
	lokiField = regexp.MustCompile(`\{\{\.(\w+)\}\}`)
)

func TestDashboard_latestMessagesReadsTheFieldsThePostLineCarries(t *testing.T) {
	var expr string
	for _, q := range readDashboard(t).queries {
		if q.group == "loki" {
			if expr != "" {
				t.Fatalf("two Loki queries; this test reads one")
			}
			expr = q.expr
		}
	}
	msg := lokiMsg.FindStringSubmatch(expr)
	fields := lokiField.FindAllStringSubmatch(expr, -1)
	if msg == nil || len(fields) == 0 {
		t.Fatalf("Loki query %q selects no msg or shows no field", expr)
	}

	line := postedLine(t, msg[1])
	for _, f := range fields {
		if _, ok := line[f[1]]; !ok {
			t.Errorf("the panel shows %s, which the %q line does not carry: %v", f[1], msg[1], line)
		}
	}
}

// postedLine registers an agent, posts to the announcement thread through
// the server serve builds, logging as serve does, and returns the one log
// line whose msg is msg.
func postedLine(t *testing.T, msg string) map[string]any {
	t.Helper()
	var logs bytes.Buffer
	h, _ := slogx.NewHandler(slogx.Options{Output: &logs, Format: slogx.JSON})
	st, srv, err := build(testConfig(t), slog.New(h), obs.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Release()
		st.Close()
	})
	where := `"where": {"machine": "dmi:` + strings.Repeat("1", 32) + `", "environment": "` + strings.Repeat("a", 32) + `", "checkouts": []}`
	for _, call := range []struct{ path, body string }{
		{"/v1/register", `{"name": "builder", "paths": ["/workspace/app"], "description": "building", "host": "h", "os": "linux", "arch": "amd64", "ttl": "1h", ` + where + `}`},
		{"/v1/posts", `{"to": "announce", "subject": "release", "body": "tagged", "identity": {"name": "no change", "description": "no change", "paths": ["no change"]}}`},
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, call.path, strings.NewReader(call.body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Grelot-Version", "v1.1.0")
		req.Header.Set("Grelot-Session", "host:session-1")
		rec := httptest.NewRecorder()
		srv.HTTPServer().Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST %s: %d %s", call.path, rec.Code, rec.Body)
		}
	}
	var found []map[string]any
	for raw := range strings.SplitSeq(logs.String(), "\n") {
		var line map[string]any
		if json.Unmarshal([]byte(raw), &line) == nil && line["msg"] == msg {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %q lines, want 1; logs: %s", len(found), msg, logs.String())
	}
	return found[0]
}

func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
