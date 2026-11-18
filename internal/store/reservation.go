//go:build !windows

package store

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/cplieger/bolttable"
	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/wire"
)

const maxReason = 280

// Reserving is a reserve request as Caller's identity. With Queue a
// conflicting request joins the queue instead of being refused.
type Reserving struct {
	Path   string
	Reason string
	Caller Caller
	TTL    time.Duration
	Queue  bool
}

// Reservation is a grant or a queue entry as a caller sees it. A queued
// one has no Expires: its TTL counts from its grant.
type Reservation struct {
	Expires time.Time
	// Since is when it was queued or, once held, granted.
	Since  time.Time
	Path   string
	Reason string
	State  string
	Holder string
	Host   string
	// Relation is the holder's relation to the caller at Path.
	Relation string
	ID       uint64
	TTL      time.Duration
}

// Reserved is what a reserve answers. Behind is the held grant or the
// queue head a queued request waits behind.
type Reserved struct {
	Behind      *Reservation
	Reservation Reservation
}

// ReservationQuery filters Reservations. Path keeps the rows at, above or
// under it; Mine keeps the caller's own. After and Limit page the list:
// the ids past After, at most Limit of them (every one at 0), read as
// ReadAs (resumes).
type ReservationQuery struct {
	ReadAs *uint64
	Path   string
	After  uint64
	Limit  int
	Mine   bool
}

// Reserve grants r.Path to r.Caller's identity for r.TTL, or queues the
// request when r.Queue is set and it conflicts: with another identity's
// overlapping grant (its own never conflict), or with any overlapping
// queued request, its own included, so a newcomer never takes a subpath
// while an older request waits for the parent. Refusals: KindInvalid for
// a bad path, reason or ttl; KindGone with no live identity; KindForbidden
// for a delegate; KindConflict naming the holder or queue head; KindFull
// past 90 % of Options.MaxBytes.
func (s *Store) Reserve(ctx context.Context, k Keyed, r *Reserving) (Reserved, bool, error) {
	if err := checkReserving(r); err != nil {
		return Reserved{}, false, err
	}
	if _, err := s.resvPolicy.grant(s.now(), r.TTL, time.Time{}); err != nil {
		return Reserved{}, false, err
	}
	return keyed(ctx, s, k, func(w *wtx) (Reserved, error) { return w.reserve(r) })
}

func checkReserving(r *Reserving) error {
	if _, err := checkPaths([]string{r.Path}); err != nil {
		return err
	}
	return fieldRefusal(textFault("reason", r.Reason, maxReason, true),
		fmt.Sprintf("a reason is required: pass --reason <text>, 1 to %d characters on one line", maxReason))
}

// reserve answers a conflict as a refusal the transaction still commits:
// the 409 names the holder the eager expiry left, so that hand-off must
// stand.
func (w *wtx) reserve(r *Reserving) (Reserved, error) {
	if err := w.settleReservations(r.Path); err != nil {
		return Reserved{}, err
	}
	me, err := w.liveAs(r.Caller)
	if err != nil {
		return Reserved{}, err
	}
	if me.Parent != 0 {
		return Reserved{}, refuse(KindForbidden, "a delegate cannot reserve or queue: ask your parent to reserve the path")
	}
	w.touch(&me)
	if perr := w.putIdentity(me.ID, &me); perr != nil {
		return Reserved{}, perr
	}
	blocker, err := w.blocker(&reservationRow{Holder: me.ID, Path: r.Path, ID: math.MaxUint64})
	if err != nil {
		return Reserved{}, err
	}
	if blocker != nil && !r.Queue {
		refusal, cerr := w.conflict(&me, r.Path, blocker)
		if cerr == nil {
			w.refuseCommitting(refusal)
		}
		return Reserved{}, cerr
	}
	row, err := w.addReservation(&me, r, blocker == nil)
	if err != nil {
		return Reserved{}, err
	}
	out := Reserved{Reservation: w.s.reservationView(w.r, &me, &row)}
	if blocker != nil {
		b := w.s.reservationView(w.r, &me, blocker)
		out.Behind = &b
	}
	return out, nil
}

// addReservation stores r as me's grant when held, else as its queued
// request.
func (w *wtx) addReservation(me *identityRow, r *Reserving, held bool) (reservationRow, error) {
	if err := w.grow(); err != nil {
		return reservationRow{}, err
	}
	id, err := w.s.t.resv.table.NextID(w.tx)
	if err != nil {
		return reservationRow{}, err
	}
	row := reservationRow{ID: id, Holder: me.ID, Path: r.Path, Reason: r.Reason, TTL: r.TTL, Since: w.now, State: reservationQueued}
	verb := "reservation queued"
	if held {
		row.State, verb = reservationHeld, "reservation granted"
		if row.Lease, err = w.s.resvPolicy.grant(w.now, r.TTL, time.Time{}); err != nil {
			return reservationRow{}, err
		}
	}
	if err := w.putReservation(id, &row); err != nil {
		return reservationRow{}, err
	}
	w.logInfo(verb, "id", id, "name", me.Name, "path", r.Path)
	return row, nil
}

