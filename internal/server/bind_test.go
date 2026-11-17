//go:build !windows

package server

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strconv"
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

// instant is the type the document writes as an optional RFC 3339
// date-time.
var instant = reflect.TypeFor[*time.Time]()

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
	if t, ok := reflect.TypeAssert[*time.Time](fv); ok && t != nil {
		return t.Format(time.RFC3339)
	}
	return fmt.Sprint(fv.Interface())
}

// leafFields is typ's fields as the document publishes them, an embedded
// struct's fields in its place.
func leafFields(typ reflect.Type) []reflect.StructField {
	var out []reflect.StructField
	for _, f := range reflect.VisibleFields(typ) {
		if !f.Anonymous {
			out = append(out, f)
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
			query := map[string]string{}
			for _, f := range leafFields(typ) {
				query[jsonName(&f)] = sent(f.Type)
			}
			v := reflect.New(typ).Elem()
			if err := bindFields(v, func(k string) string { return query[k] }); err != nil {
				t.Fatalf("bind %v: %v", query, err)
			}
			for _, f := range leafFields(typ) {
				if got, want := asSent(v.FieldByIndex(f.Index)), sent(f.Type); got != want {
					t.Errorf("%s (sent as %q) = %q, want %q", f.Name, jsonName(&f), got, want)
				}
			}
		})
	}
}

// rangedType is the wire integer types that publish their bounds.
var rangedType = reflect.TypeFor[ranged]()

func TestBind_aNumberThatIsNotWholeIsRefusedByItsJSONName(t *testing.T) {
	for name, typ := range routeInputs() {
		for _, f := range leafFields(typ) {
			if k := f.Type.Kind(); k != reflect.Uint64 && k != reflect.Int || f.Type.Implements(rangedType) {
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
		for _, f := range leafFields(typ) {
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
		t.Fatal("no route input declares a date-time field; delete this test with setField's *time.Time arm")
	}
}

func TestBind_aRangedNumberOutsideItsBoundsIsRefusedWithItsFieldError(t *testing.T) {
	cases := 0
	for name, typ := range routeInputs() {
		for _, f := range leafFields(typ) {
			if !f.Type.Implements(rangedType) {
				continue
			}
			lo, hi := reflect.Zero(f.Type).Interface().(ranged).Range()
			tag := jsonName(&f)
			outside := []string{"x", strconv.FormatInt(lo-1, 10)}
			if hi < math.MaxInt64 {
				outside = append(outside, strconv.FormatInt(hi+1, 10))
			}
			for _, v := range outside {
				cases++
				t.Run(name+"_"+tag+"="+v, func(t *testing.T) {
					err := bindFields(reflect.New(typ).Elem(), func(k string) string {
						if k == tag {
							return v
						}
						return ""
					})
					var ae *apiError
					if !errors.As(err, &ae) || len(ae.fields) != 1 {
						t.Fatalf("err %v, want one field error", err)
					}
					fe := ae.fields[0]
					if fe.Field != tag || fe.Code != wire.FieldOutOfRange || *fe.Min != lo || *fe.Max != hi {
						t.Errorf("field error %+v, want %s out_of_range %d to %d", fe, tag, lo, hi)
					}
				})
			}
		}
	}
	if cases == 0 {
		t.Fatal("no route input declares a ranged field; delete this test with setField's ranged arm")
	}
}

func TestRangeRefusal_namesItsBoundsOrOnlyTheLowestWithNoCeiling(t *testing.T) {
	for _, tc := range []struct {
		lo, hi int64
		want   string
	}{{1, 200, "limit must be 1 to 200"}, {1, math.MaxInt64, "limit must be at least 1"}} {
		if got := rangeRefusal("limit", tc.lo, tc.hi).Error(); got != tc.want {
			t.Errorf("rangeRefusal(%d, %d) = %q, want %q", tc.lo, tc.hi, got, tc.want)
		}
	}
}

func TestPathIDs_refuse0WithItsFieldError(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")
	for _, c := range []struct {
		method, path string
		web          bool
	}{
		{http.MethodGet, "/v1/messages/0/receipts", false},
		{http.MethodPatch, "/v1/reservations/0", false},
		{http.MethodDelete, "/v1/reservations/0", false},
		{http.MethodGet, "/api/conversations/0", true},
		{http.MethodPost, "/api/conversations/0/read", true},
		{http.MethodGet, "/api/messages/0/receipts", true},
	} {
		t.Run(c.method+strings.ReplaceAll(c.path, "/", "_"), func(t *testing.T) {
			var r *response
			if c.web {
				r = h.request(c.method, c.path, map[string]any{}, noKey)
			} else {
				r = h.as(sessA, c.method, c.path, map[string]any{})
			}
			fields, _ := r.body["fields"].([]any)
			var f map[string]any
			if len(fields) == 1 {
				f, _ = fields[0].(map[string]any)
			}
			if r.status != http.StatusBadRequest || r.str("code") != "invalid" || f["field"] != "id" ||
				f["code"] != "out_of_range" || fmt.Sprint(f["min"]) != "1" {
				t.Errorf("%d %s, want 400 invalid naming id out_of_range from 1", r.status, r.raw)
			}
		})
	}
}
