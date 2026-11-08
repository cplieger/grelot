package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/hook"
	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/transport"
)

// The marks the turn roles keep, beside markFork.
const (
	// markDesc is when this client last sent the session identity's
	// description, keyed by the session as the API sends it.
	markDesc = "desc"
	// markReminder is when the prompt line last asked whether the
	// description still holds.
	markReminder = "reminder"
	// markNote is a same-agent fork's note, printed once.
	markNote = "note"
	// markTurn is the last turn start the prompt role saw.
	markTurn = "turn"
	// markHeld is the last stop the stop role held.
	markHeld = "held"
	// markAsked is the stop that asked a session to register or revive,
	// keyed by the session and the server's state, so it asks once.
	markAsked = "asked"
	// markThread is the prompt line's one ask to follow or start a thread.
	markThread = "thread"
)

const (
	// reminderAge is how old a description is before the prompt line asks
	// whether it still holds, and how often it asks again.
	reminderAge = time.Hour
	// threadAge is how long an identity lives before the prompt line asks
	// it to follow or start a thread: the turn that registered it has had
	// the register answer's ask and time to act on it.
	threadAge = 30 * time.Minute
	// turnLapse is the server's bound on a turn no hook reports the end of;
	// with no prompt role to mark turn starts, a stop held within it counts
	// as held this turn.
	turnLapse = 5 * time.Minute
	wakeDown  = "down"
	// keyEvent is a lifecycle report's event field, named as the role.
	keyEvent = "event"
)

const (
	// lineRegisterTurn and lineDormant are prompt lines, under 30 words
	// as turnLine says.
	lineRegisterTurn = "grelot: not registered. Run `grelot help`, then `grelot register`. If refused, blocked or against the user's instructions, finish the task and tell the user."
	lineDormant      = "grelot: not registered: %s is dormant; run `grelot revive --ttl=<d>` (see `grelot help`). If refused, blocked or against the user's instructions, finish the task and tell the user."
	lineRegisterHold = "grelot: not registered. Register now if you can: run `grelot help`, then `grelot register` as it says. " + lineRefused + "; " + lineAskedOnce
	lineAskedOnce    = "grelot will not hold this session for it again."
	lineStale        = "grelot (automated, not the user): signal %d is stale; its mail has been read, so this turn ends"
	lineHold         = "grelot (automated, not the user): unread %s; run grelot inbox before you stop"
)

// segThread is the prompt line's ask to follow or start a thread. It
// never shares a line with the description reminder, which would take the
// line past 30 words.
const segThread = "no thread followed: `grelot follow <path>` or start yours"

// signalRE reads a fixed signal line; transport.SignalLine owns its form,
// and a match counts only when SignalLine prints it back the same.
var signalRE = regexp.MustCompile(`^\[grelot:([0-9]+)\] grelot \(automated, not the user\): ([0-9]+) new; run grelot inbox$`)

// descSent records that this session's identity just declared its
// description, for the prompt line's age. A delegate's is not the
// session's.
func (r *run) descSent() {
	if r.handle != "" {
		return
	}
	if s, err := r.session(); err == nil {
		hook.NewMarks(r.ConfigDir, r.now).Set(markDesc, s)
	}
}

// chatKey keys a mark by the hook's own session, so a rewind and its
// parent keep their own turns.
func (h *hookCall) chatKey(c hook.Class) string {
	s := c.Stdin
	if s == "" {
		s = h.session
	}
	return h.prov.Name + ":" + s
}

// identityKey keys a mark by the session the identity is registered from,
// the API's Grelot-Session.
func (h *hookCall) identityKey() string { return h.prov.Name + ":" + h.own }

