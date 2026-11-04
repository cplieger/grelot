package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// maxAgentID bounds a host's subagent id.
const maxAgentID = 256

// Revoked is what a revoke ended: the live delegates of the label, by
// handle, and whether an unused link of the label went too.
type Revoked struct {
	Handles []string
	Link    bool
}

// Revoke ends c's live delegates of label and deletes its unused link of
// label. A handle keeps the parent's name it was granted under, so a
// delegate is matched by its label alone. Refusals: KindInvalid for a bad
// label, KindForbidden for a delegate caller, KindNotFound when nothing has
// the label, KindGone for a session with no live identity.
func (s *Store) Revoke(ctx context.Context, c Caller, label string) (Revoked, error) {
	if err := checkLabel(label); err != nil {
		return Revoked{}, err
	}
	var out Revoked
	err := s.update(ctx, func(w *wtx) error {
		var rerr error
		out, rerr = w.revoke(c, label)
		return rerr
	})
	return out, err
}

func (w *wtx) revoke(c Caller, label string) (Revoked, error) {
	me, err := w.liveAs(c)
	if err != nil {
		return Revoked{}, err
	}
	if me.Parent != 0 {
		return Revoked{}, refuse(KindForbidden, "a delegate has no delegates to revoke")
	}
	var out Revoked
	if out.Handles, err = w.revokeDelegates(me.ID, label); err != nil {
		return Revoked{}, err
	}
	if out.Link, err = w.revokeLink(&me, label); err != nil {
		return Revoked{}, err
	}
	if len(out.Handles) == 0 && !out.Link {
		return Revoked{}, refuse(KindNotFound, fmt.Sprintf("no live delegate or unused link is labelled %s: grelot who lists your delegates", label))
	}
	if w.seen(&me) {
		err = w.s.t.identities.table.Put(w.tx, me.ID, &me)
	}
	return out, err
}

// revokeDelegates ends parent's live delegates of label and returns their
// handles.
func (w *wtx) revokeDelegates(parent uint64, label string) ([]string, error) {
	ids, err := w.s.delegatesOf(w.r, parent)
	if err != nil {
		return nil, err
	}
	var handles []string
	for _, id := range ids {
		d, found, serr := w.s.t.identities.settle(w, id)
		if serr != nil {
			return nil, serr
		}
		if _, l, _ := strings.Cut(d.Name, "/"); !found || l != label {
			continue
		}
		if eerr := w.endDelegate(&d, endRevoke); eerr != nil {
			return nil, eerr
		}
		handles = append(handles, d.Name)
	}
	return handles, nil
}

// revokeLink deletes me's unused link of label, reporting whether it had
// one.
func (w *wtx) revokeLink(me *identityRow, label string) (bool, error) {
	links, err := w.unexpiredLinks(me.ID)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(links, func(l linkRow) bool { return l.Label == label })
	if i < 0 {
		return false, nil
	}
	if _, err := w.s.t.links.table.Delete(w.tx, pairKey(me.ID, links[i].ID)); err != nil {
		return false, err
	}
	w.s.log.Info("delegate link revoked", "name", me.Name, "label", label)
	return true, nil
}

// Bind records agent, the host's subagent id a hook saw register c's
// delegate, on that delegate once, so SubagentStop and the subagent's own
// hooks find it by that id. Refusals: KindInvalid for a caller that names
// no handle or a bad id; KindConflict for a delegate already bound or an
// id already bound to another delegate of the parent; and those of acting
// as the handle.
func (s *Store) Bind(ctx context.Context, c Caller, agent string) error {
	if c.As == "" {
		return refuse(KindInvalid, "a binding names its delegate: send it as <handle>")
	}
	if agent == "" || len(agent) > maxAgentID || !utf8.ValidString(agent) || strings.ContainsFunc(agent, isControl) {
		return refuse(KindInvalid, fmt.Sprintf("the subagent id must be 1 to %d bytes of text, on one line", maxAgentID))
	}
	return s.update(ctx, func(w *wtx) error {
		d, err := w.liveAs(c)
		if err != nil {
			return err
		}
		if d.Agent != "" {
			return refuse(KindConflict, fmt.Sprintf("@%s is already bound to a subagent", d.Name))
		}
		if _, _, held, err := s.t.byAgent.Lookup(w.r, agentKey(d.Parent, agent)); err != nil || held {
			return orConflict(err, fmt.Sprintf("subagent %s is already bound to another delegate", agent))
		}
		d.Agent = agent
		if err := s.t.identities.table.Put(w.tx, d.ID, &d); err != nil {
			return err
		}
		s.log.Info("delegate bound", "handle", d.Name, "id", d.ID)
		return nil
	})
}

// SubagentStop ends the delegate the SubagentStop report from session
// names: the session's own identity when that is a delegate, else the
// delegate of the session's identity bound to agent. It sits outside the
// delegate gate and changes nothing for anything else, an unbound id
// included, since hosts report their internal agents' stops too.
func (s *Store) SubagentStop(ctx context.Context, session, agent string) error {
	return s.update(ctx, func(w *wtx) error {
		own, found, err := w.identityBy(s.t.bySession, session)
		if err != nil || !found || own.State != stateLive {
			return err
		}
		if own.Parent != 0 {
			return w.endDelegate(&own, endSubagent)
		}
		if agent == "" {
			return nil
		}
		id, _, found, err := s.t.byAgent.Lookup(w.r, agentKey(own.ID, agent))
		if err != nil || !found {
			return err
		}
		d, found, err := s.t.identities.settle(w, id)
		if err != nil || !found {
			return err
		}
		return w.endDelegate(&d, endSubagent)
	})
}
