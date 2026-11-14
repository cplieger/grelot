//go:build !windows

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/access/gate"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/webui/wire"
)

// noKey removes the key request adds to a write.
var noKey = map[string]string{headerKey: ""}

// webGet sends a GET and decodes its 200 answer into T.
func webGet[T any](h *harness, path string) T {
	h.t.Helper()
	r := h.request(http.MethodGet, path, nil, nil)
	if r.status != http.StatusOK {
		h.t.Fatalf("GET %s: %d %s", path, r.status, r.raw)
	}
	var out T
	if err := json.Unmarshal([]byte(r.raw), &out); err != nil {
		h.t.Fatalf("GET %s: decode: %v", path, err)
	}
	return out
}

// userPost posts as @user through the web API and returns its answer.
func userPost(h *harness, to wire.PostTarget, subject string) wire.Posted {
	h.t.Helper()
	r := h.request(http.MethodPost, "/api/posts", wire.PostRequest{Subject: subject, Body: "from the web UI", To: to}, nil)
	if r.status != http.StatusCreated {
		h.t.Fatalf("POST /api/posts: %d %s", r.status, r.raw)
	}
	var out wire.Posted
	if err := json.Unmarshal([]byte(r.raw), &out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func TestWeb_everyRouteButThePublicOnesRefusesARequestWithNoSignIn(t *testing.T) {
	h := newHarnessWith(t, store.Options{}, withAuth(testAuth(t)))
	if r := h.request(http.MethodGet, "/api/nowhere", nil, nil); r.status != http.StatusNotFound || r.str("code") != "auth_not_found" {
		t.Errorf("GET /api/nowhere: %d %s, want 404 auth_not_found", r.status, r.raw)
	}
	for i, p := range routePaths() {
		route := &wire.Routes[i]
		if route.Public {
			continue
		}
		t.Run(strings.ReplaceAll(route.Method+p, "/", "_"), func(t *testing.T) {
			r := h.request(route.Method, p, map[string]any{}, nil)
			if r.status != http.StatusUnauthorized || r.str("code") != "auth_required" || r.str("path_root") != "" {
				t.Errorf("%s %s: %d %s, want 401 auth_required with no path root", route.Method, p, r.status, r.raw)
			}
		})
	}
}

// The web API's declared refusals, which the browser's client and
// docs/openapi.json are generated from, name exactly the gate's codes for
// each route's row (access/gate Codes).
func TestWeb_everyRouteDeclaresTheGatesCodes(t *testing.T) {
	h := newHarnessWith(t, store.Options{}, withAuth(testAuth(t)))
	tbl := newTable()
	h.srv.webRoutes(tbl)
	for i := range tbl.rows {
		row := tbl.rows[i]
		route := &wire.Routes[slices.IndexFunc(wire.Routes, func(r wire.Route) bool { return r.Pattern() == row.Name })]
		t.Run(route.Name, func(t *testing.T) {
			var declared, want []string
			for _, e := range route.Errors {
				if strings.HasPrefix(e.Code, "auth_") {
					declared = append(declared, e.Code)
				}
			}
			for _, c := range gate.Codes(row) {
				want = append(want, string(c))
			}
			slices.Sort(declared)
			slices.Sort(want)
			if !slices.Equal(declared, want) {
				t.Errorf("declared %v, want the gate's %v", declared, want)
			}
		})
	}
}

// routePaths are wire.Routes' paths with their placeholders filled.
func routePaths() []string {
	out := make([]string, 0, len(wire.Routes))
	for i := range wire.Routes {
		out = append(out, strings.NewReplacer("{id}", "1", "{name}", "alpha").Replace(wire.Routes[i].Path))
	}
	return out
}

func TestWeb_aDirectPostFromUserStartsAConversationWithTheAgent(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")

	p := userPost(h, wire.PostTarget{Agent: "alpha"}, "hello alpha")
	if p.Delivered != 1 || p.Message == 0 || p.Thread == 0 {
		t.Fatalf("posted %+v, want one delivery", p)
	}
	list := webGet[wire.ConversationsPage](h, "/api/conversations?kind=direct")
	if len(list.Rows) != 1 {
		t.Fatalf("direct conversations %+v, want the one with alpha", list.Rows)
	}
	row := list.Rows[0]
	if row.ID != p.Thread || row.Kind != wire.ThreadDirect || !slices.Equal(row.Agents, []string{"alpha", "user"}) ||
		row.Last.From != "user" || row.Last.Subject != "hello alpha" || row.Participants != 2 {
		t.Errorf("row %+v, want the direct thread of alpha and user, last post from user", row)
	}
	if threads := webGet[wire.ConversationsPage](h, "/api/conversations?kind=threads"); len(threads.Rows) != 0 {
		t.Errorf("thread conversations %+v, want none", threads.Rows)
	} else if want := (wire.ConversationCounts{Direct: 1}); threads.Counts != want {
		t.Errorf("counts %+v under the threads filter, want %+v: every kind is counted", threads.Counts, want)
	}

	c := webGet[wire.Conversation](h, "/api/conversations/"+strconv.FormatUint(p.Thread, 10))
	if len(c.Posts) != 1 || c.Posts[0].FromKind != wire.SenderUser || c.Posts[0].Read != (wire.ReadCount{Total: 1}) {
		t.Errorf("posts %+v, want one from user, unread by its one recipient", c.Posts)
	}
	if len(c.Cards) != 2 || c.Cards[0].Name != "alpha" || c.Cards[0].State != wire.AgentLive || c.Cards[1].Name != "user" {
		t.Errorf("cards %+v, want alpha's then user's", c.Cards)
	}
	if c.Reservations == nil || c.Followers == nil {
		t.Errorf("reservations %v, followers %v, want empty lists, not null", c.Reservations, c.Followers)
	}

	rc := webGet[wire.Receipts](h, "/api/messages/"+strconv.FormatUint(p.Message, 10)+"/receipts")
	if len(rc.Recipients) != 1 || rc.Recipients[0].Name != "alpha" || rc.Recipients[0].State != "unread" || rc.Recipients[0].ReadAtMS != 0 {
		t.Errorf("receipts %+v, want alpha unread", rc.Recipients)
	}
}

func TestWeb_userPostsToAnnounceAndAPath(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")

	a := userPost(h, wire.PostTarget{Announce: true}, "to everyone")
	p := userPost(h, wire.PostTarget{Path: repoGrelot}, "about grelot")
	if a.Delivered != 1 || p.Delivered != 1 {
		t.Errorf("announce delivered %d, path %d, want alpha reached by both", a.Delivered, p.Delivered)
	}
	list := webGet[wire.ConversationsPage](h, "/api/conversations?kind=threads")
	var kinds []wire.ThreadKind
	for _, r := range list.Rows {
		kinds = append(kinds, r.Kind)
	}
	if !slices.Equal(kinds, []wire.ThreadKind{wire.ThreadPath, wire.ThreadAnnounce}) {
		t.Errorf("thread kinds %v, want the path thread then announce, newest first", kinds)
	}
}

func TestWeb_listConversationsPagesOnBothCursorParts(t *testing.T) {
	h := newSignedIn(t)
	parent := userPost(h, wire.PostTarget{Path: repoGrelot}, "parent")
	child := userPost(h, wire.PostTarget{Path: repoGrelot + "/web"}, "child")
	for i := range 49 {
		userPost(h, wire.PostTarget{Path: fmt.Sprintf("/workspace/r%02d", i)}, "newer")
	}

	// The parent row's newest post is the child's, so the page of 50 ends
	// between two rows that share it.
	first := webGet[wire.ConversationsPage](h, "/api/conversations")
	if len(first.Rows) != 50 || first.Next != child.Message || first.NextThread != child.Thread {
		t.Fatalf("first page %d rows, next %d thread %d, want 50 ending on thread %d's post %d",
			len(first.Rows), first.Next, first.NextThread, child.Thread, child.Message)
	}
	second := webGet[wire.ConversationsPage](h, fmt.Sprintf("/api/conversations?before=%d&before_thread=%d", first.Next, first.NextThread))
	if len(second.Rows) != 1 || second.Rows[0].ID != parent.Thread || second.Next != 0 || second.NextThread != 0 {
		t.Errorf("second page %+v, want the parent thread %d alone and no cursor", second, parent.Thread)
	}
}

func TestWeb_createPostReplaysItsKey(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")
	key := newTestKey()
	body := wire.PostRequest{Subject: "once", Body: "sent twice", To: wire.PostTarget{Agent: "alpha"}}

	first := h.request(http.MethodPost, "/api/posts", body, map[string]string{headerKey: key})
	again := h.request(http.MethodPost, "/api/posts", body, map[string]string{headerKey: key})
	if first.status != http.StatusCreated || again.status != http.StatusCreated || first.raw != again.raw {
		t.Errorf("first %d %s, again %d %s, want the same 201 answer", first.status, first.raw, again.status, again.raw)
	}
	if c := webGet[wire.Conversation](h, "/api/conversations/"+strconv.Itoa(int(first.body["thread"].(float64)))); len(c.Posts) != 1 {
		t.Errorf("posts %d, want the one post", len(c.Posts))
	}

	body.Body = "a different body"
	if r := h.request(http.MethodPost, "/api/posts", body, map[string]string{headerKey: key}); r.status != http.StatusUnprocessableEntity || r.str("code") != "key_reused" {
		t.Errorf("used key on another body: %d %s, want 422 key_reused", r.status, r.raw)
	}
	if r := h.request(http.MethodPost, "/api/posts", body, noKey); r.status != http.StatusBadRequest || !strings.Contains(r.str("error"), "requires an Idempotency-Key") {
		t.Errorf("no key: %d %s, want 400 naming the key", r.status, r.raw)
	}
}

func TestWeb_createPostRefusals(t *testing.T) {
	h := newSignedIn(t)
	cases := []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"unknownAgent", wire.PostRequest{Subject: "s", Body: "b", To: wire.PostTarget{Agent: "nobody"}}, http.StatusNotFound, "not_found"},
		{"twoTargets", wire.PostRequest{Subject: "s", Body: "b", To: wire.PostTarget{Announce: true, Agent: "x"}}, http.StatusBadRequest, "invalid"},
		{"noSubject", wire.PostRequest{Body: "b", To: wire.PostTarget{Announce: true}}, http.StatusBadRequest, "invalid"},
		{"notAnObject", []int{1}, http.StatusBadRequest, "invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if r := h.request(http.MethodPost, "/api/posts", c.body, nil); r.status != c.status || r.str("code") != c.code {
				t.Errorf("%d %s, want %d %s", r.status, r.raw, c.status, c.code)
			}
		})
	}
}

