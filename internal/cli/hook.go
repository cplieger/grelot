package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/hook"
	"github.com/cplieger/grelot/internal/proctree"
	"github.com/cplieger/grelot/internal/provider"
)

const verbHook = "hook"

// The lines a hook prints of its own.
const (
	lineUnregistered = "grelot: not registered. Register so other agents can find and message you: run `grelot help`, then `grelot register` as it says. " + lineRefused + "; grelot holds one turn end for it, never more."
	// lineRefused keeps a refused registration from costing the user's
	// task: a model reads a bare register line as a precondition of it.
	lineRefused = "If registering is refused, blocked or against the user's instructions (a sandbox, an approval reviewer, an instruction not to run commands), finish the user's task and tell the user"
	lineFork    = "grelot: this chat is a fork of another chat; it shares that chat's identity and posts only as a delegate the parent grants"
	lineHandle  = "grelot: you are @%s; prefix every grelot command with `grelot as %s`"
	lineNoCheck = "grelot: could not check this session's identity (%s); nothing is held"
	// A host keeps only a preview of a text over its context limit and does
	// not ask the model to read the rest, so these lines ask instead.
	lineManualLeftOut = "grelot: the manual is left out here to fit this host's context limit; run `grelot help` now, it holds every command and rule"
	lineCardLeftOut   = "grelot: your card and the manual are left out here to fit this host's context limit; run `grelot help` now, it holds every command and rule, and `grelot me` for your card"
)

// markFork is a fork's one line, printed once per fork session whichever
// role sees the fork first.
const markFork = "fork"

// hookCall is one run of grelot hook <provider> <role>.
type hookCall struct {
	*run
	in    hook.Input
	marks *hook.Marks
	// session is the environment's host session id, "" when the hook
	// process has none.
	session string
	// own is the host session the hook acts as: session, or the stdin's.
	own string
	// printed are the marks emit sets once it has printed the text that
	// carries them; a run that prints nothing spends none.
	printed [][2]string
	// unconfigured is a client config that does not load.
	unconfigured bool
}

// oneShot reports a one-shot run of the host (hook.OneShot), which the
// hooks never ask to register nor hold for it. It is asked only when the
// server answered that the session has no live identity, or did not say
// whether it has one, or this client cannot ack, since walking the
// process tree reads every ancestor.
func (h *hookCall) oneShot() bool {
	return hook.OneShot(h.prov, h.Lookup, h.tree)
}

// onPrint sets the mark when this run's emit prints.
func (h *hookCall) onPrint(kind, key string) {
	h.printed = append(h.printed, [2]string{kind, key})
}

// hook runs a host's hook. It never fails the host: a provider that does
// not load, a role it does not spell, a shell with no session or input it
// cannot read prints nothing and exits 0.
func (r *run) hook(ctx context.Context, args []string) error {
	if len(args) != 2 {
		fmt.Fprintln(r.Stderr, "grelot hook: run it as grelot hook <provider> <role>, as the provider's hook file says")
		return nil
	}
	p, err := r.Providers.Get(args[0])
	if err != nil || p.Session.Env == "" {
		return nil
	}
	in, err := hook.ReadInput(r.Stdin)
	if err != nil {
		return nil
	}
	// A host that leaves its session variable out of hook processes says
	// so, and the hook acts as its stdin session, the one the agent's own
	// shell carries in that variable. A value such a hook finds was
	// inherited from the shell of another session that started this host,
	// so it is ignored. Any other hook with no variable is another host
	// running these hook lines, and prints nothing.
	var session, own string
	if p.Session.HooksWithoutEnv {
		own = in.String(p.Session.Stdin)
	} else {
		session, _ = p.SessionID(r.Lookup)
		own = session
	}
	if own == "" {
		return nil
	}
	r.prov = p
	r.hookSession = p.Name + ":" + own
	h := &hookCall{run: r, in: in, session: session, own: own, marks: hook.NewMarks(r.ConfigDir, r.now)}
	// A client with no usable config writes no state, and its roles print
	// only the could-not-check line that names the config's fault.
	if _, err := r.config(); err != nil {
		h.unconfigured = true
		h.marks = hook.ReadMarks(r.ConfigDir, r.now)
	}
	h.touchHooked()
	return h.role(ctx, args[1])
}