// prompt runs at turn start: GET /v1/me?turn=start opens the turn and
// answers the one line, the register or revive line, or the stale-signal
// block. A child prints only a fork's one line, once, and asks nothing.
func (h *hookCall) prompt(ctx context.Context, role *provider.PromptRole) error {
	c := h.classify(role.Stdin.AgentID)
	out := role.Outputs.Context
	switch c.Kind {
	case hook.Replaced:
		return nil
	case hook.Child:
		if c.Fork {
			return h.emit(out, h.forkLine(c))
		}
		return nil
	}
	key := h.onceKey(rolePrompt, c)
	if role.Once && h.marks.Has(hook.MarkOnce, key) {
		return nil
	}
	h.marks.Set(markTurn, h.chatKey(c))
	seq, isSignal := signalSeq(role, h.in.String(role.Stdin.Prompt))
	me, err := h.promptMe(ctx, role, seq)
	if err != nil {
		return h.emit(out, h.failText(err, role.Budget))
	}
	if role.Once {
		h.onPrint(hook.MarkOnce, key)
	}
	if isSignal && role.Outputs.Block != nil && me.State == client.StateLive && me.SignalRead {
		return h.emit(role.Outputs.Block, fmt.Sprintf(lineStale, seq))
	}
	text := h.promptLine(me)
	if c.Kind == hook.SameAgent {
		if note := h.sameAgentNote(c); note != "" {
			text = note + "\n" + text
		}
	}
	return h.emit(out, text)
}

// sameAgentNote is the provider's note for a same-agent fork, the first
// time a prompt of the fork asks.
func (h *hookCall) sameAgentNote(c hook.Class) string {
	rec := h.prov.Session.Record
	if rec == nil || rec.SameAgentNote == "" {
		return ""
	}
	key := h.chatKey(c)
	if h.marks.Has(markNote, key) {
		return ""
	}
	h.onPrint(markNote, key)
	return "grelot: " + safe(rec.SameAgentNote)
}

// signalSeq is the seq of the fixed signal line prompt carries: the whole
// prompt, or the pattern's line group where the provider spells one.
func signalSeq(role *provider.PromptRole, prompt string) (uint64, bool) {
	text := prompt
	if re := role.SignalPattern(); re != nil {
		m := re.FindStringSubmatch(prompt)
		if m == nil {
			return 0, false
		}
		text = m[re.SubexpIndex("line")]
	}
	m := signalRE.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	seq, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil || seq == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || transport.SignalLine(seq, n) != text {
		return 0, false
	}
	return seq, true
}

// promptMe asks GET /v1/me?turn=start, with the signal the prompt
// carries, which opens the turn unless the server finds the signal read.
func (h *hookCall) promptMe(ctx context.Context, role *provider.PromptRole, seq uint64) (*client.Me, error) {
	q := url.Values{"turn": {"start"}}
	if seq > 0 {
		q.Set("signal", strconv.FormatUint(seq, 10))
	}
	h.self = true
	api, err := h.client(true)
	if err != nil {
		return nil, err
	}
	me, _, err := hookMe(ctx, api, role.Budget, q, http.Header{})
	return me, err
}

// promptLine is the prompt's line for the server's answer.
func (h *hookCall) promptLine(me *client.Me) string {
	switch {
	case me.State == client.StateLive:
		if me.Identity == nil {
			return ""
		}
		return h.turnLine(me)
	case h.oneShot():
		return ""
	case me.State == client.StateDormant:
		name := ""
		if me.Identity != nil {
			name = me.Identity.Name
		}
		return dormantLine(name)
	}
	return lineRegisterTurn
}

func dormantLine(name string) string {
	who := "your identity"
	if name != "" {
		who = "@" + safe(name)
	}
	return fmt.Sprintf(lineDormant, who)
}

// turnLine is the one line every turn starts with. It names no peer and
// stays under 30 words: the start line is the only part a provider sets.
func (h *hookCall) turnLine(me *client.Me) string {
	unread := 0
	if me.Unread != nil {
		unread = me.Unread.Total
	}
	age := h.descAge(me.Identity)
	parts := []string{"grelot: @" + safe(me.Identity.Name), fmt.Sprintf("%d unread", unread), "desc set " + ago(age) + " ago"}
	switch {
	case age > reminderAge && h.reminderDue():
		parts = append(parts, "still true? `grelot set --desc` if not")
	case h.threadDue(me.Identity):
		parts = append(parts, segThread)
	}
	if me.Wake == wakeDown {
		if start := client.FirstStart(h.prov); start != "" {
			parts = append(parts, "wake: down, start: "+safe(start))
		}
	}
	return strings.Join(append(parts, "grelot help"), " · ")
}