func TestWeb_markReadClearsTheUnreadCount(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")
	h.directPost(sessA, "user")
	h.directPost(sessA, "user")

	rows := webGet[wire.ConversationsPage](h, "/api/conversations").Rows
	if len(rows) != 1 || rows[0].Unread != 2 {
		t.Fatalf("rows %+v, want one conversation with 2 unread", rows)
	}
	path := "/api/conversations/" + strconv.FormatUint(rows[0].ID, 10) + "/read"
	if r := h.request(http.MethodPost, path, nil, noKey); r.status != http.StatusBadRequest {
		t.Errorf("mark read with no body: %d %s, want 400", r.status, r.raw)
	}
	if r := h.request(http.MethodPost, path, map[string]any{}, noKey); r.status != http.StatusBadRequest || !strings.Contains(r.str("error"), "up_to") {
		t.Errorf("mark read with no up_to: %d %s, want 400 naming up_to", r.status, r.raw)
	}
	upTo := map[string]any{"up_to": rows[0].Last.ID}
	if r := h.request(http.MethodPost, path, upTo, noKey); r.status != http.StatusOK || r.raw != `{"read":2}`+"\n" {
		t.Errorf("mark read: %d %q, want 200 {\"read\":2}", r.status, r.raw)
	}
	if rows = webGet[wire.ConversationsPage](h, "/api/conversations").Rows; rows[0].Unread != 0 {
		t.Errorf("unread after mark read %d, want 0", rows[0].Unread)
	}
	if r := h.request(http.MethodPost, path, upTo, nil); r.status != http.StatusBadRequest || !strings.Contains(r.str("error"), "takes no Idempotency-Key") {
		t.Errorf("mark read with a key: %d %s, want 400", r.status, r.raw)
	}
}

