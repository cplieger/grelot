package wirespec

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cplieger/grelot/internal/wire"
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
// takes cursor and limit, and no other row's answer embeds a List.
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
		case r.Paged && (!hasQueryField(s.Query, "cursor") || !hasQueryField(s.Query, "limit")):
			t.Errorf("Paged row %s's query %v takes no cursor or no limit", r.Name, s.Query)
		case !r.Paged && lists > 0:
			t.Errorf("row %s is not Paged but answers %v, which embeds a wire.List", r.Name, s.Response)
		}
	}
}

func isList(t reflect.Type) bool {
	return t.PkgPath() == reflect.TypeFor[wire.Error]().PkgPath() && strings.HasPrefix(t.Name(), "List[")
}

func hasQueryField(q reflect.Type, name string) bool {
	if q == nil {
		return false
	}
	for f := range q.Fields() {
		if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag == name {
			return true
		}
	}
	return false
}
