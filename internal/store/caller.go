package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/cplieger/bolttable"
)

// Caller is who an identity request acts as: the identity bound to
// Session or, with As (the Grelot-As header), the delegate of that handle,
// which only its parent's session or its own may act as. Self is the
// Grelot-Self header, which an agent sends to act as its own identity.
type Caller struct {
	Session string
	As      string
	Self    bool
}

// callerRow reads the identity c acts as for a write, running its due
// expiry and its parent's first. found is false only for a session with no
// identity; a handle that names no delegate any more is KindGone, and so
// is a delegate bound to the session whose parent has gone.
func (w *wtx) callerRow(c Caller) (identityRow, bool, error) {
	ix, key, err := callerKey(w.s, c)
	if err != nil {
		return identityRow{}, false, err
	}
	id, _, found, err := ix.Lookup(w.r, []byte(key))
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
		return w.s.t.identities.settle(w, id)
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

// callerFor is callerRow for a read, which sees due rows as the sweep will
// leave them. A delegate's row comes back with its parent's where, which
// reads judge it by; no write may store that copy.
func (s *Store) callerFor(r *bolttable.ReadTx, c Caller, now time.Time) (identityRow, bool, error) {
	ix, key, err := callerKey(s, c)
	if err != nil {
		return identityRow{}, false, err
	}
	d, found, err := s.lookupIdentity(r, ix, key)
	switch {
	case err != nil || !found:
		return identityRow{}, false, orEnded(err, c)
	case d.Parent == 0 && c.As != "":
		return identityRow{}, false, notHandle(c.As)
	case d.Parent == 0:
		return d, true, nil
	}
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

// callerKey is the unique index and key c's identity is found under: its
// session, or with As the delegate's handle.
func callerKey(s *Store, c Caller) (*bolttable.Index[uint64, identityRow], string, error) {
	if c.As == "" {
		return s.t.bySession, c.Session, nil
	}
	return s.t.byName, c.As, checkAs(c)
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

func checkAs(c Caller) error {
	if c.Self {
		return refuse(KindInvalid, "--self acts as this session's own identity and as <handle> as a delegate: pass one of them")
	}
	if !isHandle(c.As) {
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
// delegate-ended refusal for a handle.
func orEnded(err error, c Caller) error {
	if err != nil || c.As == "" {
		return err
	}
	return delegateEnded(c.As)
}

func delegateEnded(handle string) *Error {
	return &Error{Kind: KindGone, Why: "delegate_ended", Msg: fmt.Sprintf("delegate %s ended; ask your parent for a new link", handle)}
}
