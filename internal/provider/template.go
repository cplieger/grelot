package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"
)

// Data is what a door or hook output template reads. Every value is made by
// grelot; no other agent's text reaches a template.
type Data struct {
	// Line is the fixed signal line, or the fixed end line for a door's end.
	Line string
	// Session is the identity's host session id without its provider prefix.
	Session   string
	Identity  string
	DedupeKey string
	Reason    string
	Context   string
	Seq       uint64
}

// sampleData is what load and provider check render every template against.
// Its strings hold the characters a careless template breaks on: quotes,
// newlines, a control character, and the URL delimiters.
var sampleData = Data{
	Line:      "[grelot:42] grelot (automated, not the user): 2 new; run grelot inbox",
	Seq:       42,
	Session:   "sample/session?id=%",
	Identity:  "sample-agent",
	DedupeKey: "0f1e2d3c:41:1",
	Reason:    "2 new \"direct\" messages\nrun grelot inbox\x01",
	Context:   "grelot: @sample-agent \u00b7 2 unread\nwake: ok",
}

const (
	sampleSecret  = "sample-secret"
	funcSecret    = "secret"
	keyRecordGlob = "session.record.glob"
)

var (
	dataFields = []string{"Line", "Seq", "Session", "Identity", "DedupeKey", "Reason", "Context"}
	globFields = []string{"HookSession"}
	secretRE   = regexp.MustCompile(`^[a-z0-9_]+$`)
	lineNoRE   = regexp.MustCompile(`^template: t:(\d+)`)

	errNotLoaded = errors.New("provider: no such template; load the file through Load, and render only what the door's transport has")
)

// scope is where in a file a template sits, which decides the fields and
// functions it may use.
type scope int

const (
	scopeData scope = iota
	scopeHeader
	scopeGlob
)

// compiled is one parsed template with what a static walk found in it.
type compiled struct {
	t       *template.Template
	fields  map[string]bool
	secrets []string
}

func (t *compiled) uses(field string) bool { return t != nil && t.fields[field] }

func (t *compiled) render(data any) (string, error) {
	if t == nil {
		return "", errNotLoaded
	}
	var b strings.Builder
	if err := t.t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// funcs are the functions every template parses with. secret is a stub that
// refuses until ResolveSecrets binds a header template to its values.
var funcs = template.FuncMap{
	"json": jsonValue,
	"path": func(v any) string { return url.PathEscape(fmt.Sprint(v)) },
	funcSecret: func(name string) (string, error) {
		return "", fmt.Errorf("secret %s is not resolved", name)
	},
}

func jsonValue(v any) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// parseTemplate parses text for its scope and renders it once against sample
// data, so an unknown field, function or misuse fails the load. The problem
// it reports never quotes the text, which may hold a literal credential.
func parseTemplate(text string, sc scope) (*compiled, error) {
	t, err := template.New("t").Option("missingkey=error").Funcs(funcs).Parse(text)
	if err != nil {
		line := ""
		if m := lineNoRE.FindStringSubmatch(err.Error()); m != nil {
			line = " (line " + m[1] + ")"
		}
		return nil, errors.New("not a valid template" + line)
	}
	w := walker{scope: sc, fields: map[string]bool{}}
	for _, sub := range t.Templates() {
		w.node(sub.Root)
	}
	if w.err != nil {
		return nil, w.err
	}
	out := &compiled{t: t, fields: w.fields, secrets: w.secrets}
	sample := t
	var data any = &sampleData
	switch sc {
	case scopeHeader:
		if sample, err = t.Clone(); err != nil {
			return nil, err
		}
		sample.Funcs(template.FuncMap{funcSecret: func(string) string { return sampleSecret }})
	case scopeGlob:
		data = globData{HookSession: "sample-session"}
	}
	if _, err := (&compiled{t: sample}).render(data); err != nil {
		return nil, errors.New("fails when rendered against sample data")
	}
	return out, nil
}

// globData is what session.record.glob reads, and nothing else does.
type globData struct{ HookSession string }

// walker checks a parse tree: every field is one of its scope's, and secret
// is called only in a header, with one literal name, first in its pipeline.
type walker struct {
	err     error
	fields  map[string]bool
	secrets []string
	scope   scope
}

func (w *walker) fail(problem string) {
	if w.err == nil {
		w.err = errors.New(problem)
	}
}

func (w *walker) node(n parse.Node) {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			w.node(c)
		}
	case *parse.ActionNode:
		w.pipe(n.Pipe)
	case *parse.IfNode:
		w.branch(&n.BranchNode)
	case *parse.RangeNode:
		w.branch(&n.BranchNode)
	case *parse.WithNode:
		w.branch(&n.BranchNode)
	case *parse.TemplateNode:
		w.pipe(n.Pipe)
	}
}

