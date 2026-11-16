//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/accounts"
	"github.com/cplieger/grelot/internal/auth"
	"github.com/cplieger/grelot/internal/auth/authtest"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire/wiretest"
)

// contractVersion is the harness server's version and the Grelot-Version
// every agent request sends: the first v2 release, the server's minClient.
const contractVersion = "v2.0.0"

// The suite's sign-in, through the prefix internal/auth mounts.
const (
	contractAuthPrefix = "/api/auth"
	contractOwner      = "owner"
	contractPassword   = "a long enough contract passphrase"
)

// contractDoc is the published contract every answer is checked against.
const contractDoc = "docs/openapi.json"

// contractOptions are the knobs a case turns on the harness server.
type contractOptions struct {
	// limits are the server's; zero lifts every limit, so a scenario sends
	// as many requests as it needs.
	limits server.Limits
	// frozen stops the server's clock, so a bucket refills by nothing.
	frozen bool
}

// contract is one grelot server composed as serve composes it, over files
// of the test's own, served over HTTP, with its first account set up.
type contract struct {
	t *testing.T
	// hits, when set, records each row an agent of this contract calls.
	hits   map[string]bool
	doc    *wiretest.Doc
	au     *auth.Auth
	clk    *contractClock
	client *http.Client
	url    string
}

// contractClock is the server's and the store's clock: the wall clock
// moved by advance, or one instant once frozen.
type contractClock struct {
	frozen time.Time
	offset time.Duration
	mu     sync.Mutex
}

func (c *contractClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.frozen.IsZero() {
		return c.frozen
	}
	return time.Now().Add(c.offset)
}