// blocker is the row that holds q back, or nil: another identity's grant
// overlapping q first, else the oldest request queued before q that
// overlaps it, whoever queued it.
func (w *wtx) blocker(q *reservationRow) (*reservationRow, error) {
	rows, err := w.reservationsOverlapping(q.Path)
	if err != nil {
		return nil, err
	}
	var head *reservationRow
	for i := range rows {
		o := &rows[i]
		switch {
		case o.State == reservationHeld && o.Holder != q.Holder:
			return o, nil
		case o.State == reservationQueued && o.ID < q.ID && (head == nil || o.ID < head.ID):
			head = o
		}
	}
	return head, nil
}

// conflict is the 409 naming what holds path back: the holder and its
// expiry, or the queue head, whose wait ends with its identity's lease.
func (w *wtx) conflict(me *identityRow, path string, b *reservationRow) (*Error, error) {
	h, _, err := w.s.t.identities.table.Get(w.r, b.Holder)
	if err != nil {
		return nil, err
	}
	rel := party{where: me.Where, id: me.ID}.relationTo(party{where: h.Where, id: h.ID}, b.Path)
	subject := path + " overlaps " + b.Path + ","
	if path == b.Path {
		subject = path + " is"
	}
	if b.State == reservationHeld {
		return &Error{Kind: KindConflict, Why: "reserved", Msg: fmt.Sprintf(
			"%s reserved by @%s (%s · %s) until %s for %q: add --queue to be granted it when it frees",
			subject, h.Name, h.Host, rel, fmtTime(b.Lease.Expires), b.Reason,
		)}, nil
	}
	return &Error{Kind: KindConflict, Why: "queued", Msg: fmt.Sprintf(
		"%s queued first by @%s (%s · %s) since %s for %q, waiting until %s at the latest: add --queue to wait behind it",
		subject, h.Name, h.Host, rel, fmtTime(b.Since), b.Reason, fmtTime(h.Lease.Expires),
	)}, nil
}

// RenewReservation resets c's grant id to expire ttl from now,
// re-arming its warning. Refusals: KindInvalid for a bad ttl or a queued
// entry; KindGone for a session with no live identity; KindNotFound for an
// id that is not one of its rows, an expired or released one included.
func (s *Store) RenewReservation(ctx context.Context, k Keyed, c Caller, id uint64, ttl time.Duration) (Reservation, error) {
	return keyedResult(ctx, s, k, func(w *wtx) (Reservation, error) {
		me, row, err := w.ownReservation(c, id)
		if err != nil {
			return Reservation{}, err
		}
		if row.State != reservationHeld {
			return Reservation{}, refuse(KindInvalid, fmt.Sprintf("reservation %d is queued: its ttl counts from its grant", id))
		}
		if row.Lease, err = s.resvPolicy.grant(w.now, ttl, time.Time{}); err != nil {
			return Reservation{}, err
		}
		row.TTL = ttl
		if err := w.putReservation(id, &row); err != nil {
			return Reservation{}, err
		}
		w.logInfo("reservation renewed", "id", id, "name", me.Name, "path", row.Path)
		return s.reservationView(w.r, &me, &row), nil
	})
}

// Release frees c's grant id or cancels its queue entry id, and
// hands the freed paths to the queue in the same transaction. Refusals:
// KindGone for a session with no live identity; KindNotFound for an id
// that is not one of its rows, an expired or released one included.
func (s *Store) Release(ctx context.Context, k Keyed, c Caller, id uint64) (Reservation, error) {
	return keyedResult(ctx, s, k, func(w *wtx) (Reservation, error) {
		me, row, err := w.ownReservation(c, id)
		if err != nil {
			return Reservation{}, err
		}
		verb := "reservation released"
		if row.State == reservationQueued {
			verb = "reservation cancelled"
		}
		w.logInfo(verb, "id", id, "name", me.Name, "path", row.Path)
		out := s.reservationView(w.r, &me, &row)
		return out, w.freeAndHandOff([]reservationRow{row})
	})
}

