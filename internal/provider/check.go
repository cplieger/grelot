package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"
	"text/template/parse"
)

// checkData is what Check renders a template meant as JSON against: every
// string holds a quote, a backslash, a newline and a control character, so a
// value placed in JSON without json breaks the parse.
var checkData = Data{
	Line:      "[grelot:42] \"line\" \\ \n\x01",
	Seq:       42,
	Session:   "\"session\" \\ \n\x01",
	Identity:  "\"agent\" \\ \n\x01",
	DedupeKey: "\"key\" \\ \n\x01",
	Reason:    "\"reason\" \\ \n\x01",
	Context:   "\"context\" \\ \n\x01",
}

// Check validates one provider file as grelot provider check does: every key
// and template as Load checks them, then every template meant as JSON
// rendered and parsed. A template is meant as JSON when its literal text
// starts with "{", or when it is the body of an http door whose Content-Type
// header is a JSON media type. A file that fails Load answers its
// *LoadError; otherwise every JSON problem is one *LoadError, joined. No
// problem quotes the file.
func Check(name string, data []byte) error {
	p, err := Load(name, data)
	if err != nil {
		return err
	}
	var errs []error
	add := func(key string, t *compiled, always bool) {
		if t == nil || (!always && !startsJSON(t)) {
			return
		}
		out, err := t.render(&checkData)
		switch {
		case err != nil:
			errs = append(errs, &LoadError{Provider: name, Key: key, Problem: "fails when rendered against sample data"})
		case !json.Valid([]byte(out)):
			errs = append(errs, &LoadError{Provider: name, Key: key, Problem: "renders text that is not JSON; run every value placed in JSON through json"})
		}
	}
	for i := range p.Doors {
		d := &p.Doors[i]
		key := fmt.Sprintf("doors[%d].", d.index)
		switch d.Transport {
		case TransportHTTP:
			add(key+"body", d.tmpl.body, jsonContentType(d.Headers))
		case TransportStdout, TransportExit:
			add(key+keyLine, d.tmpl.line, false)
			add(key+keyIdle, d.tmpl.idle, false)
			add(key+keyEnd, d.tmpl.end, false)
		}
	}
	for _, o := range p.Hooks.outputs() {
		add("hooks."+o.key+".text", o.tmpl, false)
	}
	return errors.Join(errs...)
}

// startsJSON reports whether a template's literal text opens a JSON object.
// "{{" opens an action, which the parser never leaves in a text node.
func startsJSON(t *compiled) bool {
	nodes := t.t.Root.Nodes
	if len(nodes) == 0 {
		return false
	}
	text, ok := nodes[0].(*parse.TextNode)
	return ok && strings.HasPrefix(strings.TrimLeft(string(text.Text), " \t\r\n"), "{")
}

// jsonContentType reports whether headers set a Content-Type of
// application/json or a +json type (RFC 6839).
func jsonContentType(headers map[string]string) bool {
	for name, v := range headers {
		if !strings.EqualFold(name, "Content-Type") {
			continue
		}
		mt, _, err := mime.ParseMediaType(v)
		return err == nil && (mt == "application/json" || strings.HasSuffix(mt, "+json"))
	}
	return false
}
