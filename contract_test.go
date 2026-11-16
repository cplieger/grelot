//go:build !windows

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/wire"
)

// The scenario's sessions, one per identity it acts as.
const (
	sessAlpha = "kiro-cli:contract-alpha"
	sessBeta  = "kiro-cli:contract-beta"
	sessGamma = "kiro-cli:contract-gamma"
	sessDelta = "kiro-cli:contract-delta"
	repoPath  = "/workspace/grelot"
	otherPath = "/workspace/other"
)

const (
	hourMS    = int64(time.Hour / time.Millisecond)
	halfMS    = int64(30 * time.Minute / time.Millisecond)
	shortMS   = int64(15 * time.Minute / time.Millisecond)
	maxBodyKB = 128
)

// scenario is design section 7 proof 1's state: two keys, the identities
// acting under them and what earlier steps made.
type scenario struct {
	c                         *contract
	alpha, beta, gamma, delta *agent
	k1                        string
	direct                    int64
	held                      int64
	unreadSeq                 int64
}

// scenarioStep is one case of the agent scenario: it calls the agent row
// named row at least once, so the step list is the suite's coverage.
type scenarioStep struct {
	run  func(*scenario)
	row  string
	what string
}

var agentScenario = []scenarioStep{
	{row: "register", what: "two keys register four identities, once again, and from another provider", run: func(s *scenario) {
		t := s.c.t
		r := s.alpha.call(http.StatusCreated, "register", "/v1/register", s.alpha.registerBody("alpha", hourMS, repoPath))
		if got := num(obj(r.body, "identity"), "ttl_ms"); got != hourMS {
			t.Errorf("register: identity.ttl_ms %d, want the %d sent", got, hourMS)
		}
		s.alpha.call(http.StatusOK, "register", "/v1/register", s.alpha.registerBody("alpha", hourMS, repoPath))
		s.beta.call(http.StatusCreated, "register", "/v1/register", s.beta.registerBody("beta", hourMS, repoPath))
		s.gamma.call(http.StatusCreated, "register", "/v1/register", s.gamma.registerBody("gamma", hourMS, otherPath))
		s.delta.call(http.StatusCreated, "register", "/v1/register", s.delta.registerBody("delta", shortMS, otherPath))
		other := s.c.agent(s.k1, "claude-code:contract-alpha", "a")
		body := other.registerBody("alpha", hourMS, repoPath)
		body["take_over"] = true
		if r := other.call(http.StatusConflict, "register", "/v1/register", body); r.str("code") != "other_provider" {
			t.Errorf("a take-over from another provider: code %q, want other_provider", r.str("code"))
		}
	}},
	{row: "getMe", what: "alpha reads itself", run: func(s *scenario) {
		r := s.alpha.call(http.StatusOK, "getMe", "/v1/me", nil)
		if got := obj(r.body, "identity")["name"]; got != "alpha" {
			s.c.t.Errorf("GET /v1/me: identity.name %v, want alpha", got)
		}
	}},
	{row: "patchMe", what: "alpha renews with a new description", run: func(s *scenario) {
		r := s.alpha.call(http.StatusOK, "patchMe", "/v1/me", map[string]any{"description": "patched", "ttl_ms": hourMS, "where": s.alpha.where()})
		if got := num(obj(r.body, "identity"), "ttl_ms"); got != hourMS {
			s.c.t.Errorf("PATCH /v1/me: identity.ttl_ms %d, want the %d sent", got, hourMS)
		}
	}},
	{row: "getVersion", what: "the version and the path root", run: func(s *scenario) {
		r := s.alpha.call(http.StatusOK, "getVersion", "/v1/version", nil)
		if r.str("version") != contractVersion || r.str("path_root") != protopath.DefaultRoot {
			s.c.t.Errorf("GET /v1/version: %s, want version %s and path_root %s", r.raw, contractVersion, protopath.DefaultRoot)
		}
	}},
	{row: "listIdentities", what: "alpha lists the identities", run: func(s *scenario) {
		if r := s.alpha.call(http.StatusOK, "listIdentities", "/v1/identities", nil); r.items() == nil {
			s.c.t.Errorf("GET /v1/identities: no items: %s", r.raw)
		}
	}},
	{row: "createPost", what: "a path post, a direct one, an empty subject and a body past the cap", run: func(s *scenario) {
		t := s.c.t
		s.alpha.call(http.StatusCreated, "createPost", "/v1/posts", postBody(map[string]any{"path": repoPath}, "the plan", "a body"))
		r := s.alpha.call(http.StatusCreated, "createPost", "/v1/posts", postBody(map[string]any{"agent": "beta"}, "hello", "a body"))
		if s.direct = num(r.body, "message"); s.direct < 1 {
			t.Fatalf("the direct post answered no message: %s", r.raw)
		}
		r = s.alpha.call(http.StatusBadRequest, "createPost", "/v1/posts", postBody("announce", "", "a body"))
		if !hasField(r, "subject", "required") {
			t.Errorf("an empty subject: %s, want invalid with field subject, code required", r.raw)
		}
		big := postBody("announce", "big", strings.Repeat("x", (maxBodyKB+2)<<10))
		r = s.alpha.call(http.StatusRequestEntityTooLarge, "createPost", "/v1/posts", big)
		if r.str("code") != "too_large" || r.str("reason") != "request_body" || r.body["fields"] != nil {
			t.Errorf("a body past 128 KiB: %s, want too_large with reason request_body and no fields", r.raw)
		}
	}},
	{row: "listThreads", what: "alpha lists the threads", run: func(s *scenario) {
		if r := s.alpha.call(http.StatusOK, "listThreads", "/v1/threads", nil); len(r.items()) != 1 {
			s.c.t.Errorf("GET /v1/threads: %s, want the one path thread", r.raw)
		}
	}},
	{row: "getReceipts", what: "receipts as the sender, the recipient, a third identity and with no session", run: func(s *scenario) {
		t := s.c.t
		path := fmt.Sprintf("/v1/messages/%d/receipts", s.direct)
		s.alpha.call(http.StatusOK, "getReceipts", path, nil)
		s.beta.call(http.StatusOK, "getReceipts", path, nil)
		if r := s.gamma.call(http.StatusForbidden, "getReceipts", path, nil); r.str("code") != "forbidden" {
			t.Errorf("receipts as a third identity: code %q, want forbidden", r.str("code"))
		}
		r := s.alpha.callWith(http.StatusBadRequest, "getReceipts", path, nil, map[string]string{"Grelot-Session": ""})
		if r.str("code") != "invalid" {
			t.Errorf("receipts with no Grelot-Session: code %q, want invalid", r.str("code"))
		}
	}},
	{row: "getHistory", what: "alpha reads its history, and a history from or with nobody", run: func(s *scenario) {
		if r := s.alpha.call(http.StatusOK, "getHistory", "/v1/history", nil); len(r.items()) == 0 {
			s.c.t.Errorf("GET /v1/history: %s, want alpha's posts", r.raw)
		}
		for _, q := range []string{"from", "with"} {
			r := s.alpha.call(http.StatusNotFound, "getHistory", "/v1/history?"+q+"=nobody", nil)
			if r.str("code") != "not_found" || r.str("reason") != "identity" {
				s.c.t.Errorf("a history %s nobody: %s, want not_found with reason identity", q, r.raw)
			}
		}
	}},
	{row: "follow", what: "alpha follows a path", run: func(s *scenario) {
		s.alpha.call(http.StatusOK, "follow", "/v1/follows?path="+url.QueryEscape(otherPath), nil)
	}},
	{row: "unfollow", what: "alpha unfollows it, and cannot mute a path its work path delivers", run: func(s *scenario) {
		s.alpha.call(http.StatusOK, "unfollow", "/v1/follows?path="+url.QueryEscape(otherPath), nil)
		r := s.alpha.call(http.StatusConflict, "unfollow", "/v1/follows?path="+url.QueryEscape(repoPath+"/file.go"), nil)
		if r.str("code") != "conflict" {
			s.c.t.Errorf("an unfollow of a path the work path delivers: code %q, want conflict", r.str("code"))
		}
	}},
	{row: "waitEvent", what: "beta's standing wait returns the direct post's nudge; gamma's passes with none", run: func(s *scenario) {
		if r := s.beta.call(http.StatusOK, "waitEvent", "/v1/events/wait", map[string]any{"wait_ms": int64(5000)}); num(r.body, "seq") < 1 {
			s.c.t.Errorf("POST /v1/events/wait: %s, want a nudge", r.raw)
		}
		if r := s.gamma.call(http.StatusNoContent, "waitEvent", "/v1/events/wait", map[string]any{"wait_ms": int64(1000)}); len(r.raw) != 0 {
			s.c.t.Errorf("a standing wait that passes with no nudge: body %s, want none", r.raw)
		}
	}},
	{row: "readInbox", what: "beta reads its inbox", run: func(s *scenario) {
		r := s.beta.call(http.StatusOK, "readInbox", "/v1/inbox", nil)
		items := r.items()
		if len(items) == 0 {
			s.c.t.Fatalf("GET /v1/inbox: %s, want the direct post", r.raw)
		}
		s.unreadSeq = num(items[len(items)-1], "seq")
	}},
	{row: "ackInbox", what: "beta acks it", run: func(s *scenario) {
		s.beta.call(http.StatusOK, "ackInbox", "/v1/inbox/ack", map[string]any{"upto": s.unreadSeq})
	}},
	{row: "reserve", what: "alpha reserves the repo and beta queues behind it", run: func(s *scenario) {
		t := s.c.t
		r := s.alpha.call(http.StatusCreated, "reserve", "/v1/reservations", map[string]any{"path": repoPath, "reason": "a change", "ttl_ms": hourMS})
		resv := obj(r.body, "reservation")
		if got := num(resv, "ttl_ms"); got != hourMS {
			t.Errorf("reserve: reservation.ttl_ms %d, want the %d sent", got, hourMS)
		}
		if s.held = num(resv, "id"); s.held < 1 {
			t.Fatalf("reserve answered no id: %s", r.raw)
		}
		q := map[string]any{"path": repoPath, "reason": "next", "ttl_ms": hourMS, "queue": true}
		if r := s.beta.call(http.StatusCreated, "reserve", "/v1/reservations", q); obj(r.body, "reservation")["state"] != "queued" {
			t.Errorf("a reserve behind a grant with queue: %s, want a queued entry", r.raw)
		}
	}},
	{row: "listReservations", what: "the reservations unfiltered and by path, one held and one queued", run: func(s *scenario) {
		for _, path := range []string{"/v1/reservations", "/v1/reservations?path=" + url.QueryEscape(repoPath)} {
			r := s.alpha.call(http.StatusOK, "listReservations", path, nil)
			var states []string
			for _, it := range r.items() {
				states = append(states, fmt.Sprint(it["state"]))
			}
			if slices.Sort(states); !slices.Equal(states, []string{"held", "queued"}) {
				s.c.t.Errorf("GET %s: states %v, want held and queued: %s", path, states, r.raw)
			}
		}
	}},
	{row: "renewReservation", what: "alpha renews its grant", run: func(s *scenario) {
		r := s.alpha.call(http.StatusOK, "renewReservation", fmt.Sprintf("/v1/reservations/%d", s.held), map[string]any{"ttl_ms": halfMS})
		if got := num(obj(r.body, "reservation"), "ttl_ms"); got != halfMS {
			s.c.t.Errorf("renew: reservation.ttl_ms %d, want the %d sent", got, halfMS)
		}
	}},
	{row: "releaseReservation", what: "alpha releases it", run: func(s *scenario) {
		s.alpha.call(http.StatusOK, "releaseReservation", fmt.Sprintf("/v1/reservations/%d", s.held), nil)
	}},
	{row: "grantDelegate", what: "a grant allowing nobody, then alpha grants a delegate link", run: func(s *scenario) {
		stray := map[string]any{"label": "stray", "ttl_ms": hourMS, "allow_agents": []string{"nobody"}}
		if r := s.alpha.call(http.StatusNotFound, "grantDelegate", "/v1/delegates", stray); r.str("code") != "not_found" {
			s.c.t.Errorf("a grant allowing an agent nobody is: code %q, want not_found", r.str("code"))
		}
		s.alpha.call(http.StatusCreated, "grantDelegate", "/v1/delegates", map[string]any{"label": "helper", "ttl_ms": hourMS})
	}},
	{row: "listIdentities", what: "alpha's list meets its delegate gate, and a handle naming no delegate has ended", run: func(s *scenario) {
		if r := s.alpha.call(http.StatusConflict, "listIdentities", "/v1/identities", nil); r.str("code") != "delegate_gate" {
			s.c.t.Errorf("list past an unused link: code %q, want delegate_gate", r.str("code"))
		}
		r := s.alpha.callWith(http.StatusGone, "listIdentities", "/v1/identities", nil, map[string]string{"Grelot-As": "alpha/nobody"})
		if r.str("code") != "delegate_ended" {
			s.c.t.Errorf("list as a handle naming no delegate: code %q, want delegate_ended", r.str("code"))
		}
	}},
	{row: "revokeDelegate", what: "alpha revokes it, as its own command past the delegate gate", run: func(s *scenario) {
		s.alpha.callWith(http.StatusOK, "revokeDelegate", "/v1/delegates/helper", nil, map[string]string{"Grelot-Self": "1"})
	}},
	{row: "lifecycle", what: "alpha's turn ends", run: func(s *scenario) {
		s.alpha.call(http.StatusOK, "lifecycle", "/v1/lifecycle", map[string]any{"event": "turn_end"})
	}},
	{row: "holdEvent", what: "alpha's stop hold", run: func(s *scenario) {
		s.alpha.call(http.StatusOK, "holdEvent", "/v1/events/hold", map[string]any{"for": "stop"})
	}},
	{row: "deleteMe", what: "gamma retires", run: func(s *scenario) {
		s.gamma.call(http.StatusOK, "deleteMe", "/v1/me", nil)
	}},
	{row: "revive", what: "delta, dormant, revived from another environment and then its own", run: func(s *scenario) {
		t := s.c.t
		s.c.clk.advance(time.Duration(shortMS)*time.Millisecond + time.Minute)
		body := func(a *agent) map[string]any {
			return map[string]any{"name": "delta", "host": "host-" + a.env, "os": "linux", "arch": "amd64", "where": a.where(), "ttl_ms": hourMS}
		}
		away := s.c.agent(s.k1, "kiro-cli:contract-delta-away", "c")
		r := away.call(http.StatusNotFound, "revive", "/v1/revive", body(away))
		if r.str("code") != "not_found" || r.str("reason") != "identity" {
			t.Errorf("a revive from another environment: %s, want not_found with reason identity", r.raw)
		}
		home := s.c.agent(s.k1, "kiro-cli:contract-delta-home", "a")
		r = home.call(http.StatusOK, "revive", "/v1/revive", body(home))
		if got := num(obj(r.body, "identity"), "ttl_ms"); got != hourMS {
			t.Errorf("revive: identity.ttl_ms %d, want the %d sent", got, hourMS)
		}
	}},
}

