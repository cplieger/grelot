//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	lokiMsg      = regexp.MustCompile(`\| msg="([^"]+)"`)
	lokiJSONArgs = regexp.MustCompile(`\| json ((?:\w+="\w+"(?:, )?)+)`)
	lokiJSONPair = regexp.MustCompile(`(\w+)="(\w+)"`)
	lokiTemplate = regexp.MustCompile(`\{\{[^}]*\}\}`)
	lokiTmplRef  = regexp.MustCompile(`\.(\w+)`)
	lokiUnwrap   = regexp.MustCompile(`\| unwrap (\w+)`)
	lokiBy       = regexp.MustCompile(`\bby \(([\w, ]+)\)`)
	lokiMade     = regexp.MustCompile(`\| label_format (\w+)=`)
)

// Every Loki query reads one log line the server writes, and every label it
// groups by, unwraps or shows is a field of that line, or one the query
// makes from such fields.
func TestDashboard_everyLokiQueryReadsFieldsItsLineCarries(t *testing.T) {
	lines := serverLines(t)
	var n int
	for _, q := range readDashboard(t).queries {
		if q.group != "loki" {
			continue
		}
		n++
		t.Run(q.where, func(t *testing.T) {
			msgs := lokiMsg.FindAllStringSubmatch(q.expr, -1)
			if len(msgs) != 1 {
				t.Fatalf("query %q selects %d msg values, want 1", q.expr, len(msgs))
			}
			line, ok := lines[msgs[0][1]]
			if !ok {
				t.Fatalf("query %q reads %q lines, which the server never wrote; it wrote %v", q.expr, msgs[0][1], slices.Sorted(maps.Keys(lines)))
			}
			labels := map[string]bool{}
			if args := lokiJSONArgs.FindStringSubmatch(q.expr); args != nil {
				for _, pair := range lokiJSONPair.FindAllStringSubmatch(args[1], -1) {
					if _, ok := line[pair[2]]; !ok {
						t.Errorf("json extracts %s from %s, which the %q line does not carry: %v", pair[1], pair[2], msgs[0][1], line)
					}
					labels[pair[1]] = true
				}
			} else {
				for f := range line {
					labels[f] = true
				}
			}
			for _, m := range lokiMade.FindAllStringSubmatch(q.expr, -1) {
				labels[m[1]] = true
			}
			var used []string
			for _, tmpl := range lokiTemplate.FindAllString(q.expr, -1) {
				for _, ref := range lokiTmplRef.FindAllStringSubmatch(tmpl, -1) {
					used = append(used, ref[1])
				}
			}
			for _, u := range lokiUnwrap.FindAllStringSubmatch(q.expr, -1) {
				used = append(used, u[1])
			}
			for _, by := range lokiBy.FindAllStringSubmatch(q.expr, -1) {
				for l := range strings.SplitSeq(by[1], ",") {
					used = append(used, strings.TrimSpace(l))
				}
			}
			for _, u := range used {
				if !labels[u] {
					t.Errorf("query %q uses %s, which neither the %q line carries nor the query makes", q.expr, u, msgs[0][1])
				}
			}
		})
	}
	if n == 0 {
		t.Fatal("the dashboard has no Loki query")
	}
}

// serverLines drives the server serve builds, logging as serve does, through
// a post to each kind of thread, a held turn and a read inbox, and returns
// the last log line of each msg.
func serverLines(t *testing.T) map[string]map[string]any {
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
	keep := `"identity": {"name": "no change", "description": "no change", "paths": ["no change"]}`
	var keys int
	call := func(session, method, path, body string, want int) []byte {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Grelot-Version", "v1.1.0")
		req.Header.Set("Grelot-Session", "host:"+session)
		if method == http.MethodPost && path != "/v1/events/hold" {
			keys++
			req.Header.Set("Idempotency-Key", fmt.Sprintf(`"key-for-dashboard-test-%04d"`, keys))
		}
		rec := httptest.NewRecorder()
		srv.HTTPServer().Handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
		}
		return rec.Body.Bytes()
	}
	for _, name := range []string{"builder", "reader"} {
		call(name, http.MethodPost, "/v1/register", `{"name": "`+name+`", "paths": ["/workspace/app"], "description": "building", "host": "h", "os": "linux", "arch": "amd64", "ttl": "1h", `+where+`}`, http.StatusCreated)
	}
	for _, to := range []string{`"announce"`, `{"path": "/workspace/app/main.go"}`, `{"agent": "reader"}`} {
		call("builder", http.MethodPost, "/v1/posts", `{"to": `+to+`, "subject": "release", "body": "tagged", `+keep+`}`, http.StatusCreated)
	}
	call("reader", http.MethodPost, "/v1/events/hold", `{"for": "stop"}`, http.StatusOK)
	var page struct {
		Entries []struct {
			Seq uint64 `json:"seq"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(call("reader", http.MethodGet, "/v1/inbox", "", http.StatusOK), &page); err != nil || len(page.Entries) == 0 {
		t.Fatalf("inbox page %+v, %v; want entries", page, err)
	}
	call("reader", http.MethodPost, "/v1/inbox/ack", fmt.Sprintf(`{"upto": %d}`, page.Entries[len(page.Entries)-1].Seq), http.StatusOK)

	lines := map[string]map[string]any{}
	for raw := range strings.SplitSeq(logs.String(), "\n") {
		var line map[string]any
		if json.Unmarshal([]byte(raw), &line) == nil {
			if msg, ok := line["msg"].(string); ok {
				lines[msg] = line
			}
		}
	}
	return lines
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
