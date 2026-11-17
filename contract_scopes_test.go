//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/access/accesshttp"
	"github.com/cplieger/grelot/internal/server"
	"github.com/cplieger/grelot/internal/wire"
)

// caller is one credential the scope matrix calls each row with.
type caller int

const (
	asSession caller = iota
	asRecovery
	asOwnerKey
	asAgentKey
	asScopelessKey
	asNone
)

var callers = []caller{asSession, asRecovery, asOwnerKey, asAgentKey, asScopelessKey, asNone}

func (c caller) String() string {
	return [...]string{"session", "recovery_session", "owner_key", "agent_key", "scopeless_key", "no_credential"}[c]
}

// outcome is one cell: admitted when code is empty, else refused with
// status and code.
type outcome struct {
	code   string
	status int
}

var admit = outcome{}

func refused(status int, code string) outcome { return outcome{code: code, status: status} }

// cells is one line of the matrix. A key minted with no scope, as every
// key before scopes existed, answers as an agent key.
type cells struct {
	session, recovery, owner, agent, none outcome
}

func (l cells) of(c caller) outcome {
	switch c {
	case asSession:
		return l.session
	case asRecovery:
		return l.recovery
	case asOwnerKey:
		return l.owner
	case asAgentKey, asScopelessKey:
		return l.agent
	default:
		return l.none
	}
}

var (
	noCredential   = refused(http.StatusUnauthorized, "auth_required")
	recoveryRefuse = refused(http.StatusForbidden, "auth_session_required")
	agentLine      = cells{
		session: refused(http.StatusForbidden, "auth_key_required"), recovery: recoveryRefuse,
		owner: refused(http.StatusForbidden, "auth_scope_required"), agent: admit, none: noCredential,
	}
	ownerLine = cells{
		session: admit, recovery: recoveryRefuse, owner: admit,
		agent: refused(http.StatusForbidden, "auth_scope_required"), none: noCredential,
	}
	publicLine = cells{session: admit, recovery: admit, owner: admit, agent: admit, none: admit}
	// putSettingsMethods is putSettings' line for a request that changes
	// the sign-in methods, from a session signed in moments ago.
	putSettingsMethods = cells{
		session: admit, recovery: recoveryRefuse, owner: refused(http.StatusForbidden, "person_reserved"),
		agent: refused(http.StatusForbidden, "auth_scope_required"), none: noCredential,
	}
)

// scopeMatrix is who each row admits, by row name. The two rows agents and
// owners will share answer as agent rows until they merge, and the event
// stream as an owner row until keys of either scope subscribe.
var scopeMatrix = map[string]cells{
	"register": agentLine, "revive": agentLine, "getMe": agentLine, "patchMe": agentLine, "deleteMe": agentLine,
	"listIdentities": agentLine, "getVersion": agentLine, "createPost": agentLine, "listThreads": agentLine,
	"getReceipts": agentLine, "getHistory": agentLine, "follow": agentLine, "unfollow": agentLine,
	"ackInbox": agentLine, "reserve": agentLine, "listReservations": agentLine, "renewReservation": agentLine,
	"releaseReservation": agentLine, "grantDelegate": agentLine, "revokeDelegate": agentLine,
	"lifecycle": agentLine, "holdEvent": agentLine, "waitEvent": agentLine, "readInbox": agentLine,

	"listConversations": ownerLine, "getConversation": ownerLine, "markConversationRead": ownerLine,
	"getUserReceipts": ownerLine, "createUserPost": ownerLine, "listAgents": ownerLine, "getAgent": ownerLine,
	"getAgentHistory": ownerLine, "getStats": ownerLine, "getSettings": ownerLine, "putSettings": ownerLine,
	"events": ownerLine,

	"getSignIn": publicLine,
}

// matrix is a contract with one of each caller and a conversation of
// @user's, unread, from the agent alpha.
type matrix struct {
	c                         *contract
	recovery                  *browser
	owner, agent, scopeless   string
	conversation, lastMessage string
}

