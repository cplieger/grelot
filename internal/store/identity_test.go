//go:build !windows

package store

import (
	"errors"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	sessA = "kiro-cli:session-a"
	sessB = "kiro-cli:session-b"
	sessC = "kiro-cli:session-c"
)

// envA is one environment on one machine; the tests vary it per case.
func envA() Where { return Where{Machine: dmi("1"), Environment: hex32("a")} }

// openIdentityStore opens a store on a test clock with a capturing logger.
func openIdentityStore(t *testing.T) (*Store, *clock, logSink) {
	t.Helper()
	clk := &clock{t: testNow}
	sink := newLogSink()
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{Logger: slog.New(sink), Now: clk.now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk, sink
}

func registration(session, name string, w Where, paths ...string) Registration {
	if len(paths) == 0 {
		paths = []string{repoSubflux}
	}
	return Registration{
		Session: session, Host: "host-a/app", OS: "linux", Arch: "amd64",
		Where: &w, TTL: 6 * time.Hour,
		Fields: Fields{Name: name, Description: "moving the run surface", Paths: paths},
	}
}

func mustRegister(t *testing.T, s *Store, r Registration) Identity {
	t.Helper()
	got, _, err := s.Register(t.Context(), Keyed{}, &r)
	if err != nil {
		t.Fatalf("Register(%s, %s): %v", r.Session, r.Fields.Name, err)
	}
	return got.Identity
}

// wantRefusal fails unless err is a store refusal of kind k (and why, when
// set) whose message holds every part.
func wantRefusal(t *testing.T, err error, k Kind, why string, parts ...string) {
	t.Helper()
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("err %v, want a *Error of kind %d", err, k)
	}
	if se.Kind != k {
		t.Errorf("kind %d, want %d (%q)", se.Kind, k, se.Msg)
	}
	if why != "" && se.Why != why {
		t.Errorf("why %q, want %q (%q)", se.Why, why, se.Msg)
	}
	for _, p := range parts {
		if !strings.Contains(se.Msg, p) {
			t.Errorf("message %q lacks %q", se.Msg, p)
		}
	}
}

func TestRegister_createsTheIdentityUnderItsDeclaredLease(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	got := mustRegister(t, s, registration(sessA, "subflux-dev", envA()))
	if got.Name != "subflux-dev" || got.State != stateLive || got.Provider != "kiro-cli" {
		t.Errorf("name %q state %q provider %q, want subflux-dev live kiro-cli", got.Name, got.State, got.Provider)
	}
	if want := testNow.Add(6 * time.Hour); !got.Expires.Equal(want) {
		t.Errorf("expires %v, want now + the declared 6h", got.Expires)
	}
	if lead := got.Expires.Sub(getIdentity(t, s, got.ID).Lease.WarnAt); lead != 30*time.Minute {
		t.Errorf("warning lead %v, want 30m", lead)
	}
	if !got.RegisteredAt.Equal(testNow) {
		t.Errorf("registered_at %v, want %v", got.RegisteredAt, testNow)
	}
	if !got.EnvironmentKnown || got.MachineKind != "dmi" {
		t.Errorf("environment known %v, machine kind %q, want true and dmi", got.EnvironmentKnown, got.MachineKind)
	}
}

