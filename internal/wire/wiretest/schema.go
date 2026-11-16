package wiretest

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	componentsRef = "#/components/schemas/"
	kwOneOf       = "oneOf"
	kwAnyOf       = "anyOf"
)

// keywords is every schema keyword Check validates or may ignore without
// accepting a wrong value; Load refuses any other.
var keywords = map[string]bool{
	"$ref": true, "type": true, "properties": true, "required": true,
	"additionalProperties": true, "propertyNames": true, "enum": true,
	"const": true, "items": true, "format": true, kwOneOf: true, kwAnyOf: true,
	"discriminator": true, "minimum": true, "maximum": true, "minLength": true,
	"maxLength": true, "x-max-bytes": true, "pattern": true, "minItems": true,
	"maxItems": true, "description": true,
}

var formats = map[string]bool{"date-time": true, "uri-reference": true}

type schema struct {
	target   *schema
	addl     *schema
	keys     *schema
	items    *schema
	pattern  *regexp.Regexp
	min, max *big.Rat
	props    map[string]*schema
	constant any
	sizes    map[string]int // minLength, maxLength, x-max-bytes, minItems, maxItems
	ref      string
	format   string
	types    []string
	required []string
	enum     []any
	oneOf    []*schema
	anyOf    []*schema
	hasConst bool
	closed   bool
}

type compiler struct {
	raw   map[string]json.RawMessage
	named map[string]*schema
	refs  []*schema
}

func newCompiler(components map[string]json.RawMessage) *compiler {
	return &compiler{raw: components, named: map[string]*schema{}}
}

func (c *compiler) compileRaw(raw json.RawMessage, at string) (*schema, error) {
	if raw == nil {
		return &schema{}, nil
	}
	v, err := decode(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", at, err)
	}
	return c.compile(v, at)
}

// finish compiles every component schema once, then points each $ref at
// its target, so a recursive type needs no recursion here.
func (c *compiler) finish() error {
	for name, raw := range c.raw {
		t, err := c.compileRaw(raw, name)
		if err != nil {
			return err
		}
		c.named[name] = t
	}
	for _, s := range c.refs {
		name, ok := strings.CutPrefix(s.ref, componentsRef)
		if !ok {
			return fmt.Errorf("$ref %q: not a component schema", s.ref)
		}
		t, ok := c.named[name]
		if !ok {
			return fmt.Errorf("$ref %q: no such schema", s.ref)
		}
		s.target = t
	}
	return nil
}

func (c *compiler) compile(v any, at string) (*schema, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: schema is not an object", at)
	}
	for k := range m {
		if !keywords[k] {
			return nil, fmt.Errorf("%s: keyword %q is not validated", at, k)
		}
	}
	s := &schema{}
	if ref, ok := m["$ref"].(string); ok {
		s.ref = ref
		c.refs = append(c.refs, s)
		return s, nil
	}
	s.types = typeNames(m["type"])
	s.enum, _ = m["enum"].([]any)
	s.constant, s.hasConst = m["const"]
	if f, ok := m["format"].(string); ok {
		if !formats[f] {
			return nil, fmt.Errorf("%s: format %q is not validated", at, f)
		}
		s.format = f
	}
	steps := []func(*schema, map[string]any, string) error{
		c.compileObject, c.compileItems, c.compileUnions, compileBounds,
	}
	for _, step := range steps {
		if err := step(s, m, at); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func typeNames(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		names := make([]string, 0, len(t))
		for _, e := range t {
			name, _ := e.(string)
			names = append(names, name)
		}
		return names
	}
	return nil
}

func (c *compiler) compileItems(s *schema, m map[string]any, at string) error {
	items, ok := m["items"]
	if !ok {
		return nil
	}
	var err error
	s.items, err = c.compile(items, at+"[]")
	return err
}

func (c *compiler) compileUnions(s *schema, m map[string]any, at string) error {
	for kw, dst := range map[string]*[]*schema{kwOneOf: &s.oneOf, kwAnyOf: &s.anyOf} {
		for i, e := range asSlice(m[kw]) {
			sub, err := c.compile(e, fmt.Sprintf("%s.%s[%d]", at, kw, i))
			if err != nil {
				return err
			}
			*dst = append(*dst, sub)
		}
	}
	return nil
}

func (c *compiler) compileObject(s *schema, m map[string]any, at string) error {
	props, hasProps := m["properties"].(map[string]any)
	if hasProps {
		s.props = map[string]*schema{}
		for name, p := range props {
			sub, err := c.compile(p, at+"."+name)
			if err != nil {
				return err
			}
			s.props[name] = sub
		}
	}
	for _, r := range asSlice(m["required"]) {
		name, _ := r.(string)
		s.required = append(s.required, name)
	}
	switch a := m["additionalProperties"].(type) {
	case nil:
		s.closed = hasProps
	case bool:
		s.closed = !a
	default:
		var err error
		if s.addl, err = c.compile(a, at+".*"); err != nil {
			return err
		}
	}
	if k, ok := m["propertyNames"]; ok {
		var err error
		if s.keys, err = c.compile(k, at+".<key>"); err != nil {
			return err
		}
	}
	return nil
}

var sizeKeywords = []string{"minLength", "maxLength", "x-max-bytes", "minItems", "maxItems"}