// hasField reports whether a refusal's fields name field with code.
func hasField(r *answer, field, code string) bool {
	fields, _ := r.body["fields"].([]any)
	return slices.ContainsFunc(fields, func(v any) bool {
		f, _ := v.(map[string]any)
		return f["field"] == field && f["code"] == code
	})
}

func TestContract_agentRowsMatchTheDocument(t *testing.T) {
	c := newContract(t)
	k1, k2 := c.mintKey("machine-1"), c.mintKey("machine-2")
	hits := map[string]bool{}
	c.hits = hits
	s := &scenario{
		c: c, k1: k1,
		alpha: c.agent(k1, sessAlpha, "a"), beta: c.agent(k2, sessBeta, "b"),
		gamma: c.agent(k1, sessGamma, "a"), delta: c.agent(k1, sessDelta, "a"),
	}
	for _, step := range agentScenario {
		clear(hits)
		step.run(s)
		if !hits[step.row] {
			t.Errorf("the %s step (%s) never called its row", step.row, step.what)
		}
	}
}

// pagedRow is one agent list row, seeded with five items its key names.
type pagedRow struct {
	seed  func(*contract) *agent
	row   string
	path  string
	key   string
	order []string
}

// pagedRows are the agent list rows of design 1.2 (C), each with five
// items seeded out of the row's order and that order (gate note B1).
var pagedRows = []pagedRow{
	{
		row: "listIdentities", path: "/v1/identities", key: "name", order: []string{"alpha", "bravo", "charlie", "delta", "echo"},
		seed: func(c *contract) *agent {
			k := c.mintKey("machine")
			var reader *agent
			for _, name := range []string{"echo", "alpha", "delta", "charlie", "bravo"} {
				a := c.agent(k, "kiro-cli:page-"+name, "a")
				a.call(http.StatusCreated, "register", "/v1/register", a.registerBody(name, hourMS, "/workspace/"+name))
				reader = a
			}
			return reader
		},
	},
	{
		row: "listThreads", path: "/v1/threads", key: "path", order: []string{"/workspace/p/a", "/workspace/p/b", "/workspace/p/c", "/workspace/p/d", "/workspace/p/e"},
		seed: func(c *contract) *agent {
			a := c.agent(c.mintKey("machine"), sessAlpha, "a")
			a.call(http.StatusCreated, "register", "/v1/register", a.registerBody("alpha", hourMS, "/workspace/p"))
			for _, p := range []string{"e", "a", "d", "c", "b"} {
				a.call(http.StatusCreated, "createPost", "/v1/posts", postBody(map[string]any{"path": "/workspace/p/" + p}, "on "+p, "a body"))
			}
			return a
		},
	},
	// History pages go back in time, each page oldest first (gate note B1).
	{row: "getHistory", path: "/v1/history", key: "subject", order: []string{"h4", "h5", "h2", "h3", "h1"}, seed: seedDirect},
	{row: "readInbox", path: "/v1/inbox", key: "subject", order: []string{"h1", "h2", "h3", "h4", "h5"}, seed: seedDirect},
	{
		row: "listReservations", path: "/v1/reservations", key: "path", order: []string{"/workspace/r/e", "/workspace/r/a", "/workspace/r/d", "/workspace/r/c", "/workspace/r/b"},
		seed: func(c *contract) *agent {
			a := c.agent(c.mintKey("machine"), sessAlpha, "a")
			a.call(http.StatusCreated, "register", "/v1/register", a.registerBody("alpha", hourMS, "/workspace/r"))
			for _, p := range []string{"e", "a", "d", "c", "b"} {
				a.call(http.StatusCreated, "reserve", "/v1/reservations", map[string]any{"path": "/workspace/r/" + p, "reason": "a change", "ttl_ms": hourMS})
			}
			return a
		},
	},
}