func TestRegister_fieldRule(t *testing.T) {
	// Every field is required at registration and "no change" does
	// not apply; each refusal is a 400 naming the fix.
	long := strings.Repeat("a", 48)
	tests := []struct {
		name string
		edit func(*Registration)
		want string
	}{
		{"no name", func(r *Registration) { r.Fields.Name = "" }, "--name"},
		{"name no change", func(r *Registration) { r.Fields.Name = NoChange }, "--name"},
		{"no description", func(r *Registration) { r.Fields.Description = "" }, "--desc"},
		{"description no change", func(r *Registration) { r.Fields.Description = NoChange }, "--desc"},
		{"no paths", func(r *Registration) { r.Fields.Paths = nil }, "--paths"},
		{"paths no change", func(r *Registration) { r.Fields.Paths = []string{NoChange} }, "--paths"},
		{"upper-case name", func(r *Registration) { r.Fields.Name = "Subflux" }, "lowercase"},
		{"one-character name", func(r *Registration) { r.Fields.Name = "a" }, "2 to 48"},
		{"49-character name", func(r *Registration) { r.Fields.Name = long + "a" }, "2 to 48"},
		{"name opening with a hyphen", func(r *Registration) { r.Fields.Name = "-dev" }, "starting with"},
		{"relative path", func(r *Registration) { r.Fields.Paths = []string{"workspace/subflux"} }, "canonical"},
		{"trailing slash", func(r *Registration) { r.Fields.Paths = []string{"/workspace/subflux/"} }, "canonical"},
		{"dot-dot", func(r *Registration) { r.Fields.Paths = []string{"/workspace/../etc"} }, "canonical"},
		{"double slash", func(r *Registration) { r.Fields.Paths = []string{"/workspace//subflux"} }, "canonical"},
		{"17 paths", func(r *Registration) { r.Fields.Paths = numberedPaths(17) }, "at most 16"},
		{"281-character description", func(r *Registration) { r.Fields.Description = strings.Repeat("é", 281) }, "1 to 280"},
		{"two-line description", func(r *Registration) { r.Fields.Description = "one\ntwo" }, "one line"},
		{"two-line host", func(r *Registration) { r.Host = "a\nb" }, "host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := openIdentityStore(t)
			r := registration(sessA, "subflux-dev", envA())
			tc.edit(&r)
			r.Where = &Where{Machine: dmi("1"), Environment: hex32("a")}
			_, _, err := s.Register(t.Context(), Keyed{}, &r)
			wantRefusal(t, err, KindInvalid, "", tc.want)
			if _, err := s.Seen(t.Context(), Caller{Session: sessA}); err == nil {
				t.Error("a refused register left an identity behind")
			}
		})
	}
	for name, edit := range map[string]func(*Registration){
		"48-character name":         func(r *Registration) { r.Fields.Name = long },
		"16 paths":                  func(r *Registration) { r.Fields.Paths = numberedPaths(16) },
		"280-character description": func(r *Registration) { r.Fields.Description = strings.Repeat("é", 280) },
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := openIdentityStore(t)
			r := registration(sessA, "subflux-dev", envA())
			edit(&r)
			mustRegister(t, s, r)
		})
	}
}

func numberedPaths(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "/workspace/repo-" + string(rune('a'+i))
	}
	return out
}

func TestRegister_ttlRequiredWithinBounds(t *testing.T) {
	for _, ttl := range []time.Duration{0, 14 * time.Minute, 24*time.Hour + time.Minute} {
		s, _, _ := openIdentityStore(t)
		r := registration(sessA, "subflux-dev", envA())
		r.TTL = ttl
		_, _, err := s.Register(t.Context(), Keyed{}, &r)
		wantRefusal(t, err, KindInvalid, "", "--ttl", "15m", "24h")
	}
}

func TestRegister_whereRequiredAndChecked(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	r := registration(sessA, "subflux-dev", envA())
	r.Where = nil
	_, _, err := s.Register(t.Context(), Keyed{}, &r)
	wantRefusal(t, err, KindInvalid, "", "where is required")

	r = registration(sessA, "subflux-dev", Where{Checkouts: []Checkout{co(repoGrelot, "c")}})
	_, _, err = s.Register(t.Context(), Keyed{}, &r)
	wantRefusal(t, err, KindInvalid, "", "where.checkouts[0].repo")
}

func TestRegister_sessionNamesItsProvider(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	for _, sess := range []string{"session-a", ":session-a", "Kiro:session-a", "kiro-cli:", "kiro-cli:a\nb"} {
		_, _, err := s.Register(t.Context(), Keyed{}, new(registration(sess, "subflux-dev", envA())))
		wantRefusal(t, err, KindInvalid, "", "<provider>:<host session id>")
	}
}

func TestRegister_repeatedReturnsTheIdentityUnchanged(t *testing.T) {
	// A repeated register is not a renewal: same identity, same lease.
	s, clk, _ := openIdentityStore(t)
	first := mustRegister(t, s, registration(sessA, "subflux-dev", envA()))
	clk.advance(time.Hour)
	r := registration(sessA, "other-name", envA())
	r.TTL = 24 * time.Hour
	got, _, err := s.Register(t.Context(), Keyed{}, &r)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Existing || got.Identity.ID != first.ID || got.Identity.Name != "subflux-dev" {
		t.Errorf("got %+v, want the existing subflux-dev returned as existing", got)
	}
	if !got.Identity.Expires.Equal(first.Expires) {
		t.Errorf("expires %v, want the unchanged %v", got.Identity.Expires, first.Expires)
	}
}

