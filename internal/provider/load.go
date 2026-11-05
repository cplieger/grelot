package provider

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/go/yamlenv"
	"go.yaml.in/yaml/v3"
)

const (
	maxBudget       = 3 * time.Second
	minDedupeWindow = 60 * time.Second
	defaultLine     = "{{.Line}}"
	defaultEndExit  = 1

	keyName    = "name"
	keyExtends = "extends"
	keyDetect  = "detect"
	keyLine    = "line"
	keyEnd     = "end"
	keyEndExit = "end_exit"
	keyHold    = "hold"
	keyIdle    = "idle"
	keyURL     = "url"

	keyDedupeWindow = "dedupe_window"

	problemRequired = "required"
	problemEnvName  = "not an environment variable name"
	problemNotEmpty = "must not be empty"
)

var (
	nameRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,47}$`)
	envNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	stdinPathRE = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)
	headerRE    = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// LoadError is a provider file refused at load. Key is where in the file the
// problem is, empty for the whole file; Problem never quotes a value of the
// file, so the error is safe to log.
type LoadError struct {
	Provider string
	Key      string
	Problem  string
}

func (e *LoadError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("provider %s: %s", e.Provider, e.Problem)
	}
	return fmt.Sprintf("provider %s: %s: %s", e.Provider, e.Key, e.Problem)
}

// doorKey lists the keys a door of one transport takes beyond the common ones.
type doorKey struct {
	required []string
	optional []string
}

var commonDoorKeys = []string{keyName, "layer", "transport", "start", "asset", "notes"}

// doorKeys is the one list of transport keys; a key of another transport is
// refused like an unknown one.
var doorKeys = map[Transport]doorKey{
	TransportHTTP:   {required: []string{keyURL, "body"}, optional: []string{"match", "headers", keyDedupeWindow, "gone"}},
	TransportExec:   {required: []string{"argv"}},
	TransportStdout: {optional: []string{keyLine, keyHold, keyIdle, keyEnd, keyEndExit}},
	TransportExit:   {optional: []string{keyLine, keyHold, keyIdle, keyEnd, keyEndExit}},
}

var doorLayer = map[Transport]int{
	TransportHTTP: 1, TransportExec: 2, TransportStdout: 2, TransportExit: 2,
}

// Load decodes and checks one provider file. name is the file's name without
// its .yaml extension, and must equal the file's name key. The file never
// reads the environment: a ${…} reference in it is refused, named.
func Load(name string, data []byte) (*Provider, error) {
	fail := func(key, problem string) error { return &LoadError{Provider: name, Key: key, Problem: problem} }
	p := &Provider{}
	unresolved, err := yamlenv.Load(data, p, func(string) bool { return true },
		yamlenv.WithLookupEnv(func(string) (string, bool) { return "", false }),
		yamlenv.WithSanitizeOptions(yamlenv.WithUnknownKeyEcho(true)))
	if err != nil {
		return nil, fail("", err.Error())
	}
	if len(unresolved) > 0 {
		return nil, fail("", "environment references are not allowed: ${"+strings.Join(unresolved, "}, ${")+"}")
	}
	raw, err := presence(data)
	if err != nil {
		return nil, fail("", err.Error())
	}
	p.set = raw.top
	if err := p.check(name, raw.doors); err != nil {
		if le, ok := errors.AsType[*LoadError](err); ok {
			le.Provider = name
		}
		return nil, err
	}
	p.applyDefaults(raw.doors)
	if err := p.compile(raw.doors); err != nil {
		if le, ok := errors.AsType[*LoadError](err); ok {
			le.Provider = name
		}
		return nil, err
	}
	for i := range p.Doors {
		p.Doors[i].index = i
	}
	slices.SortStableFunc(p.Doors, func(a, b Door) int { return a.Layer - b.Layer })
	return p, nil
}

// rawKeys records which keys the file spelled, top level and per door, since
// a zero value cannot tell a key left out from one set empty.
type rawKeys struct {
	top   map[string]bool
	doors []map[string]bool
}

func presence(data []byte) (rawKeys, error) {
	var doc struct {
		Top   map[string]any   `yaml:",inline"`
		Doors []map[string]any `yaml:"doors"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return rawKeys{}, yamlenv.SanitizeDecodeError(err)
	}
	r := rawKeys{top: map[string]bool{}}
	for k := range doc.Top {
		r.top[k] = true
	}
	if doc.Doors != nil {
		r.top["doors"] = true
	}
	for _, d := range doc.Doors {
		keys := map[string]bool{}
		for k := range d {
			keys[k] = true
		}
		r.doors = append(r.doors, keys)
	}
	return r, nil
}

