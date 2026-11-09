//go:build !windows

package cli

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/client"
)

func TestWho_jsonIsTheServersAnswer(t *testing.T) {
	ts := newServer(t)
	registerAgent(t, ts, "s-1")
	viewer := newAgent(t, ts, "", "")
	delete(viewer.vars, "ALPHA_ID")
	out := viewer.must("who", "--json", "--fields", "name,description")
	if out != ts.lastAnswer(t) {
		t.Errorf("who --json printed %q, want the server's body %q unchanged", out, ts.lastAnswer(t))
	}
	var got map[string][]map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("who --json is not one JSON object: %v", err)
	}
	if len(got["identities"]) != 1 || got["identities"][0]["description"] != "testing the client" {
		t.Errorf("identities = %v, want toolbelt-dev with its description", got["identities"])
	}
}

func TestWho_plainPrintsOneBlockPerIdentity(t *testing.T) {
	ts := newServer(t)
	registerAgent(t, ts, "s-1")
	ts.call(t, "alpha:s-1", false, &client.Request{Method: http.MethodPost, Path: "/v1/posts", Body: map[string]any{
		"to": map[string]string{"path": "/workspace/docs"}, "subject": "Question", "body": "hello",
		"identity": map[string]any{"name": noChange, "paths": []string{noChange}, "description": noChange},
	}}, nil)
	ts.call(t, "alpha:s-1", false, &client.Request{
		Method: http.MethodPut, Path: "/v1/follows",
		Query: url.Values{"path": {"/workspace/docs"}},
	}, nil)
	ts.call(t, "alpha:s-1", false, &client.Request{
		Method: http.MethodPost, Path: "/v1/reservations",
		Body: map[string]any{"path": "/workspace/toolbelt/engine.go", "reason": "editing", "ttl": "30m"},
	}, nil)
	var grant struct {
		Link string `json:"link"`
	}
	ts.call(t, "alpha:s-1", true, &client.Request{
		Method: http.MethodPost, Path: "/v1/delegates",
		Body: map[string]any{"label": "review", "ttl": "1h"},
	}, &grant)
	ts.call(t, "alpha:s-1", false, &client.Request{
		Method: http.MethodPost, Path: "/v1/register",
		Body: map[string]any{"delegate": grant.Link, "description": "reviewing the diff"},
	}, nil)

	ts.clock.advance(time.Minute)
	b := newAgent(t, ts, "s-2", "")
	b.must("register", "zeta-dev", "--paths", "/workspace/zeta", "--desc", "zeta", "--ttl", "1h")
	out := b.must("who")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if i := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "@zeta-dev ") }); i > 0 {
		// The reader's own entry is its own text, so it carries no label.
		if own := lines[i]; strings.Contains(own, dataLabel) {
			t.Errorf("own entry = %q, want no label", own)
		}
		lines = lines[:i]
	}
	want := []string{
		"@toolbelt-dev · box · " + goosArch() + " · same machine, other environment · last seen 2026-11-05 09:00 UTC" + dataLabel,
		"  description: testing the client",
		"  paths: /workspace/toolbelt",
		"  follows: /workspace/docs",
		"  holds: /workspace/toolbelt/engine.go until 2026-11-05 09:30 UTC, 0 queued",
		"    @toolbelt-dev/review · online · reviewing the diff",
	}
	if len(lines) != len(want)+1 || !strings.HasPrefix(lines[0], "grelot: ") {
		t.Fatalf("who printed:\n%s\nwant the status line, then:\n%s", out, strings.Join(want, "\n"))
	}
	for i, w := range want {
		if lines[i+1] != w {
			t.Errorf("line %d = %q, want %q", i+1, lines[i+1], w)
		}
	}
}

func TestWho_flagsMapToTheQuery(t *testing.T) {
	ts := newServer(t)
	_, clone := registerAgent(t, ts, "s-1")
	b := newAgent(t, ts, "s-2", "")
	b.must("who", "tool", "dev", "--repo", "toolbelt", "--path", clone+"/internal", "--holds", clone+"/engine.go",
		"--queued", clone+"/go.mod", "--online", "--here", "--all", "--fields", "name,paths")
	got, err := url.ParseQuery(strings.SplitN(ts.last(t, http.MethodGet, "/v1/identities").URL, "?", 2)[1])
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"q": "tool dev", "repo": "toolbelt", "path": "/workspace/toolbelt/internal",
		"holds": "/workspace/toolbelt/engine.go", "queued": "/workspace/toolbelt/go.mod",
		"online": "1", "here": "1", "all": "1", "fields": "name,paths",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, got.Get(k), v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("query = %v, want only %v", got, want)
	}
}

func TestWho_aSessionWithNoIdentitySeesTheList(t *testing.T) {
	ts := newServer(t)
	registerAgent(t, ts, "s-1")
	out := newAgent(t, ts, "s-new", "").must("who")
	if !strings.Contains(out, "@toolbelt-dev · box · "+goosArch()+" · unknown · ") {
		t.Errorf("who = %q, want toolbelt-dev with relation unknown", out)
	}
}

func TestWho_noMatch(t *testing.T) {
	ts := newServer(t)
	registerAgent(t, ts, "s-1")
	if out := newAgent(t, ts, "s-2", "").must("who", "--repo", "web-terminal"); !strings.Contains(out, "grelot: no identity matches\n") {
		t.Errorf("who = %q, want the no-match line", out)
	}
}
