package server

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

const (
	repoToolbelt = "/workspace/toolbelt"
	repoWTK      = "/workspace/web-terminal-kiro"
	repoDocs     = "/workspace/docs"
	engineGo     = repoToolbelt + "/engine.go"
	guideMD      = repoDocs + "/guide.md"
)

func sessOf(name string) string { return "kiro-cli:session-" + name }

// whoHarness registers Ruling 13's agents: toolbelt-dev holding
// engine.go with one delegate, wtk-ui and wtk-server on
// web-terminal-kiro (wtk-server in another environment), and docs-writer
// holding guide.md, queued for engine.go and following the
// web-terminal-kiro thread.
func whoHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	reg := func(name, desc, repo, env string) {
		b := registerBody(name, "6h", repo)
		b["description"], b["where"] = desc, where(env)
		if r := h.as(sessOf(name), http.MethodPost, "/v1/register", b); r.status != http.StatusCreated {
			t.Fatalf("register %s: %d %s", name, r.status, r.raw)
		}
	}
	reg("toolbelt-dev", "adding the asset chooser", repoToolbelt, "a")
	reg("wtk-ui", "styling the terminal panel", repoWTK, "a")
	reg("wtk-server", "fixing the pty resize", repoWTK, "b")
	reg("docs-writer", "writing the install guide", repoDocs, "a")
	ctx := t.Context()
	reserve := func(name, path string, queue bool) {
		if _, err := h.store.Reserve(ctx, &store.Reserving{
			Caller: store.Caller{Session: sessOf(name)}, Path: path, Reason: "editing", TTL: time.Hour, Queue: queue,
		}); err != nil {
			t.Fatalf("Reserve(%s, %s): %v", name, path, err)
		}
	}
	reserve("toolbelt-dev", engineGo, false)
	reserve("docs-writer", guideMD, false)
	reserve("docs-writer", engineGo, true)
	nc := store.Fields{Name: store.NoChange, Description: store.NoChange, Paths: []string{store.NoChange}}
	if _, err := h.store.Post(ctx, &store.Posting{
		Caller: store.Caller{Session: sessOf("docs-writer")}, Subject: "install guide", Body: "a body",
		To: store.Target{Path: repoWTK}, Fields: nc,
	}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	g, err := h.store.Delegate(ctx, &store.Grant{Caller: store.Caller{Session: sessOf("toolbelt-dev")}, Label: "review", TTL: time.Hour})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if _, err := h.store.Register(ctx, &store.Registration{
		Session: sessOf("toolbelt-dev"), Link: g.Link, Fields: store.Fields{Description: "reviewing the chooser"},
	}); err != nil {
		t.Fatalf("Register delegate: %v", err)
	}
	return h
}

// entries decodes the identities list by name.
func entries(t *testing.T, r *response) map[string]map[string]any {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("GET /v1/identities: %d %s", r.status, r.raw)
	}
	list, _ := r.body["identities"].([]any)
	out := map[string]map[string]any{}
	for _, e := range list {
		m, _ := e.(map[string]any)
		name, _ := m["name"].(string)
		out[name] = m
	}
	return out
}

func sortedKeys(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }

func TestWho_listsEveryAgentWithItsDeclaredWork(t *testing.T) {
	h := whoHarness(t)
	// Ruling 13's entry, plus state, which marks a dormant one under all=1.
	wantKeys := []string{
		"arch", "delegates", "description", "expires_at", "follows", "host", "last_seen",
		"name", "online", "os", "paths", "queued", "relation", "reservations", "state",
	}
	for _, tc := range []struct {
		name     string
		headers  map[string]string
		relation string
	}{
		{"from the third agent", map[string]string{headerSession: sessOf("docs-writer")}, "same environment"},
		{"from a session with no identity", map[string]string{headerSession: "kiro-cli:nobody"}, "unknown"},
		{"with no session at all", nil, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := h.request(http.MethodGet, "/v1/identities", nil, tc.headers)
			got := entries(t, r)
			if names := slices.Sorted(maps.Keys(got)); !slices.Equal(names, []string{"docs-writer", "toolbelt-dev", "wtk-server", "wtk-ui"}) {
				t.Fatalf("identities %v, want the four agents", names)
			}
			for name, e := range got {
				if keys := sortedKeys(e); !slices.Equal(keys, wantKeys) {
					t.Errorf("%s has fields %v, want %v", name, keys, wantKeys)
				}
			}
			tool, docs := got["toolbelt-dev"], got["docs-writer"]
			if rel := tool["relation"]; rel != tc.relation {
				t.Errorf("toolbelt-dev relation %v, want %s", rel, tc.relation)
			}
			if res := tool["reservations"]; !jsonHas(res, "path", engineGo) || !jsonHas(res, "queue", float64(1)) {
				t.Errorf("toolbelt-dev reservations %v, want engine.go with a queue of 1", res)
			}
			if q := docs["queued"]; !jsonHas(q, "path", engineGo) || !jsonHas(q, "place", float64(1)) {
				t.Errorf("docs-writer queued %v, want engine.go at place 1", q)
			}
			if res := docs["reservations"]; !jsonHas(res, "path", guideMD) || !jsonHas(res, "queue", float64(0)) {
				t.Errorf("docs-writer reservations %v, want guide.md with no queue", res)
			}
			if f := docs["follows"]; !slices.Equal(anyStrings(f), []string{repoWTK}) {
				t.Errorf("docs-writer follows %v, want [%s]", f, repoWTK)
			}
			ds, _ := tool["delegates"].([]any)
			if len(ds) != 1 {
				t.Fatalf("toolbelt-dev delegates %v, want one", tool["delegates"])
			}
			d, _ := ds[0].(map[string]any)
			if keys := sortedKeys(d); !slices.Equal(keys, []string{"description", "last_seen", "name", "online"}) {
				t.Errorf("delegate fields %v, want name, description, online and last_seen", keys)
			}
			if d["name"] != "toolbelt-dev/review" || d["description"] != "reviewing the chooser" {
				t.Errorf("delegate %v, want toolbelt-dev/review reviewing the chooser", d)
			}
			for _, leak := range []string{strings.Repeat("a", 32), strings.Repeat("1", 32), "kiro-cli:session", "allow_", "scope"} {
				if strings.Contains(r.raw, leak) {
					t.Errorf("the list carries %q: %s", leak, r.raw)
				}
			}
		})
	}
}

