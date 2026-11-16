//go:build !windows

package server

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/cplieger/grelot/internal/wire"
	"github.com/cplieger/grelot/internal/wirespec"
)

// routeInputs is every Params and Query type the wire.Routes rows' specs
// declare, by route and role.
func routeInputs() map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := range wire.Routes {
		name := wire.Routes[i].Name
		s, _ := wirespec.Lookup(name)
		if s.Params != nil {
			out[name+"_params"] = s.Params
		}
		if s.Query != nil {
			out[name+"_query"] = s.Query
		}
	}
	return out
}

// jsonName is the name the generated client sends a field under.
func jsonName(f *reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}

func TestBind_everyRouteInputIsFilledByItsJSONTags(t *testing.T) {
	for name, typ := range routeInputs() {
		t.Run(name, func(t *testing.T) {
			sent := map[string]string{}
			for f := range typ.Fields() {
				sent[jsonName(&f)] = "7"
			}
			v := reflect.New(typ).Elem()
			if err := bindFields(v, func(k string) string { return sent[k] }); err != nil {
				t.Fatalf("bind %v: %v", sent, err)
			}
			for f, fv := range v.Fields() {
				if got := fmt.Sprint(fv.Interface()); got != "7" {
					t.Errorf("%s (sent as %q) = %q, want 7", f.Name, jsonName(&f), got)
				}
			}
		})
	}
}

func TestBind_aNumberThatIsNotWholeIsRefusedByItsJSONName(t *testing.T) {
	for name, typ := range routeInputs() {
		for f := range typ.Fields() {
			if k := f.Type.Kind(); k != reflect.Uint64 && k != reflect.Int {
				continue
			}
			tag := jsonName(&f)
			t.Run(name+"_"+tag, func(t *testing.T) {
				err := bindFields(reflect.New(typ).Elem(), func(k string) string {
					if k == tag {
						return "-1"
					}
					return ""
				})
				if err == nil || err.Error() != tag+" must be a whole number" {
					t.Errorf("err %v, want %q", err, tag+" must be a whole number")
				}
			})
		}
	}
}