func compileBounds(s *schema, m map[string]any, at string) error {
	var err error
	if s.min, err = rat(m["minimum"]); err != nil {
		return fmt.Errorf("%s: minimum: %w", at, err)
	}
	if s.max, err = rat(m["maximum"]); err != nil {
		return fmt.Errorf("%s: maximum: %w", at, err)
	}
	for _, kw := range sizeKeywords {
		n, ok := m[kw].(json.Number)
		if !ok {
			continue
		}
		i, ierr := n.Int64()
		if ierr != nil {
			return fmt.Errorf("%s: %s: %w", at, kw, ierr)
		}
		if s.sizes == nil {
			s.sizes = map[string]int{}
		}
		s.sizes[kw] = int(i)
	}
	if p, ok := m["pattern"].(string); ok {
		if s.pattern, err = regexp.Compile(p); err != nil {
			return fmt.Errorf("%s: pattern: %w", at, err)
		}
	}
	return nil
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func rat(v any) (*big.Rat, error) {
	n, ok := v.(json.Number)
	if !ok {
		return nil, nil
	}
	r, ok := new(big.Rat).SetString(n.String())
	if !ok {
		return nil, fmt.Errorf("%q is not a number", n)
	}
	return r, nil
}

func (s *schema) validate(at string, v any) error {
	if s.target != nil {
		return s.target.validate(at, v)
	}
	if err := s.validateType(at, v); err != nil {
		return err
	}
	if s.hasConst && !reflect.DeepEqual(v, s.constant) {
		return fmt.Errorf("%s: %v is not the const %v", at, v, s.constant)
	}
	if s.enum != nil && !slices.ContainsFunc(s.enum, func(e any) bool { return reflect.DeepEqual(e, v) }) {
		return fmt.Errorf("%s: %v not in enum %v", at, v, s.enum)
	}
	if err := s.validateUnions(at, v); err != nil {
		return err
	}
	switch x := v.(type) {
	case string:
		return s.validateString(at, x)
	case json.Number:
		return s.validateNumber(at, x)
	case []any:
		return s.validateArray(at, x)
	case map[string]any:
		return s.validateObject(at, x)
	}
	return nil
}

func (s *schema) validateType(at string, v any) error {
	if len(s.types) == 0 {
		return nil
	}
	got := typeOf(v)
	for _, t := range s.types {
		if t == got || (t == "number" && got == "integer") {
			return nil
		}
	}
	return fmt.Errorf("%s: want type %s, got %s", at, strings.Join(s.types, " or "), got)
}

func typeOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if r, ok := new(big.Rat).SetString(x.String()); ok && r.IsInt() {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	default:
		return "object"
	}
}

func (s *schema) validateUnions(at string, v any) error {
	if s.oneOf != nil {
		n := 0
		for _, sub := range s.oneOf {
			if sub.validate(at, v) == nil {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("%s: matches %d oneOf cases, want 1", at, n)
		}
	}
	if s.anyOf != nil && !slices.ContainsFunc(s.anyOf, func(sub *schema) bool { return sub.validate(at, v) == nil }) {
		return fmt.Errorf("%s: matches no anyOf case", at)
	}
	return nil
}

// bound reports whether n breaks the size keyword kw, a minimum when its
// name starts with "min" and a maximum otherwise.
func (s *schema) bound(at, kw string, n int) error {
	limit, ok := s.sizes[kw]
	if !ok {
		return nil
	}
	if strings.HasPrefix(kw, "min") && n < limit || !strings.HasPrefix(kw, "min") && n > limit {
		return fmt.Errorf("%s: %d against %s %d", at, n, kw, limit)
	}
	return nil
}

func (s *schema) validateString(at, v string) error {
	runes := utf8.RuneCountInString(v)
	for _, err := range []error{
		s.bound(at, "minLength", runes), s.bound(at, "maxLength", runes), s.bound(at, "x-max-bytes", len(v)),
	} {
		if err != nil {
			return err
		}
	}
	if s.pattern != nil && !s.pattern.MatchString(v) {
		return fmt.Errorf("%s: %q does not match pattern %s", at, v, s.pattern)
	}
	switch s.format {
	case "date-time":
		if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
			return fmt.Errorf("%s: %q is not an RFC 3339 date-time", at, v)
		}
	case "uri-reference":
		if _, err := url.Parse(v); err != nil {
			return fmt.Errorf("%s: %q is not a uri-reference", at, v)
		}
	}
	return nil
}

func (s *schema) validateNumber(at string, v json.Number) error {
	r, _ := new(big.Rat).SetString(v.String())
	if s.min != nil && r.Cmp(s.min) < 0 {
		return fmt.Errorf("%s: %s below minimum %s", at, v, s.min.RatString())
	}
	if s.max != nil && r.Cmp(s.max) > 0 {
		return fmt.Errorf("%s: %s above maximum %s", at, v, s.max.RatString())
	}
	return nil
}

func (s *schema) validateArray(at string, v []any) error {
	if err := s.bound(at, "minItems", len(v)); err != nil {
		return err
	}
	if err := s.bound(at, "maxItems", len(v)); err != nil {
		return err
	}
	if s.items == nil {
		return nil
	}
	for i, e := range v {
		if err := s.items.validate(fmt.Sprintf("%s[%d]", at, i), e); err != nil {
			return err
		}
	}
	return nil
}

func (s *schema) validateObject(at string, v map[string]any) error {
	for _, name := range s.required {
		if _, ok := v[name]; !ok {
			return fmt.Errorf("%s: required field %q missing", at, name)
		}
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if err := s.validateMember(at+"."+k, k, v[k]); err != nil {
			return err
		}
	}
	return nil
}

func (s *schema) validateMember(field, key string, v any) error {
	if s.keys != nil {
		if err := s.keys.validate(field, key); err != nil {
			return fmt.Errorf("key %q: %w", key, err)
		}
	}
	if p, ok := s.props[key]; ok {
		return p.validate(field, v)
	}
	if s.addl != nil {
		return s.addl.validate(field, v)
	}
	if s.closed {
		return fmt.Errorf("%s: field not declared", field)
	}
	return nil
}
