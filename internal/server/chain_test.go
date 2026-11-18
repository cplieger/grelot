//go:build !windows

package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

// An agent row runs the identity chain: the server's version on every
// answer, the key's bucket before the client's version, the client's
// version before the row's keyed rule, and the session's status line on an
// answer to a request that named one.
// drained is one request's burst, refilled once in 1000 seconds.
var drained = Limits{Rate: 0.001, Burst: 1, KeyWaits: 1, ServerWaits: 1}

func TestChain_anAgentRowRunsTheIdentityChain(t *testing.T) {
	for _, c := range []struct {
		name       string
		method     string
		path       string
		headers    map[string]string
		limits     Limits
		drain      bool
		wantStatus int
		wantCode   string
		wantLine   bool
	}{
		{
			name: "no client version", method: http.MethodGet, path: "/v1/me",
			headers: map[string]string{wire.HeaderVersion: "", wire.HeaderSession: sessA}, limits: unlimited,
			wantStatus: http.StatusUpgradeRequired, wantCode: "upgrade_required",
		},
		{
			name: "a client below the floor", method: http.MethodGet, path: "/v1/me",
			headers: map[string]string{wire.HeaderVersion: "v1.0.0", wire.HeaderSession: sessA}, limits: unlimited,
			wantStatus: http.StatusUpgradeRequired, wantCode: "upgrade_required",
		},
		{
			name: "a client below the floor on a keyed row sending no key", method: http.MethodPost, path: "/v1/register",
			headers: map[string]string{wire.HeaderVersion: "v1.0.0", wire.HeaderSession: sessA, wire.HeaderIdempotencyKey: ""}, limits: unlimited,
			wantStatus: http.StatusUpgradeRequired, wantCode: "upgrade_required",
		},
		{
			name: "an empty bucket before the version", method: http.MethodGet, path: "/v1/me",
			headers: map[string]string{wire.HeaderVersion: "", wire.HeaderSession: sessA}, limits: drained, drain: true,
			wantStatus: http.StatusTooManyRequests, wantCode: "rate_limited",
		},
		{
			name: "a session's read", method: http.MethodGet, path: "/v1/me",
			headers: map[string]string{wire.HeaderSession: sessA}, limits: unlimited,
			wantStatus: http.StatusOK, wantLine: true,
		},
		{
			name: "a read with no session", method: http.MethodGet, path: "/v1/version",
			limits: unlimited, wantStatus: http.StatusOK,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarnessWith(t, store.Options{}, func(o *Options) { o.Limits = c.limits })
			if c.drain {
				h.request(http.MethodGet, "/v1/version", nil, nil)
			}
			r := h.request(c.method, c.path, nil, c.headers)
			if r.status != c.wantStatus || r.str("code") != c.wantCode {
				t.Errorf("%d %s, want %d %q", r.status, r.raw, c.wantStatus, c.wantCode)
			}
			if got := r.header.Get(wire.HeaderVersion); got != testVersion {
				t.Errorf("Grelot-Version %q, want the server's %s", got, testVersion)
			}
			if got := r.line(); (got != "") != c.wantLine {
				t.Errorf("Grelot-Status %q, want one: %v", got, c.wantLine)
			}
		})
	}
}

// An owner row reads no Grelot-* header: a session's request with no
// client version is served, gets no status line, and takes no bucket token.
func TestChain_anOwnerRowReadsNoAgentHeaders(t *testing.T) {
	h := newSignedIn(t, func(o *Options) { o.Limits = drained })
	for range 5 {
		r := h.request(http.MethodGet, "/api/settings", nil, map[string]string{wire.HeaderVersion: "", wire.HeaderSession: sessA})
		if r.status != http.StatusOK {
			t.Fatalf("GET /api/settings: %d %s, want 200", r.status, r.raw)
		}
		if got := r.header.Get(wire.HeaderVersion); got != testVersion {
			t.Errorf("Grelot-Version %q, want the server's %s", got, testVersion)
		}
		if got := r.line(); got != "" {
			t.Errorf("Grelot-Status %q, want none", got)
		}
	}
}

func TestChain_aPublicRowAnswersWithTheServersVersion(t *testing.T) {
	h := newHarnessWith(t, store.Options{}, withAuth(testAuth(t)))
	r := h.request(http.MethodGet, "/api/sign-in", nil, map[string]string{wire.HeaderVersion: ""})
	if r.status != http.StatusOK || r.header.Get(wire.HeaderVersion) != testVersion {
		t.Errorf("GET /api/sign-in: %d %s with Grelot-Version %q, want 200 with %s",
			r.status, r.raw, r.header.Get(wire.HeaderVersion), testVersion)
	}
}

