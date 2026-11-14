//go:build !windows

package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cplieger/bolttable"
)

// Caller is who an identity request acts as: the identity bound to
// Session or, with As (the Grelot-As header), the delegate of that handle,
// which only its parent's session or its own may act as. Self is the
// Grelot-Self header, which an agent sends to act as its own identity past
// the delegate gate. Key is the machine key the request came with: an
// identity acts only for the key that registered it. User acts as @user and
// takes neither Session nor As; only the web UI's signed-in handlers may
// set it.
type Caller struct {
	Session string
	As      string
	Key     int64
	Self    bool
	User    bool
}

// ErrOtherKey refuses a request naming an identity or session another
// machine's key holds.
var ErrOtherKey = refuse(KindForbidden, "this identity belongs to another machine's key: register this session under its own name")

// keyOwns refuses c acting on row when row is another live key's; @user
// is the web UI's and belongs to no key.
func (s *Store) keyOwns(c Caller, row *identityRow) error {
	if c.User {
		return nil
	}
	return s.bound(row.Key, c.Key)
}

// bound refuses key acting on a row held binds, unless held no longer
// authenticates: a row of a revoked, expired or deleted key belongs to no
// machine, so rotating a machine's key never strands its sessions. Key ids
// are never reused, so a dead key stays dead and the answer may come from
// outside the transaction.
func (s *Store) bound(held, key int64) error {
	if held == key {
		return nil
	}
	live, err := s.keyLive(held)
	switch {
	case err != nil:
		return err
	case live:
		return ErrOtherKey
	}
	return nil
}

// keyMayTake reports whether key may act on a row held binds: bound's
// answer, with another live key's refusal as false.
func (s *Store) keyMayTake(held, key int64) (bool, error) {
	err := s.bound(held, key)
	if errors.Is(err, ErrOtherKey) {
		return false, nil
	}
	return err == nil, err
}

// ownFor is the identity bound to session, refused when another live key
// holds it.
func (w *wtx) ownFor(session string, key int64) (identityRow, bool, error) {
	own, found, err := w.identityBy(w.s.t.bySession, session)
	if err == nil && found {
		err = w.s.bound(own.Key, key)
	}
	return own, found, err
}

// rebind binds row and its delegates to key, which bound admitted.
func (w *wtx) rebind(row *identityRow, key int64) error {
	if row.Key == key {
		return nil
	}
	row.Key = key
	if err := w.putIdentity(row.ID, row); err != nil {
		return err
	}
	ids, err := w.s.delegatesOf(w.r, row.ID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		d, found, err := w.s.t.identities.table.Get(w.r, id)
		if err != nil {
			return err
		}
		if !found || d.Key == key {
			continue
		}
		d.Key = key
		if err := w.putIdentity(id, &d); err != nil {
			return err
		}
	}
	return nil
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
	if err != nil || !found {
		return identityRow{}, false, orEnded(err, c)
	}
	if kerr := w.s.keyOwns(c, &d); kerr != nil {
		return identityRow{}, false, kerr
	}
	switch {
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
	if err == nil && found && row.State == stateLive && !c.Self && !c.User {
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
	if kerr := s.keyOwns(c, &row); kerr != nil {
		return identityRow{}, false, kerr
	}
	v := settled(&row, now, s.dormantTTL)
	switch {
	case v == nil:
		return identityRow{}, false, orEnded(nil, c)
	case v.Parent == 0 && c.As != "":
		return identityRow{}, false, notHandle(c.As)
	case v.Parent == 0 && v.State == stateLive && !c.Self && !c.User:
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

// callerID is the id of the row c names: its session's identity or the
// delegate of its handle.
func (s *Store) callerID(r *bolttable.ReadTx, c Caller) (id uint64, found bool, err error) {
	if cerr := checkCaller(c); cerr != nil {
		return 0, false, cerr
	}
	if c.As != "" || c.User {
		name := c.As
		if c.User {
			name = userName
		}
		id, _, found, err = s.t.byName.Lookup(r, []byte(name))
		return id, found, err
	}
	id, _, found, err = s.t.bySession.Lookup(r, []byte(c.Session))
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
	case c.User && (c.Session != "" || c.As != "" || c.Self):
		return refuse(KindInvalid, "a caller acting as @user names no session, handle or --self")
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
// delegate-ended refusal for a handle.
func orEnded(err error, c Caller) error {
	switch {
	case err != nil:
		return err
	case c.As != "":
		return delegateEnded(c.As)
	}
	return nil
}

func delegateEnded(handle string) *Error {
	return &Error{Kind: KindGone, Why: "delegate_ended", Msg: fmt.Sprintf("delegate @%s ended; ask your parent for a new link", handle)}
}