func TestRegister_liveNameNamesItsHolder(t *testing.T) {
	tests := []struct {
		name     string
		caller   Where
		relation string
	}{
		{"from the same environment", envA(), "same environment"},
		{"from another machine", Where{Machine: dmi("2"), Environment: hex32("b")}, "other machine"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := openIdentityStore(t)
			mustRegister(t, s, registration(sessA, "subflux-dev", envA()))
			_, _, err := s.Register(t.Context(), Keyed{}, new(registration(sessB, "subflux-dev", tc.caller)))
			wantRefusal(t, err, KindConflict, "", "subflux-dev", "moving the run surface", "host-a/app",
				tc.relation, "provider kiro-cli", "last seen 2026-11-04 05:39 UTC", "--take-over")
		})
	}
}

func TestTakeOver_refusedOutsideTheEnvironmentOrProvider(t *testing.T) {
	// refused for an empty environment, then another environment, then
	// another provider; a matching host label never counts.
	tests := []struct {
		name, session  string
		holder, caller Where
		why            string
		parts          []string
	}{
		{
			"no caller environment", sessB, envA(),
			Where{Machine: dmi("1")},
			"no_environment",
			[]string{"this session has no environment id; register a fresh identity"},
		},
		{
			"no caller environment, other provider", "claude-code:x", envA(),
			Where{Machine: dmi("1")},
			"no_environment",
			[]string{"no environment id"},
		},
		{
			"matching host label, other environment", sessB, envA(),
			Where{Machine: dmi("1"), Environment: hex32("b")},
			"other_environment",
			[]string{"subflux-dev is not in this environment (same machine, other environment)", "choose another name"},
		},
		{
			"environment copied to another machine of one kind", sessB, envA(),
			Where{Machine: dmi("2"), Environment: hex32("a")},
			"other_environment",
			[]string{"(other machine)"},
		},
		{
			"environment copied across machine-id and dmi", sessB,
			Where{Machine: machineID("1"), Environment: hex32("a")},
			Where{Machine: dmi("1"), Environment: hex32("a")},
			"other_environment",
			[]string{"(other environment, machine not comparable (dmi · machine-id))"},
		},
		{
			"same environment, other provider", "claude-code:x", envA(), envA(), "other_provider",
			[]string{"subflux-dev belongs to provider kiro-cli in this environment", "only within its own provider"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _, sink := openIdentityStore(t)
			orig := mustRegister(t, s, registration(sessA, "subflux-dev", tc.holder))
			r := registration(tc.session, "subflux-dev", tc.caller)
			r.TakeOver = true
			_, _, err := s.Register(t.Context(), Keyed{}, &r)
			wantRefusal(t, err, KindConflict, tc.why, tc.parts...)
			lines := sink.lines(slog.LevelInfo, "move refused")
			if len(lines) != 1 || attr(lines[0], "why") != tc.why {
				t.Errorf("move refused lines %v, want one with why %s", lines, tc.why)
			}
			if got, err := s.Seen(t.Context(), Caller{Session: sessA}); err != nil || got.ID != orig.ID {
				t.Errorf("the holder's session lost its identity: %+v, %v", got, err)
			}
		})
	}
}