func (w *walker) branch(b *parse.BranchNode) {
	w.pipe(b.Pipe)
	w.node(b.List)
	w.node(b.ElseList)
}

func (w *walker) pipe(p *parse.PipeNode) {
	if p == nil {
		return
	}
	for i, c := range p.Cmds {
		if id, ok := c.Args[0].(*parse.IdentifierNode); ok && id.Ident == funcSecret {
			w.secret(c, i == 0)
			continue
		}
		for _, a := range c.Args {
			w.arg(a)
		}
	}
}

func (w *walker) arg(n parse.Node) {
	switch n := n.(type) {
	case *parse.FieldNode:
		w.field(n.Ident)
	case *parse.VariableNode:
		switch {
		case len(n.Ident) > 1 && n.Ident[0] == "$":
			w.field(n.Ident[1:])
		case len(n.Ident) > 1:
			w.fail("a variable has no fields; read the data from . or $")
		}
	case *parse.ChainNode:
		w.fail("template data has no nested fields")
	case *parse.PipeNode:
		w.pipe(n)
	case *parse.IdentifierNode:
		if n.Ident == funcSecret {
			w.fail(`secret is called as {{secret "<name>"}}`)
		}
	}
}

func (w *walker) field(ident []string) {
	allowed := dataFields
	if w.scope == scopeGlob {
		allowed = globFields
	}
	name := ident[0]
	switch {
	case !slices.Contains(allowed, name) && name == globFields[0]:
		w.fail(".HookSession is read only in session.record.glob")
	case !slices.Contains(allowed, name):
		w.fail(fmt.Sprintf(".%s is not template data; the data is .%s", name, strings.Join(allowed, ", .")))
	case len(ident) > 1:
		w.fail("template data has no nested fields")
	default:
		w.fields[name] = true
	}
}

func (w *walker) secret(c *parse.CommandNode, first bool) {
	if w.scope != scopeHeader {
		w.fail("secret is allowed only in an http door's header values")
		return
	}
	var name *parse.StringNode
	if len(c.Args) == 2 {
		name, _ = c.Args[1].(*parse.StringNode)
	}
	if !first || name == nil || !secretRE.MatchString(name.Text) {
		w.fail(`secret takes one string literal matching [a-z0-9_]+, as {{secret "<name>"}}`)
		return
	}
	if !slices.Contains(w.secrets, name.Text) {
		w.secrets = append(w.secrets, name.Text)
	}
}

