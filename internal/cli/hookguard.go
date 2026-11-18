package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/hook"
	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/provider"
)

// The guard's lines, each naming one grant another agent holds over a
// written file, and the last line counting the grants left out when the
// rest would pass the host's context limit, after some lines or alone. A
// host may show them after the write's result, so they read true before
// and after it.
const (
	lineGuardHeld   = "grelot: @%s holds %s until %s (%s); your write goes ahead. Coordinate with grelot post '@%s' or reserve it after them."
	lineGuardQueued = "grelot: @%s holds %s until %s (%s); your write goes ahead. You are queued for %s (reservation %d): coordinate with grelot post '@%s' or wait for the grant, which reaches you as mail."
	lineGuardMore   = "grelot: other agents hold more reservations over files in this write, %d not shown; grelot reservations lists them."
	lineGuardCount  = "grelot: other agents hold reservations over files in this write, %d not shown; grelot reservations lists them."
)

// guard runs before a file-write tool: it names each other agent holding a
// written file, and prints nothing otherwise or on any error. It never
// stops the write, so it exits 0 whatever happens, and it only reads.
func (h *hookCall) guard(ctx context.Context, role *provider.GuardRole) {
	if h.unconfigured {
		return
	}
	if c := h.classify(role.Stdin.AgentID); c.Kind == hook.Child || c.Kind == hook.Replaced {
		return
	}
	locals := h.writtenFiles(role)
	if len(locals) == 0 || h.oneShot() {
		return
	}
	// The budget bounds the whole wait the write sees, git's lookups of
	// the paths included, not only the request.
	ctx, cancel := budgetCtx(ctx, role.Budget)
	defer cancel()
	paths := h.repoPaths(ctx, locals)
	if len(paths) == 0 {
		return
	}
	h.self = true
	api, err := h.client(true)
	if err != nil {
		return
	}
	held, err := api.Holders(ctx, paths)
	if err != nil {
		return
	}
	_ = h.emit(role.Outputs.Context, guardText(held, role.ContextLimit, role.Outputs.Context))
}

// writtenFiles is each local file the hook input names: each stdin path's
// value, then the patch's files, relative ones under the stdin cwd and
// dropped when it has none.
func (h *hookCall) writtenFiles(role *provider.GuardRole) []string {
	var out []string
	for _, p := range role.Stdin.Paths {
		if v := h.in.String(p); v != "" {
			out = append(out, v)
		}
	}
	if role.Stdin.Patch == "" {
		return out
	}
	cwd := h.in.String(role.Stdin.Cwd)
	for _, p := range hook.PatchPaths(h.in.String(role.Stdin.Patch)) {
		switch {
		case filepath.IsAbs(p):
			out = append(out, p)
		case cwd != "":
			out = append(out, filepath.Join(cwd, p))
		}
	}
	return out
}

// repoPaths is the canonical form of each local inside a clone with a
// remote, in order with repeats dropped, at most protopath.MaxHolderPaths; a path
// that does not resolve is left out.
func (h *hookCall) repoPaths(ctx context.Context, locals []string) []string {
	res, err := h.paths()
	if err != nil {
		return nil
	}
	var out []string
	for loc, err := range res.Paths(ctx, locals) {
		if err != nil || loc.Clone == nil || loc.Clone.Repo == "" || slices.Contains(out, loc.Canonical) {
			continue
		}
		if out = append(out, loc.Canonical); len(out) == protopath.MaxHolderPaths {
			break
		}
	}
	return out
}

// guardText is one line per grant in held, in answer order, keeping the
// most lines whose rendered text through out fits limit (0 for none) with
// a last line counting the grants left out.
func guardText(held []client.Holding, limit int, out *provider.Output) string {
	lines := make([]string, 0, len(held))
	for i := range held {
		lines = append(lines, guardLine(&held[i]))
	}
	text := strings.Join(lines, "\n")
	if limit == 0 || len(rendered(out, text)) <= limit {
		return text
	}
	for n := len(lines) - 1; n > 0; n-- {
		text = strings.Join(append(slices.Clip(lines[:n]), fmt.Sprintf(lineGuardMore, len(lines)-n)), "\n")
		if len(rendered(out, text)) <= limit {
			return text
		}
	}
	return fmt.Sprintf(lineGuardCount, len(lines))
}

func guardLine(e *client.Holding) string {
	g := e.Held
	holder, path, until, reason := safe(g.Holder), safe(g.Path), stamp(g.Expires), safe(g.Reason)
	if q := e.Queued; q != nil {
		return fmt.Sprintf(lineGuardQueued, holder, path, until, reason, safe(q.Path), q.ID, holder)
	}
	return fmt.Sprintf(lineGuardHeld, holder, path, until, reason, holder)
}