func TestWeb_aConversationMarksThePostsTheUserHasNotRead(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")
	h.directPost(sessA, "user")
	row := webGet[wire.ConversationsPage](h, "/api/conversations").Rows[0]
	conversation := "/api/conversations/" + strconv.FormatUint(row.ID, 10)

	if r := h.request(http.MethodGet, conversation, nil, noKey); r.status != http.StatusOK || !strings.Contains(r.raw, `"unread":true`) {
		t.Errorf("before mark read: %d %s, want a post marked unread", r.status, r.raw)
	}
	if r := h.request(http.MethodPost, conversation+"/read", map[string]any{"up_to": row.Last.ID}, noKey); r.status != http.StatusOK {
		t.Fatalf("mark read: %d %s", r.status, r.raw)
	}
	if r := h.request(http.MethodGet, conversation, nil, noKey); strings.Contains(r.raw, `"unread"`) {
		t.Errorf("after mark read: %s, want no unread field", r.raw)
	}
}

func TestWeb_agentRoutes(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")
	h.mustRegister(sessB, "bravo")
	userPost(h, wire.PostTarget{Agent: "alpha"}, "for alpha")

	list := webGet[wire.AgentList](h, "/api/agents")
	var names []string
	for _, a := range list.Agents {
		names = append(names, a.Name)
	}
	if !slices.Equal(names, []string{"alpha", "bravo"}) {
		t.Errorf("agents %q, want alpha and bravo, without user", names)
	}

	page := webGet[wire.AgentPage](h, "/api/agents/alpha")
	if page.Agent.Name != "alpha" || page.Agent.Host != "host-a/app" || !slices.Equal(page.Agent.Paths, []string{repoGrelot}) ||
		page.Agent.RegisteredMS != testNow.UnixMilli() || page.Agent.ExpiresMS != testNow.Add(6*time.Hour).UnixMilli() {
		t.Errorf("agent %+v, want alpha's card registered now for 6h", page.Agent)
	}
	if page.Reservations == nil || page.Delegates == nil {
		t.Errorf("reservations %v, delegates %v, want empty lists", page.Reservations, page.Delegates)
	}

	hist := webGet[wire.HistoryPage](h, "/api/agents/alpha/history")
	if len(hist.Posts) != 1 || hist.Posts[0].Subject != "for alpha" || hist.Posts[0].Conversation.Kind != wire.ThreadDirect {
		t.Errorf("history %+v, want the direct post from user", hist.Posts)
	}
	if r := h.request(http.MethodGet, "/api/agents/nobody", nil, nil); r.status != http.StatusNotFound || r.str("code") != "not_found" {
		t.Errorf("unknown agent: %d %s, want 404", r.status, r.raw)
	}
}