// descAge is the description's age: from the later of the identity's
// registration and the last description this client sent for its session.
func (h *hookCall) descAge(id *client.Identity) time.Duration {
	since := id.RegisteredAt
	if t, ok := h.marks.When(markDesc, h.identityKey()); ok && t.After(since) {
		since = t
	}
	if since.IsZero() {
		return 0
	}
	return max(0, h.now().Sub(since))
}

func (h *hookCall) reminderDue() bool {
	key := h.identityKey()
	if t, ok := h.marks.When(markReminder, key); ok && h.now().Sub(t) < reminderAge {
		return false
	}
	h.onPrint(markReminder, key)
	return true
}

// threadDue reports, once per session, an identity past threadAge that
// followsNoThread, outside a one-shot run.
func (h *hookCall) threadDue(id *client.Identity) bool {
	key := h.identityKey()
	if !followsNoThread(id) || h.now().Sub(id.RegisteredAt) < threadAge || h.marks.Has(markThread, key) || h.oneShot() {
		return false
	}
	h.onPrint(markThread, key)
	return true
}

// ago is d in whole minutes under an hour, hours under two days, else days.
func ago(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d d", int(d.Hours()/24))
}

// stop runs at turn end: POST /v1/events/hold holds the turn once for
// unread wake mail, and a session with no live identity once in the
// session's life, never a one-shot run. A second stop in one turn holds
// nothing and reports the turn's end, so a first nudge the open turn
// deferred goes out at once.
func (h *hookCall) stop(ctx context.Context, role *provider.StopRole) error {
	c := h.classify(role.Stdin.AgentID)
	if c.Kind == hook.Replaced || c.Kind == hook.Child {
		return nil
	}
	key := h.onceKey(roleStop, c)
	if role.Once && h.marks.Has(hook.MarkOnce, key) {
		return nil
	}
	h.self = true
	api, err := h.client(true)
	if err != nil {
		return h.emit(role.Outputs.Notice, h.failText(err, role.Budget))
	}
	if h.heldThisTurn(c) {
		postLifecycle(ctx, api, role.Budget, map[string]any{keyEvent: roleTurnEnd})
		return nil
	}
	// Mail this client cannot ack stays unread, so a hold would hold every
	// turn until the client is upgraded.
	if line := client.CannotAck(h.ConfigDir, h.Version); line != "" {
		postLifecycle(ctx, api, role.Budget, map[string]any{keyEvent: roleTurnEnd})
		if h.oneShot() {
			return nil
		}
		return h.emit(role.Outputs.Notice, "grelot: unread mail stays unread until this client is upgraded: "+line)
	}
	var held client.Held
	// A host that cannot be held, or runs one stop per turn, ends the turn
	// at this hold whatever it answers.
	last := role.OncePerTurn || role.Outputs.Block == nil
	bctx, cancel := budgetCtx(ctx, role.Budget)
	_, err = api.Do(bctx, &client.Request{
		Method: http.MethodPost, Path: "/v1/events/hold", Body: map[string]any{"for": "stop", "last": last}, Once: true,
	}, &held)
	cancel()
	text, block, err := h.stopText(&held, err, h.chatKey(c))
	if err != nil {
		return h.emit(role.Outputs.Notice, h.failText(err, role.Budget))
	}
	out := role.Outputs.Notice
	if block {
		out = role.Outputs.Block
		h.marks.Set(markHeld, h.chatKey(c))
	}
	if role.Once {
		h.onPrint(hook.MarkOnce, key)
	}
	return h.emit(out, text)
}