// seedDirect has alpha send beta five direct posts, h1 to h5, on paths
// apart so no card joins them, and returns beta.
func seedDirect(c *contract) *agent {
	k := c.mintKey("machine")
	alpha, beta := c.agent(k, sessAlpha, "a"), c.agent(k, sessBeta, "a")
	alpha.call(http.StatusCreated, "register", "/v1/register", alpha.registerBody("alpha", hourMS, "/workspace/a"))
	beta.call(http.StatusCreated, "register", "/v1/register", beta.registerBody("beta", hourMS, "/workspace/b"))
	for i := 1; i <= 5; i++ {
		alpha.call(http.StatusCreated, "createPost", "/v1/posts", postBody(map[string]any{"agent": "beta"}, fmt.Sprintf("h%d", i), "a body"))
	}
	return beta
}

// withQuery is path with the query parameters kv appended.
func withQuery(path string, kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + q.Encode()
}

// walk reads row page by page with limit 2 from the first page, checking
// each page's total and next_cursor, and returns the items' key values.
func walk(t *testing.T, a *agent, pr *pagedRow) []string {
	t.Helper()
	var got []string
	cursor := ""
	for page := 1; page <= len(pr.order); page++ {
		path := withQuery(pr.path, "limit", "2")
		if cursor != "" {
			path = withQuery(path, "cursor", cursor)
		}
		r := a.call(http.StatusOK, pr.row, path, nil)
		items := r.items()
		if items == nil {
			t.Errorf("page %d: no items: %s", page, r.raw)
			return got
		}
		if total := num(r.body, "total"); total != int64(len(pr.order)) {
			t.Errorf("page %d: total %d, want %d", page, total, len(pr.order))
		}
		for _, it := range items {
			got = append(got, fmt.Sprint(it[pr.key]))
		}
		cursor = r.str("next_cursor")
		last := len(got) >= len(pr.order)
		switch {
		case last && cursor != "":
			t.Errorf("page %d is the last but carries next_cursor", page)
		case !last && cursor == "":
			t.Errorf("page %d of %d items carries no next_cursor", page, len(got))
		}
		if cursor == "" {
			return got
		}
	}
	t.Errorf("the walk did not end within %d pages", len(pr.order))
	return got
}