func newMatrix(t *testing.T, o contractOptions) *matrix {
	t.Helper()
	c := newContractWith(t, o)
	m := &matrix{c: c, agent: c.mintKey("agent-machine"), owner: c.mintScoped("owner-machine", wire.ScopeOwner)}
	m.scopeless = c.mintThroughAccess("scopeless-machine")
	alpha := c.agent(m.agent, sessAlpha, "a")
	alpha.call(http.StatusCreated, "register", "/v1/register", alpha.registerBody("alpha", hourMS, "/workspace/a"))
	alpha.call(http.StatusCreated, "createPost", "/v1/posts", postBody(map[string]any{"agent": "user"}, "for the owner", "a body"))
	rows, _ := m.as(asSession, http.MethodGet, wire.Path("listConversations", nil), nil, nil).body["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("the owner sees %d conversations, want alpha's one", len(rows))
	}
	row, _ := rows[0].(map[string]any)
	m.conversation = strconv.FormatInt(num(row, "id"), 10)
	m.lastMessage = strconv.FormatInt(num(obj(row, "last"), "id"), 10)
	m.recovery = c.recoverySession()
	return m
}

// mintThroughAccess mints a key with no scope through access's key route,
// as the owner's browser can.
func (c *contract) mintThroughAccess(label string) string {
	c.t.Helper()
	r := c.send(c.owner, "", http.MethodPost, wire.AuthPrefix+"/apikeys", map[string]any{"label": label, "scopes": []string{}}, nil)
	if r.status != http.StatusCreated || r.str("key") == "" {
		c.t.Fatalf("mint %s through access: %d %s", label, r.status, r.raw)
	}
	return r.str("key")
}

// recoverySession is a browser holding a recovery session for the owner,
// opened from the admin socket's recovery token as its page does.
func (c *contract) recoverySession() *browser {
	c.t.Helper()
	tok, err := c.au.Recovery(c.t.Context(), contractOwner)
	if err != nil {
		c.t.Fatal(err)
	}
	b := &browser{cookies: map[string]string{}}
	if r := c.send(b, "", http.MethodPost, wire.AuthPrefix+"/recovery", map[string]string{"token": tok.Value}, nil); r.status != http.StatusOK {
		c.t.Fatalf("start the recovery: %d %s", r.status, r.raw)
	}
	return b
}

// send makes one request as the browser b or the key, neither for no
// credential, and returns its answer; an event stream's answer is its
// headers alone.
func (c *contract) send(b *browser, key, method, path string, body any, headers map[string]string) *answer {
	t := c.t
	t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case b != nil:
		b.add(req)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	case key != "":
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if b != nil {
		b.keep(resp)
	}
	ans := &answer{status: resp.StatusCode, header: resp.Header}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return ans
	}
	if ans.raw, err = io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(ans.raw))
	dec.UseNumber()
	_ = dec.Decode(&ans.body)
	return ans
}

func (m *matrix) as(who caller, method, path string, body any, headers map[string]string) *answer {
	m.c.t.Helper()
	switch who {
	case asSession:
		return m.c.send(m.c.owner, "", method, path, body, headers)
	case asRecovery:
		return m.c.send(m.recovery, "", method, path, body, headers)
	case asOwnerKey:
		return m.c.send(nil, m.owner, method, path, body, headers)
	case asAgentKey:
		return m.c.send(nil, m.agent, method, path, body, headers)
	case asScopelessKey:
		return m.c.send(nil, m.scopeless, method, path, body, headers)
	default:
		return m.c.send(nil, "", method, path, body, headers)
	}
}

// settings is the stored settings, read as the owner.
func (m *matrix) settings() *answer {
	m.c.t.Helper()
	r := m.as(asSession, http.MethodGet, wire.Path("getSettings", nil), nil, nil)
	if r.status != http.StatusOK {
		m.c.t.Fatalf("read the settings: %d %s", r.status, r.raw)
	}
	return r
}