func TestTakeOver_movesTheIdentityWithinItsEnvironment(t *testing.T) {
	tests := []struct {
		name           string
		holder, caller Where
	}{
		{"one machine, one environment", envA(), envA()},
		{"one side's machine unknown", Where{Machine: dmi("1"), Environment: hex32("a")}, Where{Environment: hex32("a")}},
		{"two containers on one home volume", Where{Environment: hex32("a")}, Where{Environment: hex32("a")}},
		{"a cloned VM sending its source's machine", Where{Machine: machineID("1"), Environment: hex32("a")}, Where{Machine: machineID("1"), Environment: hex32("a")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, clk, sink := openIdentityStore(t)
			orig := mustRegister(t, s, registration(sessA, "subflux-dev", tc.holder))
			putRows(t, s, func(w *wtx) error {
				row := getIdentity(t, s, orig.ID)
				row.Follows, row.Read, row.Shown, row.Seq = []string{"/workspace/sse"}, 4, 5, 6
				return s.t.identities.table.Put(w.tx, row.ID, &row)
			})
			if err := s.EndSession(t.Context(), sessA, 0); err != nil {
				t.Fatal(err)
			}
			clk.advance(10 * time.Minute)
			r := registration(sessB, "subflux-dev", tc.caller)
			r.TakeOver, r.TTL = true, 2*time.Hour
			got, _, err := s.Register(t.Context(), Keyed{}, &r)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Moved || got.Identity.ID != orig.ID {
				t.Fatalf("got %+v, want identity %d moved", got, orig.ID)
			}
			row := getIdentity(t, s, orig.ID)
			if row.Session != sessB || row.Ended {
				t.Errorf("session %q ended %v, want %s and ended cleared", row.Session, row.Ended, sessB)
			}
			if !slices.Equal(row.Follows, []string{"/workspace/sse"}) || row.Read != 4 || row.Shown != 5 || row.Seq != 6 {
				t.Errorf("follows %v read %d shown %d seq %d, want them moved unchanged", row.Follows, row.Read, row.Shown, row.Seq)
			}
			if want := clk.now().Add(2 * time.Hour); !row.Lease.Expires.Equal(want) || !row.RegisteredAt.Equal(testNow) {
				t.Errorf("expires %v registered %v, want a new lease to %v and the first registration kept", row.Lease.Expires, row.RegisteredAt, want)
			}
			_, err = s.Seen(t.Context(), Caller{Session: sessA})
			wantRefusal(t, err, KindGone, "none")
			if n := len(sink.lines(slog.LevelInfo, "identity moved")); n != 1 {
				t.Errorf("%d identity moved lines, want 1", n)
			}
		})
	}
}

// expireToDormant registers name from sessA under a 15-minute lease and
// sweeps it dormant; it returns the identity as registered.
func expireToDormant(t *testing.T, s *Store, clk *clock, name string) Identity {
	t.Helper()
	r := registration(sessA, name, envA(), repoSubflux, repoGrelot)
	r.TTL = 15 * time.Minute
	id := mustRegister(t, s, r)
	clk.advance(15 * time.Minute)
	mustSweep(t, s)
	return id
}

func TestRegister_dormantNameNamesTheRow(t *testing.T) {
	for _, takeOver := range []bool{false, true} {
		s, clk, _ := openIdentityStore(t)
		expireToDormant(t, s, clk, "subflux-dev")
		r := registration(sessB, "subflux-dev", envA())
		r.TakeOver = takeOver
		_, _, err := s.Register(t.Context(), Keyed{}, &r)
		wantRefusal(t, err, KindConflict, "",
			"subflux-dev is dormant until 2026-11-07 05:54 UTC; choose another or revive it from its own environment",
			"host-a/app", "same environment", "provider kiro-cli", "dormant since 2026-11-04 05:54 UTC",
			repoSubflux+", "+repoGrelot, "moving the run surface",
			"`grelot revive subflux-dev --ttl=<d>` only if it is your own earlier session")
	}
}

func TestRegister_aDueNameExpiresBeforeTheWrite(t *testing.T) {
	// No sweep: the register itself runs the due expiry.
	s, clk, _ := openIdentityStore(t)
	r := registration(sessA, "subflux-dev", envA())
	r.TTL = 15 * time.Minute
	mustRegister(t, s, r)
	clk.advance(15 * time.Minute)
	_, _, err := s.Register(t.Context(), Keyed{}, new(registration(sessB, "subflux-dev", envA())))
	wantRefusal(t, err, KindConflict, "", "is dormant until")
}

func TestRegister_aNamePastItsPurgeIsFreeWithoutASweep(t *testing.T) {
	// The register runs both due expiries, the dormancy and the purge,
	// in its own transaction.
	s, clk, _ := openIdentityStore(t)
	r := registration(sessA, "subflux-dev", envA())
	r.TTL = 15 * time.Minute
	old := mustRegister(t, s, r)
	clk.advance(15*time.Minute + 72*time.Hour)
	got := mustRegister(t, s, registration(sessB, "subflux-dev", envA()))
	if got.ID == old.ID {
		t.Errorf("the purged identity's id %d came back", old.ID)
	}
	if n := countRows(t, s, s.t.identities.table); n != 2 {
		t.Errorf("%d identity rows, want @user's and the new one, the purged one gone", n)
	}
}