func (c *contractClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

func newContract(t *testing.T) *contract {
	t.Helper()
	return newContractWith(t, contractOptions{})
}

// newContractWith builds the server the way serve and build do, with o's
// limits and clock, and creates the owner account over HTTP from a setup
// token, as the setup page does.
func newContractWith(t *testing.T, o contractOptions) *contract {
	t.Helper()
	doc, err := wiretest.Load(contractDoc)
	if err != nil {
		t.Fatalf("load the contract: %v", err)
	}
	clk := &contractClock{}
	if o.frozen {
		clk.frozen = time.Now().UTC().Truncate(time.Second)
	}
	limits := o.limits
	if limits == (server.Limits{}) {
		limits = server.Limits{Rate: 1e9, Burst: 1e9, KeyWaits: 1e6, ServerWaits: 1e6}
	}
	dir := t.TempDir()
	public, _ := url.Parse(authtest.PublicURL)
	cfg := &server.Config{
		PathRoot: protopath.DefaultRoot, PublicURL: public,
		StorePath: filepath.Join(dir, "grelot.db"), AccountsPath: filepath.Join(dir, "accounts.db"),
	}
	logger := slog.New(slog.DiscardHandler)
	acc, err := accounts.Open(cfg.AccountsPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = acc.Close() })
	metrics := obs.New()
	au, err := auth.New(t.Context(), acc, &auth.Options{
		Logger: logger, PublicURL: cfg.PublicURL, Errors: server.AuthErrors(logger, metrics),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = au.Close(context.WithoutCancel(t.Context())) })
	metrics.ServeBuildInfo(contractVersion)
	hub, events := server.NewHub(), server.NewEvents(logger)
	st, err := store.Open(cfg.StorePath, &store.Options{
		Logger: logger, Metrics: metrics, Now: clk.now, Retention: acc.Retention, PathRoot: cfg.PathRoot,
		Woke: hub.Woke, Changed: events.Changed, KeyLive: au.KeyLive,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(&server.Options{
		Store: st, Logger: logger, Now: clk.now, Hub: hub, Events: events, Metrics: metrics, Auth: au,
		Version: contractVersion, Config: *cfg, Limits: limits, CursorKey: acc.Secrets().CursorKey,
	})
	t.Cleanup(func() { srv.Release(context.WithoutCancel(t.Context())) })
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	c := &contract{t: t, doc: doc, au: au, clk: clk, client: hs.Client(), url: hs.URL}
	c.setUp()
	return c
}

// setUp creates the owner account through access's setup route.
func (c *contract) setUp() {
	c.t.Helper()
	tok, err := c.au.SetupToken(c.t.Context())
	if err != nil {
		c.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": tok.Value, "username": contractOwner, "password": contractPassword})
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodPost, c.url+contractAuthPrefix+"/setup/password", bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		c.t.Fatalf("setup: %d %s", resp.StatusCode, raw)
	}
}

// mintKey mints an agent key through the admin socket's call.
func (c *contract) mintKey(label string) string {
	c.t.Helper()
	key, err := c.au.CreateKey(c.t.Context(), label)
	if err != nil {
		c.t.Fatalf("mint key %s: %v", label, err)
	}
	return key
}

// agentRow is one agent route of design section 1.2: the operation the
// contract must declare for it, and whether it takes an Idempotency-Key.
type agentRow struct {
	name, method, path string
	keyed              bool
}

var agentRows = []agentRow{
	{"register", http.MethodPost, "/v1/register", true},
	{"revive", http.MethodPost, "/v1/revive", true},
	{"getMe", http.MethodGet, "/v1/me", false},
	{"patchMe", http.MethodPatch, "/v1/me", true},
	{"deleteMe", http.MethodDelete, "/v1/me", true},
	{"listIdentities", http.MethodGet, "/v1/identities", false},
	{"getVersion", http.MethodGet, "/v1/version", false},
	{"createPost", http.MethodPost, "/v1/posts", true},
	{"listThreads", http.MethodGet, "/v1/threads", false},
	{"getReceipts", http.MethodGet, "/v1/messages/{id}/receipts", false},
	{"getHistory", http.MethodGet, "/v1/history", false},
	{"follow", http.MethodPut, "/v1/follows", true},
	{"unfollow", http.MethodDelete, "/v1/follows", true},
	{"ackInbox", http.MethodPost, "/v1/inbox/ack", true},
	{"reserve", http.MethodPost, "/v1/reservations", true},
	{"listReservations", http.MethodGet, "/v1/reservations", false},
	{"renewReservation", http.MethodPatch, "/v1/reservations/{id}", true},
	{"releaseReservation", http.MethodDelete, "/v1/reservations/{id}", true},
	{"grantDelegate", http.MethodPost, "/v1/delegates", true},
	{"revokeDelegate", http.MethodDelete, "/v1/delegates/{label}", true},
	{"lifecycle", http.MethodPost, "/v1/lifecycle", false},
	{"holdEvent", http.MethodPost, "/v1/events/hold", false},
	{"waitEvent", http.MethodPost, "/v1/events/wait", false},
	{"readInbox", http.MethodGet, "/v1/inbox", false},
}

func agentRowNamed(t *testing.T, name string) agentRow {
	t.Helper()
	i := slices.IndexFunc(agentRows, func(r agentRow) bool { return r.name == name })
	if i < 0 {
		t.Fatalf("no agent row %s", name)
	}
	return agentRows[i]
}

// agent is one session of a machine: its key, its Grelot-Session and the
// version it sends, with the where of its environment.
type agent struct {
	c       *contract
	hits    map[string]bool
	key     string
	session string
	version string
	env     string
}

// agent is a v2 client of key acting as session, in environment env.
func (c *contract) agent(key, session, env string) *agent {
	return &agent{c: c, key: key, session: session, version: contractVersion, env: env, hits: c.hits}
}

// where is a's where report: one machine, its environment, no checkout.
func (a *agent) where() map[string]any {
	return map[string]any{
		"machine": "dmi:" + strings.Repeat("1", 32), "environment": strings.Repeat(a.env, 32), "checkouts": []any{},
	}
}

func (a *agent) registerBody(name string, ttlMS int64, paths ...string) map[string]any {
	return map[string]any{
		"name": name, "paths": paths, "description": "testing the contract",
		"host": "host-" + a.env, "os": "linux", "arch": "amd64", "where": a.where(), "ttl_ms": ttlMS,
	}
}

func postBody(to any, subject, body string) map[string]any {
	return map[string]any{
		"to": to, "subject": subject, "body": body,
		"identity": map[string]any{"name": store.NoChange, "description": store.NoChange, "paths": []string{store.NoChange}},
	}
}

// answer is one answer, its body kept raw and decoded.
type answer struct {
	header http.Header
	body   map[string]any
	raw    []byte
	status int
}

func (r *answer) str(key string) string {
	v, _ := r.body[key].(string)
	return v
}

// obj is the member key of m as an object, nil when it is not one.
func obj(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

// num is the member key of m as an integer, -1 when it is not one.
func num(m map[string]any, key string) int64 {
	n, ok := m[key].(json.Number)
	if !ok {
		return -1
	}
	v, err := n.Int64()
	if err != nil {
		return -1
	}
	return v
}

// items is a page's items, nil when the answer carries none.
func (r *answer) items() []map[string]any {
	raw, ok := r.body["items"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		m, _ := v.(map[string]any)
		out = append(out, m)
	}
	return out
}

var idemKeys atomic.Int64

func newIdemKey() string { return fmt.Sprintf(`"contract-%016d"`, idemKeys.Add(1)) }

// send makes one request of row at path, a concrete path of the row's
// template with its query, with an Idempotency-Key when the row is keyed,
// and returns the answer unchecked. headers override a's, an empty value
// leaving the header out.
func (a *agent) send(row, path string, body any, headers map[string]string) *answer {
	t := a.c.t
	t.Helper()
	r := agentRowNamed(t, row)
	if p, _, _ := strings.Cut(path, "?"); !templateMatches(r.path, p) {
		t.Fatalf("%s is not a path of %s %s", path, row, r.path)
	}
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.method, a.c.url+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.key)
	set := map[string]string{"Grelot-Version": a.version, "Grelot-Session": a.session}
	if body != nil {
		set["Content-Type"] = "application/json"
	}
	if r.keyed {
		set["Idempotency-Key"] = newIdemKey()
	}
	maps.Copy(set, headers)
	for k, v := range set {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := a.c.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", r.method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	ans := &answer{status: resp.StatusCode, header: resp.Header, raw: raw}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	_ = dec.Decode(&ans.body)
	if a.hits != nil {
		a.hits[row] = true
	}
	return ans
}

// call is send checked against the contract and the paired assertions,
// with want the status the design gives the case.
func (a *agent) call(want int, row, path string, body any) *answer {
	a.c.t.Helper()
	return a.callWith(want, row, path, body, nil)
}

func (a *agent) callWith(want int, row, path string, body any, headers map[string]string) *answer {
	t := a.c.t
	t.Helper()
	r := agentRowNamed(t, row)
	ans := a.send(row, path, body, headers)
	if ans.status != want {
		t.Errorf("%s %s: status %d, want %d: %s", r.method, path, ans.status, want, ans.raw)
	}
	a.c.check(r.method, path, row, ans)
	session := a.session
	if v, ok := headers["Grelot-Session"]; ok {
		session = v
	}
	checkHeaders(t, r.method+" "+path, ans, session != "")
	return ans
}

// check holds ans to its operation in the contract and to the forms the
// design gives every answer.
func (c *contract) check(method, path, row string, ans *answer) {
	t := c.t
	t.Helper()
	op, err := c.doc.Check(method, path, ans.status, ans.header, ans.raw)
	switch {
	case err != nil:
		t.Errorf("%s %s against the contract: %v", method, path, err)
	case op != row:
		t.Errorf("%s %s matched operation %s, want %s", method, path, op, row)
	}
	checkForms(t, method+" "+path, "$", ans.body)
	if ans.status >= http.StatusBadRequest {
		checkEnvelope(t, method+" "+path, ans)
	}
}

// templateMatches reports whether path is a path of template.
func templateMatches(template, path string) bool {
	ts, ps := strings.Split(template, "/"), strings.Split(path, "/")
	if len(ts) != len(ps) {
		return false
	}
	for i, s := range ts {
		if strings.HasPrefix(s, "{") {
			if ps[i] == "" {
				return false
			}
		} else if s != ps[i] {
			return false
		}
	}
	return true
}

// instantMembers are the members, beside every *_at, that carry an instant.
var instantMembers = []string{"since", "until", "dormant_since", "last_seen", "written"}

// checkForms walks v for design 1.1's wire forms: no duration string
// (ttl, wait), every *_ms an integer, every instant RFC 3339.
func checkForms(t *testing.T, where, at string, v any) {
	t.Helper()
	switch v := v.(type) {
	case map[string]any:
		for k, m := range v {
			p := at + "." + k
			switch {
			case k == "ttl" || k == "wait":
				t.Errorf("%s: %s is a duration string, want %s_ms", where, p, k)
			case strings.HasSuffix(k, "_ms"):
				if n, ok := m.(json.Number); !ok || strings.ContainsAny(n.String(), ".eE") {
					t.Errorf("%s: %s = %v, want an integer of milliseconds", where, p, m)
				}
			case strings.HasSuffix(k, "_at") || slices.Contains(instantMembers, k):
				if s, ok := m.(string); !ok {
					t.Errorf("%s: %s = %v, want an RFC 3339 string", where, p, m)
				} else if _, err := time.Parse(time.RFC3339, s); err != nil {
					t.Errorf("%s: %s = %q is not RFC 3339", where, p, s)
				}
			}
			checkForms(t, where, p, m)
		}
	case []any:
		for i, m := range v {
			checkForms(t, where, at+"["+strconv.Itoa(i)+"]", m)
		}
	}
}

// members is what a refusal's code carries beside error, code and
// request_id (design 1.4): each required member and the optional ones.
type members struct {
	required, optional []string
}

// codeMembers is design 1.4's table for the codes the agent rows answer.
var codeMembers = map[string]members{
	"invalid":           {optional: []string{"fields"}},
	"cursor_invalid":    {},
	"cursor_mismatch":   {},
	"not_found":         {required: []string{"reason"}},
	"gone":              {required: []string{"reason"}},
	"none":              {},
	"dormant":           {},
	"delegate_ended":    {},
	"conflict":          {},
	"reserved":          {},
	"queued":            {},
	"other_environment": {},
	"other_provider":    {},
	"no_environment":    {},
	"other_repository":  {},
	"delegate_gate":     {},
	"forbidden":         {},
	"too_large":         {optional: []string{"fields", "reason"}},
	"key_reused":        {},
	"store_full":        {},
	"rate_limited":      {required: []string{"retry_after_ms"}},
	"too_many_waits":    {required: []string{"retry_after_ms"}},
	"timeout":           {required: []string{"retry_after_ms"}},
	"shutting_down":     {},
	"upgrade_required":  {},
	"internal_error":    {},
}

// checkEnvelope holds a refusal to the members design 1.4 gives its code.
func checkEnvelope(t *testing.T, where string, ans *answer) {
	t.Helper()
	code := ans.str("code")
	for _, k := range []string{"error", "code", "request_id"} {
		if ans.str(k) == "" {
			t.Errorf("%s: the refusal has no %s: %s", where, k, ans.raw)
		}
	}
	m, known := codeMembers[code]
	if strings.HasPrefix(code, "auth_") {
		m, known = members{optional: []string{"reason"}}, true
	}
	if !known {
		t.Errorf("%s: code %q is not one design 1.4 gives an agent row", where, code)
		return
	}
	for _, k := range m.required {
		if _, ok := ans.body[k]; !ok {
			t.Errorf("%s: %s carries no %s: %s", where, code, k, ans.raw)
		}
	}
	for k := range ans.body {
		if k != "error" && k != "code" && k != "request_id" && !slices.Contains(m.required, k) && !slices.Contains(m.optional, k) {
			t.Errorf("%s: %s carries %s, which design 1.4 does not give it: %s", where, code, k, ans.raw)
		}
	}
}

// checkHeaders holds an admitted answer to design 2.1's headers: the
// server's Grelot-Version always, a status line when the request named a
// session the chain read, and a whole-seconds Retry-After on a 429 with
// retry_after_ms equal to it.
func checkHeaders(t *testing.T, where string, ans *answer, session bool) {
	t.Helper()
	if v := ans.header.Get("Grelot-Version"); v != contractVersion {
		t.Errorf("%s: Grelot-Version %q, want %s", where, v, contractVersion)
	}
	line := ans.header.Get("Grelot-Status")
	switch {
	case ans.status == http.StatusUpgradeRequired && line != "":
		t.Errorf("%s: a 426 carries Grelot-Status %q", where, line)
	case ans.status != http.StatusUpgradeRequired && session && !strings.HasPrefix(line, "grelot: "):
		t.Errorf("%s: Grelot-Status %q, want the session's status line", where, line)
	}
	if ans.status == http.StatusTooManyRequests {
		checkRetryAfter(t, where, ans)
	}
}

func checkRetryAfter(t *testing.T, where string, ans *answer) {
	t.Helper()
	secs, err := strconv.Atoi(ans.header.Get("Retry-After"))
	if err != nil || secs < 1 {
		t.Errorf("%s: Retry-After %q, want whole seconds", where, ans.header.Get("Retry-After"))
		return
	}
	if got := num(ans.body, "retry_after_ms"); got != int64(secs)*1000 {
		t.Errorf("%s: retry_after_ms %d, want %d for Retry-After %d", where, got, secs*1000, secs)
	}
}