// settingsBody is a save of the stored settings with the retention moved,
// and, when methods, single sign-on switched.
func (m *matrix) settingsBody(methods bool) map[string]any {
	s := m.settings()
	retention := 5 * hourMS
	if num(s.body, "retention_ms") == retention {
		retention = 6 * hourMS
	}
	ms := obj(s.body, "methods")
	if methods {
		ms["oidc"] = ms["oidc"] != true
	}
	return map[string]any{"revision": max(num(s.body, "revision"), 0), "retention_ms": retention, "methods": ms}
}

// view is what the owner reads of the store and the settings, so a refused
// write is seen to change nothing.
func (m *matrix) view() string {
	m.c.t.Helper()
	var out []byte
	for _, row := range []string{"listConversations", "listAgents", "getSettings"} {
		out = append(out, m.as(asSession, http.MethodGet, wire.Path(row, nil), nil, nil).raw...)
	}
	return string(out)
}

// rowCall is one request of r that its row serves when admitted: the
// headers an agent sends, a key on a keyed row, and a body a write acts on.
type rowCall struct {
	body    any
	headers map[string]string
	method  string
	path    string
}

func (m *matrix) callFor(r *wire.Route, methods bool) rowCall {
	params := map[string]string{"id": m.conversation, "label": "laptop", "name": "alpha"}
	if r.Name == "getUserReceipts" {
		params["id"] = m.lastMessage
	}
	call := rowCall{method: r.Method, path: wire.Path(r.Name, params), headers: map[string]string{}}
	if r.Access == wire.AccessAgent {
		call.headers["Grelot-Version"] = contractVersion
		call.headers["Grelot-Session"] = "kiro-cli:matrix"
	}
	if r.Keyed {
		call.headers["Idempotency-Key"] = newIdemKey()
	}
	switch r.Name {
	case "markConversationRead":
		up, _ := strconv.ParseUint(m.lastMessage, 10, 64)
		call.body = map[string]any{"up_to": up}
	case "createUserPost":
		call.body = map[string]any{"to": map[string]any{"announce": true}, "subject": "from the matrix", "body": "a body"}
	case "putSettings":
		call.body = m.settingsBody(methods)
	case "getStats":
		call.path += "?range=6h"
	default:
		if r.Method != http.MethodGet && r.Method != http.MethodDelete {
			call.body = map[string]any{}
		}
	}
	return call
}

// cell calls r as who and holds the answer to want: admitted, so it
// carries the server's version and no gate code, or refused with want's
// status and code, declared on the row, and no change to what the owner
// sees.
func (m *matrix) cell(t *testing.T, r *wire.Route, who caller, want outcome, methods bool) {
	t.Helper()
	call := m.callFor(r, methods)
	before := m.view()
	ans := m.as(who, call.method, call.path, call.body, call.headers)
	if want == admit {
		if ans.header.Get("Grelot-Version") != contractVersion || strings.HasPrefix(ans.str("code"), "auth_") {
			t.Errorf("%s %s as %s: %d %s, want it admitted", call.method, call.path, who, ans.status, ans.raw)
		}
		return
	}
	if ans.status != want.status || ans.str("code") != want.code {
		t.Errorf("%s %s as %s: %d %s, want %d %s", call.method, call.path, who, ans.status, ans.raw, want.status, want.code)
	}
	m.c.check(call.method, call.path, r.Name, ans)
	if after := m.view(); after != before {
		t.Errorf("%s %s as %s was refused but changed what the owner sees:\nbefore %s\nafter  %s", call.method, call.path, who, before, after)
	}
}