func TestRegister_freshFromTheDormantSessionDetachesIt(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	old := expireToDormant(t, s, clk, "subflux-dev")
	fresh := mustRegister(t, s, registration(sessA, "subflux-two", envA()))
	if fresh.ID == old.ID {
		t.Fatal("the fresh registration reused the dormant id")
	}
	row := getIdentity(t, s, old.ID)
	if row.State != stateDormant || row.Session != "" || row.Name != "subflux-dev" {
		t.Errorf("state %q session %q name %q, want dormant, detached, name kept", row.State, row.Session, row.Name)
	}
	_, _, err := s.Register(t.Context(), Keyed{}, new(registration(sessB, "subflux-dev", envA())))
	wantRefusal(t, err, KindConflict, "", "is dormant until")
}

func TestSet_renewMovesTheLeaseAndReArmsTheWarning(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	r := registration(sessA, "subflux-dev", envA())
	r.TTL = time.Hour
	mustRegister(t, s, r)
	clk.advance(20 * time.Minute)
	w := envA()
	got, err := s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{TTL: 2 * time.Hour, Where: &w})
	if err != nil {
		t.Fatal(err)
	}
	warn := getIdentity(t, s, got.ID).Lease.WarnAt
	if want := clk.now().Add(2 * time.Hour); !got.Expires.Equal(want) || !warn.Equal(want.Add(-30*time.Minute)) {
		t.Errorf("expires %v warn %v, want %v and 30m before", got.Expires, warn, want)
	}
	_, err = s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{TTL: 2 * time.Hour})
	wantRefusal(t, err, KindInvalid, "", "where is required")
	_, err = s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{TTL: 10 * time.Minute, Where: &w})
	wantRefusal(t, err, KindInvalid, "", "--ttl")
}

func TestSet_renewMergesTheSentCheckout(t *testing.T) {
	// A renew sends the checkout of the working directory's clone
	// only; the stored entries of the other repos stay.
	s, _, _ := openIdentityStore(t)
	where := Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c"), co(repoGrelot, "d")}}
	id := mustRegister(t, s, registration(sessA, "subflux-dev", where, repoSubflux, repoGrelot))
	sent := Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "e")}}
	if _, err := s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{TTL: time.Hour, Where: &sent}); err != nil {
		t.Fatal(err)
	}
	got := getIdentity(t, s, id.ID).Where.Checkouts
	if want := []Checkout{co(repoSubflux, "e"), co(repoGrelot, "d")}; !slices.Equal(got, want) {
		t.Errorf("checkouts %v, want %v", got, want)
	}
}

func TestSet_newPathsNeedWhereAndReplaceTheCheckouts(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	where := Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c"), co(repoGrelot, "d")}}
	id := mustRegister(t, s, registration(sessA, "subflux-dev", where, repoSubflux, repoGrelot))
	_, err := s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{Fields: Fields{Paths: []string{repoSubflux}}})
	wantRefusal(t, err, KindInvalid, "", "where is required")

	sent := Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}}
	got, err := s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{Fields: Fields{Paths: []string{repoSubflux}}, Where: &sent})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Paths, []string{repoSubflux}) {
		t.Errorf("paths %v", got.Paths)
	}
	if co := getIdentity(t, s, id.ID).Where.Checkouts; !slices.Equal(co, sent.Checkouts) {
		t.Errorf("checkouts %v, want only the sent %v", co, sent.Checkouts)
	}
	if !got.Expires.Equal(id.Expires) {
		t.Errorf("a set without --ttl moved the expiry to %v", got.Expires)
	}
}

func TestSet_newPathsDropAStoredCheckoutTheClientDidNotSend(t *testing.T) {
	// A set with paths sends the checkouts of the paths it names,
	// unlike a renew, so a repo it sent none for (no clone there any
	// more) loses its stored entry.
	s, _, _ := openIdentityStore(t)
	where := Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c"), co(repoGrelot, "d")}}
	id := mustRegister(t, s, registration(sessA, "subflux-dev", where, repoSubflux, repoGrelot))
	sent := Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}}
	if _, err := s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{Fields: Fields{Paths: []string{repoSubflux, repoGrelot, "/workspace/sse"}}, Where: &sent}); err != nil {
		t.Fatal(err)
	}
	if got := getIdentity(t, s, id.ID).Where.Checkouts; !slices.Equal(got, sent.Checkouts) {
		t.Errorf("checkouts %v, want only the sent %v", got, sent.Checkouts)
	}
}

