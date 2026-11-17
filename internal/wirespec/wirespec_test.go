package wirespec

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/access"
	"github.com/cplieger/go/wiregen/v2"
	"github.com/cplieger/grelot/internal/wire"
	"github.com/cplieger/grelot/internal/wiregate"
)

// A route added to wire.Routes or to specs alone fails here, before the
// generator writes a document that lacks it or the server binds it bare.
func TestSpecCoversEveryRow(t *testing.T) {
	rows := map[string]bool{}
	for i := range wire.Routes {
		name := wire.Routes[i].Name
		if rows[name] {
			t.Errorf("two rows are named %s", name)
		}
		rows[name] = true
		if _, ok := specs[name]; !ok {
			t.Errorf("row %s has no spec", name)
		}
	}
	for name := range specs {
		if !rows[name] {
			t.Errorf("spec %s names no row", name)
		}
	}
}

// TestPagedRowsAnswerAList holds design 1.3's rule: a Paged row answers a
// named struct whose one embedded field is a wire.List instantiation and
// its query embeds wire.Page, and no other row's answer embeds a List or
// query embeds a Page.
func TestPagedRowsAnswerAList(t *testing.T) {
	for i := range wire.Routes {
		r := &wire.Routes[i]
		s := specs[r.Name]
		embedded := 0
		lists := 0
		if s.Response != nil && s.Response.Kind() == reflect.Struct {
			for f := range s.Response.Fields() {
				if !f.Anonymous {
					continue
				}
				embedded++
				if isList(f.Type) {
					lists++
				}
			}
		}
		switch {
		case r.Paged && (s.Response == nil || isList(s.Response) || embedded != 1 || lists != 1):
			t.Errorf("Paged row %s answers %v, want a named struct embedding one wire.List", r.Name, s.Response)
		case r.Paged && !embedsPage(s.Query):
			t.Errorf("Paged row %s's query %v embeds no wire.Page", r.Name, s.Query)
		case !r.Paged && lists > 0:
			t.Errorf("row %s is not Paged but answers %v, which embeds a wire.List", r.Name, s.Response)
		case !r.Paged && embedsPage(s.Query):
			t.Errorf("row %s is not Paged but its query %v embeds a wire.Page", r.Name, s.Query)
		}
	}
}

func isList(t reflect.Type) bool {
	return t.PkgPath() == reflect.TypeFor[wire.Error]().PkgPath() && strings.HasPrefix(t.Name(), "List[")
}

func embedsPage(q reflect.Type) bool {
	if q == nil {
		return false
	}
	for f := range q.Fields() {
		if f.Anonymous && f.Type == reflect.TypeFor[wire.Page]() {
			return true
		}
	}
	return false
}

// endpoint is the Registry endpoint of the row named name.
func endpoint(t *testing.T, reg *wiregen.Registry, name string) *wiregen.Endpoint {
	t.Helper()
	i := slices.IndexFunc(reg.Endpoints, func(e wiregen.Endpoint) bool { return e.Name == name })
	if i < 0 {
		t.Fatalf("Registry has no endpoint %s", name)
	}
	return &reg.Endpoints[i]
}

// Every row's declared auth_ codes are exactly the gate's for that row,
// each with access's status, so the generated union and the running gate
// cannot drift (design 1.1).
func TestRowsDeclareTheGatesCodes(t *testing.T) {
	reg := Registry()
	for i := range wire.Routes {
		r := &wire.Routes[i]
		t.Run(r.Name, func(t *testing.T) {
			declared := map[string]int{}
			for _, e := range endpoint(t, reg, r.Name).Errors {
				if strings.HasPrefix(e.Code, "auth_") {
					declared[e.Code] = e.Status
				}
			}
			want := map[string]int{}
			for _, c := range wiregate.Codes(r) {
				want[string(c)] = access.NewError(c, "", nil).Status
			}
			if !maps.Equal(declared, want) {
				t.Errorf("declared %v, want the gate's %v", declared, want)
			}
		})
	}
}

// Every row's union holds grelot's common codes: a deadline passed, the
// server stopping, a failure of its own, and on every row the gate
// authenticates the key bucket's 429; a list row adds the cursor's two
// refusals (design 1.1).
func TestRowsDeclareTheCommonCodes(t *testing.T) {
	reg := Registry()
	for i := range wire.Routes {
		r := &wire.Routes[i]
		t.Run(r.Name, func(t *testing.T) {
			declared := map[string]int{}
			for _, e := range endpoint(t, reg, r.Name).Errors {
				declared[e.Code] = e.Status
			}
			want := map[string]int{"timeout": 503, "shutting_down": 503, "internal_error": 500}
			if r.Access != wire.AccessPublic {
				want["rate_limited"] = 429
			}
			if r.Paged {
				want["cursor_invalid"] = 400
				want["cursor_mismatch"] = 400
			}
			for code, status := range want {
				if got, ok := declared[code]; !ok || got != status {
					t.Errorf("%s: declared %d (present %v), want %d", code, got, ok, status)
				}
			}
			if _, ok := declared["rate_limited"]; ok && r.Access == wire.AccessPublic {
				t.Error("a public row declares rate_limited, which takes no key's token")
			}
		})
	}
}

// Each row names the credential its Access admits: an agent row a bearer
// key, an owner row the session cookie, a public row none (design 2).
func TestRowsDeclareTheirCredential(t *testing.T) {
	reg := Registry()
	if s := reg.OpenAPI.SecuritySchemes["bearerKey"]; s.Type != "http" || s.Scheme != "bearer" {
		t.Errorf("bearerKey is %+v, want http bearer", s)
	}
	if s := reg.OpenAPI.SecuritySchemes["sessionCookie"]; s.Type != "apiKey" || s.In != "cookie" || s.Name != "auth_session" {
		t.Errorf("sessionCookie is %+v, want the auth_session cookie", s)
	}
	for i := range wire.Routes {
		r := &wire.Routes[i]
		var want []wiregen.SecurityRequirement
		switch r.Access {
		case wire.AccessAgent:
			want = []wiregen.SecurityRequirement{{"bearerKey": nil}}
		case wire.AccessOwner:
			want = []wiregen.SecurityRequirement{{"sessionCookie": nil}}
		default:
		}
		if got := endpoint(t, reg, r.Name).Security; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: security %v, want %v", r.Name, got, want)
		}
	}
}

// A wire.Kind added with no wiregen kind stops the generator rather than
// reaching the document as JSON.
func TestKindOf_mapsEveryKindAndRefusesAnother(t *testing.T) {
	for k, want := range map[wire.Kind]wiregen.Kind{wire.KindJSON: wiregen.KindJSON, wire.KindEventStream: wiregen.KindEventStream} {
		if got := kindOf(&wire.Route{Name: "r", Kind: k}); got != want {
			t.Errorf("kindOf(%d) = %d, want %d", k, got, want)
		}
	}
	t.Run("an unmapped kind panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("kindOf of an unmapped kind returned")
			}
		}()
		kindOf(&wire.Route{Name: "r", Kind: wire.KindEventStream + 1})
	})
}
