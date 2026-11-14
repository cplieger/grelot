//go:build !windows

package server

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/cplieger/grelot/internal/webui/wire"
)

// routeInputs is every Params and Query type wire.Routes declares, by
// route and role.
func routeInputs() map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := range wire.Routes {
		r := &wire.Routes[i]
		if r.Params != nil {
			out[r.Name+"_params"] = r.Params
		}
		if r.Query != nil {
			out[r.Name+"_query"] = r.Query
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
			if f.Type.Kind() != reflect.Uint64 {
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
