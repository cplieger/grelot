//go:build !windows

package server

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"
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
// on a field kind it cannot fill, so a wire type it misses fails the
// server's tests rather than a request.
func bindFields(v reflect.Value, get func(string) string) error {
	for f, fv := range v.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		s := get(name)
		if s == "" {
			continue
		}
		switch f.Type.Kind() {
		case reflect.String:
			fv.SetString(s)
		case reflect.Uint64:
			n, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return badRequest(name + " must be a whole number")
			}
			fv.SetUint(n)
		default:
			panic("server: cannot bind " + v.Type().Name() + "." + f.Name + " of kind " + f.Type.Kind().String())
		}
	}
	return nil
}