func (p *Provider) check(name string, doorSets []map[string]bool) error {
	switch {
	case p.Name == "":
		return &LoadError{Key: keyName, Problem: problemRequired}
	case !nameRE.MatchString(p.Name):
		return &LoadError{Key: keyName, Problem: "must match " + nameRE.String()}
	case p.Name != name:
		return &LoadError{Key: keyName, Problem: fmt.Sprintf("is %q but the file is named %q", p.Name, name+".yaml")}
	case p.Extends != "" && !nameRE.MatchString(p.Extends):
		return &LoadError{Key: keyExtends, Problem: "must match " + nameRE.String()}
	case p.Extends == p.Name:
		return &LoadError{Key: keyExtends, Problem: "a provider cannot extend itself"}
	case p.Extends != "" && p.set[keyDetect]:
		return &LoadError{Key: keyDetect, Problem: "a file that extends another is chosen only by name"}
	}
	switch p.Evidence {
	case "", EvidenceMeasured, EvidenceDocumented, EvidenceUnverified:
	default:
		return &LoadError{Key: "evidence", Problem: "must be measured, documented or unverified"}
	}
	for i, v := range p.Detect.Env {
		if !envNameRE.MatchString(v) {
			return &LoadError{Key: fmt.Sprintf("detect.env[%d]", i), Problem: problemEnvName}
		}
	}
	if err := p.Session.check(); err != nil {
		return err
	}
	if err := checkDoors(p.Doors, doorSets); err != nil {
		return err
	}
	return p.Hooks.check()
}