// touchHooked marks that the host's hooks run, for run.hooksLost: on this
// machine, for the hook's session, and for a subagent's own session where
// the provider names it, which is the one that subagent's shell carries.
func (h *hookCall) touchHooked() {
	if h.prov.Hooks.LostFix == "" {
		return
	}
	h.marks.Touch(hook.MarkHostHooked, h.prov.Name, hookedRefresh)
	h.marks.Touch(hook.MarkSessionHooked, h.hookSession, hookedRefresh)
	if sub := h.prov.Hooks.SubagentStop; sub != nil && sub.Stdin.Session != "" {
		if id := h.in.String(sub.Stdin.Session); id != "" {
			h.marks.Touch(hook.MarkSessionHooked, h.prov.Name+":"+id, hookedRefresh)
		}
	}
}

// hookedRefresh is how often a hook rewrites its hooked marks: each run
// costs one read otherwise.
const hookedRefresh = 24 * time.Hour

// The roles grelot hook runs, as provider files name them.
const (
	roleSessionStart = "session_start"
	rolePrompt       = "prompt"
	roleStop         = "stop"
	roleTurnEnd      = "turn_end"
	roleSessionEnd   = "session_end"
	roleSubagentStop = "subagent_stop"
)

// role runs the named role when the provider spells it and its when
// filter passes.
func (h *hookCall) role(ctx context.Context, name string) error {
	hs := &h.prov.Hooks
	switch name {
	case roleSessionStart:
		if hs.SessionStart != nil && h.in.Matches(hs.SessionStart.When) {
			return h.sessionStart(ctx, hs.SessionStart)
		}
	case rolePrompt:
		if hs.Prompt != nil && h.in.Matches(hs.Prompt.When) {
			return h.prompt(ctx, hs.Prompt)
		}
	case roleStop:
		if hs.Stop != nil && h.in.Matches(hs.Stop.When) {
			return h.stop(ctx, hs.Stop)
		}
	case roleTurnEnd, roleSessionEnd, roleSubagentStop:
		h.report(ctx, name)
	}
	return nil
}

// report runs a lifecycle role when the provider spells it and its when
// filter passes.
func (h *hookCall) report(ctx context.Context, name string) {
	hs := &h.prov.Hooks
	if name == roleSubagentStop {
		if hs.SubagentStop != nil && h.in.Matches(hs.SubagentStop.When) {
			h.subagentStop(ctx, hs.SubagentStop)
		}
		return
	}
	role := hs.SessionEnd
	if name == roleTurnEnd {
		role = hs.TurnEnd
	}
	if role != nil && h.in.Matches(role.When) {
		h.lifecycle(ctx, name, role)
	}
}

func (h *hookCall) classify(agentID string) hook.Class {
	return hook.Classify(h.prov, h.in, agentID, hook.Shell{Lookup: h.Lookup, Home: h.Home, Session: h.session}, h.marks)
}

// onceKey keys a role's once mark by the hook's own session.
func (h *hookCall) onceKey(role string, c hook.Class) string {
	s := c.Stdin
	if s == "" {
		s = h.session
	}
	return role + "\x00" + h.prov.Name + ":" + s
}

func (h *hookCall) sessionStart(ctx context.Context, role *provider.SessionStartRole) error {
	c := h.classify(role.Stdin.AgentID)
	out := role.Outputs.Context
	switch c.Kind {
	case hook.Replaced:
		return nil
	case hook.Child:
		return h.emit(out, h.childStart(ctx, c, h.in.String(role.Stdin.AgentID) != "", role.Budget))
	}
	key := h.onceKey(roleSessionStart, c)
	if role.Once && h.marks.Has(hook.MarkOnce, key) {
		return nil
	}
	text, err := h.parentStart(ctx, role)
	if err != nil {
		return h.emit(out, h.failText(err, role.Budget))
	}
	if role.Once {
		h.onPrint(hook.MarkOnce, key)
	}
	return h.emit(out, text)
}

