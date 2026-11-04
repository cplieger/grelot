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
	WarnAt       time.Time
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
	MachineKind      string
	Paths            []string
	Follows          []string
	ID               uint64
	TTL              time.Duration
	EnvironmentKnown bool
	Ended            bool
}

// Registration is a register request from Session, "<provider>:<id>".
type Registration struct {
	Where    *Where
	Session  string
	Host     string
	OS       string
	Arch     string
	Proc     string
	Fields   Fields
	TTL      time.Duration
	TakeOver bool
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
func (s *Store) Register(ctx context.Context, r *Registration) (Registered, error) {
	provider, err := checkSession(r.Session)
	if err != nil {
		return Registered{}, err
	}
	var out Registered
	err = s.update(ctx, func(w *wtx) error {
		var uerr error
		out, uerr = w.register(r, provider)
		return uerr
	})
	return out, err
}

func (w *wtx) register(r *Registration, provider string) (Registered, error) {
	own, ownFound, err := w.identityBy(w.s.t.bySession, r.Session)
	if err != nil {
		return Registered{}, err
	}
	if ownFound && own.State == stateLive {
		if w.seen(&own) {
			if perr := w.s.t.identities.table.Put(w.tx, own.ID, &own); perr != nil {
				return Registered{}, perr
			}
		}
		return Registered{Identity: w.s.view(&own), Existing: true}, nil
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
		w.s.log.Info("identity moved", "name", row.Name, "id", row.ID, "provider", provider)
	}
	return Registered{Identity: w.s.view(&row), Moved: moved}, nil
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
		return identityRow{}, nil, false, w.s.dormantNameRefusal(&row, *r.Where)
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
	row.Ended = false
	row.LastSeen = w.now
	return row, added, held, nil
}

// Set applies c to session's live identity and returns it. Refusals:
// KindGone for a session with no live identity, KindInvalid for a bad
// change, KindConflict for a name another identity holds.
func (s *Store) Set(ctx context.Context, session string, c Change) (Identity, error) {
	var out Identity
	err := s.update(ctx, func(w *wtx) error {
		row, err := w.liveBySession(session)
		if err != nil {
			return err
		}
		if err := w.applyChange(&row, c); err != nil {
			return err
		}
		row.Ended = false
		row.LastSeen = w.now
		if err := s.t.identities.table.Put(w.tx, row.ID, &row); err != nil {
			return err
		}
		out = s.view(&row)
		return nil
	})
	return out, err
}

func (w *wtx) applyChange(row *identityRow, c Change) error {
	if c.TTL == 0 && c.Where == nil && c.Fields.Name == "" && c.Fields.Description == "" && len(c.Fields.Paths) == 0 {
		return refuse(KindInvalid, "nothing to set: pass --name, --paths, --desc or --ttl")
	}
	if err := w.applyFields(row, c.Fields, c.Where, fieldsSome, c.TTL > 0); err != nil {
		return err
	}
	if c.TTL > 0 {
		ls, err := identityPolicy.grant(w.now, c.TTL, time.Time{})
		if err != nil {
			return err
		}
		row.TTL, row.Lease = c.TTL, ls
	}
	return nil
}

// applyFields applies a set's or a post's identity fields and where to
// row, owing a join card for each path it adds. Where is checked when it
// is sent, when the paths change, and on a renewal, which must send it.
func (w *wtx) applyFields(row *identityRow, f Fields, sent *Where, mode fieldMode, renew bool) error {
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
// it is marked ended, and its expiry becomes min(current, now + 1 h), its
// warning moving with it. Nothing restores the lease but a renewal.
func (s *Store) EndSession(ctx context.Context, session string) error {
	return s.update(ctx, func(w *wtx) error {
		row, err := w.liveBySession(session)
		if err != nil {
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

// Retire deletes session's live identity, owing a left card for each of its
// paths, frees its name and hands its reservations to their queues. Its
// delivery rows stay until their own expiry. A session with no live
// identity is KindGone.
func (s *Store) Retire(ctx context.Context, session string) error {
	return s.update(ctx, func(w *wtx) error {
		row, err := w.liveBySession(session)
		if err != nil {
			return err
		}
		if err := w.freeHolder(row.ID); err != nil {
			return err
		}
		w.queueCards(kindLeft, &row, row.Paths)
		if err := deleteRow(w, s.t.identities, row.ID); err != nil {
			return err
		}
		s.forgetSeen(row.ID)
		s.log.Info("identity retired", "name", row.Name, "id", row.ID, "provider", row.Provider)
		return nil
	})
}

// Seen records a request from session other than the standing wait and
// returns its identity, live or dormant. A live one has ended cleared and
// last_seen refreshed, its row written only when ended was set or the
// stored last_seen is lastSeenEvery old; a dormant one is never written.
// A session with no identity is KindGone.
func (s *Store) Seen(ctx context.Context, session string) (Identity, error) {
	var row identityRow
	var found bool
	if err := s.db.View(ctx, func(r *bolttable.ReadTx) error {
		var err error
		row, found, err = s.lookupIdentity(r, s.t.bySession, session)
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
		row, found, err := w.identityBy(s.t.bySession, session)
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
		out = s.view(&row)
		return nil
	})
	return out, err
}

// seen records a request from row's own session in memory and, when
// seenDue, clears ended and refreshes last_seen, reporting whether row
// must be written.
func (w *wtx) seen(row *identityRow) bool {
	w.s.noteSeen(row.ID, w.now)
	if !seenDue(row, w.now) {
		return false
	}
	row.Ended, row.LastSeen = false, w.now
	return true
}

// seenDue reports whether a request at now must write row: ended is set or
// the stored last_seen is lastSeenEvery old.
func seenDue(row *identityRow, now time.Time) bool {
	return row.Ended || now.Sub(row.LastSeen) >= lastSeenEvery
}

// WhoQuery filters Who. Path keeps identities with a work path at, above
// or under it; Text keeps those whose name, description, host label or a
// path holds it, ignoring case; All adds dormant ones; Here keeps only
// those that share the caller's environment.
type WhoQuery struct {
	Path string
	Text string
	All  bool
	Here bool
}

// Peer is an identity as a caller sees it, with its relation to the
// caller. A dormant peer's Expires is its purge time. Only the caller's
// own entry carries MachineKind and EnvironmentKnown.
type Peer struct {
	Relation string
	Identity
}

// Who lists live identities, by name, as session's identity sees them; a
// session with no live identity sees every relation as unknown.
func (s *Store) Who(ctx context.Context, session string, q WhoQuery) ([]Peer, error) {
	if _, err := s.Seen(ctx, session); err != nil && !isKind(err, KindGone) {
		return nil, err
	}
	text := strings.ToLower(q.Text)
	var out []Peer
	err := s.db.View(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		var caller party
		if me, found, err := s.lookupIdentity(r, s.t.bySession, session); err != nil {
			return err
		} else if found && me.State == stateLive {
			caller = party{where: me.Where, id: me.ID}
		}
		return s.t.identities.table.Scan(r, bolttable.Range{}, func(_ uint64, v *identityRow) (bool, error) {
			if v = settled(v, now, s.dormantTTL); v != nil && q.shows(v, text, caller.where) {
				out = append(out, s.peer(caller, v))
			}
			return true, nil
		})
	})
	slices.SortFunc(out, func(a, b Peer) int { return strings.Compare(a.Name, b.Name) })
	return out, err
}

// peer is v as caller sees it.
func (s *Store) peer(caller party, v *identityRow) Peer {
	p := Peer{Identity: s.view(v), Relation: caller.relationTo(party{where: v.Where, id: v.ID}, "")}
	if v.ID != caller.id {
		p.MachineKind, p.EnvironmentKnown = "", false
	}
	return p
}

// shows reports whether q lists v for a caller at where; text is q.Text
// lowercased.
func (q WhoQuery) shows(v *identityRow, text string, where Where) bool {
	if (v.State == stateDormant && !q.All) || (q.Here && !shareEnvironment(where, v.Where)) {
		return false
	}
	return whoMatches(v, text, q.Path)
}

func whoMatches(v *identityRow, text, path string) bool {
	if path != "" && !slices.ContainsFunc(v.Paths, func(p string) bool { return under(p, path) || under(path, p) }) {
		return false
	}
	if text == "" {
		return true
	}
	hay := append([]string{v.Name, v.Description, v.Host}, v.Paths...)
	return slices.ContainsFunc(hay, func(h string) bool { return strings.Contains(strings.ToLower(h), text) })
}

// settled is v as a read sees it before the sweeper reaches it: a live row
// past its expiry reads dormant, a dormant one past its purge reads nil.
func settled(v *identityRow, now time.Time, dormantTTL time.Duration) *identityRow {
	if !v.Lease.due(now) {
		return v
	}
	if v.State == stateDormant {
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

// liveBySession is the live identity an identity route acts as.
func (w *wtx) liveBySession(session string) (identityRow, error) {
	row, found, err := w.identityBy(w.s.t.bySession, session)
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

// liveFor is liveBySession for a read.
func (s *Store) liveFor(r *bolttable.ReadTx, session string, now time.Time) (identityRow, error) {
	row, found, err := s.lookupIdentity(r, s.t.bySession, session)
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

func noIdentity() *Error {
	return &Error{
		Kind: KindGone, Why: "none",
		Msg: "this session has no identity: run grelot register <name> --paths <path> --desc <what and why> --ttl <d>",
	}
}

// dormantIdentity is the revive offer a dormant identity's session gets
// from every route that acts as the identity.
func (s *Store) dormantIdentity(r *bolttable.ReadTx, row *identityRow, now time.Time) error {
	n, err := s.unread(r, row.ID, row.Read, now)
	if err != nil {
		return err
	}
	return &Error{
		Kind: KindGone, Why: "dormant",
		Msg: fmt.Sprintf("your identity %s expired %d h ago with %d unread; `grelot revive --ttl <d>` restores it",
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

func (s *Store) dormantNameRefusal(holder *identityRow, caller Where) *Error {
	return refuse(KindConflict, fmt.Sprintf(
		"%s is dormant until %s; choose another or revive it from its own environment\n%s · %s · provider %s · dormant since %s\npaths: %s\ndescription: %s\n`grelot revive %s --ttl <d>` only if it is your own earlier session",
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
		return fc, missingField(mode, "paths", "--paths <path>[,<path>...]")
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
		Expires: r.Lease.Expires, WarnAt: r.Lease.WarnAt, RegisteredAt: r.RegisteredAt,
		DormantSince: r.DormantSince, LastSeen: s.lastSeen(r),
		Name: r.Name, Description: r.Description, State: r.State,
		Host: r.Host, OS: r.OS, Arch: r.Arch, Provider: r.Provider,
		MachineKind: machineKindOf(r.Where.Machine),
		Paths:       slices.Clone(r.Paths), Follows: slices.Clone(r.Follows),
		ID: r.ID, TTL: r.TTL, EnvironmentKnown: r.Where.Environment != "", Ended: r.Ended,
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