// heldThisTurn reports a stop already held since the last turn start.
func (h *hookCall) heldThisTurn(c hook.Class) bool {
	key := h.chatKey(c)
	held, ok := h.marks.When(markHeld, key)
	if !ok {
		return false
	}
	if turn, ok := h.marks.When(markTurn, key); ok {
		return !held.Before(turn)
	}
	return h.now().Sub(held) < turnLapse
}

// stopText is the stop hold's reason, and whether it holds the turn; the
// error is a hold the server did not answer. A hold with only a down wake
// prints nothing: the prompt line names the start line every turn. The
// register or revive hold is spent once printed, per chat and state.
func (h *hookCall) stopText(held *client.Held, err error, chat string) (text string, block bool, _ error) {
	if err == nil {
		if !held.Block {
			return "", false, nil
		}
		text = fmt.Sprintf(lineHold, countsLine(&held.Counts))
		if start := client.FirstStart(h.prov); held.Wake == wakeDown && start != "" {
			text += " · wake: down, start: " + safe(start)
		}
		return text, true, nil
	}
	ae, isAPI := errors.AsType[*client.APIError](err)
	switch {
	case !isAPI || ae.HTTP != http.StatusGone:
		return "", false, err
	case ae.Code != client.StateNone && ae.Code != client.StateDormant, h.oneShot():
		return "", false, nil
	}
	key := chat + "\x00" + ae.Code
	if h.marks.Has(markAsked, key) {
		return "", false, nil
	}
	h.onPrint(markAsked, key)
	if ae.Code == client.StateNone {
		return lineRegisterHold, true, nil
	}
	return dormantLine("") + " " + lineAskedOnce, true, nil
}

// lifecycle reports turn_end or session_end and prints nothing, whatever
// the server answers.
func (h *hookCall) lifecycle(ctx context.Context, event string, role *provider.LifecycleRole) {
	if h.in.String(role.Stdin.AgentID) != "" {
		return
	}
	c := h.classify("")
	if c.Kind == hook.Replaced || c.Kind == hook.Child {
		return
	}
	// A session end reported from a session other than the identity's
	// (a rewind, a step) never ends the identity.
	if event == roleSessionEnd && c.Stdin != "" && c.Stdin != h.own {
		return
	}
	h.self = true
	if api, err := h.client(true); err == nil {
		postLifecycle(ctx, api, role.Budget, map[string]any{keyEvent: event})
	}
}

// subagentStop ends the delegate a subagent's own session holds, and
// prints nothing. A subagent sharing its parent's session holds none the
// server could end.
func (h *hookCall) subagentStop(ctx context.Context, role *provider.SubagentStopRole) {
	sub := h.in.String(cmp.Or(role.Stdin.Session, h.prov.Session.Stdin))
	if sub == "" || sub == h.session {
		return
	}
	api, err := h.sessionAPI(sub)
	if err != nil {
		return
	}
	postLifecycle(ctx, api, role.Budget, map[string]any{keyEvent: roleSubagentStop, "agent_id": h.in.String(role.Stdin.AgentID)})
}

func postLifecycle(ctx context.Context, api *client.API, budget time.Duration, body map[string]any) {
	ctx, cancel := budgetCtx(ctx, budget)
	defer cancel()
	_, _ = api.Do(ctx, &client.Request{Method: http.MethodPost, Path: "/v1/lifecycle", Body: body, Once: true}, nil)
}

// sessionAPI is a hook API acting as the host session stdin, a subagent's
// own, with no delegate header.
func (h *hookCall) sessionAPI(stdin string) (*client.API, error) {
	cfg, err := h.config()
	if err != nil {
		return nil, err
	}
	api, err := client.NewOnceAPI(cfg, h.Version, h.ConfigDir)
	if err != nil {
		return nil, err
	}
	api.Session = h.prov.Name + ":" + stdin
	return api, nil
}

// budgetCtx bounds one hook request by the role's budget, provider.DefaultBudget when
// the role sets none.
func budgetCtx(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if budget == 0 {
		budget = provider.DefaultBudget
	}
	return context.WithTimeout(ctx, budget)
}