// parentStart is the session start text of a session that acts for its
// own identity: the card, the status line and the manual; the revive
// offer; or the register line with the take-over hint a take-over start
// may get. An error is a server that did not answer it.
func (h *hookCall) parentStart(ctx context.Context, role *provider.SessionStartRole) (string, error) {
	h.self = true
	api, err := h.client(true)
	if err != nil {
		return "", err
	}
	q, header := h.startRequest(ctx, role)
	me, resp, err := hookMe(ctx, api, role.Budget, q, header)
	if err != nil {
		return "", err
	}
	return h.startText(role, me, resp.Status), nil
}

// startRequest is session start's query and headers: proc only for a
// take-over start, and the machine and environment in headers, never the
// query, which access logs keep.
func (h *hookCall) startRequest(ctx context.Context, role *provider.SessionStartRole) (url.Values, http.Header) {
	q := url.Values{}
	if h.takesOver(role) {
		if proc := proctree.Proc(h.tree(), h.prov.Session.Process, h.prov.Session.ProcessShared); proc != "" {
			q.Set("proc", proc)
		}
	}
	header := http.Header{}
	w := h.machineAndEnvironment(ctx)
	if w.Environment != "" {
		header.Set(client.HeaderEnvironment, w.Environment)
	}
	if w.Machine != "" {
		header.Set(client.HeaderMachine, w.Machine)
	}
	return q, header
}

// takesOver says whether this start may get the take-over hint: one whose
// source is not a fork, and is in take_over unless every start may.
func (h *hookCall) takesOver(role *provider.SessionStartRole) bool {
	s := role.Source
	if s == nil {
		return role.TakeOverEveryStart
	}
	source := h.in.String(s.Path)
	if slices.Contains(s.Fork, source) {
		return false
	}
	return role.TakeOverEveryStart || slices.Contains(s.TakeOver, source)
}

func (h *hookCall) startText(role *provider.SessionStartRole, me *client.Me, status string) string {
	if me.State != client.StateLive && h.oneShot() {
		return ""
	}
	switch me.State {
	case client.StateLive, client.StateDormant:
		return h.identityStart(role, me, status)
	}
	text := lineUnregistered + "\n"
	if me.Hint != nil && me.Hint.Line != "" {
		text += "grelot: " + safe(me.Hint.Line) + "\n"
	}
	return text
}

// identityStart is the card, the status line and the manual, or, when that
// is over the host's context limit, the most of it that fits with a line
// naming what was left out; the leanest form is printed whatever its size.
func (h *hookCall) identityStart(role *provider.SessionStartRole, me *client.Me, status string) string {
	var b strings.Builder
	writeCard(&b, me.Identity)
	card := b.String()
	line := h.statusLine(status) + "\n"
	join := func(card, tail string) string {
		if me.State == client.StateDormant {
			return line + card + tail
		}
		return card + line + tail
	}
	texts := []string{join(card, manual), join(card, lineManualLeftOut+"\n"), join("", lineCardLeftOut+"\n")}
	for _, text := range texts[:len(texts)-1] {
		if role.ContextLimit == 0 || len(rendered(role.Outputs.Context, text)) <= role.ContextLimit {
			return text
		}
	}
	return texts[len(texts)-1]
}

