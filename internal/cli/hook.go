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

// hookBudget is a hook's request budget when its role sets none.
const hookBudget = 3 * time.Second

// The lines a hook prints of its own.
const (
	lineUnregistered = "grelot: not registered. Register before you start work: run `grelot help`, then `grelot register` as it says."
	lineFork         = "grelot: this chat is a fork of another chat; it shares that chat's identity and posts only as a delegate the parent grants"
	lineHandle       = "grelot: you are @%s; prefix every grelot command with `grelot as %s`"
	lineNoCheck      = "grelot: could not check this session's identity (%s); nothing is held"
)

// markFork is a fork's one line, printed once per fork session whichever
// role sees the fork first.
const markFork = "fork"

// hookCall is one run of grelot hook <provider> <role>.
type hookCall struct {
	*run
	in    hook.Input
	marks *hook.Marks
	// session is the environment's host session id.
	session string
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
	if err != nil {
		return nil
	}
	session, err := p.SessionID(r.Lookup)
	if err != nil {
		return nil
	}
	in, err := hook.ReadInput(r.Stdin)
	if err != nil {
		return nil
	}
	r.prov = p
	h := &hookCall{run: r, in: in, session: session, marks: hook.NewMarks(r.ConfigDir, r.now)}
	return h.role(ctx, args[1])
}

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
		if role := lifecycleRole(hs, name); role != nil && h.in.Matches(role.When) {
			h.lifecycle(ctx, name, role)
		}
	}
	return nil
}

func lifecycleRole(hs *provider.Hooks, name string) *provider.LifecycleRole {
	switch name {
	case roleTurnEnd:
		return hs.TurnEnd
	case roleSessionEnd:
		return hs.SessionEnd
	}
	return hs.SubagentStop
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
	err := h.emit(out, h.parentStart(ctx, role))
	if role.Once {
		h.marks.Set(hook.MarkOnce, key)
	}
	return err
}

// parentStart is the session start text of a session that acts for its
// own identity: the card, the status line and the manual; the revive
// offer; or the register line with the take-over hint a take_over source
// may get.
func (h *hookCall) parentStart(ctx context.Context, role *provider.SessionStartRole) string {
	h.self = true
	api, err := h.client(true)
	if err != nil {
		return failLine(err, role.Budget)
	}
	q, header := h.startRequest(ctx, role.Source)
	me, resp, err := hookMe(ctx, api, role.Budget, q, header)
	if err != nil {
		return failLine(err, role.Budget)
	}
	return h.startText(me, resp.Status)
}

// startRequest is session start's query and headers: proc only for a
// take_over source, and the machine and environment in headers, never the
// query, which access logs keep.
func (h *hookCall) startRequest(ctx context.Context, s *provider.Source) (url.Values, http.Header) {
	q := url.Values{}
	if s != nil {
		source := h.in.String(s.Path)
		if slices.Contains(s.TakeOver, source) && !slices.Contains(s.Fork, source) {
			if proc := proctree.Proc(h.tree(), h.prov.Session.Process, h.prov.Session.ProcessShared); proc != "" {
				q.Set("proc", proc)
			}
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

func (h *hookCall) startText(me *client.Me, status string) string {
	var b strings.Builder
	switch me.State {
	case client.StateLive, client.StateDormant:
		if me.State == client.StateDormant {
			b.WriteString(h.statusLine(status) + "\n")
		}
		writeCard(&b, me.Identity)
		if me.State == client.StateLive {
			b.WriteString(h.statusLine(status) + "\n")
		}
		b.WriteString(manual)
	default:
		b.WriteString(lineUnregistered + "\n")
		if me.Hint != nil && me.Hint.Line != "" {
			b.WriteString("grelot: " + safe(me.Hint.Line) + "\n")
		}
	}
	return b.String()
}

// childStart is a child's session start text: the fork's one line once,
// or the handle line of a delegate a subagent's own session holds. A
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
		return ""
	}
	api, err := client.NewHookAPI(cfg, h.Version, h.ConfigDir)
	if err != nil {
		return ""
	}
	api.Session = h.prov.Name + ":" + c.Stdin
	me, _, err := hookMe(ctx, api, budget, url.Values{}, http.Header{})
	if err != nil || me.State != client.StateLive || me.Identity == nil || !strings.Contains(me.Identity.Name, "/") {
		return ""
	}
	name := safe(me.Identity.Name)
	return fmt.Sprintf(lineHandle, name, name) + "\n"
}

// forkLine is the fork's one line the first time a role of the fork asks.
func (h *hookCall) forkLine(c hook.Class) string {
	key := h.prov.Name + ":" + c.Stdin
	if h.marks.Has(markFork, key) {
		return ""
	}
	h.marks.Set(markFork, key)
	return lineFork + "\n"
}

// hookMe sends GET /v1/me within the role's budget.
func hookMe(ctx context.Context, api *client.API, budget time.Duration, q url.Values, header http.Header) (*client.Me, client.Response, error) {
	if budget == 0 {
		budget = hookBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return api.Me(ctx, q, header)
}

// failLine is what a hook prints when the server did not say whether the
// session has an identity: the 426's own line, or that grelot could not
// check and why, so neither an outage nor a client fault holds the agent.
func failLine(err error, budget time.Duration) string {
	if budget == 0 {
		budget = hookBudget
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
// host takes it, and exits with the output's code. A role whose host
// spells no such output prints nothing.
func (h *hookCall) emit(out *provider.Output, text string) error {
	text = strings.TrimSuffix(text, "\n")
	if out == nil || text == "" {
		return nil
	}
	s, err := out.Render(&provider.Data{Context: text, Reason: text})
	if err != nil {
		return nil
	}
	w := h.Stdout
	if out.Stream == provider.StreamStderr {
		w = h.Stderr
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	fmt.Fprint(w, s)
	if out.Exit != 0 {
		return &exitError{code: out.Exit}
	}
	return nil
}