func TestContract_agentListsPageByCursor(t *testing.T) {
	for i := range pagedRows {
		pr := &pagedRows[i]
		t.Run(pr.row, func(t *testing.T) {
			c := newContract(t)
			a := pr.seed(c)
			if got := walk(t, a, pr); !slices.Equal(got, pr.order) {
				t.Errorf("the walk gave %v, want %v", got, pr.order)
			}
			for _, limit := range []string{"0", "201"} {
				r := a.call(http.StatusBadRequest, pr.row, withQuery(pr.path, "limit", limit), nil)
				if !hasLimitRange(r) {
					t.Errorf("limit %s: %s, want invalid with {field: limit, code: out_of_range, min: 1, max: 200}", limit, r.raw)
				}
			}
		})
	}
}

func hasLimitRange(r *answer) bool {
	fields, _ := r.body["fields"].([]any)
	return r.str("code") == "invalid" && slices.ContainsFunc(fields, func(v any) bool {
		f, _ := v.(map[string]any)
		return f["field"] == "limit" && f["code"] == "out_of_range" && num(f, "min") == 1 && num(f, "max") == 200
	})
}

func TestContract_aCursorIsBoundToItsRowQueryAndKey(t *testing.T) {
	c := newContract(t)
	alpha := c.agent(c.mintKey("machine-1"), sessAlpha, "a")
	beta := c.agent(c.mintKey("machine-2"), sessBeta, "b")
	alpha.call(http.StatusCreated, "register", "/v1/register", alpha.registerBody("alpha", hourMS, "/workspace/p"))
	beta.call(http.StatusCreated, "register", "/v1/register", beta.registerBody("beta", hourMS, "/workspace/q"))
	for _, p := range []string{"a", "b", "c"} {
		alpha.call(http.StatusCreated, "createPost", "/v1/posts", postBody(map[string]any{"path": "/workspace/p/" + p}, "on "+p, "a body"))
	}
	first := func(path string) string {
		t.Helper()
		cursor := alpha.call(http.StatusOK, "listThreads", path, nil).str("next_cursor")
		if cursor == "" {
			t.Fatalf("GET %s answered no next_cursor", path)
		}
		return cursor
	}
	threads := first("/v1/threads?limit=2")
	filtered := first(withQuery("/v1/threads?limit=2", "path", "/workspace/p"))
	tampered := []byte(threads)
	tampered[len(tampered)/2] ^= 'A' ^ 'B'
	for _, tc := range []struct {
		who  *agent
		name string
		row  string
		path string
		code string
	}{
		{alpha, "a threads cursor sent to history", "getHistory", withQuery("/v1/history?limit=2", "cursor", threads), "cursor_mismatch"},
		{alpha, "a cursor sent with another path filter", "listThreads", withQuery("/v1/threads?limit=2", "path", "/workspace", "cursor", filtered), "cursor_mismatch"},
		{beta, "a cursor sent with another key", "listThreads", withQuery("/v1/threads?limit=2", "cursor", threads), "cursor_mismatch"},
		{alpha, "a cursor with one byte changed", "listThreads", withQuery("/v1/threads?limit=2", "cursor", string(tampered)), "cursor_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.who.c.t = t
			if r := tc.who.call(http.StatusBadRequest, tc.row, tc.path, nil); r.str("code") != tc.code {
				t.Errorf("%s: %s, want %s", tc.path, r.raw, tc.code)
			}
		})
	}
	c.t = t
}