// checkURL refuses a URL whose scheme, host or port a template action could
// choose: the literal text before the first action must reach the path or
// the query.
func checkURL(raw string) error {
	prefix, _, templated := strings.Cut(raw, "{{")
	_, authority, _ := strings.Cut(prefix, "://")
	u, err := url.Parse(prefix)
	switch {
	case err != nil:
		return errors.New("not a URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("must start with a literal http:// or https://")
	case u.User != nil:
		return errors.New("must carry no credentials; send them in a header through secret")
	case templated && !strings.ContainsAny(authority, "/?"):
		return errors.New("template actions may appear only in the path and the query; the scheme, host and port are literal")
	case u.Host == "":
		return errors.New("needs a host")
	}
	return nil
}

// doorTemplates are a door's parsed templates. They are shared by every copy
// of the Door, so ResolveSecrets binds all copies at once.
type doorTemplates struct {
	headers  map[string]*compiled
	resolved map[string]*template.Template
	url      *compiled
	body     *compiled
	line     *compiled
	idle     *compiled
	end      *compiled
	argv     []*compiled
}

func (d *Door) compile(set map[string]bool) error {
	dt := &doorTemplates{}
	var err error
	switch d.Transport {
	case TransportHTTP:
		err = dt.compileHTTP(d, set)
	case TransportExec:
		dt.argv = make([]*compiled, len(d.Argv))
		for i, text := range d.Argv {
			if dt.argv[i], err = parseKey(fmt.Sprintf("argv[%d]", i), text, scopeData); err != nil {
				break
			}
		}
	default:
		err = dt.compileLine(d, set)
	}
	if err != nil {
		return err
	}
	d.tmpl = dt
	return nil
}

func parseKey(key, text string, sc scope) (*compiled, error) {
	t, err := parseTemplate(text, sc)
	if err != nil {
		return nil, &LoadError{Key: key, Problem: err.Error()}
	}
	return t, nil
}

func (dt *doorTemplates) compileHTTP(d *Door, set map[string]bool) error {
	if err := checkURL(d.URL); err != nil {
		return &LoadError{Key: keyURL, Problem: err.Error()}
	}
	var err error
	if dt.url, err = parseKey(keyURL, d.URL, scopeData); err != nil {
		return err
	}
	if dt.body, err = parseKey("body", d.Body, scopeData); err != nil {
		return err
	}
	dt.headers = map[string]*compiled{}
	for _, name := range slices.Sorted(maps.Keys(d.Headers)) {
		if dt.headers[name], err = parseKey("headers."+name, d.Headers[name], scopeHeader); err != nil {
			return err
		}
	}
	if !set[keyDedupeWindow] && dt.usesDedupeKey() {
		return &LoadError{Key: keyDedupeWindow, Problem: "required when a template renders .DedupeKey"}
	}
	return nil
}

func (dt *doorTemplates) compileLine(d *Door, set map[string]bool) error {
	var err error
	if dt.line, err = parseKey(keyLine, d.Line, scopeData); err != nil {
		return err
	}
	if dt.end, err = parseKey(keyEnd, d.End, scopeData); err != nil {
		return err
	}
	if set[keyIdle] {
		dt.idle, err = parseKey(keyIdle, d.Idle, scopeData)
	}
	return err
}

func (dt *doorTemplates) usesDedupeKey() bool {
	if dt.url.uses("DedupeKey") || dt.body.uses("DedupeKey") {
		return true
	}
	for _, h := range dt.headers {
		if h.uses("DedupeKey") {
			return true
		}
	}
	return false
}

// Request is an http door's rendered request.
type Request struct {
	Header map[string]string
	URL    string
	Body   string
}

// RenderRequest renders an http door's URL, headers and body. A door whose
// headers name a secret renders only after ResolveSecrets.
func (d *Door) RenderRequest(data *Data) (Request, error) {
	if d.tmpl == nil || d.Transport != TransportHTTP {
		return Request{}, errNotLoaded
	}
	u, err := d.tmpl.url.render(data)
	if err != nil {
		return Request{}, err
	}
	body, err := d.tmpl.body.render(data)
	if err != nil {
		return Request{}, err
	}
	req := Request{URL: u, Body: body, Header: map[string]string{}}
	for name, h := range d.tmpl.headers {
		t := h
		if r, ok := d.tmpl.resolved[name]; ok {
			t = &compiled{t: r}
		}
		v, err := t.render(data)
		if err != nil {
			return Request{}, err
		}
		req.Header[name] = v
	}
	return req, nil
}

// SecretNames lists the secret names an http door's headers call, each once.
func (d *Door) SecretNames() []string {
	if d.tmpl == nil {
		return nil
	}
	var names []string
	for _, h := range d.tmpl.headers {
		for _, s := range h.secrets {
			if !slices.Contains(names, s) {
				names = append(names, s)
			}
		}
	}
	slices.Sort(names)
	return names
}

// ResolveSecrets binds the door's secret calls to the values lookup returns,
// once, before the door is rendered; it is not safe to call while another
// goroutine renders the door. An error names the secret, never a value.
func (d *Door) ResolveSecrets(lookup func(name string) (string, error)) error {
	if d.tmpl == nil {
		return errNotLoaded
	}
	values := map[string]string{}
	for _, name := range d.SecretNames() {
		v, err := lookup(name)
		if err != nil {
			return fmt.Errorf("secret %s: %w", name, err)
		}
		values[name] = v
	}
	resolved := map[string]*template.Template{}
	for name, h := range d.tmpl.headers {
		if len(h.secrets) == 0 {
			continue
		}
		c, err := h.t.Clone()
		if err != nil {
			return err
		}
		c.Funcs(template.FuncMap{funcSecret: func(n string) string { return values[n] }})
		resolved[name] = c
	}
	d.tmpl.resolved = resolved
	return nil
}

// RenderArgv renders an exec door's argv, one template per argument.
func (d *Door) RenderArgv(data *Data) ([]string, error) {
	if d.tmpl == nil || d.Transport != TransportExec {
		return nil, errNotLoaded
	}
	argv := make([]string, 0, len(d.tmpl.argv))
	for _, t := range d.tmpl.argv {
		a, err := t.render(data)
		if err != nil {
			return nil, err
		}
		argv = append(argv, a)
	}
	return argv, nil
}

// RenderLine renders a stdout or exit door's line, for a nudge.
func (d *Door) RenderLine(data *Data) (string, error) { return d.lineTemplates().line.render(data) }

// RenderEnd renders a stdout or exit door's end output; data.Line is the end
// line.
func (d *Door) RenderEnd(data *Data) (string, error) { return d.lineTemplates().end.render(data) }

// RenderIdle renders a stdout or exit door's idle output, printed when its
// hold lapses. A door with no hold has none, and RenderIdle fails.
func (d *Door) RenderIdle(data *Data) (string, error) { return d.lineTemplates().idle.render(data) }

// lineTemplates is never nil; a door of another transport gets no templates,
// so each render answers errNotLoaded.
func (d *Door) lineTemplates() *doorTemplates {
	if d.tmpl == nil || (d.Transport != TransportStdout && d.Transport != TransportExit) {
		return &doorTemplates{}
	}
	return d.tmpl
}

// Render renders a hook output's text.
func (o *Output) Render(data *Data) (string, error) { return o.tmpl.render(data) }

// RenderGlob renders session.record.glob for a hook's stdin session, which
// must be one path component: no separator, no "." or "..", and no glob
// metacharacter, so the pattern cannot reach another session's record.
func (r *Record) RenderGlob(hookSession string) (string, error) {
	if hookSession == "" || hookSession == "." || hookSession == ".." || strings.ContainsAny(hookSession, "/\\*?[]\x00") {
		return "", errors.New("the hook session is not one path component")
	}
	return r.tmpl.render(globData{HookSession: hookSession})
}

func (r *Record) compile() error {
	t, err := parseTemplate(r.Glob, scopeGlob)
	if err != nil {
		return &LoadError{Key: keyRecordGlob, Problem: err.Error()}
	}
	if !t.uses(globFields[0]) {
		return &LoadError{Key: keyRecordGlob, Problem: "must render {{.HookSession}}"}
	}
	sample, err := t.render(globData{HookSession: "sample-session"})
	if err != nil {
		return &LoadError{Key: keyRecordGlob, Problem: err.Error()}
	}
	if _, err := path.Match(sample, ""); err != nil || strings.HasPrefix(sample, "/") || strings.Contains("/"+sample+"/", "/../") {
		return &LoadError{Key: keyRecordGlob, Problem: "must be a relative glob pattern inside the state directory"}
	}
	r.tmpl = t
	return nil
}

func (h *Hooks) compile() error {
	for _, out := range h.outputs() {
		t, err := parseTemplate(out.Text, scopeData)
		if err != nil {
			return &LoadError{Key: "hooks." + out.key + ".text", Problem: err.Error()}
		}
		out.tmpl = t
	}
	return nil
}

// compile parses every template of a checked file with its defaults applied.
func (p *Provider) compile(doorSets []map[string]bool) error {
	for i := range p.Doors {
		if err := p.Doors[i].compile(doorSets[i]); err != nil {
			if le, ok := errors.AsType[*LoadError](err); ok {
				le.Key = fmt.Sprintf("doors[%d].%s", i, le.Key)
			}
			return err
		}
	}
	if r := p.Session.Record; r != nil {
		if err := r.compile(); err != nil {
			return err
		}
	}
	return p.Hooks.compile()
}
