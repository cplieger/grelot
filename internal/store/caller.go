package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/cplieger/bolttable"
)

// Caller is who an identity request acts as: the identity bound to
// Session or, with As (the Grelot-As header), the delegate of that handle,
// which only its parent's session or its own may act as. Agent, a host's
// subagent id a hook names, acts as the delegate of Session's identity
// bound to it. Self is the Grelot-Self header, which an agent sends to act
// as its own identity past the delegate gate.
type Caller struct {
	Session string
	As      string
	Agent   string
	Self    bool
}

// callerRow reads the identity c acts as for a write, running its due
// expiry and its parent's first. found is false only for a session with no
// identity; a handle that names no delegate any more is KindGone, and so
// is a delegate bound to the session whose parent has gone.
func (w *wtx) callerRow(c Caller) (identityRow, bool, error) {
	id, found, err := w.s.callerID(w.r, c)
	if err != nil || !found {
		return identityRow{}, false, orEnded(err, c)
	}
	d, found, err := w.s.t.identities.table.Get(w.r, id)
	switch {
	case err != nil || !found:
		return identityRow{}, false, orEnded(err, c)
	case d.Parent == 0 && c.As != "":
		return identityRow{}, false, notHandle(c.As)
	case d.Parent == 0:
		return w.ownRow(id, c)
	}
	// The parent's expiry ends the delegate, so it is settled first.
	parent, pfound, err := w.s.t.identities.settle(w, d.Parent)
	if err != nil {
		return identityRow{}, false, err
	}
	if !pfound || parent.State != stateLive {
		return identityRow{}, false, delegateEnded(d.Name)
	}
	d, found, err = w.s.t.identities.settle(w, id)
	if err != nil || !found {
		return identityRow{}, false, orEnded(err, c)
	}
	err = asAllowed(c, &d, &parent)
	return d, true, err
}

// ownRow reads the identity a session acts as for itself, past the gate.
func (w *wtx) ownRow(id uint64, c Caller) (identityRow, bool, error) {
	row, found, err := w.s.t.identities.settle(w, id)
	if err == nil && found && row.State == stateLive && !c.Self {
		err = w.s.gateRefusal(w.r, &row, w.now)
	}
	if err != nil {
		return identityRow{}, false, err
	}
	return row, found, nil
}

// callerFor is callerRow for a read, which sees due rows as the sweep will
// leave them. A delegate's row comes back with its parent's where, which
// reads judge it by; no write may store that copy.
func (s *Store) callerFor(r *bolttable.ReadTx, c Caller, now time.Time) (identityRow, bool, error) {
	id, found, err := s.callerID(r, c)
	if err != nil || !found {
		return identityRow{}, false, orEnded(err, c)
	}
	row, found, err := s.t.identities.table.Get(r, id)
	if err != nil || !found {
		return identityRow{}, false, orEnded(err, c)
	}
	v := settled(&row, now, s.dormantTTL)
	switch {
	case v == nil:
		return identityRow{}, false, orEnded(nil, c)
	case v.Parent == 0 && c.As != "":
		return identityRow{}, false, notHandle(c.As)
	case v.Parent == 0 && v.State == stateLive && !c.Self:
		if gerr := s.gateRefusal(r, v, now); gerr != nil {
			return identityRow{}, false, gerr
		}
		return *v, true, nil
	case v.Parent == 0:
		return *v, true, nil
	}
	return s.delegateFor(r, c, v, now)
}

// delegateFor is callerFor's answer for the delegate d, which acts only
// while its parent is live.
func (s *Store) delegateFor(r *bolttable.ReadTx, c Caller, v *identityRow, now time.Time) (identityRow, bool, error) {
	d := *v
	parent, found, err := s.t.identities.table.Get(r, d.Parent)
	if err != nil {
		return identityRow{}, false, err
	}
	if p := settled(&parent, now, s.dormantTTL); !found || p == nil || p.State != stateLive {
		return identityRow{}, false, delegateEnded(d.Name)
	}
	if err := asAllowed(c, &d, &parent); err != nil {
		return identityRow{}, false, err
	}
	d.Where = parent.Where
	return d, true, nil
}