// The cursor bound of design 1.1: a 1024-byte path's cursor fits in 1458
// characters, and one character more is refused unread.
const (
	maxCursorChars = 1458
	maxPathBytes   = 1024
)

func TestContract_theLongestPositionFitsTheCursorBound(t *testing.T) {
	c := newContract(t)
	a := c.agent(c.mintKey("machine"), sessAlpha, "a")
	component := func(ch string) string { return strings.Repeat(ch, 252) }
	long := "/workspace/r/" + strings.Join([]string{component("a"), component("b"), component("c"), component("d")}, "/")
	if len(long) != maxPathBytes {
		t.Fatalf("the long path is %d bytes, want %d", len(long), maxPathBytes)
	}
	a.call(http.StatusCreated, "register", "/v1/register", a.registerBody("alpha", hourMS, long, "/workspace/r/z"))
	for _, p := range []string{long, "/workspace/r/z"} {
		a.call(http.StatusCreated, "createPost", "/v1/posts", postBody(map[string]any{"path": p}, "a post", "a body"))
	}

	r := a.call(http.StatusOK, "listThreads", "/v1/threads?limit=1", nil)
	items, cursor := r.items(), r.str("next_cursor")
	if len(items) != 1 || items[0]["path"] != long || cursor == "" {
		t.Fatalf("the first page: %.300s, want the long path's thread and a next_cursor", r.raw)
	}
	if len(cursor) > maxCursorChars {
		t.Errorf("the long path's cursor is %d characters, over the %d bound", len(cursor), maxCursorChars)
	}
	r = a.call(http.StatusOK, "listThreads", withQuery("/v1/threads?limit=1", "cursor", cursor), nil)
	if items := r.items(); len(items) != 1 || items[0]["path"] != "/workspace/r/z" || r.str("next_cursor") != "" {
		t.Errorf("the second page: %s, want the /workspace/r/z thread and no next_cursor", r.raw)
	}
	r = a.call(http.StatusBadRequest, "listThreads", withQuery("/v1/threads?limit=1", "cursor", strings.Repeat("A", maxCursorChars+1)), nil)
	if r.str("code") != "cursor_invalid" {
		t.Errorf("a %d-character cursor: %s, want cursor_invalid", maxCursorChars+1, r.raw)
	}
}

