//go:build !windows

package server

import (
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/grelot/internal/wire"
)

// ranged is a wire integer type that publishes its bounds.
type ranged interface{ Range() (lo, hi int64) }

// pathParams reads a route's Params struct from r's path wildcards, each
// field by its json tag, the name the document publishes.
func pathParams[T any](r *http.Request) (T, error) { return bind[T](r.PathValue) }

// queryParams reads a route's Query struct from r's query string, each
// field by its json tag, the name the document publishes.
func queryParams[T any](r *http.Request) (T, error) { return bind[T](r.URL.Query().Get) }

func bind[T any](get func(string) string) (T, error) {
	var out T
	err := bindFields(reflect.ValueOf(&out).Elem(), get)
	return out, err
}

// bindFields fills each field of the struct v from get, keyed by the
// field's json tag name, and an embedded struct's fields as its own, as
// encoding/json and the document flatten them; an empty value leaves the
// field zero, so an optional instant is a pointer that stays nil. It
// panics on a field kind setField cannot fill, so a wire type it misses
// fails the server's tests rather than a request.
func bindFields(v reflect.Value, get func(string) string) error {
	for f, fv := range v.Fields() {
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			if err := bindFields(fv, get); err != nil {
				return err
			}
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		s := get(name)
		if s == "" {
			continue
		}
		ok, err := setField(fv, name, s)
		if !ok {
			panic("server: cannot bind " + v.Type().Name() + "." + f.Name + " of kind " + f.Type.Kind().String())
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// setField fills fv from s, the value sent under name: a *time.Time from
// an RFC 3339 instant, a ranged type from a whole number within its
// bounds, another integer from a whole number. It reports false for a
// kind it cannot fill.
func setField(fv reflect.Value, name, s string) (bool, error) {
	if fv.Type() == reflect.TypeFor[*time.Time]() {
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return true, badRequest(name + " must be an RFC 3339 instant")
		}
		fv.Set(reflect.ValueOf(&at))
		return true, nil
	}
	if rg, ok := reflect.TypeAssert[ranged](fv); ok && (fv.CanInt() || fv.CanUint()) {
		return true, setRanged(fv, rg, name, s)
	}
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(s)
	case reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return true, badRequest(name + " must be a whole number")
		}
		fv.SetUint(n)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return true, badRequest(name + " must be a whole number")
		}
		fv.SetInt(n)
	default:
		return false, nil
	}
	return true, nil
}

// setRanged fills fv, of a ranged type, from s, a whole number within
// the type's bounds. Convert, rather than SetUint, fills an unsigned type,
// whose bounds start at 1, so n is never negative there.
func setRanged(fv reflect.Value, rg ranged, name, s string) error {
	lo, hi := rg.Range()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < lo || n > hi {
		return rangeRefusal(name, lo, hi)
	}
	fv.Set(reflect.ValueOf(n).Convert(fv.Type()))
	return nil
}

// rangeRefusal refuses name's value outside lo to hi, the bounds the
// document publishes for it; an id's hi is math.MaxInt64, which the text
// leaves out.
func rangeRefusal(name string, lo, hi int64) *apiError {
	msg := name + " must be " + strconv.FormatInt(lo, 10) + " to " + strconv.FormatInt(hi, 10)
	if hi == math.MaxInt64 {
		msg = name + " must be at least " + strconv.FormatInt(lo, 10)
	}
	return fieldRefusal(msg, wire.FieldError{Field: name, Code: wire.FieldOutOfRange, Min: new(lo), Max: new(hi)})
}