func TestSet_renameAndRefusals(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	mustRegister(t, s, registration(sessB, "taken-name", envA()))
	mustRegister(t, s, registration(sessA, "subflux-dev", envA()))
	got, err := s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{Fields: Fields{Name: "subflux-web", Description: "new work"}})
	if err != nil || got.Name != "subflux-web" || got.Description != "new work" {
		t.Errorf("rename: %+v, %v", got, err)
	}
	_, err = s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{Fields: Fields{Name: "taken-name"}})
	wantRefusal(t, err, KindConflict, "", "taken-name is held")
	_, err = s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{Fields: Fields{Name: "Bad"}})
	wantRefusal(t, err, KindInvalid, "", "lowercase")
	_, err = s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{})
	wantRefusal(t, err, KindInvalid, "", "nothing to set")

	_, err = s.Set(t.Context(), Keyed{}, Caller{Session: sessC}, Change{Fields: Fields{Description: "x"}})
	wantRefusal(t, err, KindGone, "none", "grelot register")
	clk.advance(6 * time.Hour)
	_, err = s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{Fields: Fields{Description: "x"}})
	wantRefusal(t, err, KindGone, "dormant", "grelot revive --ttl")
}

func TestEndSession_capsTheLeaseAtAnHour(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	me := mustRegister(t, s, registration(sessA, "subflux-dev", envA()))
	clk.advance(time.Hour)
	if err := s.EndSession(t.Context(), sessA, 0); err != nil {
		t.Fatal(err)
	}
	row := getIdentity(t, s, me.ID)
	end := clk.now().Add(time.Hour)
	if !row.Ended || !row.Lease.Expires.Equal(end) || !row.Lease.WarnAt.Equal(end.Add(-30*time.Minute)) {
		t.Errorf("ended %v expires %v warn %v, want ended, %v and its warning 30m before", row.Ended, row.Lease.Expires, row.Lease.WarnAt, end)
	}

	s2, _, _ := openIdentityStore(t)
	r := registration(sessA, "short-lease", envA())
	r.TTL = 30 * time.Minute
	short := mustRegister(t, s2, r)
	before := getIdentity(t, s2, short.ID).Lease
	if err := s2.EndSession(t.Context(), sessA, 0); err != nil {
		t.Fatal(err)
	}
	if row := getIdentity(t, s2, short.ID); !row.Lease.Expires.Equal(before.Expires) || !row.Lease.WarnAt.Equal(before.WarnAt) {
		t.Errorf("a lease under the hour moved: %+v, want %v / %v", row.Lease, before.Expires, before.WarnAt)
	}
}

func TestEndSession_aResumeRestoresNothing(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	mustRegister(t, s, registration(sessA, "subflux-dev", envA()))
	if err := s.EndSession(t.Context(), sessA, 0); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	got, err := s.Seen(t.Context(), Caller{Session: sessA})
	if err != nil {
		t.Fatal(err)
	}
	if got.Ended || !got.Expires.Equal(testNow.Add(time.Hour)) {
		t.Errorf("ended %v expires %v, want ended cleared and the capped %v kept", got.Ended, got.Expires, testNow.Add(time.Hour))
	}
	w := envA()
	if got, err = s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{TTL: 6 * time.Hour, Where: &w}); err != nil || !got.Expires.Equal(clk.now().Add(6*time.Hour)) {
		t.Errorf("renew after resume: %v, %v", got.Expires, err)
	}
}

func TestSeen_lastSeenWrittenAtMostEveryFiveMinutes(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	id := mustRegister(t, s, registration(sessA, "subflux-dev", envA()))
	clk.advance(2 * time.Minute)
	got, err := s.Seen(t.Context(), Caller{Session: sessA})
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastSeen.Equal(clk.now()) {
		t.Errorf("last seen %v, want the request at %v", got.LastSeen, clk.now())
	}
	if row := getIdentity(t, s, id.ID); !row.LastSeen.Equal(testNow) {
		t.Errorf("stored last seen %v after 2 minutes, want the unwritten %v", row.LastSeen, testNow)
	}
	clk.advance(3 * time.Minute)
	if _, err := s.Seen(t.Context(), Caller{Session: sessA}); err != nil {
		t.Fatal(err)
	}
	if row := getIdentity(t, s, id.ID); !row.LastSeen.Equal(clk.now()) {
		t.Errorf("stored last seen %v after 5 minutes, want %v", row.LastSeen, clk.now())
	}
}