func TestScopeMatrix(t *testing.T) {
	m := newMatrix(t, contractOptions{})
	for i := range wire.Routes {
		r := &wire.Routes[i]
		line, ok := scopeMatrix[r.Name]
		if !ok {
			t.Errorf("row %s has no line in the matrix", r.Name)
			continue
		}
		for _, who := range callers {
			t.Run(r.Name+"/"+who.String(), func(t *testing.T) { m.cell(t, r, who, line.of(who), false) })
		}
	}
	put := &wire.Routes[slices.IndexFunc(wire.Routes, func(r wire.Route) bool { return r.Name == "putSettings" })]
	for _, who := range callers {
		t.Run("putSettings_methods/"+who.String(), func(t *testing.T) { m.cell(t, put, who, putSettingsMethods.of(who), true) })
	}
	for name := range scopeMatrix {
		if !slices.ContainsFunc(wire.Routes, func(r wire.Route) bool { return r.Name == name }) {
			t.Errorf("the matrix has a line for %s, which no row is named", name)
		}
	}
}

func TestAgentKeyRefusedOwnerRoutes(t *testing.T) {
	m := newMatrix(t, contractOptions{})
	for i := range wire.Routes {
		r := &wire.Routes[i]
		if r.Access != wire.AccessOwner {
			continue
		}
		for _, who := range []caller{asAgentKey, asScopelessKey} {
			t.Run(r.Name+"/"+who.String(), func(t *testing.T) {
				m.cell(t, r, who, refused(http.StatusForbidden, "auth_scope_required"), false)
			})
		}
	}
}

func TestOwnerKeyRateLimited(t *testing.T) {
	m := newMatrix(t, contractOptions{limits: server.Limits{Rate: 1, Burst: 2}, frozen: true})
	conversations, stats := wire.Path("listConversations", nil), wire.Path("getStats", nil)+"?range=6h"
	for i := range 2 {
		if r := m.as(asOwnerKey, http.MethodGet, conversations, nil, nil); r.status != http.StatusOK {
			t.Fatalf("the owner key's request %d in a burst of 2: %d %s, want 200", i+1, r.status, r.raw)
		}
	}
	for _, path := range []string{conversations, stats} {
		r := m.as(asOwnerKey, http.MethodGet, path, nil, nil)
		if r.status != http.StatusTooManyRequests || r.str("code") != "rate_limited" {
			t.Errorf("GET %s past the owner key's burst: %d %s, want 429 rate_limited", path, r.status, r.raw)
		}
		if v := r.header.Get("Grelot-Version"); v != contractVersion {
			t.Errorf("GET %s's 429: Grelot-Version %q, want %s", path, v, contractVersion)
		}
		checkRetryAfter(t, "GET "+path, r)
		m.c.check(http.MethodGet, strings.TrimSuffix(path, "?range=6h"), map[string]string{conversations: "listConversations", stats: "getStats"}[path], r)
	}
	for i := range 10 {
		if r := m.as(asSession, http.MethodGet, conversations, nil, nil); r.status != http.StatusOK {
			t.Errorf("the session's request %d: %d %s, want 200, since a session takes no token", i+1, r.status, r.raw)
		}
	}
}

// hasOnlyField reports whether r refuses exactly one field, field, with
// code, and the bounds min and max when they are not -1.
func hasOnlyField(r *answer, field, code string, minMS, maxMS int64) bool {
	fields, _ := r.body["fields"].([]any)
	if len(fields) != 1 {
		return false
	}
	f, _ := fields[0].(map[string]any)
	if f["field"] != field || f["code"] != code {
		return false
	}
	return num(f, "min") == minMS && num(f, "max") == maxMS
}

func TestPutSettings_anOwnerKeyChangingTheMethodsIsPersonReserved(t *testing.T) {
	m := newMatrix(t, contractOptions{})
	path := wire.Path("putSettings", nil)
	before := m.settings().raw
	r := m.as(asOwnerKey, http.MethodPut, path, m.settingsBody(true), nil)
	if r.status != http.StatusForbidden || r.str("code") != "person_reserved" || !hasOnlyField(r, "methods", "person_reserved", -1, -1) {
		t.Errorf("an owner key changing the methods: %d %s, want 403 person_reserved naming methods", r.status, r.raw)
	}
	m.c.check(http.MethodPut, path, "putSettings", r)
	if after := m.settings().raw; !bytes.Equal(after, before) {
		t.Errorf("the settings after the refusal are %s, want %s", after, before)
	}
}