// ownReservation settles row id and returns it with c's live identity,
// whose request it stores; a row that is not c's is KindNotFound.
func (w *wtx) ownReservation(c Caller, id uint64) (identityRow, reservationRow, error) {
	row, found, err := w.s.t.resv.settle(w, id)
	if err != nil {
		return identityRow{}, reservationRow{}, err
	}
	me, err := w.liveAs(c)
	if err != nil {
		return identityRow{}, reservationRow{}, err
	}
	if !found || row.Holder != me.ID {
		return identityRow{}, reservationRow{}, notFound(wire.ReasonReservation, fmt.Sprintf(
			"no reservation %d of yours: it expired or was released; grelot reservations --mine lists yours", id,
		))
	}
	w.touch(&me)
	err = w.putIdentity(me.ID, &me)
	return me, row, err
}

// Reservations lists the unexpired grants and the queue entries, in id
// order, as c's identity sees them; a session with no live
// identity sees every relation as unknown and reads as 0. Refusals:
// KindInvalid for a bad path; ErrCursorMismatch.
func (s *Store) Reservations(ctx context.Context, c Caller, q ReservationQuery) (Listing[Reservation], error) {
	if q.Path != "" {
		if _, err := checkPaths([]string{q.Path}); err != nil {
			return Listing[Reservation]{}, err
		}
	}
	if _, err := s.Seen(ctx, c); err != nil && !isKind(err, KindGone) {
		return Listing[Reservation]{}, err
	}
	var out []Reservation
	var readAs uint64
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		me, err := s.pagedCaller(r, c, now, q.ReadAs)
		if err != nil {
			return err
		}
		readAs = me.ID
		if q.Mine && me.ID == 0 {
			return nil
		}
		return s.scanReservations(r, q, me.ID, func(v *reservationRow) {
			if !v.Lease.due(now) {
				out = append(out, s.reservationView(r, &me, v))
			}
		})
	})
	l := listing(out, pastKey(q.After, func(v *Reservation) uint64 { return v.ID }), q.Limit)
	l.ReadAs = readAs
	return l, err
}

// Holding is one grant another agent holds over a queried path, overlapping
// it either way: covering it or lying under it. Queued is the caller's own
// oldest queue entry overlapping both the grant and a queried path the grant
// overlaps, nil for none.
type Holding struct {
	Queued *Reservation
	Held   Reservation
}

// Holders lists, in grant id order, each unexpired grant overlapping any of
// paths whose holder is neither c's identity nor its parent, once. It reads
// only: no last-seen touch and no expiry sweep. Refusals: KindInvalid for no
// path, a bad one or more than protopath.MaxHolderPaths distinct ones;
// KindGone with no live identity.
func (s *Store) Holders(ctx context.Context, c Caller, paths []string) ([]Holding, error) {
	ps, err := checkHolderPaths(paths)
	if err != nil {
		return nil, err
	}
	var out []Holding
	err = s.read(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		me, found, cerr := s.callerFor(r, c, now)
		switch {
		case cerr != nil:
			return cerr
		case !found:
			return noIdentity()
		case me.State == stateDormant:
			return s.dormantIdentity(r, &me, now)
		}
		hs := holdings{held: map[uint64]reservationRow{}, queued: map[uint64]reservationRow{}}
		for _, p := range ps {
			if herr := s.holdersOf(r, &me, p, now, &hs); herr != nil {
				return herr
			}
		}
		for _, id := range slices.Sorted(maps.Keys(hs.held)) {
			g := hs.held[id]
			h := Holding{Held: s.reservationView(r, &me, &g)}
			if q, ok := hs.queued[id]; ok {
				v := s.reservationView(r, &me, &q)
				h.Queued = &v
			}
			out = append(out, h)
		}
		return nil
	})
	return out, err
}

// checkHolderPaths is paths with repeats dropped. It refuses at the first
// distinct path past the cap, so a longer query costs no more to refuse.
func checkHolderPaths(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		if _, err := checkPaths([]string{p}); err != nil {
			return nil, err
		}
		if slices.Contains(out, p) {
			continue
		}
		if out = append(out, p); len(out) > protopath.MaxHolderPaths {
			return nil, holderPathsRefusal()
		}
	}
	if len(out) == 0 {
		return nil, holderPathsRefusal()
	}
	return out, nil
}

func holderPathsRefusal() *Error {
	return refuse(KindInvalid, fmt.Sprintf("ask for 1 to %d distinct paths", protopath.MaxHolderPaths))
}

// holdings is Holders' answer as it is gathered, by grant id: the grant and
// the caller's oldest queue entry on it.
type holdings struct {
	held   map[uint64]reservationRow
	queued map[uint64]reservationRow
}