func TestEndSession_aRepeatedRegisterOrWhoClearsEndedAndKeepsTheCap(t *testing.T) {
	for _, call := range []string{"register", "who"} {
		t.Run(call, func(t *testing.T) {
			s, clk, _ := openIdentityStore(t)
			r := registration(sessA, "subflux-dev", envA())
			me := mustRegister(t, s, r)
			if err := s.EndSession(t.Context(), sessA, 0); err != nil {
				t.Fatal(err)
			}
			clk.advance(time.Minute)
			var err error
			if call == "register" {
				var got Registered
				got, _, err = s.Register(t.Context(), Keyed{}, &r)
				if !got.Existing {
					t.Errorf("repeated register %+v, want the existing identity", got)
				}
			} else {
				_, err = s.Who(t.Context(), Caller{Session: sessA}, &WhoQuery{})
			}
			if err != nil {
				t.Fatal(err)
			}
			row := getIdentity(t, s, me.ID)
			if row.Ended {
				t.Error("ended still set, want it cleared")
			}
			if !row.Lease.Expires.Equal(testNow.Add(time.Hour)) {
				t.Errorf("expires %v, want the capped %v kept", row.Lease.Expires, testNow.Add(time.Hour))
			}
		})
	}
}

func TestSeen_forgetsAnIdentityOnceItIsGone(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	mustRegister(t, s, registration(sessA, "agent-a", envA()))
	b := mustRegister(t, s, registration(sessB, "agent-b", envA()))
	clk.advance(time.Minute)
	for _, session := range []string{sessA, sessB} {
		if _, err := s.Seen(t.Context(), Caller{Session: session}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Retire(t.Context(), Keyed{}, Caller{Session: sessA}); err != nil {
		t.Fatal(err)
	}
	if got := seenIDs(s); !slices.Equal(got, []uint64{b.ID}) {
		t.Errorf("request times kept for %v after the retirement, want only %d", got, b.ID)
	}
	clk.set(b.Expires)
	mustSweep(t, s)
	clk.set(b.Expires.Add(DefaultDormantTTL))
	mustSweep(t, s)
	if got := seenIDs(s); len(got) != 0 {
		t.Errorf("request times kept for %v after a retirement and a purge, want none", got)
	}
}

func TestSweep_dropsARequestTimeNotedAfterTheRetirement(t *testing.T) {
	// a read can note its request after a retire on an older snapshot;
	// ids are never reused, so the entry would stay for the process's life.
	s, clk, _ := openIdentityStore(t)
	a := mustRegister(t, s, registration(sessA, "agent-a", envA()))
	b := mustRegister(t, s, registration(sessB, "agent-b", envA()))
	if _, _, err := s.Retire(t.Context(), Keyed{}, Caller{Session: sessA}); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	s.noteSeen(a.ID, clk.now())
	s.noteSeen(b.ID, clk.now())
	mustSweep(t, s)
	if got := seenIDs(s); !slices.Equal(got, []uint64{b.ID}) {
		t.Errorf("request times kept for %v after the sweep, want only the live %d", got, b.ID)
	}
}

// seenIDs lists the identities with a request time in memory, in order.
func seenIDs(s *Store) []uint64 {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	return slices.Sorted(maps.Keys(s.seen))
}

func TestSeen_isGoneWithoutAnIdentity(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	_, err := s.Seen(t.Context(), Caller{Session: sessA})
	wantRefusal(t, err, KindGone, "none")
}

func peerNames(ps []Peer) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name + " " + p.Relation
	}
	return out
}

