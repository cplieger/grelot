package wiregate_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/access"
	"github.com/cplieger/access/gate"
	"github.com/cplieger/grelot/internal/wire"
	"github.com/cplieger/grelot/internal/wiregate"
)

// The codes every authenticating row answers (design 1.1, from access's
// refusal.go) and the mount's code for a path no row matches.
var (
	authenticating = []string{
		"auth_method_not_allowed", "auth_required", "auth_session_expired", "auth_invalid_credentials",
		"auth_key_invalid", "auth_session_required", "auth_unavailable",
	}
	mount = []string{"auth_not_found"}
)

func TestRoute_mapsEachAccessToItsClass(t *testing.T) {
	cases := []struct {
		access wire.Access
		class  gate.Class
	}{
		{wire.AccessAgent, gate.Key},
		{wire.AccessOwner, gate.Session},
		{wire.AccessPublic, gate.Public},
	}
	for _, c := range cases {
		r := &wire.Route{Name: "row", Method: http.MethodPost, Path: "/v1/row/{id}", Access: c.access}
		got := wiregate.Route(r)
		if got.Class != c.class || got.Scope != "" {
			t.Errorf("Access %d: class %d scope %q, want class %d and no scope", c.access, got.Class, got.Scope, c.class)
		}
		if got.Name != "POST /v1/row/{id}" || got.Method != http.MethodPost || got.Path != "/v1/row/{id}" {
			t.Errorf("Access %d: %+v, want the row's method and path, named by its pattern", c.access, got)
		}
	}
}

func TestRoute_panicsOnARowAdmittingNoOne(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Route of a zero Access returned, want a panic")
		}
	}()
	wiregate.Route(&wire.Route{Name: "row", Method: http.MethodGet, Path: "/v1/row"})
}

func TestCodes_isTheGatesCodesThenTheMounts(t *testing.T) {
	cases := []struct {
		name   string
		access wire.Access
		method string
		want   []string
	}{
		{"agent read", wire.AccessAgent, http.MethodGet, slices.Concat(authenticating, []string{"auth_key_required"}, mount)},
		{"agent write", wire.AccessAgent, http.MethodPost, slices.Concat(authenticating, []string{"auth_key_required"}, mount)},
		{"owner read", wire.AccessOwner, http.MethodGet, slices.Concat(authenticating, mount)},
		{"owner write", wire.AccessOwner, http.MethodPut, slices.Concat(authenticating, []string{"auth_cross_origin"}, mount)},
		{"public read", wire.AccessPublic, http.MethodGet, slices.Concat([]string{"auth_method_not_allowed"}, mount)},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			r := &wire.Route{Name: "row", Method: c.method, Path: "/v1/row", Access: c.access}
			codes := wiregate.Codes(r)
			got := make([]string, 0, len(codes))
			for _, code := range codes {
				got = append(got, string(code))
			}
			if want := slices.Sorted(slices.Values(c.want)); !slices.Equal(slices.Sorted(slices.Values(got)), want) {
				t.Errorf("Codes = %v, want %v", got, want)
			}
			if gateCodes := gate.Codes(wiregate.Route(r)); !slices.Equal(codes, append(gateCodes, access.CodeNotFound)) {
				t.Errorf("Codes = %v, want gate.Codes %v followed by the mount's", codes, gateCodes)
			}
		})
	}
}