func TestPutSettings_anOwnerKeyChangesTheRetention(t *testing.T) {
	m := newMatrix(t, contractOptions{})
	body := m.settingsBody(false)
	r := m.as(asOwnerKey, http.MethodPut, wire.Path("putSettings", nil), body, nil)
	if r.status != http.StatusOK || num(r.body, "retention_ms") != body["retention_ms"] || num(r.body, "revision") != 1 {
		t.Errorf("an owner key changing the retention: %d %s, want 200 with retention_ms %d and revision 1", r.status, r.raw, body["retention_ms"])
	}
}

func TestPutSettings_aSessionChangesTheMethodsOnlyAfterARecentSignIn(t *testing.T) {
	m := newMatrix(t, contractOptions{})
	path := wire.Path("putSettings", nil)
	m.c.authAhead.Store(int64(11 * time.Minute))
	r := m.as(asSession, http.MethodPut, path, m.settingsBody(true), nil)
	if r.status != http.StatusForbidden || r.str("code") != "recent_auth_required" {
		t.Errorf("a methods change 11 minutes after sign-in: %d %s, want 403 recent_auth_required", r.status, r.raw)
	}
	m.c.check(http.MethodPut, path, "putSettings", r)
	if s := m.c.send(m.c.owner, "", http.MethodPost, wire.AuthPrefix+"/stepup/password", map[string]string{"password": contractPassword}, nil); s.status != http.StatusNoContent {
		t.Fatalf("step-up: %d %s, want 204", s.status, s.raw)
	}
	if r := m.as(asSession, http.MethodPut, path, m.settingsBody(true), nil); r.status != http.StatusOK {
		t.Errorf("the methods change after a step-up: %d %s, want 200", r.status, r.raw)
	}
}

func TestPutSettings_aStaleRevisionIsRefused(t *testing.T) {
	m := newMatrix(t, contractOptions{})
	path := wire.Path("putSettings", nil)
	first := m.settingsBody(false)
	if r := m.as(asSession, http.MethodPut, path, first, nil); r.status != http.StatusOK || num(r.body, "revision") != 1 {
		t.Fatalf("the first save of revision 0: %d %s, want 200 and revision 1", r.status, r.raw)
	}
	again := m.settingsBody(false)
	again["revision"] = 0
	r := m.as(asSession, http.MethodPut, path, again, nil)
	if r.status != http.StatusConflict || r.str("code") != "stale_revision" {
		t.Errorf("a second save of revision 0: %d %s, want 409 stale_revision", r.status, r.raw)
	}
	m.c.check(http.MethodPut, path, "putSettings", r)
	if s := m.settings(); num(s.body, "revision") != 1 || num(s.body, "retention_ms") != first["retention_ms"] {
		t.Errorf("the settings after the stale save: %s, want revision 1 and the first save's retention", s.raw)
	}
}

func TestPutSettings_aRetentionOutOfRangeNamesItsField(t *testing.T) {
	m := newMatrix(t, contractOptions{})
	path := wire.Path("putSettings", nil)
	minMS, maxMS := hourMS, 90*24*hourMS
	for _, c := range []struct {
		code string
		ms   int64
	}{
		{"out_of_range", halfMS},
		{"out_of_range", maxMS + 60_000},
		{"out_of_range", -1},
		{"not_whole_minutes", hourMS + 30_000},
	} {
		body := m.settingsBody(false)
		body["retention_ms"] = c.ms
		r := m.as(asSession, http.MethodPut, path, body, nil)
		if r.status != http.StatusBadRequest || r.str("code") != "invalid" || !hasOnlyField(r, "retention_ms", c.code, minMS, maxMS) {
			t.Errorf("retention_ms %d: %d %s, want 400 invalid with retention_ms %s from %d to %d", c.ms, r.status, r.raw, c.code, minMS, maxMS)
		}
		m.c.check(http.MethodPut, path, "putSettings", r)
	}
}