func TestWho_filtersCombineWithAnd(t *testing.T) {
	h := whoHarness(t)
	now := h.clk.now()
	ui, server := h.idOf(sessOf("wtk-ui")), h.idOf(sessOf("wtk-server"))
	h.srv.presence.opened(ui)
	h.srv.presence.closed(ui, now.Add(-91*time.Second))
	h.srv.presence.opened(server)
	h.srv.presence.closed(server, now.Add(-30*time.Second))
	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		{"the repo exactly", "repo=web-terminal-kiro", []string{"wtk-server", "wtk-ui"}},
		{"never a repo name prefix", "repo=web-terminal", nil},
		{"the holder of a file", "holds=" + engineGo, []string{"toolbelt-dev"}},
		{"the holder of its folder", "holds=" + repoToolbelt, []string{"toolbelt-dev"}},
		{"the waiter", "queued=" + engineGo, []string{"docs-writer"}},
		{"repo and text", "repo=web-terminal-kiro&q=pty", []string{"wtk-server"}},
		{"a path overlapping work paths or a follow", "path=" + repoWTK + "/src", []string{"docs-writer", "wtk-server", "wtk-ui"}},
		{"online drops a wait closed over 90 s ago", "online=1", []string{"wtk-server"}},
		{"here keeps the caller's environment", "here=1", []string{"docs-writer", "toolbelt-dev", "wtk-ui"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := entries(t, h.as(sessOf("docs-writer"), http.MethodGet, "/v1/identities?"+tc.query, nil))
			if names := slices.Sorted(maps.Keys(got)); !slices.Equal(names, tc.want) {
				t.Errorf("%s: %v, want %v", tc.query, names, tc.want)
			}
		})
	}
}

func TestWho_fieldsKeepsTheNamedOnesAndName(t *testing.T) {
	h := whoHarness(t)
	got := entries(t, h.as(sessOf("docs-writer"), http.MethodGet, "/v1/identities?fields=description", nil))
	if len(got) != 4 {
		t.Fatalf("%d identities, want 4", len(got))
	}
	for name, e := range got {
		if keys := sortedKeys(e); !slices.Equal(keys, []string{"description", "name"}) {
			t.Errorf("%s has %v, want name and description alone", name, keys)
		}
	}
}

func TestWho_refusals(t *testing.T) {
	h := whoHarness(t)
	for _, tc := range []struct {
		name, query string
		parts       []string
	}{
		{"an unknown field", "fields=description,bogus", []string{`"bogus"`, "valid fields are", "reservations", "queued", "delegates"}},
		{"a non-canonical holds", "holds=" + repoToolbelt + "/../docs", []string{"holds", "/workspace/<repo>"}},
		{"a relative path", "path=toolbelt", []string{"path", "/workspace/<repo>"}},
		{"a repo with a slash", "repo=cplieger/docs", []string{"repo", "/workspace/<repo>"}},
		{"a flag other than 0 or 1", "online=yes", []string{"online must be 1 or 0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := h.as(sessOf("docs-writer"), http.MethodGet, "/v1/identities?"+tc.query, nil)
			if r.status != http.StatusBadRequest {
				t.Fatalf("%s: %d %s, want 400", tc.query, r.status, r.raw)
			}
			for _, p := range tc.parts {
				if !strings.Contains(r.str("error"), p) {
					t.Errorf("%s: error %q lacks %q", tc.query, r.str("error"), p)
				}
			}
			if r.str("path_root") != DefaultPathRoot {
				t.Errorf("%s: path_root %q, want %s", tc.query, r.str("path_root"), DefaultPathRoot)
			}
		})
	}
}

func TestRegister_twoRemotesNamedDocsAre409(t *testing.T) {
	h := newHarness(t)
	docs := func(name, id, remote string) map[string]any {
		w := where("a")
		w["checkouts"] = []any{map[string]any{"repo": repoDocs, "id": strings.Repeat(id, 32), "remote": strings.Repeat(remote, 32)}}
		b := registerBody(name, "6h", repoDocs)
		b["where"] = w
		return b
	}
	if r := h.as(sessA, http.MethodPost, "/v1/register", docs("docs-a", "1", "e")); r.status != http.StatusCreated {
		t.Fatalf("register docs-a: %d %s", r.status, r.raw)
	}
	r := h.as(sessB, http.MethodPost, "/v1/register", docs("docs-b", "2", "f"))
	want := "/workspace/docs is another repository for docs-a; give one of them another name with `repos:` in your client config"
	if r.status != http.StatusConflict || r.str("error") != want || r.str("code") != "other_repository" {
		t.Errorf("got %d %s, want 409 %q", r.status, r.raw, want)
	}
	if r.str("path_root") != DefaultPathRoot {
		t.Errorf("path_root %q, want %s", r.str("path_root"), DefaultPathRoot)
	}
}

// jsonHas reports whether the JSON list v holds an object whose key is
// want.
func jsonHas(v any, key string, want any) bool {
	list, _ := v.([]any)
	return slices.ContainsFunc(list, func(e any) bool {
		m, _ := e.(map[string]any)
		return m[key] == want
	})
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}
