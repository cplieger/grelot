//go:build !windows

package store

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cplieger/bolttable"
)

// NoChange is the literal an identity field takes on a post to keep its
// value; a post's Paths keeps the paths as the one element NoChange.
const NoChange = "no change"

const (
	maxPaths       = 16
	maxPathBytes   = 1024
	maxPathDepth   = 64
	maxDescription = 280
	maxLabel       = 128
	maxSessionID   = 256
	// endedLease is how long an identity outlives its session's SessionEnd
	// report: a closed tab's identity goes within the hour.
	endedLease = time.Hour
	// lastSeenEvery bounds last_seen writes. It is display only, so a
	// restart may lose up to this much of it.
	lastSeenEvery = 5 * time.Minute
)

var (
	namePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,47}$`)
	providerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
)

// Fields are the three identity fields an agent declares.
type Fields struct {
	Name        string
	Description string
	Paths       []string
}

// Identity is an identity as its own session sees it. It carries no
// machine, environment or checkout value, only whether each is known.
type Identity struct {
	Expires      time.Time
	RegisteredAt time.Time
	DormantSince time.Time
	LastSeen     time.Time
	Name         string
	Description  string
	State        string
	Host         string
	OS           string
	Arch         string
	Provider     string
	// MachineKind is the kind of the identity's machine value, "" when
	// unknown.
	MachineKind string
	// Notice is a line for the caller about this answer: a delegate's
	// renewal capped at its parent's expiry.
	Notice  string
	Paths   []string
	Follows []string
	ID      uint64
	// Read is the read cursor: the highest seq the identity has fetched.
	Read             uint64
	TTL              time.Duration
	EnvironmentKnown bool
	Ended            bool
	// Threaded is set once the identity has followed a path or posted in
	// a path thread, and stays set.
	Threaded bool
}

// Registration is a register request from Session, "<provider>:<id>".
// With Link it registers the delegate that link grants, and takes only
// Fields.Description. Self is the Grelot-Self header, which a session
// holding delegates needs to get its own identity back.
type Registration struct {
	Where    *Where
	Session  string
	Link     string
	Host     string
	OS       string
	Arch     string
	Proc     string
	Fields   Fields
	TTL      time.Duration
	TakeOver bool
	Self     bool
}

// Registered is what a register answers.
type Registered struct {
	Identity Identity
	// Existing reports a session that already had a live identity, returned
	// unchanged: a repeated register is not a renewal.
	Existing bool
	// Moved reports a take-over that moved a live identity to this session.
	Moved bool
}

// Change is a set or a renew. An empty field keeps its value; a TTL renews
// the lease. Where must come with a renewal and with new paths.
type Change struct {
	Where  *Where
	Fields Fields
	TTL    time.Duration
}

// Register creates session's identity, takes a live one over, or returns
// the session's live identity unchanged. Refusals: KindInvalid for a bad
// field, ttl or where; KindConflict for a held name or a refused take-over.
func (s *Store) Register(ctx context.Context, k Keyed, r *Registration) (Registered, bool, error) {
	provider, err := checkSession(r.Session)
	if err != nil {
		return Registered{}, false, err
	}
	return keyed(ctx, s, k, func(w *wtx) (Registered, error) {
		if r.Link != "" {
			return w.registerDelegate(r)
		}
		return w.register(r, provider)
	})
}

func (w *wtx) register(r *Registration, provider string) (Registered, error) {
	own, ownFound, err := w.identityBy(w.s.t.bySession, r.Session)
	if err != nil {
		return Registered{}, err
	}
	if ownFound && own.State == stateLive {
		return w.existing(&own, r.Self)
	}
	row, added, moved, err := w.registerRow(r, provider)
	if err != nil {
		return Registered{}, err
	}
	if ownFound && own.ID != row.ID {
		if err := w.detach(&own); err != nil {
			return Registered{}, err
		}
	}
	if err := w.s.t.identities.table.Put(w.tx, row.ID, &row); err != nil {
		return Registered{}, err
	}
	w.queueCards(kindJoin, &row, added)
	if moved {
		if err := w.capDelegates(&row); err != nil {
			return Registered{}, err
		}
		w.logInfo("identity moved", "name", row.Name, "id", row.ID, "provider", provider)
	}
	return Registered{Identity: w.s.view(&row), Moved: moved}, nil
}

// existing answers a register from the session of the live identity own,
// unchanged but for its request time, past the delegate gate.
func (w *wtx) existing(own *identityRow, self bool) (Registered, error) {
	if !self {
		if err := w.s.gateRefusal(w.r, own, w.now); err != nil {
			return Registered{}, err
		}
	}
	if w.seen(own) {
		if err := w.s.t.identities.table.Put(w.tx, own.ID, own); err != nil {
			return Registered{}, err
		}
	}
	v, err := w.viewOf(own)
	return Registered{Identity: v, Existing: true}, err
}

// detach unbinds a dormant row from its session, so another identity can
// bind to that session; the row stays revivable by name.
func (w *wtx) detach(row *identityRow) error {
	row.Session = ""
	return w.s.t.identities.table.Put(w.tx, row.ID, row)
}

// registerRow validates r and returns the row it registers, a new one or
// the live identity of r's name when a take-over may move it, and the
// paths it adds, each owed a join card: a take-over adds only paths the
// moved identity did not have.
func (w *wtx) registerRow(r *Registration, provider string) (row identityRow, added []string, moved bool, err error) {
	fc, err := resolveFields(nil, r.Fields, fieldsNew)
	if err == nil {
		err = checkLabels(r.Host, r.OS, r.Arch, r.Proc)
	}
	if err == nil {
		err = checkWhere(r.Where, fc.paths)
	}
	if err != nil {
		return identityRow{}, nil, false, err
	}
	ls, err := identityPolicy.grant(w.now, r.TTL, time.Time{})
	if err != nil {
		return identityRow{}, nil, false, err
	}
	row, held, err := w.identityBy(w.s.t.byName, fc.name)
	if err != nil {
		return identityRow{}, nil, false, err
	}
	switch {
	case held && row.State == stateDormant:
		return identityRow{}, nil, false, dormantNameRefusal(&row, *r.Where)
	case held && !r.TakeOver:
		return identityRow{}, nil, false, w.s.liveNameRefusal(&row, *r.Where)
	case held:
		if err := w.s.moveRefusal(verbTakeOver, &row, *r.Where, provider); err != nil {
			return identityRow{}, nil, false, err
		}
	default:
		id, err := w.s.t.identities.table.NextID(w.tx)
		if err != nil {
			return identityRow{}, nil, false, err
		}
		row = identityRow{ID: id, RegisteredAt: w.now, State: stateLive}
	}
	added = slices.DeleteFunc(slices.Clone(fc.paths), func(p string) bool { return slices.Contains(row.Paths, p) })
	row.Name, row.Paths, row.Description = fc.name, fc.paths, fc.description
	row.Host, row.OS, row.Arch, row.Proc = r.Host, r.OS, r.Arch, r.Proc
	row.Session, row.Provider = r.Session, provider
	row.Where = cloneWhere(*r.Where)
	row.TTL, row.Lease = r.TTL, ls
	w.touch(&row)
	if err := w.repoConflict(&row); err != nil {
		return identityRow{}, nil, false, err
	}
	return row, added, held, nil
}

// Set applies ch to c's live identity and returns it. Refusals: KindGone
// for a session with no live identity, KindInvalid for a bad change,
// KindConflict for a name another identity holds.
func (s *Store) Set(ctx context.Context, k Keyed, c Caller, ch Change) (Identity, error) {
	return keyedResult(ctx, s, k, func(w *wtx) (Identity, error) {
		row, err := w.liveAs(c)
		if err != nil {
			return Identity{}, err
		}
		notice, err := w.applyChange(&row, ch)
		if err != nil {
			return Identity{}, err
		}
		w.touch(&row)
		if perr := s.t.identities.table.Put(w.tx, row.ID, &row); perr != nil {
			return Identity{}, perr
		}
		out, err := w.viewOf(&row)
		out.Notice = notice
		return out, err
	})
}

// applyChange applies c to row and returns the notice a delegate's renewal
// capped at its parent's expiry owes.
func (w *wtx) applyChange(row *identityRow, c Change) (string, error) {
	if c.TTL == 0 && c.Where == nil && c.Fields.Name == "" && c.Fields.Description == "" && len(c.Fields.Paths) == 0 {
		return "", refuse(KindInvalid, "nothing to set: pass --name, --paths, --desc or --ttl")
	}
	if err := w.applyFields(row, c.Fields, c.Where, fieldsSome, c.TTL > 0); err != nil {
		return "", err
	}
	if c.TTL == 0 {
		return "", nil
	}
	var parent identityRow
	if row.Parent != 0 {
		var err error
		// callerRow settled the parent live before this runs.
		if parent, _, err = w.s.t.identities.table.Get(w.r, row.Parent); err != nil {
			return "", err
		}
	}
	ls, err := identityPolicy.grant(w.now, c.TTL, parent.Lease.Expires)
	if err != nil {
		return "", err
	}
	row.TTL, row.Lease = c.TTL, ls
	if row.Parent == 0 {
		return "", w.capDelegates(row)
	}
	if w.now.Add(c.TTL).After(ls.Expires) {
		return fmt.Sprintf("your lease stops at your parent's expiry, %s: @%s renews first", fmtTime(ls.Expires), parent.Name), nil
	}
	return "", nil
}

// applyFields applies a set's or a post's identity fields and where to
// row, owing a join card for each path it adds. Where is checked when it
// is sent, when the paths change, and on a renewal, which must send it. A
// delegate's where is its parent's, so one it sends is ignored.
func (w *wtx) applyFields(row *identityRow, f Fields, sent *Where, mode fieldMode, renew bool) error {
	if row.Parent != 0 {
		return applyDelegateFields(row, f, mode)
	}
	fc, err := resolveFields(row, f, mode)
	if err == nil && fc.name != row.Name {
		err = w.nameFree(fc.name)
	}
	if err == nil && (fc.pathsChanged || renew || sent != nil) {
		err = checkWhere(sent, fc.paths)
	}
	if err != nil {
		return err
	}
	applyWhere(row, sent, fc.pathsChanged, fc.paths)
	if sent != nil {
		if err := w.repoConflict(row); err != nil {
			return err
		}
	}
	row.Name, row.Paths, row.Description = fc.name, fc.paths, fc.description
	w.queueCards(kindJoin, row, fc.added)
	return nil
}

// applyWhere stores a set's or renew's where: new paths come with the
// checkouts of every path they name, so those replace the stored ones; a
// renew sends only the working directory's clone, so it is merged.
func applyWhere(row *identityRow, sent *Where, pathsChanged bool, paths []string) {
	switch {
	case pathsChanged:
		row.Where = cloneWhere(*sent)
	case sent != nil:
		row.Where = Where{
			Machine: sent.Machine, Environment: sent.Environment,
			Checkouts: mergeCheckouts(row.Where.Checkouts, sent.Checkouts, paths),
		}
	}
}

// nameFree refuses a rename to a name another identity, live or dormant,
// holds.
func (w *wtx) nameFree(name string) error {
	_, held, err := w.identityBy(w.s.t.byName, name)
	if err == nil && held {
		err = refuse(KindConflict, fmt.Sprintf("%s is held by another identity, live or dormant: choose another name", name))
	}
	return err
}

// EndSession records the SessionEnd report on session's own live identity:
// it is marked ended, its delegates end, and its expiry becomes
// min(current, now + 1 h), its warning moving with it. Nothing restores
// the lease but a renewal.
func (s *Store) EndSession(ctx context.Context, session string) error {
	return s.update(ctx, func(w *wtx) error {
		row, err := w.liveAs(Caller{Session: session, Self: true})
		if err != nil {
			return err
		}
		if err := w.endDelegates(row.ID, endSessionEnd); err != nil {
			return err
		}
		row.Ended = true
		if limit := w.now.Add(endedLease); row.Lease.Expires.After(limit) {
			row.Lease.Expires = limit
			row.Lease.WarnAt = later(w.now, limit.Add(-identityPolicy.lead(row.TTL)))
		}
		return s.t.identities.table.Put(w.tx, row.ID, &row)
	})
}

// Retire deletes c's live identity, owing a left card for each of its
// paths, frees its name, hands its reservations to their queues, and ends
// its delegates and unused links; a delegate's retirement ends only it.
// It ends the receipt watches it sent and leaves, counted retired, those
// waiting for it. Its delivery rows stay until their own expiry. It
// returns the retired identity's id. A session with no live identity is
// KindGone.
func (s *Store) Retire(ctx context.Context, k Keyed, c Caller) (id uint64, replayed bool, err error) {
	return keyed(ctx, s, k, func(w *wtx) (uint64, error) {
		row, err := w.liveAs(c)
		if err != nil {
			return 0, err
		}
		if row.Parent != 0 {
			return row.ID, w.endDelegate(&row, endBye)
		}
		if err := w.leaveWatches(&row); err != nil {
			return 0, err
		}
		if err := w.freeHolder(row.ID); err != nil {
			return 0, err
		}
		if err := w.endDelegates(row.ID, endParent); err != nil {
			return 0, err
		}
		w.queueCards(kindLeft, &row, row.Paths)
		// The row's delete cascades to its unused links.
		if err := deleteRow(w, s.t.identities, row.ID); err != nil {
			return 0, err
		}
		w.forgetSeen(row.ID)
		w.logInfo("identity retired", "name", row.Name, "id", row.ID, "provider", row.Provider)
		return row.ID, nil
	})
}

// Live returns, in the order given, the ids among ids whose identity is
// live; a dormant, retired or unknown id is left out.
func (s *Store) Live(ctx context.Context, ids []uint64) ([]uint64, error) {
	out := []uint64{}
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		for _, id := range ids {
			row, found, err := s.t.identities.table.Get(r, id)
			if err != nil {
				return err
			}
			if found && row.State == stateLive {
				out = append(out, id)
			}
		}
		return nil
	})
	return out, err
}

// Seen records a request as c other than the standing wait and
// returns its identity, live or dormant. A live one has ended cleared and
// last_seen refreshed, its row written only when ended was set or the
// stored last_seen is lastSeenEvery old; a dormant one is never written.
// A session with no identity is KindGone.
func (s *Store) Seen(ctx context.Context, c Caller) (Identity, error) {
	var row identityRow
	var found bool
	if err := s.read(ctx, func(r *bolttable.ReadTx) error {
		var err error
		row, found, err = s.callerFor(r, c, s.now())
		return err
	}); err != nil {
		return Identity{}, err
	}
	switch {
	case !found:
		return Identity{}, noIdentity()
	case row.State == stateDormant:
		return s.view(&row), nil
	}
	now := s.now()
	s.noteSeen(row.ID, now)
	if !seenDue(&row, now) {
		return s.view(&row), nil
	}
	var out Identity
	err := s.update(ctx, func(w *wtx) error {
		row, found, err := w.callerRow(c)
		if err != nil {
			return err
		}
		if !found {
			return noIdentity()
		}
		if row.State == stateLive && w.seen(&row) {
			if err := s.t.identities.table.Put(w.tx, row.ID, &row); err != nil {
				return err
			}
		}
		var verr error
		out, verr = w.viewOf(&row)
		return verr
	})
	return out, err
}

// Watcher returns the identity a standing wait from c holds for, and
// records the request time in memory only: a standing wait never clears
// ended and is no commit. Refusals: KindGone with Why dormant or none, as
// on every route; KindInvalid for a delegate, which is pull only.
func (s *Store) Watcher(ctx context.Context, c Caller) (Identity, error) {
	var row identityRow
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		var err error
		row, err = s.liveFor(r, c, s.now())
		return err
	})
	switch {
	case err != nil:
		return Identity{}, err
	case row.Parent != 0:
		return Identity{}, refuse(KindInvalid, "a delegate is pull only; use grelot wait")
	}
	s.noteSeen(row.ID, s.now())
	return s.view(&row), nil
}

// seen records a request from row's own session in memory and, when
// seenDue, clears ended and refreshes last_seen, reporting whether row
// must be written.
func (w *wtx) seen(row *identityRow) bool {
	w.s.noteSeen(row.ID, w.now)
	if !seenDue(row, w.now) {
		return false
	}
	w.touch(row)
	return true
}

// touch records a request from row's own session: it clears ended and
// stamps last_seen. Clearing ended wakes row, as a nudge the closed
// session held back may now be due.
func (w *wtx) touch(row *identityRow) {
	if row.Ended {
		w.wake(row.ID)
	}
	row.Ended, row.LastSeen = false, w.now
}

// seenDue reports whether a request at now must write row: ended is set or
// the stored last_seen is lastSeenEvery old.
func seenDue(row *identityRow, now time.Time) bool {
	return row.Ended || now.Sub(row.LastSeen) >= lastSeenEvery
}

// settled is v as a read sees it before the sweeper reaches it: a live row
// past its expiry reads dormant, a dormant one past its purge and a
// delegate past its expiry read nil.
func settled(v *identityRow, now time.Time, dormantTTL time.Duration) *identityRow {
	if !v.Lease.due(now) {
		return v
	}
	if v.State == stateDormant || v.Parent != 0 {
		return nil
	}
	d := dormantRow(v, dormantTTL)
	if d.Lease.due(now) {
		return nil
	}
	return &d
}

// identityBy reads the identity under a unique index key for a write,
// running its due expiry first.
func (w *wtx) identityBy(ix *bolttable.Index[uint64, identityRow], key string) (identityRow, bool, error) {
	id, _, found, err := ix.Lookup(w.r, []byte(key))
	if err != nil || !found {
		return identityRow{}, false, err
	}
	return w.s.t.identities.settle(w, id)
}

// lookupIdentity is identityBy for a read, which sees a due row as the
// sweep will leave it.
func (s *Store) lookupIdentity(r *bolttable.ReadTx, ix *bolttable.Index[uint64, identityRow], key string) (identityRow, bool, error) {
	id, _, found, err := ix.Lookup(r, []byte(key))
	if err != nil || !found {
		return identityRow{}, false, err
	}
	row, found, err := s.t.identities.table.Get(r, id)
	if err != nil || !found {
		return identityRow{}, false, err
	}
	v := settled(&row, s.now(), s.dormantTTL)
	if v == nil {
		return identityRow{}, false, nil
	}
	return *v, true, nil
}

func noIdentity() *Error {
	return &Error{
		Kind: KindGone, Why: "none",
		Msg: "this session has no identity: read grelot help, then run grelot register <name> --paths <path> --desc <what and why> --ttl=<d>; if registering is refused or against the user's instructions, finish the user's task and tell the user",
	}
}

// dormantIdentity is the revive offer a dormant identity's session gets
// from every route that acts as the identity.
func (s *Store) dormantIdentity(r *bolttable.ReadTx, row *identityRow, now time.Time) error {
	n, err := s.unread(r, row, now)
	if err != nil {
		return err
	}
	return &Error{
		Kind: KindGone, Why: "dormant",
		Msg: fmt.Sprintf("your identity @%s expired %d h ago with %d unread; `grelot revive --ttl=<d>` restores it; if reviving is refused or against the user's instructions, finish the user's task and tell the user",
			row.Name, int(now.Sub(row.DormantSince).Hours()), n),
	}
}

// liveNameRefusal is the 409 for a live name, naming its holder so the
// caller can tell whether it is its own earlier session.
func (s *Store) liveNameRefusal(holder *identityRow, caller Where) *Error {
	return refuse(KindConflict, fmt.Sprintf(
		"%s is held by a live identity: %s (%s · %s · provider %s · last seen %s); choose another name, or add --take-over only if it is your own earlier session",
		holder.Name, holder.Description, holder.Host, relation(caller, holder.Where, ""), holder.Provider, fmtTime(s.lastSeen(holder)),
	))
}

func dormantNameRefusal(holder *identityRow, caller Where) *Error {
	return refuse(KindConflict, fmt.Sprintf(
		"%s is dormant until %s; choose another or revive it from its own environment\n%s · %s · provider %s · dormant since %s\npaths: %s\ndescription: %s\n`grelot revive %s --ttl=<d>` only if it is your own earlier session",
		holder.Name, fmtTime(holder.Lease.Expires), holder.Host, relation(caller, holder.Where, ""), holder.Provider,
		fmtTime(holder.DormantSince), strings.Join(holder.Paths, ", "), holder.Description, holder.Name,
	))
}

// The two requests that move an identity to another session.
const (
	verbTakeOver = "take-over"
	verbRevive   = "revive"
)

// moveRefusal checks a move of target to the caller's session in the order
// of the take-over: an empty caller environment, an environment not shared,
// another provider. A revive answers an environment not shared as not
// found, since it may only find its own environment's rows.
func (s *Store) moveRefusal(verb string, target *identityRow, caller Where, provider string) *Error {
	var e *Error
	switch {
	case caller.Environment == "":
		return s.refuseMove(verb, target.Name, noEnvironment())
	case !shareEnvironment(caller, target.Where) && verb == verbRevive:
		e = &Error{Kind: KindNotFound, Why: "other_environment", Msg: fmt.Sprintf(
			"no dormant identity %s in this environment (%s)", target.Name, relation(caller, target.Where, ""),
		)}
	case !shareEnvironment(caller, target.Where):
		e = &Error{Kind: KindConflict, Why: "other_environment", Msg: fmt.Sprintf(
			"%s is not in this environment (%s); a %s moves an identity only within its own environment and provider: choose another name",
			target.Name, relation(caller, target.Where, ""), verb,
		)}
	case provider != target.Provider:
		e = &Error{Kind: KindConflict, Why: "other_provider", Msg: fmt.Sprintf(
			"%s belongs to provider %s in this environment; a %s moves an identity only within its own provider: choose another name",
			target.Name, target.Provider, verb,
		)}
	default:
		return nil
	}
	return s.refuseMove(verb, target.Name, e)
}

func noEnvironment() *Error {
	return &Error{
		Kind: KindConflict, Why: "no_environment",
		Msg: "this session has no environment id; register a fresh identity",
	}
}

// refuseMove logs a refused move as move refused with its why.
func (s *Store) refuseMove(verb, name string, e *Error) *Error {
	s.log.Info("move refused", "verb", verb, "name", name, "why", e.Why)
	return e
}

// fieldMode says what an empty field means.
type fieldMode int

const (
	// fieldsNew is a registration: every field is required, and NoChange
	// is refused.
	fieldsNew fieldMode = iota
	// fieldsEvery is a post: every field is required, and NoChange keeps it.
	fieldsEvery
	// fieldsSome is a set: an empty field keeps its value.
	fieldsSome
)

// fieldChange is the identity fields after a request, and the work paths
// it added, each owed a join card.
type fieldChange struct {
	name, description string
	paths, added      []string
	pathsChanged      bool
}

// resolveFields applies f to cur, nil for a registration, under mode.
func resolveFields(cur *identityRow, f Fields, mode fieldMode) (fieldChange, error) {
	var fc fieldChange
	if cur != nil {
		fc = fieldChange{name: cur.Name, description: cur.Description, paths: cur.Paths}
	}
	var err error
	if fc.name, err = resolveText(fc.name, f.Name, mode, "name", "--name <name>", checkName); err != nil {
		return fc, err
	}
	if fc.description, err = resolveText(fc.description, f.Description, mode,
		"description", "--desc <what you are doing and why>", checkDescription); err != nil {
		return fc, err
	}
	switch {
	case len(f.Paths) == 0 && mode == fieldsSome, len(f.Paths) == 1 && mode.keeps(f.Paths[0]):
		return fc, nil
	case len(f.Paths) == 0 || slices.Contains(f.Paths, NoChange):
		return fc, missingField(mode, "paths", "--paths '<path>[,<path>...]'")
	}
	paths, err := checkPaths(f.Paths)
	if err != nil {
		return fc, err
	}
	fc.pathsChanged = cur == nil || !slices.Equal(paths, cur.Paths)
	for _, p := range paths {
		if cur == nil || !slices.Contains(cur.Paths, p) {
			fc.added = append(fc.added, p)
		}
	}
	fc.paths = paths
	return fc, nil
}

// keeps reports whether field value v keeps the current value under m.
func (m fieldMode) keeps(v string) bool {
	return m == fieldsSome && v == "" || m != fieldsNew && v == NoChange
}

// resolveText is one text field after a request: cur kept, or v checked.
func resolveText(cur, v string, mode fieldMode, field, flag string, check func(string) error) (string, error) {
	switch {
	case mode.keeps(v):
		return cur, nil
	case v == "" || v == NoChange:
		return cur, missingField(mode, field, flag)
	}
	return v, check(v)
}

func missingField(mode fieldMode, field, flag string) *Error {
	if mode == fieldsNew {
		return refuse(KindInvalid, fmt.Sprintf("%s is required to register, and %q does not apply: pass %s", field, NoChange, flag))
	}
	return refuse(KindInvalid, fmt.Sprintf("%s is required on every post: pass %s, or %q to keep it", field, flag, NoChange))
}

func checkName(n string) error {
	if !namePattern.MatchString(n) {
		return refuse(KindInvalid, fmt.Sprintf("name %q must be 2 to 48 lowercase letters, digits and hyphens, starting with a letter or digit", n))
	}
	return nil
}

func checkDescription(d string) error {
	if n := utf8.RuneCountInString(d); n > maxDescription || !utf8.ValidString(d) || strings.ContainsFunc(d, isControl) {
		return refuse(KindInvalid, fmt.Sprintf("the description must be 1 to %d characters of text, on one line", maxDescription))
	}
	return nil
}

// checkPaths validates work paths and drops repeats, keeping their order.
func checkPaths(ps []string) ([]string, error) {
	var out []string
	for _, p := range ps {
		if !isCanonicalPath(p) || len(p) > maxPathBytes || strings.Count(p, "/") > maxPathDepth || strings.ContainsFunc(p, isControl) {
			return nil, refuse(KindInvalid, fmt.Sprintf(
				"path %q must be a canonical path: absolute, clean, no .., at most %d bytes and %d components", p, maxPathBytes, maxPathDepth,
			))
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) > maxPaths {
		return nil, refuse(KindInvalid, fmt.Sprintf("at most %d work paths: narrow --paths", maxPaths))
	}
	return out, nil
}

// checkSession validates a Grelot-Session value and returns its provider.
func checkSession(session string) (string, error) {
	provider, id, ok := strings.Cut(session, ":")
	if !ok || !providerPattern.MatchString(provider) || id == "" || len(id) > maxSessionID || strings.ContainsFunc(id, isControl) {
		return "", refuse(KindInvalid, "the session must be <provider>:<host session id>; check the provider file's session settings")
	}
	return provider, nil
}

func checkLabels(host, goos, arch, proc string) error {
	for _, l := range []struct{ field, v string }{{"host", host}, {"os", goos}, {"arch", arch}, {"proc", proc}} {
		if utf8.RuneCountInString(l.v) > maxLabel || !utf8.ValidString(l.v) || strings.ContainsFunc(l.v, isControl) {
			return refuse(KindInvalid, fmt.Sprintf("%s must be at most %d characters of text, on one line", l.field, maxLabel))
		}
	}
	return nil
}

func isControl(r rune) bool { return unicode.IsControl(r) }

func cloneWhere(w Where) Where {
	w.Checkouts = slices.Clone(w.Checkouts)
	return w
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func fmtTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

func (s *Store) view(r *identityRow) Identity {
	return Identity{
		Expires: r.Lease.Expires, RegisteredAt: r.RegisteredAt,
		DormantSince: r.DormantSince, LastSeen: s.lastSeen(r),
		Name: r.Name, Description: r.Description, State: r.State,
		Host: r.Host, OS: r.OS, Arch: r.Arch, Provider: r.Provider,
		MachineKind: machineKindOf(r.Where.Machine),
		Paths:       slices.Clone(r.Paths), Follows: slices.Clone(r.Follows),
		ID: r.ID, Read: r.Read, TTL: r.TTL, EnvironmentKnown: r.Where.Environment != "", Ended: r.Ended,
		Threaded: r.Threaded,
	}
}

func machineKindOf(m string) string {
	if m == "" {
		return ""
	}
	return machineKind(m)
}

// noteSeen keeps the newest request time in memory, so last_seen stays
// current between its throttled writes.
func (s *Store) noteSeen(id uint64, t time.Time) {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	if t.After(s.seen[id]) {
		s.seen[id] = t
	}
}

// forgetSeen drops id's request time once its row is deleted; ids are never
// reused, so a kept entry would stay for the life of the process.
func (s *Store) forgetSeen(id uint64) {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	delete(s.seen, id)
}

// forgetSeen drops id's request time once this transaction commits, so a
// rollback keeps a live identity's.
func (w *wtx) forgetSeen(id uint64) {
	w.tx.AfterCommit(func() { w.s.forgetSeen(id) })
}

// pruneSeen drops every request time whose identity row is gone, which
// catches a read that noted on a snapshot older than the delete. It must
// run in a write transaction: a read one could predate a registration
// that already noted its new id, and drop that live entry.
func (w *wtx) pruneSeen() error {
	w.s.seenMu.Lock()
	defer w.s.seenMu.Unlock()
	for id := range w.s.seen {
		_, found, err := w.s.t.identities.table.Get(w.r, id)
		if err != nil {
			return err
		}
		if !found {
			delete(w.s.seen, id)
		}
	}
	return nil
}

func (s *Store) lastSeen(r *identityRow) time.Time {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	return later(r.LastSeen, s.seen[r.ID])
}