// Every JSON row's keyed rule is its Keyed flag: a keyed row refuses a
// request with no Idempotency-Key and an unkeyed row one that sends a key.
func TestChain_everyRowTakesTheKeyedRuleOfItsFlag(t *testing.T) {
	h := newSignedIn(t)
	fill := strings.NewReplacer("{id}", "1", "{name}", "alpha", "{label}", "review")
	for i := range wire.Routes {
		route := &wire.Routes[i]
		if route.Kind != wire.KindJSON {
			continue
		}
		t.Run(route.Name, func(t *testing.T) {
			var body any
			if route.Method != http.MethodGet {
				body = map[string]any{}
			}
			headers := map[string]string{wire.HeaderSession: sessA, wire.HeaderIdempotencyKey: newTestKey()}
			want := "this route takes no Idempotency-Key"
			if route.Keyed {
				headers[wire.HeaderIdempotencyKey] = ""
				want = "requires an Idempotency-Key"
			}
			r := h.request(route.Method, fill.Replace(route.Path), body, headers)
			if r.status != http.StatusBadRequest || r.str("code") != "invalid" || !strings.Contains(r.str("error"), want) {
				t.Errorf("%s %s: %d %s, want 400 invalid saying %q", route.Method, route.Path, r.status, r.raw, want)
			}
		})
	}
}

func TestCheckBindings(t *testing.T) {
	identity := binding{identity: func(*http.Request, store.Caller, store.Keyed) (int, any, error) { return 0, nil, nil }}
	owner := binding{owner: func(*http.Request, store.Keyed) (any, error) { return nil, nil }}
	stream := binding{stream: http.NotFoundHandler()}
	agentRow := wire.Route{Name: "agentRow", Method: http.MethodGet, Path: "/v1/a", Access: wire.AccessAgent}
	ownerRow := wire.Route{Name: "ownerRow", Method: http.MethodGet, Path: "/v1/o", Access: wire.AccessOwner}
	publicRow := wire.Route{Name: "publicRow", Method: http.MethodGet, Path: "/v1/p", Access: wire.AccessPublic}
	streamRow := wire.Route{Name: "streamRow", Method: http.MethodGet, Path: "/v1/s", Access: wire.AccessOwner, Kind: wire.KindEventStream}
	noAccess := wire.Route{Name: "noAccess", Method: http.MethodGet, Path: "/v1/n"}
	for _, c := range []struct {
		name     string
		bindings map[string]binding
		wantErr  string
		routes   []wire.Route
	}{
		{
			name:   "every row bound by its class",
			routes: []wire.Route{agentRow, ownerRow, publicRow, streamRow},
			bindings: map[string]binding{
				"agentRow": identity, "ownerRow": owner, "publicRow": owner, "streamRow": stream,
			},
		},
		{
			name: "a row with no binding", routes: []wire.Route{agentRow, ownerRow},
			bindings: map[string]binding{"agentRow": identity}, wantErr: "ownerRow",
		},
		{
			name: "a binding with no row", routes: []wire.Route{agentRow},
			bindings: map[string]binding{"agentRow": identity, "stray": owner}, wantErr: "stray",
		},
		{
			name: "an agent row bound to an owner function", routes: []wire.Route{agentRow},
			bindings: map[string]binding{"agentRow": owner}, wantErr: "agentRow",
		},
		{
			name: "an owner row bound to an identity function", routes: []wire.Route{ownerRow},
			bindings: map[string]binding{"ownerRow": identity}, wantErr: "ownerRow",
		},
		{
			name: "an agent row bound to both", routes: []wire.Route{agentRow},
			bindings: map[string]binding{"agentRow": {identity: identity.identity, owner: owner.owner}}, wantErr: "agentRow",
		},
		{
			name: "a stream row bound to an owner function", routes: []wire.Route{streamRow},
			bindings: map[string]binding{"streamRow": owner}, wantErr: "streamRow",
		},
		{
			name: "a row with no access", routes: []wire.Route{noAccess},
			bindings: map[string]binding{"noAccess": owner}, wantErr: "noAccess",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := checkBindings(c.routes, c.bindings)
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("checkBindings: %v, want nil", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Errorf("checkBindings: %v, want an error naming %s", err, c.wantErr)
			}
		})
	}
}