// callerID is the id of the row c names: its session's identity, the
// delegate of its handle, or the delegate its session's identity bound to
// its agent id.
func (s *Store) callerID(r *bolttable.ReadTx, c Caller) (id uint64, found bool, err error) {
	if cerr := checkCaller(c); cerr != nil {
		return 0, false, cerr
	}
	if c.As != "" {
		id, _, found, err = s.t.byName.Lookup(r, []byte(c.As))
		return id, found, err
	}
	id, _, found, err = s.t.bySession.Lookup(r, []byte(c.Session))
	if err != nil || !found || c.Agent == "" {
		return id, found, err
	}
	id, _, found, err = s.t.byAgent.Lookup(r, agentKey(id, c.Agent))
	return id, found, err
}

// liveAs is the live identity a write acts as for c.
func (w *wtx) liveAs(c Caller) (identityRow, error) {
	row, found, err := w.callerRow(c)
	switch {
	case err != nil:
		return identityRow{}, err
	case !found:
		return identityRow{}, noIdentity()
	case row.State == stateDormant:
		return identityRow{}, w.s.dormantIdentity(w.r, &row, w.now)
	}
	return row, nil
}

// liveFor is liveAs for a read.
func (s *Store) liveFor(r *bolttable.ReadTx, c Caller, now time.Time) (identityRow, error) {
	row, found, err := s.callerFor(r, c, now)
	switch {
	case err != nil:
		return identityRow{}, err
	case !found:
		return identityRow{}, noIdentity()
	case row.State == stateDormant:
		return identityRow{}, s.dormantIdentity(r, &row, now)
	}
	return row, nil
}

func checkCaller(c Caller) error {
	switch {
	case c.Agent != "" && (c.As != "" || c.Self):
		return refuse(KindInvalid, "a subagent id acts as the delegate bound to it: send it without --self or as <handle>")
	case c.As == "":
		return nil
	case c.Self:
		return refuse(KindInvalid, "--self acts as this session's own identity and as <handle> as a delegate: pass one of them")
	case !isHandle(c.As):
		return notHandle(c.As)
	}
	return nil
}

// isHandle reports whether h is a delegate handle: two names joined by
// one slash, each under the name grammar.
func isHandle(h string) bool {
	parent, label, ok := strings.Cut(h, "/")
	return ok && namePattern.MatchString(parent) && namePattern.MatchString(label)
}

func notHandle(h string) *Error {
	return refuse(KindInvalid, fmt.Sprintf("%q is not a delegate handle: grelot as takes <parent>/<label>", h))
}

// asAllowed accepts a handle only from its parent's session or the
// delegate's own, so another session cannot act as someone's delegate. A
// session the delegate is bound to needs no handle.
func asAllowed(c Caller, d, parent *identityRow) error {
	if c.As == "" || (d.Session != "" && c.Session == d.Session) || (parent.Session != "" && c.Session == parent.Session) {
		return nil
	}
	return refuse(KindForbidden, fmt.Sprintf("@%s acts only from its parent's session or its own, and this session is neither", d.Name))
}

// orEnded is err, else nothing for a session with no identity and the
// delegate-ended refusal for a handle or an agent id.
func orEnded(err error, c Caller) error {
	switch {
	case err != nil:
		return err
	case c.As != "":
		return delegateEnded(c.As)
	case c.Agent != "":
		return &Error{Kind: KindGone, Why: "delegate_ended", Msg: "no delegate is bound to this subagent; ask your parent for a new link"}
	}
	return nil
}

func delegateEnded(handle string) *Error {
	return &Error{Kind: KindGone, Why: "delegate_ended", Msg: fmt.Sprintf("delegate %s ended; ask your parent for a new link", handle)}
}
