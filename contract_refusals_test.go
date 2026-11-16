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
	"sync"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/server"
)

// rowRequest is one well-formed request of an agent row.
type rowRequest struct {
	body any
	path string
}

// agentRequests is a well-formed request of every agent row from a:
// register and revive name name, a post goes to peer and an id is 1.
func agentRequests(a *agent, name, peer string) map[string]rowRequest {
	return map[string]rowRequest{
		"register": {a.registerBody(name, hourMS, repoPath), "/v1/register"},
		"revive": {
			map[string]any{"name": name, "host": "host-" + a.env, "os": "linux", "arch": "amd64", "where": a.where(), "ttl_ms": hourMS},
			"/v1/revive",
		},
		"getMe":              {nil, "/v1/me"},
		"patchMe":            {map[string]any{"description": "patched", "ttl_ms": hourMS, "where": a.where()}, "/v1/me"},
		"deleteMe":           {nil, "/v1/me"},
		"listIdentities":     {nil, "/v1/identities"},
		"getVersion":         {nil, "/v1/version"},
		"createPost":         {postBody(map[string]any{"agent": peer}, "hello", "a body"), "/v1/posts"},
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
}

// probe is send held to the contract whatever its status.
func (a *agent) probe(row string, req rowRequest, headers map[string]string) *answer {
	a.c.t.Helper()
	ans := a.send(row, req.path, req.body, headers)
	a.c.check(agentRowNamed(a.c.t, row).method, req.path, row, ans)
	return ans
}

// refuses is probe failing unless the answer's code is code.
func (a *agent) refuses(code, row string, req rowRequest, headers map[string]string) *answer {
	a.c.t.Helper()
	ans := a.probe(row, req, headers)
	if ans.str("code") != code {
		a.c.t.Errorf("%s %s: %d %s, want code %s", row, req.path, ans.status, ans.raw, code)
	}
	return ans
}

// asSelf is the header of an agent's own command, which passes its
// delegate gate.
var asSelf = map[string]string{"Grelot-Self": "1"}

// ownedElsewhere are the codes a row's union carries that another test
// provokes: the client floor (TestV1ClientUpgradeRequired, every row), a
// key's bucket (TestContract_aKeyPastItsBucketGets429), the cursor
// (TestContract_aCursorIsBoundToItsRowQueryAndKey) and the gate's own,
// plus what no request can choose: a deadline, a stop, a failure.
func ownedElsewhere(code string) bool {
	return strings.HasPrefix(code, "auth_") || slices.Contains([]string{
		"upgrade_required", "rate_limited", "cursor_invalid", "cursor_mismatch", "timeout", "shutting_down", "internal_error",
	}, code)
}

// raceOnly are the codes a row answers only when its identity goes dormant
// or retires between two of its reads, which no request can time: getMe
// reads the identity, then a live one's status.
var raceOnly = map[string][]string{"getMe": {"none", "dormant"}}

// declaredCodes is every agent row's union in the document, less the
// codes ownedElsewhere and raceOnly.
func declaredCodes(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(contractDoc)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Responses   map[string]struct {
				Codes map[string]any `json:"x-error-codes"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, row := range agentRows {
		for _, resp := range doc.Paths[row.path][strings.ToLower(row.method)].Responses {
			for code := range resp.Codes {
				if !ownedElsewhere(code) && !slices.Contains(raceOnly[row.name], code) {
					out[row.name] = append(out[row.name], code)
				}
			}
		}
	}
	return out
}

// TestContract_eachAgentRowAnswersEveryCodeItDeclares holds both sides of
// each agent row's union: check refuses a code the row's union leaves out,
// and every code it lists must come back from some case here.
func TestContract_eachAgentRowAnswersEveryCodeItDeclares(t *testing.T) {
	seen := map[string]map[string]bool{}
	parts := []struct {
		run  func(*contract)
		name string
		opts contractOptions
	}{
		{name: "the agent scenario", run: runScenario},
		{name: "the caller's refusals on every row", run: callerRefusals},
		{name: "each row's own refusals", run: ownRefusals},
		{name: "a standing wait replaced and ended", run: waitEnds},
		{name: "a key past its held waits", run: tooManyWaits, opts: contractOptions{limits: server.Limits{Rate: 1e9, Burst: 1e9, KeyWaits: 1, ServerWaits: 1e6}}},
		{name: "a nearly full store", run: storeFull, opts: contractOptions{maxBytes: 2 << 20}},
	}
	for _, p := range parts {
		t.Run(p.name, func(t *testing.T) {
			c := newContractWith(t, p.opts)
			c.codes = seen
			p.run(c)
		})
	}
	for row, codes := range declaredCodes(t) {
		for _, code := range codes {
			if !seen[row][code] {
				t.Errorf("%s declares %s, which no case provoked", row, code)
			}
		}
	}
}

// callerRefusals sends every agent row each refusal of the caller its
// headers name, each row's Idempotency-Key with another request, and a
// body past the cap.
func callerRefusals(c *contract) {
	k1, k2 := c.mintKey("machine-1"), c.mintKey("machine-2")
	alpha, beta, gated := c.agent(k1, sessAlpha, "a"), c.agent(k2, sessBeta, "b"), c.agent(k1, sessGamma, "a")
	alpha.call(http.StatusCreated, "register", "/v1/register", alpha.registerBody("alpha", hourMS, repoPath))
	beta.call(http.StatusCreated, "register", "/v1/register", beta.registerBody("beta", hourMS, repoPath))
	gated.call(http.StatusCreated, "register", "/v1/register", gated.registerBody("gated", hourMS, otherPath))
	gated.call(http.StatusCreated, "grantDelegate", "/v1/delegates", map[string]any{"label": "helper", "ttl_ms": hourMS})
	reviver := c.agent(k1, "kiro-cli:kr-revive", "a")
	reviver.call(http.StatusCreated, "register", "/v1/register", reviver.registerBody("kr-revive", shortMS, otherPath))
	dormant := map[string]*agent{}
	for i, row := range agentRows {
		d := c.agent(k1, fmt.Sprintf("kiro-cli:dormant-%d", i), "a")
		d.call(http.StatusCreated, "register", "/v1/register", d.registerBody(fmt.Sprintf("dormant-%d", i), shortMS, otherPath))
		dormant[row.name] = d
	}
	c.clk.advance(time.Duration(shortMS)*time.Millisecond + time.Minute)
	otherKey := c.agent(k1, sessBeta, "b")
	for i, row := range agentRows {
		fresh := c.agent(k1, fmt.Sprintf("kiro-cli:none-%d", i), "a")
		req := func(a *agent) rowRequest { return agentRequests(a, fmt.Sprintf("none-%d", i), "beta")[row.name] }
		alpha.probe(row.name, req(alpha), map[string]string{"Grelot-Self": "2"})
		otherKey.probe(row.name, req(otherKey), nil)
		fresh.probe(row.name, req(fresh), nil)
		dormant[row.name].probe(row.name, req(dormant[row.name]), nil)
		alpha.probe(row.name, req(alpha), map[string]string{"Grelot-As": "alpha/nobody"})
		gated.probe(row.name, req(gated), nil)
		if row.keyed || row.method == http.MethodPost || row.method == http.MethodPatch {
			big := rowRequest{map[string]any{"pad": strings.Repeat("x", (maxBodyKB+2)<<10)}, req(alpha).path}
			alpha.refuses("too_large", row.name, big, asSelf)
		}
	}
	for i, row := range agentRows {
		if row.keyed {
			keyReused(c, k1, i, row, reviver)
		}
	}
}

// keyReused sends row once with a key from a session of its own, or
// reviver's dormant one, then the same key with the request changed by
// Grelot-Self, or on revokeDelegate, whose gate needs Self, by its query;
// the key binds both.
func keyReused(c *contract, key string, i int, row agentRow, reviver *agent) {
	t := c.t
	name := fmt.Sprintf("kr-%d", i)
	a := c.agent(key, "kiro-cli:"+name, "a")
	reqs := agentRequests(a, name, "alpha")
	switch row.name {
	case "register":
	case "revive":
		a, reqs = reviver, agentRequests(reviver, "kr-revive", "alpha")
	default:
		a.call(http.StatusCreated, "register", "/v1/register", a.registerBody(name, hourMS, "/workspace/"+name))
	}
	switch row.name {
	case "reserve", "renewReservation", "releaseReservation":
		r := a.call(http.StatusCreated, "reserve", "/v1/reservations", map[string]any{"path": "/workspace/" + name + "/x", "reason": "a change", "ttl_ms": hourMS})
		reqs["reserve"] = rowRequest{map[string]any{"path": "/workspace/" + name + "/y", "reason": "a change", "ttl_ms": hourMS}, "/v1/reservations"}
		id := num(obj(r.body, "reservation"), "id")
		reqs["renewReservation"] = rowRequest{map[string]any{"ttl_ms": hourMS}, fmt.Sprintf("/v1/reservations/%d", id)}
		reqs["releaseReservation"] = rowRequest{nil, fmt.Sprintf("/v1/reservations/%d", id)}
	case "revokeDelegate":
		a.call(http.StatusCreated, "grantDelegate", "/v1/delegates", map[string]any{"label": "helper", "ttl_ms": hourMS})
	}
	idem := map[string]string{"Idempotency-Key": newIdemKey()}
	again := reqs[row.name]
	if row.name == "revokeDelegate" {
		idem["Grelot-Self"] = "1"
		again.path += "?again=1"
	}
	if first := a.probe(row.name, reqs[row.name], idem); first.status >= http.StatusBadRequest {
		t.Errorf("%s's first send with its key: %s", row.name, first.raw)
		return
	}
	idem["Grelot-Self"] = "1"
	a.refuses("key_reused", row.name, again, idem)
}

// checkout is a where of a's with one checkout of repoPath on remote.
func checkout(a *agent, remote string) map[string]any {
	w := a.where()
	w["checkouts"] = []any{map[string]any{"repo": repoPath, "id": strings.Repeat("1", 32), "remote": strings.Repeat(remote, 32)}}
	return w
}

// ownRefusals provokes each row's refusals of its own request.
func ownRefusals(c *contract) {
	t := c.t
	k := c.mintKey("machine")
	alpha, beta, gamma := c.agent(k, sessAlpha, "a"), c.agent(k, sessBeta, "a"), c.agent(k, sessGamma, "a")
	body := alpha.registerBody("alpha", hourMS, repoPath)
	body["where"] = checkout(alpha, "a")
	alpha.call(http.StatusCreated, "register", "/v1/register", body)
	beta.call(http.StatusCreated, "register", "/v1/register", beta.registerBody("beta", hourMS, repoPath))
	gamma.call(http.StatusCreated, "register", "/v1/register", gamma.registerBody("gamma", hourMS, repoPath))
	rho, rho2 := c.agent(k, "kiro-cli:rho", "a"), c.agent(k, "kiro-cli:rho2", "a")
	rho.call(http.StatusCreated, "register", "/v1/register", rho.registerBody("rho", shortMS, repoPath))
	rho2.call(http.StatusCreated, "register", "/v1/register", rho2.registerBody("rho2", shortMS, otherPath))

	t.Run("register", func(t *testing.T) {
		c.t = t
		zeta := c.agent(k, "kiro-cli:zeta", "a")
		zeta.refuses("conflict", "register", rowRequest{zeta.registerBody("alpha", hourMS, repoPath), "/v1/register"}, nil)
		away := c.agent(k, "kiro-cli:away", "b")
		over := away.registerBody("alpha", hourMS, repoPath)
		over["take_over"] = true
		away.refuses("other_environment", "register", rowRequest{over, "/v1/register"}, nil)
		none := c.agent(k, "kiro-cli:noenv", "")
		over = none.registerBody("alpha", hourMS, repoPath)
		over["take_over"] = true
		none.refuses("no_environment", "register", rowRequest{over, "/v1/register"}, nil)
		other := zeta.registerBody("zeta", hourMS, repoPath)
		other["where"] = checkout(zeta, "b")
		zeta.refuses("other_repository", "register", rowRequest{other, "/v1/register"}, nil)
		link := alpha.callWith(http.StatusCreated, "grantDelegate", "/v1/delegates", map[string]any{"label": "once", "ttl_ms": hourMS}, asSelf).str("link")
		sub := map[string]any{"delegate": link, "description": "a helper", "host": "host-a", "os": "linux", "arch": "amd64", "where": alpha.where()}
		alpha.callWith(http.StatusCreated, "register", "/v1/register", sub, asSelf)
		c.agent(k, "kiro-cli:late", "a").refuses("link_gone", "register", rowRequest{sub, "/v1/register"}, nil)
		alpha.callWith(http.StatusOK, "revokeDelegate", "/v1/delegates/once", nil, asSelf)
	})
	t.Run("patchMe", func(t *testing.T) {
		c.t = t
		beta.refuses("conflict", "patchMe", rowRequest{map[string]any{"name": "alpha"}, "/v1/me"}, nil)
		beta.refuses("other_repository", "patchMe", rowRequest{map[string]any{"ttl_ms": hourMS, "where": checkout(beta, "b")}, "/v1/me"}, nil)
	})
	t.Run("createPost", func(t *testing.T) {
		c.t = t
		beta.refuses("not_found", "createPost", rowRequest{postBody(map[string]any{"agent": "nobody"}, "hello", "a body"), "/v1/posts"}, nil)
		renamed := postBody("announce", "hello", "a body")
		renamed["identity"] = map[string]any{"name": "alpha", "description": "no change", "paths": []string{"no change"}}
		beta.refuses("conflict", "createPost", rowRequest{renamed, "/v1/posts"}, nil)
		moved := postBody("announce", "hello", "a body")
		moved["identity"] = map[string]any{"name": "no change", "description": "no change", "paths": []string{repoPath, otherPath}}
		moved["where"] = checkout(beta, "b")
		beta.refuses("other_repository", "createPost", rowRequest{moved, "/v1/posts"}, nil)
	})
	t.Run("ids naming nothing", func(t *testing.T) {
		c.t = t
		beta.refuses("not_found", "getReceipts", rowRequest{nil, "/v1/messages/999999/receipts"}, nil)
		beta.refuses("not_found", "renewReservation", rowRequest{map[string]any{"ttl_ms": hourMS}, "/v1/reservations/999999"}, nil)
		beta.refuses("not_found", "releaseReservation", rowRequest{nil, "/v1/reservations/999999"}, nil)
		beta.refuses("not_found", "revokeDelegate", rowRequest{nil, "/v1/delegates/nobody"}, nil)
	})
	t.Run("reserve", func(t *testing.T) {
		c.t = t
		alpha.call(http.StatusCreated, "reserve", "/v1/reservations", map[string]any{"path": repoPath + "/a", "reason": "a change", "ttl_ms": hourMS})
		beta.refuses("reserved", "reserve", rowRequest{map[string]any{"path": repoPath + "/a", "reason": "next", "ttl_ms": hourMS}, "/v1/reservations"}, nil)
		beta.call(http.StatusCreated, "reserve", "/v1/reservations", map[string]any{"path": repoPath, "reason": "all of it", "ttl_ms": hourMS, "queue": true})
		gamma.refuses("queued", "reserve", rowRequest{map[string]any{"path": repoPath + "/b", "reason": "b", "ttl_ms": hourMS}, "/v1/reservations"}, nil)
	})
	t.Run("grantDelegate", func(t *testing.T) {
		c.t = t
		gamma.call(http.StatusCreated, "grantDelegate", "/v1/delegates", map[string]any{"label": "twice", "ttl_ms": hourMS})
		gamma.refuses("conflict", "grantDelegate", rowRequest{map[string]any{"label": "twice", "ttl_ms": hourMS}, "/v1/delegates"}, asSelf)
	})
	t.Run("revive", func(t *testing.T) {
		c.t = t
		c.clk.advance(time.Duration(shortMS)*time.Millisecond + time.Minute)
		alpha.refuses("conflict", "revive", agentRequests(alpha, "rho", "")["revive"], asSelf)
		rho2.refuses("dormant", "lifecycle", rowRequest{map[string]any{"event": "session_end"}, "/v1/lifecycle"}, nil)
		claude := c.agent(k, "claude-code:rho2", "a")
		claude.refuses("other_provider", "revive", agentRequests(claude, "rho2", "")["revive"], nil)
		none := c.agent(k, "kiro-cli:rho-noenv", "")
		none.refuses("no_environment", "revive", agentRequests(none, "rho2", "")["revive"], nil)
		req := agentRequests(rho, "rho", "")["revive"]
		req.body.(map[string]any)["where"] = checkout(rho, "b")
		rho.refuses("other_repository", "revive", req, nil)
	})
	c.t = t
}

// waitEnds has a session's two standing waits overlap, so one of them is
// replaced, and a wait after the session's reported end end at once.
func waitEnds(c *contract) {
	t := c.t
	k := c.mintKey("machine")
	a, b := c.agent(k, sessAlpha, "a"), c.agent(k, sessBeta, "a")
	a.call(http.StatusCreated, "register", "/v1/register", a.registerBody("alpha", hourMS, repoPath))
	b.call(http.StatusCreated, "register", "/v1/register", b.registerBody("beta", hourMS, repoPath))
	wait := rowRequest{map[string]any{"wait_ms": int64(3000)}, "/v1/events/wait"}
	// A wait with no nudge state may answer 204 before the next opens, so
	// the pair is sent again until one of them is replaced.
	replaced := false
	for range 20 {
		var first *answer
		var wg sync.WaitGroup
		wg.Go(func() { first = a.send("waitEvent", wait.path, wait.body, nil) })
		second := a.send("waitEvent", wait.path, map[string]any{"wait_ms": int64(500)}, nil)
		wg.Wait()
		for _, ans := range []*answer{first, second} {
			c.check(http.MethodPost, wait.path, "waitEvent", ans)
			replaced = replaced || ans.str("code") == "replaced"
		}
		if replaced {
			break
		}
	}
	if !replaced {
		t.Error("twenty pairs of overlapping waits of one session: none replaced")
	}
	b.call(http.StatusOK, "lifecycle", "/v1/lifecycle", map[string]any{"event": "session_end"})
	b.refuses("ended", "waitEvent", wait, nil)
}

// tooManyWaits holds a key's one wait open with an inbox read, which holds
// to its wait_ms with no mail, and sends the key's other session a
// standing wait and a held inbox read until one is refused. A probe that
// takes the slot first refuses the hold instead, so the hold is opened
// again.
func tooManyWaits(c *contract) {
	t := c.t
	k := c.mintKey("machine")
	a, b := c.agent(k, sessAlpha, "a"), c.agent(k, sessBeta, "a")
	a.call(http.StatusCreated, "register", "/v1/register", a.registerBody("alpha", hourMS, repoPath))
	b.call(http.StatusCreated, "register", "/v1/register", b.registerBody("beta", hourMS, otherPath))
	for _, req := range []struct {
		row string
		rowRequest
	}{
		{"waitEvent", rowRequest{map[string]any{"wait_ms": int64(1)}, "/v1/events/wait"}},
		{"readInbox", rowRequest{nil, "/v1/inbox?wait_ms=1"}},
	} {
		refused := false
		for attempt := 0; attempt < 5 && !refused; attempt++ {
			var wg sync.WaitGroup
			wg.Go(func() { a.send("readInbox", "/v1/inbox?wait_ms=1500", nil, nil) })
			time.Sleep(50 * time.Millisecond) // the hold's head start; a lost race is the next attempt
			for deadline := time.Now().Add(time.Second); !refused && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				refused = b.probe(req.row, req.rowRequest, nil).str("code") == "too_many_waits"
			}
			wg.Wait()
		}
		if !refused {
			t.Errorf("%s past the key's one held wait: never too_many_waits", req.row)
		}
	}
}

// storeFull fills the store past 90 % of its ceiling with posts, then
// sends each row that adds rows.
func storeFull(c *contract) {
	t := c.t
	k := c.mintKey("machine")
	a := c.agent(k, sessAlpha, "a")
	a.call(http.StatusCreated, "register", "/v1/register", a.registerBody("alpha", hourMS, repoPath))
	filler := rowRequest{postBody("announce", "filler", strings.Repeat("x", 60<<10)), "/v1/posts"}
	for i := 0; ; i++ {
		ans := a.probe("createPost", filler, nil)
		if ans.str("code") == "store_full" {
			break
		}
		if ans.status != http.StatusCreated || i == 200 {
			t.Fatalf("filling the store, post %d: %d %s", i, ans.status, ans.raw)
		}
	}
	fresh := c.agent(k, "kiro-cli:fresh", "a")
	fresh.refuses("store_full", "register", rowRequest{fresh.registerBody("fresh", hourMS, repoPath), "/v1/register"}, nil)
	a.refuses("store_full", "patchMe", rowRequest{map[string]any{"paths": []string{repoPath, otherPath}, "where": a.where()}, "/v1/me"}, nil)
	a.refuses("store_full", "follow", rowRequest{nil, "/v1/follows?path=" + url.QueryEscape(otherPath)}, nil)
	a.refuses("store_full", "reserve", rowRequest{map[string]any{"path": repoPath, "reason": "a change", "ttl_ms": hourMS}, "/v1/reservations"}, nil)
	a.refuses("store_full", "grantDelegate", rowRequest{map[string]any{"label": "helper", "ttl_ms": hourMS}, "/v1/delegates"}, nil)
}
