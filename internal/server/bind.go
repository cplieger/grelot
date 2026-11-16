//go:build !windows

package server

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// pathParams reads a web route's Params struct from r's path wildcards,
// each field by its json tag, the name the generated client fills.
func pathParams[T any](r *http.Request) (T, error) { return bind[T](r.PathValue) }

// queryParams reads a web route's Query struct from r's query string,
// each field by its json tag, the name the generated client sends.
func queryParams[T any](r *http.Request) (T, error) { return bind[T](r.URL.Query().Get) }

func bind[T any](get func(string) string) (T, error) {
	var out T
	err := bindFields(reflect.ValueOf(&out).Elem(), get)
	return out, err
}

// bindFields fills each field of the struct v from get, keyed by the
// field's json tag name; an empty value leaves the field zero. It panics
// on a field kind setField cannot fill, so a wire type it misses fails
// the server's tests rather than a request.
func bindFields(v reflect.Value, get func(string) string) error {
	for f, fv := range v.Fields() {
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

// setField fills fv from s, the value sent under name: a time.Time from
// an RFC 3339 instant, an integer from a whole number. It reports false
// for a kind it cannot fill.
func setField(fv reflect.Value, name, s string) (bool, error) {
	if fv.Type() == reflect.TypeFor[time.Time]() {
		at, err := instantParam(s, name)
		fv.Set(reflect.ValueOf(at))
		return true, err
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