func (s *Session) check() error {
	if s.Env != "" && !envNameRE.MatchString(s.Env) {
		return &LoadError{Key: "session.env", Problem: problemEnvName}
	}
	if s.Stdin != "" && !stdinPathRE.MatchString(s.Stdin) {
		return &LoadError{Key: "session.stdin", Problem: "not a stdin field path"}
	}
	for i, proc := range s.Process {
		if proc == "" || strings.ContainsAny(proc, `/\`) {
			return &LoadError{Key: fmt.Sprintf("session.process[%d]", i), Problem: "must be a process name with no directory"}
		}
	}
	if len(s.ProcessShared) > 0 && len(s.Process) == 0 {
		return &LoadError{Key: "session.process_shared", Problem: "needs session.process"}
	}
	for i, arg := range s.ProcessShared {
		if arg == "" {
			return &LoadError{Key: fmt.Sprintf("session.process_shared[%d]", i), Problem: problemNotEmpty}
		}
	}
	if s.Record != nil {
		return s.Record.check()
	}
	return nil
}

func (r *Record) check() error {
	if r.DirEnv == "" && r.DirDefault == "" {
		return &LoadError{Key: "session.record", Problem: "needs dir_env or dir_default"}
	}
	if r.DirEnv != "" && !envNameRE.MatchString(r.DirEnv) {
		return &LoadError{Key: "session.record.dir_env", Problem: problemEnvName}
	}
	if r.Glob == "" {
		return &LoadError{Key: keyRecordGlob, Problem: problemRequired}
	}
	for key, path := range map[string]string{"parent": r.Parent, "reason": r.Reason} {
		if !stdinPathRE.MatchString(path) {
			return &LoadError{Key: "session.record." + key, Problem: "required, a JSON field path"}
		}
	}
	return nil
}

func checkDoors(doors []Door, sets []map[string]bool) error {
	seen := map[string]bool{}
	for i := range doors {
		d := &doors[i]
		key := fmt.Sprintf("doors[%d]", i)
		if err := d.check(sets[i]); err != nil {
			if le, ok := errors.AsType[*LoadError](err); ok {
				le.Key = key + "." + le.Key
			}
			return err
		}
		if seen[d.Name] {
			return &LoadError{Key: key + ".name", Problem: fmt.Sprintf("door %q appears twice", d.Name)}
		}
		seen[d.Name] = true
	}
	return nil
}

func (d *Door) check(set map[string]bool) error {
	if !nameRE.MatchString(d.Name) {
		return &LoadError{Key: keyName, Problem: "required, must match " + nameRE.String()}
	}
	keys, ok := doorKeys[d.Transport]
	if !ok {
		return &LoadError{Key: "transport", Problem: "must be http, exec, stdout or exit"}
	}
	if want := doorLayer[d.Transport]; d.Layer != want {
		return &LoadError{Key: "layer", Problem: fmt.Sprintf("a %s door is layer %d", d.Transport, want)}
	}
	for k := range set {
		if !slices.Contains(commonDoorKeys, k) && !slices.Contains(keys.required, k) && !slices.Contains(keys.optional, k) {
			return &LoadError{Key: k, Problem: fmt.Sprintf("not a key of the %s transport", d.Transport)}
		}
	}
	for _, k := range keys.required {
		if !set[k] {
			return &LoadError{Key: k, Problem: fmt.Sprintf("required for the %s transport", d.Transport)}
		}
	}
	if d.Asset != "" && (!fs.ValidPath(d.Asset) || d.Asset == ".") {
		return &LoadError{Key: "asset", Problem: "must be a relative path inside the providers folder"}
	}
	switch d.Transport {
	case TransportHTTP:
		return d.checkHTTP(set)
	case TransportExec:
		return d.checkExec()
	default:
		return d.checkLine(set)
	}
}

func (d *Door) checkHTTP(set map[string]bool) error {
	if d.URL == "" {
		return &LoadError{Key: keyURL, Problem: problemNotEmpty}
	}
	if d.Body == "" {
		return &LoadError{Key: "body", Problem: problemNotEmpty}
	}
	if d.Match.Provider != "" && !nameRE.MatchString(d.Match.Provider) {
		return &LoadError{Key: "match.provider", Problem: "must match " + nameRE.String()}
	}
	for h := range d.Headers {
		if !headerRE.MatchString(h) {
			return &LoadError{Key: "headers", Problem: fmt.Sprintf("%q is not a header name", h)}
		}
	}
	if set[keyDedupeWindow] && d.DedupeWindow < minDedupeWindow {
		return &LoadError{Key: keyDedupeWindow, Problem: "must be at least 60s, the longest gap between two tries of one key"}
	}
	for i, status := range d.Gone {
		if status < 400 || status > 499 || status == 401 || status == 403 {
			return &LoadError{Key: fmt.Sprintf("gone[%d]", i), Problem: "must be a 4xx other than 401 and 403, which mean the door's credential"}
		}
	}
	return nil
}

func (d *Door) checkExec() error {
	if len(d.Argv) == 0 || d.Argv[0] == "" {
		return &LoadError{Key: "argv", Problem: "needs a program as its first argument"}
	}
	return nil
}

func (d *Door) checkLine(set map[string]bool) error {
	if set[keyHold] {
		if d.Hold <= 0 {
			return &LoadError{Key: keyHold, Problem: "must be a positive duration"}
		}
		if !set[keyIdle] || d.Idle == "" {
			return &LoadError{Key: keyHold, Problem: "needs idle, the output printed when the hold lapses"}
		}
	} else if set[keyIdle] {
		return &LoadError{Key: keyIdle, Problem: "needs hold"}
	}
	if d.EndExit < 0 || d.EndExit > 255 {
		return &LoadError{Key: keyEndExit, Problem: "must be an exit code from 0 to 255"}
	}
	return nil
}

func (h *Hooks) check() error {
	roles := []struct {
		check interface{ check() error }
		key   string
	}{
		{key: "session_start", check: h.SessionStart},
		{key: "prompt", check: h.Prompt},
		{key: "stop", check: h.Stop},
		{key: "turn_end", check: h.TurnEnd},
		{key: "session_end", check: h.SessionEnd},
		{key: "subagent_stop", check: h.SubagentStop},
	}
	for _, r := range roles {
		if err := r.check.check(); err != nil {
			if le, ok := errors.AsType[*LoadError](err); ok {
				le.Key = "hooks." + r.key + "." + le.Key
			}
			return err
		}
	}
	return nil
}

// Each role's check takes a nil receiver for a role the file leaves out.

func (r *SessionStartRole) check() error {
	if r == nil {
		return nil
	}
	return firstErr(r.checkRole(), r.Stdin.check(), checkOutput("outputs.context", r.Outputs.Context), r.Source.check())
}

func (r *StopRole) check() error {
	if r == nil {
		return nil
	}
	return firstErr(r.checkRole(), r.Stdin.check(), checkOutput("outputs.block", r.Outputs.Block), checkOutput("outputs.notice", r.Outputs.Notice))
}

func (r *LifecycleRole) check() error {
	if r == nil {
		return nil
	}
	return firstErr(r.checkRole(), r.Stdin.check())
}

func (r *PromptRole) check() error {
	if r == nil {
		return nil
	}
	if err := firstErr(r.checkRole(), checkPath("stdin.agent_id", r.Stdin.AgentID), checkPath("stdin.prompt", r.Stdin.Prompt),
		checkOutput("outputs.context", r.Outputs.Context), checkOutput("outputs.block", r.Outputs.Block)); err != nil {
		return err
	}
	if r.Signal == "" {
		return nil
	}
	re, err := regexp.Compile(r.Signal)
	if err != nil {
		return &LoadError{Key: "signal", Problem: "not an RE2 pattern"}
	}
	if re.SubexpIndex("line") < 0 {
		return &LoadError{Key: "signal", Problem: "needs a named group (?P<line>...)"}
	}
	r.signal = re
	return nil
}

func (r *Role) checkRole() error {
	if r.Event == "" {
		return &LoadError{Key: "event", Problem: "required, the host's event name"}
	}
	if r.Budget < 0 || r.Budget > maxBudget {
		return &LoadError{Key: "budget", Problem: "must be at most 3s, the default budget"}
	}
	for path, values := range r.When {
		if !stdinPathRE.MatchString(path) {
			return &LoadError{Key: "when", Problem: fmt.Sprintf("%q is not a stdin field path", path)}
		}
		if len(values) == 0 {
			return &LoadError{Key: "when." + path, Problem: "needs at least one value"}
		}
	}
	return nil
}

func (s Stdin) check() error { return checkPath("stdin.agent_id", s.AgentID) }

func (s *Source) check() error {
	if s == nil {
		return nil
	}
	if !stdinPathRE.MatchString(s.Path) {
		return &LoadError{Key: "source.path", Problem: "required, a stdin field path"}
	}
	return nil
}

func checkPath(key, path string) error {
	if path != "" && !stdinPathRE.MatchString(path) {
		return &LoadError{Key: key, Problem: "not a stdin field path"}
	}
	return nil
}

func checkOutput(key string, o *Output) error {
	if o == nil {
		return nil
	}
	if o.Text == "" {
		return &LoadError{Key: key + ".text", Problem: problemRequired}
	}
	switch o.Stream {
	case "", StreamStdout, StreamStderr:
	default:
		return &LoadError{Key: key + ".stream", Problem: "must be stdout or stderr"}
	}
	if o.Exit < 0 || o.Exit > 255 {
		return &LoadError{Key: key + ".exit", Problem: "must be an exit code from 0 to 255"}
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) applyDefaults(doorSets []map[string]bool) {
	for i := range p.Doors {
		p.Doors[i].applyDefaults(p.Name, doorSets[i])
	}
	for _, o := range p.Hooks.outputs() {
		if o.Stream == "" {
			o.Stream = StreamStdout
		}
	}
}

// roleOutput is one output a file spells, with its key below hooks.
type roleOutput struct {
	*Output
	key string
}

func (d *Door) applyDefaults(provider string, set map[string]bool) {
	switch d.Transport {
	case TransportHTTP:
		if d.Match.Provider == "" {
			d.Match.Provider = provider
		}
	case TransportStdout, TransportExit:
		if !set[keyLine] {
			d.Line = defaultLine
		}
		if !set[keyEnd] {
			d.End = d.Line
		}
		if !set[keyEndExit] {
			d.EndExit = defaultEndExit
		}
	}
}

// outputs lists every output the file spells, for defaults and checks that
// apply to all of them.
func (h *Hooks) outputs() []roleOutput {
	var out []roleOutput
	if r := h.SessionStart; r != nil {
		out = append(out, roleOutput{r.Outputs.Context, "session_start.outputs.context"})
	}
	if r := h.Prompt; r != nil {
		out = append(out, roleOutput{r.Outputs.Context, "prompt.outputs.context"}, roleOutput{r.Outputs.Block, "prompt.outputs.block"})
	}
	if r := h.Stop; r != nil {
		out = append(out, roleOutput{r.Outputs.Block, "stop.outputs.block"}, roleOutput{r.Outputs.Notice, "stop.outputs.notice"})
	}
	return slices.DeleteFunc(out, func(o roleOutput) bool { return o.Output == nil })
}