func TestContract_aKeyPastItsBucketGets429(t *testing.T) {
	c := newContractWith(t, contractOptions{limits: server.Limits{Rate: 1, Burst: 2}, frozen: true})
	a := c.agent(c.mintKey("machine"), "", "a")
	a.call(http.StatusOK, "getVersion", "/v1/version", nil)
	a.call(http.StatusOK, "getVersion", "/v1/version", nil)
	if r := a.call(http.StatusTooManyRequests, "getVersion", "/v1/version", nil); r.str("code") != "rate_limited" {
		t.Errorf("the third request in a burst of 2: %s, want rate_limited", r.raw)
	}
}

// v1ErrorBody is a released v1 client's refusal decode, errorBody in
// v1.1.0's internal/client/api.go, frozen with its tag.
type v1ErrorBody struct {
	Error    string `json:"error"`
	Code     string `json:"code"`
	PathRoot string `json:"path_root"`
}

// upgradeSentence is design 1.6's 426 line from a release server of
// version contractVersion to a client sending sent ("" for none).
func upgradeSentence(sent string) string {
	client := sent
	if sent == "" {
		client = "with no Grelot-Version"
	}
	return fmt.Sprintf("this grelot server (%s) does not speak client %s; install grelot %s from https://github.com/cplieger/grelot/releases/tag/%s, then check both with grelot version",
		contractVersion, client, contractVersion, contractVersion)
}