// holdersOf adds to hs the grants overlapping p that Holders lists, each
// with the caller's queue entries overlapping both p and the grant.
func (s *Store) holdersOf(r *bolttable.ReadTx, me *identityRow, p string, now time.Time, hs *holdings) error {
	ids, err := overlapIDs(r, s.t.resvPath, p)
	if err != nil {
		return err
	}
	var held, queued []reservationRow
	for _, id := range ids {
		v, found, err := s.t.resv.table.Get(r, id)
		switch {
		case err != nil:
			return err
		case !found:
		case v.State == reservationQueued && v.Holder == me.ID:
			queued = append(queued, v)
		case v.State == reservationHeld && !v.Lease.due(now) && v.Holder != me.ID && v.Holder != me.Parent:
			held = append(held, v)
		}
	}
	for i := range held {
		hs.add(&held[i], queued)
	}
	return nil
}

// add records the grant g with the oldest of queued overlapping it.
func (hs *holdings) add(g *reservationRow, queued []reservationRow) {
	hs.held[g.ID] = *g
	for i := range queued {
		q := &queued[i]
		// Ids grow with each request, so the lowest is the oldest.
		if cur, ok := hs.queued[g.ID]; overlaps(q.Path, g.Path) && (!ok || q.ID < cur.ID) {
			hs.queued[g.ID] = *q
		}
	}
}

func (s *Store) scanReservations(r *bolttable.ReadTx, q ReservationQuery, holder uint64, fn func(*reservationRow)) error {
	visit := func(_ []byte, id uint64, _ []byte) (bool, error) {
		v, found, err := s.t.resv.table.Get(r, id)
		if err == nil && found && (q.Path == "" || overlaps(v.Path, q.Path)) {
			fn(&v)
		}
		return err == nil, err
	}
	if q.Mine {
		return s.t.resvHolder.Scan(r, bolttable.Range{Prefix: be64(holder)}, visit)
	}
	return s.t.resv.table.Scan(r, bolttable.Range{}, func(id uint64, _ *reservationRow) (bool, error) {
		return visit(nil, id, nil)
	})
}

// reservationView is v as me sees it; a zero me reads every relation as
// unknown.
func (s *Store) reservationView(r *bolttable.ReadTx, me *identityRow, v *reservationRow) Reservation {
	out := Reservation{
		Expires: v.Lease.Expires, Since: v.Since, Path: v.Path, Reason: v.Reason,
		State: v.State, ID: v.ID, TTL: v.TTL,
	}
	h, found, err := s.t.identities.table.Get(r, v.Holder)
	if err != nil || !found {
		out.Relation = relationUnknown
		return out
	}
	out.Holder, out.Host = h.Name, h.Host
	out.Relation = party{where: me.Where, id: me.ID}.relationTo(party{where: h.Where, id: h.ID}, v.Path)
	return out
}

// freeAndHandOff is the one way a grant frees or a queue entry ends. It
// deletes every row in rows first, then walks the queue once, in order,
// granting each request that overlaps no other identity's grant and no
// older request still queued. Deleting the whole set before the walk is
// what keeps a retiring identity's own queue entries from being granted
// back to it.
func (w *wtx) freeAndHandOff(rows []reservationRow) error {
	for i := range rows {
		if _, err := w.s.t.resv.table.Delete(w.tx, rows[i].ID); err != nil {
			return err
		}
		w.changed.Agents = true
	}
	queue, err := w.queued()
	if err != nil {
		return err
	}
	for i := range queue {
		q := &queue[i]
		ok, err := w.grantable(q)
		if err != nil {
			return err
		}
		if ok {
			if err := w.handOver(q); err != nil {
				return err
			}
		}
	}
	return nil
}

// freeHolder passes every grant and queue entry of identity holder to one
// freeAndHandOff, for its retirement or expiry.
func (w *wtx) freeHolder(holder uint64) error {
	var rows []reservationRow
	err := w.s.t.resvHolder.Scan(w.r, bolttable.Range{Prefix: be64(holder)}, func(_ []byte, id uint64, _ []byte) (bool, error) {
		v, found, err := w.s.t.resv.table.Get(w.r, id)
		if found {
			rows = append(rows, v)
		}
		return err == nil, err
	})
	if err != nil || len(rows) == 0 {
		return err
	}
	w.logInfo("reservations freed", "holder", holder, "count", len(rows))
	return w.freeAndHandOff(rows)
}

