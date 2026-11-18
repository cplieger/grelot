// Package wiretest checks an HTTP answer against an OpenAPI 3.1 document for
// the outside-in suite. It covers the JSON Schema subset wiregen emits, and
// Load refuses a keyword outside it rather than skip it. An object schema
// with properties and no additionalProperties is closed: wiregen writes one
// for every struct, so a member the struct does not declare is drift.
package wiretest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Doc is a loaded OpenAPI document.
type Doc struct {
	paths []pathItem
}

type pathItem struct {
	ops      map[string]*operation // lower-case method
	template []string
}

type operation struct {
	responses map[int]*response
	id        string
}

type response struct {
	media   map[string]*schema // media type to body schema
	headers map[string]header
	codes   []string // x-error-codes, sorted; nil when undeclared
}

type header struct {
	schema   *schema
	required bool
}

var methods = []string{"get", "put", "post", "delete", "options", "head", "patch"}

// Load reads and compiles the document at path. It fails on a schema keyword
// or format Check cannot validate, an unresolvable $ref and a bad pattern.
func Load(path string) (*Doc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c := newCompiler(doc.Components.Schemas)
	d := &Doc{}
	for tmpl, item := range doc.Paths {
		p := pathItem{template: strings.Split(tmpl, "/"), ops: map[string]*operation{}}
		for _, m := range methods {
			rawOp, ok := item[m]
			if !ok {
				continue
			}
			op, err := compileOperation(c, rawOp)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", strings.ToUpper(m), tmpl, err)
			}
			p.ops[m] = op
		}
		d.paths = append(d.paths, p)
	}
	if err := c.finish(); err != nil {
		return nil, err
	}
	return d, nil
}

type rawResponse struct {
	Content map[string]struct {
		Schema json.RawMessage `json:"schema"`
	} `json:"content"`
	Headers map[string]struct {
		Schema   json.RawMessage `json:"schema"`
		Required bool            `json:"required"`
	} `json:"headers"`
	Codes map[string]json.RawMessage `json:"x-error-codes"`
}

func compileOperation(c *compiler, raw json.RawMessage) (*operation, error) {
	var op struct {
		Responses   map[string]rawResponse `json:"responses"`
		OperationID string                 `json:"operationId"`
	}
	if err := json.Unmarshal(raw, &op); err != nil {
		return nil, err
	}
	out := &operation{id: op.OperationID, responses: map[int]*response{}}
	for key, r := range op.Responses {
		status, err := strconv.Atoi(key)
		if err != nil {
			return nil, fmt.Errorf("response %q: not a status code", key)
		}
		if out.responses[status], err = compileResponse(c, &r); err != nil {
			return nil, fmt.Errorf("%d: %w", status, err)
		}
	}
	return out, nil
}

func compileResponse(c *compiler, r *rawResponse) (*response, error) {
	resp := &response{media: map[string]*schema{}, headers: map[string]header{}}
	for media, body := range r.Content {
		s, err := c.compileRaw(body.Schema, "body")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", media, err)
		}
		resp.media[media] = s
	}
	for name, h := range r.Headers {
		s, err := c.compileRaw(h.Schema, name)
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", name, err)
		}
		resp.headers[name] = header{required: h.Required, schema: s}
	}
	if r.Codes != nil {
		resp.codes = make([]string, 0, len(r.Codes))
		for code := range r.Codes {
			resp.codes = append(resp.codes, code)
		}
		slices.Sort(resp.codes)
	}
	return resp, nil
}

// Operations returns every operationId in the document, sorted.
func (d *Doc) Operations() []string {
	var ids []string
	for _, p := range d.paths {
		for _, op := range p.ops {
			ids = append(ids, op.id)
		}
	}
	slices.Sort(ids)
	return ids
}

// Check validates one answer: the operation matching method and path (an
// escaped request path, query allowed) declares status, header carries its
// media type and required headers, an error's code is in the status's codes,
// and body validates against the schema. It returns the operationId matched.
func (d *Doc) Check(method, path string, status int, h http.Header, body []byte) (string, error) {
	op, err := d.find(method, path)
	if err != nil {
		return "", err
	}
	resp, ok := op.responses[status]
	if !ok {
		return op.id, fmt.Errorf("%s: status %d not declared", op.id, status)
	}
	if err := resp.check(h, body); err != nil {
		return op.id, fmt.Errorf("%s %d: %w", op.id, status, err)
	}
	return op.id, nil
}

// find matches path segment by segment; a literal segment outranks a
// parameter, so /v1/items/mine is not read as /v1/items/{id}.
func (d *Doc) find(method, path string) (*operation, error) {
	path, _, _ = strings.Cut(path, "?")
	segs := strings.Split(path, "/")
	var best *pathItem
	bestScore, tie := -1, false
	for i := range d.paths {
		p := &d.paths[i]
		score, ok := match(p.template, segs)
		if !ok {
			continue
		}
		switch {
		case score > bestScore:
			best, bestScore, tie = p, score, false
		case score == bestScore:
			tie = true
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no operation path matches %s", path)
	}
	if tie {
		return nil, fmt.Errorf("two operation paths match %s", path)
	}
	op, ok := best.ops[strings.ToLower(method)]
	if !ok {
		return nil, fmt.Errorf("%s %s: method not declared", method, path)
	}
	return op, nil
}

func match(template, segs []string) (int, bool) {
	if len(template) != len(segs) {
		return 0, false
	}
	literals := 0
	for i, t := range template {
		if strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}") {
			if segs[i] == "" {
				return 0, false
			}
			continue
		}
		if t != segs[i] {
			return 0, false
		}
		literals++
	}
	return literals, true
}

func (r *response) check(h http.Header, body []byte) error {
	if err := r.checkHeaders(h); err != nil {
		return err
	}
	if len(r.media) == 0 {
		if len(body) != 0 {
			return errors.New("a body where none is declared")
		}
		return nil
	}
	ct := h.Get("Content-Type")
	if ct == "" {
		return errors.New("no Content-Type")
	}
	media, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return fmt.Errorf("Content-Type %q: %w", ct, err)
	}
	s, ok := r.media[media]
	if !ok {
		return fmt.Errorf("Content-Type %s not declared", media)
	}
	if media != "application/json" {
		return nil // an event stream's frames are not one JSON value
	}
	v, err := decode(body)
	if err != nil {
		return err
	}
	if err := s.validate("$", v); err != nil {
		return err
	}
	return r.checkCode(v)
}

func (r *response) checkHeaders(h http.Header) error {
	for name, hd := range r.headers {
		v := h.Get(name)
		switch {
		case v != "":
			if err := hd.schema.validate(name, v); err != nil {
				return err
			}
		case hd.required:
			return fmt.Errorf("required header %s missing", name)
		}
	}
	return nil
}

func (r *response) checkCode(v any) error {
	if r.codes == nil {
		return nil
	}
	obj, _ := v.(map[string]any)
	code, ok := obj["code"].(string)
	if !ok {
		return errors.New("error answer carries no code")
	}
	if _, found := slices.BinarySearch(r.codes, code); !found {
		return fmt.Errorf("code %q not among %v", code, r.codes)
	}
	return nil
}

func decode(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("body is not JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("body is not one JSON value: trailing data")
	}
	return v, nil
}