func TestWeb_anAgentIsOnlineWhileItHoldsAStandingWait(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")
	h.mustRegister(sessB, "bravo")
	if page := webGet[wire.AgentPage](h, "/api/agents/alpha"); page.Agent.Online {
		t.Error("alpha online before any wait")
	}
	card, err := h.store.AgentCard(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	h.srv.presence.opened(card.ID, sessA)

	if page := webGet[wire.AgentPage](h, "/api/agents/alpha"); !page.Agent.Online {
		t.Error("alpha's page: offline while its wait is open")
	}
	online := map[string]bool{}
	for _, a := range webGet[wire.AgentList](h, "/api/agents").Agents {
		online[a.Name] = a.Online
	}
	if !online["alpha"] || online["bravo"] {
		t.Errorf("agent list online %v, want alpha only", online)
	}
}

func TestWeb_queryAndPathRefusals(t *testing.T) {
	h := newSignedIn(t)
	cases := []struct {
		path   string
		status int
	}{
		{"/api/conversations?kind=mine", http.StatusBadRequest},
		{"/api/conversations?before=soon", http.StatusBadRequest},
		{"/api/conversations?before=5&before_thread=x", http.StatusBadRequest},
		{"/api/conversations?before_thread=3", http.StatusBadRequest},
		{"/api/conversations?before=3", http.StatusBadRequest},
		{"/api/conversations/0", http.StatusBadRequest},
		{"/api/conversations/x", http.StatusBadRequest},
		{"/api/conversations/999", http.StatusNotFound},
		{"/api/agents/alpha/history?before=-1", http.StatusBadRequest},
		{"/api/messages/999/receipts", http.StatusNotFound},
		{"/api/stats?range=1h", http.StatusBadRequest},
		{"/api/stats", http.StatusBadRequest},
		{"/api/nowhere", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.path, "/", "_"), func(t *testing.T) {
			if r := h.request(http.MethodGet, c.path, nil, nil); r.status != c.status {
				t.Errorf("GET %s: %d %s, want %d", c.path, r.status, r.raw, c.status)
			}
		})
	}
}