// childStart is a child's session start text: the fork's one line once,
// or the handle line of a delegate a subagent's own session holds, or the
// could-not-check line when that session's check got no answer. A
// subagent sharing its parent's session asks nothing, since the answer
// would be the parent's, and a step's session never holds an identity.
func (h *hookCall) childStart(ctx context.Context, c hook.Class, subagent bool, budget time.Duration) string {
	if c.Fork {
		return h.forkLine(c)
	}
	if !subagent || c.Stdin == "" || c.Stdin == h.session {
		return ""
	}
	cfg, err := h.config()
	if err != nil {
		return failLine(err, budget)
	}
	api, err := client.NewOnceAPI(cfg, h.Version, h.ConfigDir)
	if err != nil {
		return failLine(err, budget)
	}
	api.Session = h.prov.Name + ":" + c.Stdin
	me, _, err := hookMe(ctx, api, budget, url.Values{}, http.Header{})
	if err != nil {
		return failLine(err, budget)
	}
	if me.State != client.StateLive || me.Identity == nil || !strings.Contains(me.Identity.Name, "/") {
		return ""
	}
	name := safe(me.Identity.Name)
	return fmt.Sprintf(lineHandle, name, name) + "\n"
}

// forkLine is the fork's one line the first time a role of the fork asks,
// and nothing from a client with no config, which could not keep it once.
func (h *hookCall) forkLine(c hook.Class) string {
	key := h.prov.Name + ":" + c.Stdin
	if h.unconfigured || h.marks.Has(markFork, key) {
		return ""
	}
	h.onPrint(markFork, key)
	return lineFork + "\n"
}

// hookMe sends GET /v1/me within the role's budget.
func hookMe(ctx context.Context, api *client.API, budget time.Duration, q url.Values, header http.Header) (*client.Me, client.Response, error) {
	if budget == 0 {
		budget = provider.DefaultBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return api.Me(ctx, q, header)
}

// failText is failLine outside a one-shot run, whose output stays its
// answer alone whatever the server does.
func (h *hookCall) failText(err error, budget time.Duration) string {
	if h.oneShot() {
		return ""
	}
	return failLine(err, budget)
}

// failLine is what a hook prints when the server did not say whether the
// session has an identity: the 426's own line, or that grelot could not
// check and why, so neither an outage nor a client fault holds the agent.
func failLine(err error, budget time.Duration) string {
	if budget == 0 {
		budget = provider.DefaultBudget
	}
	var why string
	ae, isAPI := errors.AsType[*client.APIError](err)
	_, unreachable := errors.AsType[*client.UnreachableError](err)
	switch {
	case isAPI && ae.HTTP == http.StatusUpgradeRequired:
		return "grelot: " + ae.Msg + "\n"
	case isAPI:
		why = fmt.Sprintf("the server answered %d: %s", ae.HTTP, ae.Msg)
	case errors.Is(err, context.DeadlineExceeded):
		why = fmt.Sprintf("no answer within %s", budget)
	case unreachable:
		why = "the server cannot be reached"
	default:
		why = safe(strings.TrimPrefix(err.Error(), "grelot: "))
	}
	return fmt.Sprintf(lineNoCheck, why) + "\n"
}

// emit prints text through the provider's output, which says how the
// host takes it, sets the onPrint marks, and exits with the output's
// code. A role whose host spells no such output prints nothing.
func (h *hookCall) emit(out *provider.Output, text string) error {
	printed := h.printed
	h.printed = nil
	s := rendered(out, text)
	if s == "" {
		return nil
	}
	w := h.Stdout
	if out.Stream == provider.StreamStderr {
		w = h.Stderr
	}
	if _, err := fmt.Fprint(w, s); err != nil {
		return nil
	}
	for _, m := range printed {
		h.marks.Set(m[0], m[1])
	}
	if out.Exit != 0 {
		return &exitError{code: out.Exit}
	}
	return nil
}

// rendered is text as emit prints it through out, ending in a newline, or
// "" when out is nil, the text is empty or the template fails.
func rendered(out *provider.Output, text string) string {
	text = strings.TrimSuffix(text, "\n")
	if out == nil || text == "" {
		return ""
	}
	s, err := out.Render(&provider.Data{Context: text, Reason: text})
	if err != nil {
		return ""
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}