func TestV1ClientUpgradeRequired(t *testing.T) {
	c := newContract(t)
	k := c.mintKey("machine")
	observer, sender := c.agent(k, "kiro-cli:observer", "a"), c.agent(k, "kiro-cli:sender", "a")
	observer.call(http.StatusCreated, "register", "/v1/register", observer.registerBody("observer", hourMS, repoPath))
	sender.call(http.StatusCreated, "register", "/v1/register", sender.registerBody("sender", hourMS, repoPath))
	newcomer := c.agent(k, "kiro-cli:newcomer", "a")
	requests := map[string]struct {
		body any
		path string
	}{
		"register":           {newcomer.registerBody("newcomer", hourMS, repoPath), "/v1/register"},
		"revive":             {map[string]any{"name": "sender", "where": sender.where(), "ttl_ms": hourMS}, "/v1/revive"},
		"getMe":              {nil, "/v1/me"},
		"patchMe":            {map[string]any{"description": "patched", "ttl_ms": hourMS}, "/v1/me"},
		"deleteMe":           {nil, "/v1/me"},
		"listIdentities":     {nil, "/v1/identities"},
		"getVersion":         {nil, "/v1/version"},
		"createPost":         {postBody(map[string]any{"agent": "observer"}, "hello", "a body"), "/v1/posts"},
		"listThreads":        {nil, "/v1/threads"},
		"getReceipts":        {nil, "/v1/messages/1/receipts"},
		"getHistory":         {nil, "/v1/history"},
		"follow":             {nil, "/v1/follows?path=" + url.QueryEscape(otherPath)},
		"unfollow":           {nil, "/v1/follows?path=" + url.QueryEscape(otherPath)},
		"ackInbox":           {map[string]any{"upto": 1}, "/v1/inbox/ack"},
		"reserve":            {map[string]any{"path": repoPath, "reason": "a change", "ttl_ms": hourMS}, "/v1/reservations"},
		"listReservations":   {nil, "/v1/reservations"},
		"renewReservation":   {map[string]any{"ttl_ms": hourMS}, "/v1/reservations/1"},
		"releaseReservation": {nil, "/v1/reservations/1"},
		"grantDelegate":      {map[string]any{"label": "helper", "ttl_ms": hourMS}, "/v1/delegates"},
		"revokeDelegate":     {nil, "/v1/delegates/helper"},
		"lifecycle":          {map[string]any{"event": "turn_end"}, "/v1/lifecycle"},
		"holdEvent":          {map[string]any{"for": "stop"}, "/v1/events/hold"},
		"waitEvent":          {map[string]any{"wait_ms": int64(1000)}, "/v1/events/wait"},
		"readInbox":          {nil, "/v1/inbox"},
	}
	for _, sent := range []string{"v1.1.0", "v1.1.1", "v1.1.2", "v1.2.0-dev.9", ""} {
		for _, row := range agentRows {
			req, ok := requests[row.name]
			if !ok {
				t.Fatalf("no request for agent row %s", row.name)
			}
			t.Run(fmt.Sprintf("%s under %q", row.name, sent), func(t *testing.T) {
				c.t = t
				a := sender
				if row.name == "register" {
					a = newcomer
				}
				r := a.callWith(http.StatusUpgradeRequired, row.name, req.path, req.body, map[string]string{"Grelot-Version": sent})
				want := upgradeSentence(sent)
				if r.str("code") != "upgrade_required" || r.str("error") != want {
					t.Errorf("code %q, error %q; want upgrade_required and %q", r.str("code"), r.str("error"), want)
				}
				var v1 v1ErrorBody
				if err := json.Unmarshal(r.raw, &v1); err != nil || v1.Error != want {
					t.Errorf("a v1 client decodes %s to error %q (%v), want %q", r.raw, v1.Error, err, want)
				}
			})
		}
	}
	c.t = t

	t.Run("the refused register and post changed nothing", func(t *testing.T) {
		c.t = t
		var names []string
		for _, it := range observer.call(http.StatusOK, "listIdentities", "/v1/identities", nil).items() {
			names = append(names, fmt.Sprint(it["name"]))
		}
		if slices.Sort(names); !slices.Equal(names, []string{"observer", "sender"}) {
			t.Errorf("identities %v after the refused register, want observer and sender", names)
		}
		r := observer.call(http.StatusOK, "getHistory", "/v1/history", nil)
		for _, it := range r.items() {
			if it["subject"] == "hello" {
				t.Errorf("observer's history after the refused post holds it: %s", r.raw)
			}
		}
	})
	for _, served := range []string{"v2.0.0", "v2.0.0-dev.1", "v2.1.0", "dev"} {
		t.Run("a client of "+served+" is served", func(t *testing.T) {
			c.t = t
			sender.callWith(http.StatusOK, "getVersion", "/v1/version", nil, map[string]string{"Grelot-Version": served})
			sender.callWith(http.StatusOK, "getMe", "/v1/me", nil, map[string]string{"Grelot-Version": served})
		})
	}
	c.t = t
}

