//go:build !windows

package server

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

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

// instant is the type the document writes as an RFC 3339 date-time.
var instant = reflect.TypeFor[time.Time]()

// sent is the text the test sends for a field of type t: an RFC 3339
// instant for a date-time, 7 for the rest.
func sent(t reflect.Type) string {
	if t == instant {
		return "2026-11-16T14:28:30+01:00"
	}
	return "7"
}

// asSent is a bound field's value in the form sent wrote it.
func asSent(fv reflect.Value) string {
	if t, ok := reflect.TypeAssert[time.Time](fv); ok {
		return t.Format(time.RFC3339)
	}
	return fmt.Sprint(fv.Interface())
}

// jsonName is the name the generated client sends a field under.
func jsonName(f *reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}

func TestBind_everyRouteInputIsFilledByItsJSONTags(t *testing.T) {
	for name, typ := range routeInputs() {
		t.Run(name, func(t *testing.T) {
			query := map[string]string{}
			for f := range typ.Fields() {
				query[jsonName(&f)] = sent(f.Type)
			}
			v := reflect.New(typ).Elem()
			if err := bindFields(v, func(k string) string { return query[k] }); err != nil {
				t.Fatalf("bind %v: %v", query, err)
			}
			for f, fv := range v.Fields() {
				if got, want := asSent(fv), sent(f.Type); got != want {
					t.Errorf("%s (sent as %q) = %q, want %q", f.Name, jsonName(&f), got, want)
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

func TestBind_anInstantThatIsNotRFC3339IsRefusedByItsJSONName(t *testing.T) {
	cases := 0
	for name, typ := range routeInputs() {
		for f := range typ.Fields() {
			if f.Type != instant {
				continue
			}
			cases++
			tag := jsonName(&f)
			t.Run(name+"_"+tag, func(t *testing.T) {
				err := bindFields(reflect.New(typ).Elem(), func(k string) string {
					if k == tag {
						return "2026-11-16"
					}
					return ""
				})
				if err == nil || err.Error() != tag+" must be an RFC 3339 instant" {
					t.Errorf("err %v, want %q", err, tag+" must be an RFC 3339 instant")
				}
			})
		}
	}
	if cases == 0 {
		t.Fatal("no route input declares a date-time field; delete this test with bindFields' time.Time arm")
	}
}
