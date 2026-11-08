// Package hook holds what a grelot hook decides before it asks the server:
// the fields of the host's hook input, which session the hook acts for, and
// the per-session marks it keeps between runs.
package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
)

// maxInput caps the hook input read from stdin; a host's hook payload is
// a few KiB, so more is not a payload grelot reads.
const maxInput = 1 << 20

// Input is a hook's stdin, one JSON object.
type Input struct {
	fields map[string]any
}

// ReadInput reads r as one JSON object. Empty input is an empty object, so
// a host that sends nothing still gets its environment session's hook.
func ReadInput(r io.Reader) (Input, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxInput+1))
	if err != nil {
		return Input{}, err
	}
	if len(data) > maxInput {
		return Input{}, errors.New("hook input over 1 MiB")
	}
	return parseInput(data)
}

// parseInput reads data as one JSON object.
func parseInput(data []byte) (Input, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Input{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return Input{}, err
	}
	return Input{fields: fields}, nil
}

// String is the value at a dotted field path, "a.b", as text: a string as
// is, a number or boolean as JSON spells it, "" for a missing path, null,
// an object or an array.
func (in Input) String(path string) string {
	if path == "" {
		return ""
	}
	var v any = in.fields
	for part := range strings.SplitSeq(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		if v, ok = m[part]; !ok {
			return ""
		}
	}
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return ""
}

// Matches reports whether every path of when holds one of its values, the
// role filter a provider file spells as when.
func (in Input) Matches(when map[string][]string) bool {
	for path, values := range when {
		if !slices.Contains(values, in.String(path)) {
			return false
		}
	}
	return true
}