// grantable reports whether queue entry q may be granted now: nothing
// holds it back, and its holder is live. A holder past its lease is owed
// its own expiry, which ends q; until then q keeps its place.
func (w *wtx) grantable(q *reservationRow) (bool, error) {
	b, err := w.blocker(q)
	if err != nil || b != nil {
		return false, err
	}
	h, found, err := w.s.t.identities.table.Get(w.r, q.Holder)
	return found && h.State == stateLive && !h.Lease.due(w.now), err
}

// handOver grants queue entry q from now for its stored duration, capped
// at the store's current RESERVATION_MAX_TTL, and owes its holder the news.
func (w *wtx) handOver(q *reservationRow) error {
	ls, err := w.s.resvPolicy.grant(w.now, min(q.TTL, w.s.resvPolicy.max), time.Time{})
	if err != nil {
		return err
	}
	q.State, q.Since, q.Lease = reservationHeld, w.now, ls
	if err := w.putReservation(q.ID, q); err != nil {
		return err
	}
	w.logInfo("reservation handed over", "id", q.ID, "holder", q.Holder, "path", q.Path)
	w.oweEvent(q.Holder, kindReservationGranted, fmt.Sprintf(
		"you now hold reservation %d on %s until %s (%s from its grant, as you queued it) for %q: run `grelot release %d` when you are done",
		q.ID, q.Path, fmtTime(q.Lease.Expires), fmtDur(q.Lease.Expires.Sub(w.now)), q.Reason, q.ID,
	))
	return nil
}

// queued lists the queue entries in enqueue order.
func (w *wtx) queued() ([]reservationRow, error) {
	var out []reservationRow
	err := w.s.t.resvQueue.Scan(w.r, bolttable.Range{}, func(_ []byte, id uint64, _ []byte) (bool, error) {
		v, found, err := w.s.t.resv.table.Get(w.r, id)
		if found {
			out = append(out, v)
		}
		return err == nil, err
	})
	return out, err
}

// reservationsOverlapping reads the rows at, above or under p.
func (w *wtx) reservationsOverlapping(p string) ([]reservationRow, error) {
	ids, err := overlapIDs(w.r, w.s.t.resvPath, p)
	if err != nil {
		return nil, err
	}
	out := make([]reservationRow, 0, len(ids))
	for _, id := range ids {
		v, found, err := w.s.t.resv.table.Get(w.r, id)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, v)
		}
	}
	return out, nil
}

// settleReservations runs the due expiry of every grant overlapping p, so
// a write touching p finds the queue already served.
func (w *wtx) settleReservations(p string) error {
	ids, err := overlapIDs(w.r, w.s.t.resvPath, p)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, _, err := w.s.t.resv.settle(w, id); err != nil {
			return err
		}
	}
	return nil
}

// heldOverlapping settles and returns the grants at, above or under p.
func (w *wtx) heldOverlapping(p string) ([]reservationRow, error) {
	if err := w.settleReservations(p); err != nil {
		return nil, err
	}
	rows, err := w.reservationsOverlapping(p)
	return slices.DeleteFunc(rows, func(r reservationRow) bool { return r.State != reservationHeld }), err
}

// warnReservation owes reservation_expiring once, ahead of the expiry by
// the policy's lead.
func warnReservation(w *wtx, _ uint64, v *reservationRow) error {
	left := int(math.Ceil(v.Lease.Expires.Sub(w.now).Minutes()))
	w.oweEvent(v.Holder, kindReservationExpiring, fmt.Sprintf(
		"your reservation %d on %s expires in %d min (at %s); if you still need it, run `grelot renew %d --ttl=<d>`, otherwise `grelot release %d`",
		v.ID, v.Path, left, fmtTime(v.Lease.Expires), v.ID, v.ID,
	))
	return nil
}

// expireReservation tells the holder its grant expired and hands the path
// to the queue. A row still owed its warning reached its expiry with no
// sweep in between, which only a server that was down leaves behind.
func expireReservation(w *wtx, _ uint64, v *reservationRow) error {
	text := fmt.Sprintf("your reservation %d on %s expired at %s and is free now; reserve it again if you still need it",
		v.ID, v.Path, fmtTime(v.Lease.Expires))
	if !v.Lease.WarnAt.IsZero() {
		text = fmt.Sprintf("your reservation %d on %s expired at %s while the server was down, so no warning went out first; it is free now, reserve it again if you still need it",
			v.ID, v.Path, fmtTime(v.Lease.Expires))
	}
	w.logInfo("reservation expired", "id", v.ID, "holder", v.Holder, "path", v.Path)
	w.oweEvent(v.Holder, kindReservationExpired, text)
	return w.freeAndHandOff([]reservationRow{*v})
}

// overlaps reports whether either path contains the other.
func overlaps(a, b string) bool { return under(a, b) || under(b, a) }