func TestWeb_anUnknownKindIsRefusedByItsParameterName(t *testing.T) {
	r := newSignedIn(t).request(http.MethodGet, "/api/conversations?kind=mine", nil, nil)
	if msg, _ := r.body["error"].(string); r.status != http.StatusBadRequest || !strings.HasPrefix(msg, "kind ") {
		t.Errorf("%d %s, want 400 with an error naming kind", r.status, r.raw)
	}
}

func TestWeb_statsCountsTheRangeInBuckets(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")
	userPost(h, wire.PostTarget{Agent: "alpha"}, "counted")

	st := webGet[wire.Stats](h, "/api/stats?range=24h")
	if st.BucketMS != time.Hour.Milliseconds() || st.EndMS-st.StartMS != 24*time.Hour.Milliseconds() || len(st.Posts) != 24 {
		t.Fatalf("window %d to %d by %d with %d buckets, want 24 one-hour buckets", st.StartMS, st.EndMS, st.BucketMS, len(st.Posts))
	}
	last := st.Posts[23]
	if !last.Open || last.Direct != 1 || st.Posts[22].Open || last.StartMS != st.EndMS-st.BucketMS {
		t.Errorf("last bucket %+v, want the open one holding the direct post", last)
	}
	if st.StartedMS != testNow.UnixMilli() {
		t.Errorf("started %d, want the server's start %d", st.StartedMS, testNow.UnixMilli())
	}
	if st.Samples == nil || st.Events == nil || st.HeldTurns == nil {
		t.Errorf("samples %v, events %v, held turns %v, want empty lists", st.Samples, st.Events, st.HeldTurns)
	}
}

func TestWeb_reservationStatesAreTheWireMembers(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")
	h.mustRegister(sessB, "bravo")
	body := map[string]any{"path": repoGrelot, "reason": "editing", "ttl": "1h"}
	if r := h.as(sessA, http.MethodPost, "/v1/reservations", body); r.status != http.StatusCreated {
		t.Fatalf("reserve: %d %s", r.status, r.raw)
	}
	body["queue"] = true
	if r := h.as(sessB, http.MethodPost, "/v1/reservations", body); r.status != http.StatusCreated {
		t.Fatalf("queue: %d %s", r.status, r.raw)
	}
	for name, want := range map[string]wire.ReservationState{"alpha": wire.ReservationHeld, "bravo": wire.ReservationQueued} {
		rs := webGet[wire.AgentPage](h, "/api/agents/"+name).Reservations
		if len(rs) != 1 || rs[0].State != want {
			t.Errorf("%s's reservations %+v, want one %s", name, rs, want)
		}
	}
}

func TestWeb_anExpiredAgentIsTheWireDormantState(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")
	h.clk.advance(7 * time.Hour)
	if a := webGet[wire.AgentPage](h, "/api/agents/alpha").Agent; a.State != wire.AgentDormant || a.Online {
		t.Errorf("alpha past its 6h lease: state %q online %v, want %q offline", a.State, a.Online, wire.AgentDormant)
	}
}

func TestWeb_everyEventKindIsAWireMember(t *testing.T) {
	want := map[obs.EventKind]wire.EventKind{
		obs.EventPostRefused: wire.EventPostRefused, obs.EventPushRetry: wire.EventPushRetry,
		obs.EventPushGaveUp: wire.EventPushGaveUp, obs.EventNudgeRepeat: wire.EventNudgeRepeat,
	}
	for k, w := range want {
		if got := wire.EventKind(k.String()); got != w {
			t.Errorf("obs kind %d sends %q, want %q", k, got, w)
		}
	}
}