func TestWho_listsLiveIdentitiesWithTheirRelation(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	same := Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}}
	mustRegister(t, s, registration(sessA, "agent-a", same))
	mustRegister(t, s, registration(sessB, "agent-b", same))
	mustRegister(t, s, registration(sessC, "agent-c", Where{Machine: dmi("2"), Environment: hex32("a")}, repoGrelot))
	r := registration("kiro-cli:session-d", "agent-d", envA())
	r.TTL = 15 * time.Minute
	mustRegister(t, s, r)
	clk.advance(15 * time.Minute)

	who := func(sess string, q WhoQuery) []string {
		t.Helper()
		ps, err := s.Who(t.Context(), Caller{Session: sess}, &q)
		if err != nil {
			t.Fatal(err)
		}
		return peerNames(ps)
	}
	// A same-checkout pair reads same environment without a path.
	if got, want := who(sessA, WhoQuery{}), []string{"agent-a you", "agent-b same environment", "agent-c other machine"}; !slices.Equal(got, want) {
		t.Errorf("who %v, want %v", got, want)
	}
	if got, want := who(sessA, WhoQuery{Here: true}), []string{"agent-a you", "agent-b same environment"}; !slices.Equal(got, want) {
		t.Errorf("who --here %v, want %v", got, want)
	}
	all, err := s.Who(t.Context(), Caller{Session: sessA}, &WhoQuery{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 || all[3].Name != "agent-d" || all[3].State != stateDormant || !all[3].Expires.Equal(testNow.Add(15*time.Minute+72*time.Hour)) {
		t.Errorf("who --all %+v, want agent-d dormant with its purge time", all)
	}
	if got, want := who(sessA, WhoQuery{Path: repoSubflux + "/internal"}), []string{"agent-a you", "agent-b same environment"}; !slices.Equal(got, want) {
		t.Errorf("who --path under subflux %v, want %v", got, want)
	}
	if got, want := who(sessA, WhoQuery{Path: "/workspace"}), []string{"agent-a you", "agent-b same environment", "agent-c other machine"}; !slices.Equal(got, want) {
		t.Errorf("who --path /workspace %v, want %v", got, want)
	}
	if got, want := who(sessA, WhoQuery{Text: "GRELOT"}), []string{"agent-c other machine"}; !slices.Equal(got, want) {
		t.Errorf("who text %v, want %v", got, want)
	}
	if got, want := who("kiro-cli:nobody", WhoQuery{Here: true}), []string(nil); !slices.Equal(got, want) {
		t.Errorf("who --here with no identity %v, want none", got)
	}
	if got := who("kiro-cli:nobody", WhoQuery{}); !slices.Equal(got, []string{"agent-a unknown", "agent-b unknown", "agent-c unknown"}) {
		t.Errorf("who with no identity %v, want every relation unknown", got)
	}
}

func TestWho_showsAPeerOnlyItsRelation(t *testing.T) {
	// a peer's machine kind and whether it has an environment stay in
	// the logs; the caller still sees its own.
	s, _, _ := openIdentityStore(t)
	mustRegister(t, s, registration(sessA, "agent-a", envA()))
	mustRegister(t, s, registration(sessB, "agent-b", envA()))
	for _, caller := range []string{sessA, "kiro-cli:session-none"} {
		ps, err := s.Who(t.Context(), Caller{Session: caller}, &WhoQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(ps) != 2 {
			t.Fatalf("who %v, want both", peerNames(ps))
		}
		for _, p := range ps {
			wantKind, wantKnown := "", false
			if caller == sessA && p.Name == "agent-a" {
				wantKind, wantKnown = "dmi", true
			}
			if p.MachineKind != wantKind {
				t.Errorf("caller %s sees %s with machine kind %q, want %q", caller, p.Name, p.MachineKind, wantKind)
			}
			if p.EnvironmentKnown != wantKnown {
				t.Errorf("caller %s sees %s with environment known %v, want %v", caller, p.Name, p.EnvironmentKnown, wantKnown)
			}
		}
	}
}

func TestWho_aReadSeesAPurgeTheSweepHasNotRun(t *testing.T) {
	// Reads check expiry themselves: past its expiry and its
	// DORMANT_TTL, an unswept identity is gone even from who --all.
	s, clk, _ := openIdentityStore(t)
	r := registration(sessA, "agent-a", envA())
	r.TTL = 15 * time.Minute
	mustRegister(t, s, r)
	clk.advance(15*time.Minute + 72*time.Hour)
	ps, err := s.Who(t.Context(), Caller{Session: sessB}, &WhoQuery{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 0 {
		t.Errorf("who --all %v, want nothing after the purge time", peerNames(ps))
	}
}