func TestSettingsFieldsAreKnown(t *testing.T) {
	var got []string
	rt := reflect.TypeFor[wire.SettingsRequest]()
	for f := range rt.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		got = append(got, name)
	}
	slices.Sort(got)
	if want := []string{"methods", "retention_ms", "revision"}; !slices.Equal(got, want) {
		t.Errorf("wire.SettingsRequest's fields are %v, want %v: a new setting names the person-only kind it reaches first", got, want)
	}
}

// accountsView is the accounts file's users, keys (less their last use)
// and settings, so a refused call is seen to change nothing there.
func (c *contract) accountsView() string {
	c.t.Helper()
	users, err := c.acc.Access().ListUsers(c.t.Context())
	if err != nil {
		c.t.Fatal(err)
	}
	keys, err := c.acc.Access().ListAPIKeys(c.t.Context())
	if err != nil {
		c.t.Fatal(err)
	}
	for i := range keys {
		keys[i].LastUsedAt = time.Time{}
	}
	raw, err := json.Marshal([]any{users, keys, c.acc.Current()})
	if err != nil {
		c.t.Fatal(err)
	}
	return string(raw)
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// personOnly are access's routes of the person-only kinds a stepped-up
// session alone reaches: changing how anyone signs in, and minting or
// rotating a key.
var personOnly = []string{
	"authPasswordSet", "authPasswordRemove", "authPasskeyRegisterBegin", "authPasskeyRegisterFinish",
	"authPasskeyRemove", "authMethodReenable", "authOIDCStartLink", "authOIDCLinkPending", "authOIDCUnlink",
	"authKeyCreate", "authKeyRotate",
}

func TestPersonOnlyKinds(t *testing.T) {
	m := newMatrix(t, contractOptions{})
	c := m.c
	classes := map[string]accesshttp.Class{}
	for _, rt := range c.au.Routes() {
		classes[rt.Name] = rt.Class
		if rt.Class == accesshttp.ClassPublic {
			continue
		}
		if !slices.Contains([]accesshttp.Class{accesshttp.ClassSession, accesshttp.ClassRecentAuth, accesshttp.ClassRecovery}, rt.Class) {
			t.Errorf("%s (%s %s) is class %s, which a key can reach", rt.Name, rt.Method, rt.Path, rt.Class)
		}
		path := pathParam.ReplaceAllString(rt.Path, "1")
		for _, who := range []caller{asOwnerKey, asAgentKey, asScopelessKey} {
			t.Run(rt.Name+"/"+who.String(), func(t *testing.T) {
				before := c.accountsView()
				r := m.as(who, rt.Method, path, nil, nil)
				if r.status != http.StatusForbidden || r.str("code") != "auth_session_required" {
					t.Errorf("%s %s as %s: %d %s, want 403 auth_session_required", rt.Method, path, who, r.status, r.raw)
				}
				if after := c.accountsView(); after != before {
					t.Errorf("%s %s as %s changed the accounts file:\nbefore %s\nafter  %s", rt.Method, path, who, before, after)
				}
			})
		}
	}
	for _, name := range personOnly {
		if got, ok := classes[name]; !ok || got != accesshttp.ClassRecentAuth {
			t.Errorf("%s is class %q (mounted %v), want %s", name, got, ok, accesshttp.ClassRecentAuth)
		}
	}
	keys, err := c.acc.Access().ListAPIKeys(t.Context())
	if err != nil || len(keys) == 0 {
		t.Fatalf("keys %v, %v; want the matrix's", keys, err)
	}
	c.authAhead.Store(int64(11 * time.Minute))
	rotate := wire.AuthPrefix + "/apikeys/" + strconv.FormatInt(keys[0].ID, 10) + "/rotate"
	if r := m.as(asSession, http.MethodPost, rotate, nil, nil); r.status != http.StatusForbidden || r.str("code") != "auth_recent_auth_required" {
		t.Errorf("a key rotation 11 minutes after sign-in: %d %s, want 403 auth_recent_auth_required", r.status, r.raw)
	}
}