func TestOpenAPICoversAgentRoutes(t *testing.T) {
	raw, err := os.ReadFile(contractDoc)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, row := range agentRows {
		op, ok := doc.Paths[row.path][strings.ToLower(row.method)]
		if !ok || op.OperationID != row.name {
			t.Errorf("%s has no operation %s %s (found %q)", contractDoc, row.method, row.path, op.OperationID)
		}
	}
}

// TestOpenAPIErrorRequiresItsMembers holds the document's envelope to
// design 1.1: error, code and request_id are on every refusal, the rest
// only when they carry something.
func TestOpenAPIErrorRequiresItsMembers(t *testing.T) {
	raw, err := os.ReadFile(contractDoc)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Required []string `json:"required"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(slices.Values(doc.Components.Schemas["Error"].Required))
	if want := []string{"code", "error", "request_id"}; !slices.Equal(got, want) {
		t.Errorf("%s's Error requires %v, want %v", contractDoc, got, want)
	}
}

// TestOpenAPIDeclaresTheQueryBounds holds the document to the bounds the
// server enforces: every paged row's limit, the inbox's wait_ms, a
// standing wait's wait_ms and the history window's RFC 3339 since and until.
func TestOpenAPIDeclaresTheQueryBounds(t *testing.T) {
	raw, err := os.ReadFile(contractDoc)
	if err != nil {
		t.Fatal(err)
	}
	type bounded struct {
		Minimum *int64 `json:"minimum"`
		Maximum *int64 `json:"maximum"`
		Format  string `json:"format"`
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name   string  `json:"name"`
				In     string  `json:"in"`
				Schema bounded `json:"schema"`
			} `json:"parameters"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]bounded `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	want := func(where string, got bounded, lo, hi int64) {
		t.Helper()
		if got.Minimum == nil || got.Maximum == nil || *got.Minimum != lo || *got.Maximum != hi {
			t.Errorf("%s: %s bounds %v to %v, want %d to %d", contractDoc, where, deref(got.Minimum), deref(got.Maximum), lo, hi)
		}
	}
	query := func(r wire.Route, name string) bounded {
		t.Helper()
		for _, p := range doc.Paths[r.Path][strings.ToLower(r.Method)].Parameters {
			if p.In == "query" && p.Name == name {
				return p.Schema
			}
		}
		t.Errorf("%s: %s has no query parameter %s", contractDoc, r.Name, name)
		return bounded{}
	}
	paged := 0
	for _, r := range wire.Routes {
		if r.Paged {
			paged++
			want(r.Name+"'s limit", query(r, wire.QueryLimit), 1, wire.PageLimitMax)
		}
		if r.Name == "readInbox" {
			want("readInbox's wait_ms", query(r, "wait_ms"), 0, wire.MaxWaitMS)
		}
		if r.Name == "getHistory" {
			for _, name := range []string{"since", "until"} {
				if got := query(r, name).Format; got != "date-time" {
					t.Errorf("%s: getHistory's %s has format %q, want date-time", contractDoc, name, got)
				}
			}
		}
	}
	if paged == 0 {
		t.Error("wire.Routes has no paged row")
	}
	want("WaitRequest's wait_ms", doc.Components.Schemas["WaitRequest"].Properties["wait_ms"], 1, wire.MaxWaitMS)
}

// deref prints a bound the document may leave out.
func deref(p *int64) any {
	if p == nil {
		return "none"
	}
	return *p
}

// notYetCovered are the wire.Routes rows the suite has no case for yet:
// the owner rows (B3) and the stream (B4).
var notYetCovered = []string{
	"listConversations", "getConversation", "markConversationRead", "getUserReceipts", "createUserPost",
	"listAgents", "getAgent", "getAgentHistory", "getStats", "getSettings", "putSettings", "getSignIn", "events",
}

func TestContractCoversEveryRoute(t *testing.T) {
	covered := map[string]bool{}
	for _, step := range agentScenario {
		covered[step.row] = true
	}
	rows := map[string]wire.Route{}
	for _, r := range wire.Routes {
		rows[r.Name] = r
	}
	for _, r := range wire.Routes {
		if !covered[r.Name] && !slices.Contains(notYetCovered, r.Name) {
			t.Errorf("row %s (%s %s) has no case", r.Name, r.Method, r.Path)
		}
	}
	for _, ar := range agentRows {
		r, ok := rows[ar.name]
		switch {
		case !ok:
			t.Errorf("agent row %s is not in wire.Routes", ar.name)
		case r.Method != ar.method || r.Path != ar.path:
			t.Errorf("row %s is %s %s in wire.Routes, want %s %s", ar.name, r.Method, r.Path, ar.method, ar.path)
		}
		if !covered[ar.name] {
			t.Errorf("agent row %s has no case", ar.name)
		}
	}
	for _, name := range notYetCovered {
		if _, ok := rows[name]; !ok {
			t.Errorf("%s is exempt but no wire.Routes row is named so", name)
		}
	}
}
