package store

import (
	"log/slog"
	"slices"
	"testing"
	"time"
)

func revival(session, name string, w Where) Revival {
	return Revival{
		Session: session, Name: name, Host: "borgcube/web-terminal-kiro", OS: "linux", Arch: "arm64",
		Where: &w, TTL: 6 * time.Hour,
	}
}

func mustRevive(t *testing.T, s *Store, r Revival) Revived {
	t.Helper()
	got, err := s.Revive(t.Context(), &r)
	if err != nil {
		t.Fatalf("Revive(%s, %q): %v", r.Session, r.Name, err)
	}
	return got
}

func TestRevive_sameSessionRestoresTheIdentity(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	old := expireToDormant(t, s, clk, "subflux-dev")
	deliverEvents(t, s, old.ID, 3)
	editIdentity(t, s, old.ID, func(row *identityRow) {
		row.Follows = []string{"/workspace/docs"}
		row.Read, row.Shown = 1, 1
		row.Nudged, row.Nudges, row.NudgedAt = 3, 4, testNow
		row.Ended = true
	})
	clk.advance(time.Hour)
	now := clk.now()

	got := mustRevive(t, s, revival(sessA, "", envA()))
	if got.Identity.ID != old.ID || got.Moved {
		t.Errorf("id %d moved %v, want %d in place", got.Identity.ID, got.Moved, old.ID)
	}
	if got.Unread != 2 {
		t.Errorf("unread %d, want the 2 deliveries past read", got.Unread)
	}
	id := got.Identity
	if id.State != stateLive || !id.DormantSince.IsZero() || id.Ended {
		t.Errorf("state %q since %v ended %v, want live, not dormant, not ended", id.State, id.DormantSince, id.Ended)
	}
	if !id.Expires.Equal(now.Add(6*time.Hour)) || !id.WarnAt.Equal(now.Add(6*time.Hour-30*time.Minute)) {
		t.Errorf("expires %v warn %v, want a new 6 h lease warned 30 min ahead", id.Expires, id.WarnAt)
	}
	if !id.RegisteredAt.Equal(testNow) || !slices.Equal(id.Follows, []string{"/workspace/docs"}) {
		t.Errorf("registered %v follows %v, want both kept", id.RegisteredAt, id.Follows)
	}
	if id.Host != "borgcube/web-terminal-kiro" || id.Arch != "arm64" {
		t.Errorf("host %q arch %q, want the reviving session's labels", id.Host, id.Arch)
	}
	row := getIdentity(t, s, old.ID)
	if row.Read != 1 || row.Shown != 1 || row.Seq != 3 {
		t.Errorf("read %d shown %d seq %d, want the cursors kept", row.Read, row.Shown, row.Seq)
	}
	if row.Nudged != 0 || row.Nudges != 0 || !row.NudgedAt.IsZero() {
		t.Errorf("nudged %d nudges %d at %v, want the nudge state reset", row.Nudged, row.Nudges, row.NudgedAt)
	}
	if _, err := s.Set(t.Context(), sessA, Change{Fields: Fields{Description: "back at it"}}); err != nil {
		t.Errorf("Set after the revival: %v", err)
	}
}

func TestRevive_sameSessionWithNoEnvironment(t *testing.T) {
	// The environment rules guard a move to another session; a session
	// reviving its own identity, by name or not, moves nothing.
	for _, name := range []string{"", "subflux-dev"} {
		t.Run("name "+name, func(t *testing.T) {
			s, clk, _ := openIdentityStore(t)
			old := expireToDormant(t, s, clk, "subflux-dev")
			got := mustRevive(t, s, revival(sessA, name, Where{}))
			if got.Identity.ID != old.ID || got.Moved || got.Identity.EnvironmentKnown {
				t.Errorf("id %d moved %v environment known %v, want %d revived in place with no environment",
					got.Identity.ID, got.Moved, got.Identity.EnvironmentKnown, old.ID)
			}
		})
	}
}

func TestRevive_unreadCountsOnlyUnexpiredMail(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	old := expireToDormant(t, s, clk, "subflux-dev")
	deliverEvents(t, s, old.ID, 1)
	putRows(t, s, func(w *wtx) error {
		return s.t.deliveries.table.Put(w.tx, pairKey(old.ID, 2), &deliveryRow{
			Agent: old.ID, Seq: 2, Kind: kindIdentityExpiring, Lease: lease{Expires: clk.now().Add(time.Minute)},
		})
	})
	clk.advance(time.Hour)
	if got := mustRevive(t, s, revival(sessA, "", envA())); got.Unread != 1 {
		t.Errorf("unread %d, want 1: the delivery past its expiry is gone", got.Unread)
	}
}

func TestRevive_leaseAndWhereRequired(t *testing.T) {
	tests := []struct {
		edit func(*Revival)
		name string
		want string
	}{
		{name: "no where", edit: func(r *Revival) { r.Where = nil }, want: "where is required"},
		{name: "no ttl", edit: func(r *Revival) { r.TTL = 0 }, want: "a ttl is required: pass --ttl <d>, from 15m to 24h"},
		{name: "ttl under the bounds", edit: func(r *Revival) { r.TTL = 14 * time.Minute }, want: "from 15m to 24h"},
		{name: "ttl over the bounds", edit: func(r *Revival) { r.TTL = 25 * time.Hour }, want: "from 15m to 24h"},
		{name: "bad machine", edit: func(r *Revival) { r.Where.Machine = "dmi:xyz" }, want: "where.machine"},
		{name: "checkout outside the paths", edit: func(r *Revival) { r.Where.Checkouts = []Checkout{co("/workspace/other", "1")} }, want: "where.checkouts[0].repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, clk, _ := openIdentityStore(t)
			old := expireToDormant(t, s, clk, "subflux-dev")
			r := revival(sessA, "", envA())
			tt.edit(&r)
			_, err := s.Revive(t.Context(), &r)
			wantRefusal(t, err, KindInvalid, "", tt.want)
			if row := getIdentity(t, s, old.ID); row.State != stateDormant {
				t.Errorf("state %q, want the refused revive to leave it dormant", row.State)
			}
		})
	}
}

func TestRevive_sameSessionRefusals(t *testing.T) {
	t.Run("no dormant identity", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		_, err := s.Revive(t.Context(), new(revival(sessA, "", envA())))
		wantRefusal(t, err, KindNotFound, "", "this session has no dormant identity")
	})
	t.Run("a live identity", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		mustRegister(t, s, registration(sessA, "subflux-dev", envA()))
		_, err := s.Revive(t.Context(), new(revival(sessA, "", envA())))
		wantRefusal(t, err, KindConflict, "", "this session already has the live identity subflux-dev")
	})
}

func TestRevive_byNameMovesItWithinItsEnvironmentAndProvider(t *testing.T) {
	tests := []struct {
		stored, caller Where
		name           string
	}{
		{name: "one machine", stored: envA(), caller: envA()},
		{name: "a cloned VM sending one machine", stored: envA(), caller: Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "7")}}},
		{name: "the caller's machine unknown", stored: envA(), caller: Where{Environment: hex32("a")}},
		{name: "two containers on one home volume", stored: Where{Environment: hex32("a")}, caller: Where{Environment: hex32("a")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, clk, sink := openIdentityStore(t)
			r := registration(sessA, "subflux-dev", tt.stored)
			r.TTL = 15 * time.Minute
			old := mustRegister(t, s, r)
			clk.advance(15 * time.Minute)
			mustSweep(t, s)

			got := mustRevive(t, s, revival(sessB, "subflux-dev", tt.caller))
			if got.Identity.ID != old.ID || !got.Moved || got.Identity.State != stateLive {
				t.Errorf("id %d moved %v state %q, want %d moved and live", got.Identity.ID, got.Moved, got.Identity.State, old.ID)
			}
			if me, err := s.Seen(t.Context(), sessB); err != nil || me.ID != old.ID {
				t.Errorf("Seen(sessB) = %d, %v; want the revived identity", me.ID, err)
			}
			_, err := s.Seen(t.Context(), sessA)
			wantRefusal(t, err, KindGone, "none")
			if n := len(sink.lines(slog.LevelInfo, "move refused")); n != 0 {
				t.Errorf("%d move refused lines, want none", n)
			}
		})
	}
}

func TestRevive_byNameRefusalsInOrder(t *testing.T) {
	other := func(m, env string) Where { return Where{Machine: m, Environment: env} }
	tests := []struct {
		setup   func(t *testing.T, s *Store)
		caller  Where
		name    string
		session string
		target  string
		why     string
		want    string
		kind    Kind
	}{
		{
			name: "a live identity before no environment", session: sessB, target: "subflux-dev", caller: other(dmi("1"), ""),
			setup: func(t *testing.T, s *Store) { mustRegister(t, s, registration(sessB, "other-dev", envA())) },
			kind:  KindConflict, want: "this session already has the live identity other-dev",
		},
		{
			name: "no environment before an unknown name", session: sessB, target: "ghost-dev", caller: other(dmi("1"), ""),
			kind: KindConflict, why: "no_environment", want: "this session has no environment id; register a fresh identity",
		},
		{
			name: "an unknown name", session: sessB, target: "ghost-dev", caller: envA(),
			kind: KindNotFound, want: "no dormant identity ghost-dev",
		},
		{
			name: "a live name", session: sessB, target: "live-dev", caller: envA(),
			setup: func(t *testing.T, s *Store) { mustRegister(t, s, registration(sessC, "live-dev", envA())) },
			kind:  KindNotFound, want: "no dormant identity live-dev",
		},
		{
			name: "a matching host label in another environment", session: sessB, target: "subflux-dev", caller: other(dmi("1"), hex32("b")),
			kind: KindNotFound, why: "other_environment", want: "no dormant identity subflux-dev in this environment (same machine, other environment)",
		},
		{
			name: "a copied environment on another machine", session: sessB, target: "subflux-dev", caller: other(dmi("2"), hex32("a")),
			kind: KindNotFound, why: "other_environment", want: "no dormant identity subflux-dev in this environment (other machine)",
		},
		{
			name: "a copied environment on a machine of another kind", session: sessB, target: "subflux-dev", caller: other(machineID("1"), hex32("a")),
			kind: KindNotFound, why: "other_environment", want: "no dormant identity subflux-dev in this environment (unknown)",
		},
		{
			name: "another provider", session: "claude-code:session-b", target: "subflux-dev", caller: envA(),
			kind: KindConflict, why: "other_provider",
			want: "subflux-dev belongs to provider kiro-cli in this environment; a revive moves an identity only within its own provider: choose another name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, clk, sink := openIdentityStore(t)
			old := expireToDormant(t, s, clk, "subflux-dev")
			if tt.setup != nil {
				tt.setup(t, s)
			}
			_, err := s.Revive(t.Context(), new(revival(tt.session, tt.target, tt.caller)))
			wantRefusal(t, err, tt.kind, tt.why, tt.want)
			lines := sink.lines(slog.LevelInfo, "move refused")
			if tt.why == "" && len(lines) != 0 {
				t.Errorf("%d move refused lines, want none", len(lines))
			}
			if tt.why != "" && (len(lines) != 1 || attr(lines[0], "why") != tt.why || attr(lines[0], "verb") != "revive") {
				t.Errorf("move refused lines %v, want one revive line with why %s", lines, tt.why)
			}
			if row := getIdentity(t, s, old.ID); row.State != stateDormant || row.Session != sessA {
				t.Errorf("state %q session %q, want it dormant on its own session", row.State, row.Session)
			}
		})
	}
}

func TestRevive_byNameDetachesTheCallersOwnDormantIdentity(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	target := expireToDormant(t, s, clk, "subflux-dev")
	r := registration(sessB, "other-dev", envA())
	r.TTL = 15 * time.Minute
	own := mustRegister(t, s, r)
	clk.advance(15 * time.Minute)
	mustSweep(t, s)

	if got := mustRevive(t, s, revival(sessB, "subflux-dev", envA())); got.Identity.ID != target.ID {
		t.Fatalf("revived %d, want %d", got.Identity.ID, target.ID)
	}
	row := getIdentity(t, s, own.ID)
	if row.State != stateDormant || row.Session != "" {
		t.Errorf("state %q session %q, want the caller's own row dormant and detached", row.State, row.Session)
	}
	if got := mustRevive(t, s, revival(sessC, "other-dev", envA())); got.Identity.ID != own.ID {
		t.Errorf("revived %d, want the detached row %d still revivable by name", got.Identity.ID, own.ID)
	}
}

func TestRevive_aFreshRegistrationLeavesTheDormantRowRevivableByName(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	old := expireToDormant(t, s, clk, "subflux-dev")
	mustRegister(t, s, registration(sessA, "subflux-two", envA()))
	_, err := s.Revive(t.Context(), new(revival(sessA, "subflux-dev", envA())))
	wantRefusal(t, err, KindConflict, "", "this session already has the live identity subflux-two")
	if got := mustRevive(t, s, revival(sessB, "subflux-dev", envA())); got.Identity.ID != old.ID || !got.Moved {
		t.Errorf("revived %d moved %v, want %d moved to the new session", got.Identity.ID, got.Moved, old.ID)
	}
}

func TestRevive_mergesTheSentCheckout(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	stored := Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "1"), co(repoGrelot, "2")}}
	r := registration(sessA, "subflux-dev", stored, repoSubflux, repoGrelot)
	r.TTL = 15 * time.Minute
	old := mustRegister(t, s, r)
	clk.advance(15 * time.Minute)
	sent := Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoGrelot, "3")}}
	mustRevive(t, s, revival(sessA, "", sent))
	got := byRepo(getIdentity(t, s, old.ID).Where.Checkouts)
	if want := []Checkout{co(repoGrelot, "3"), co(repoSubflux, "1")}; !slices.Equal(got, want) {
		t.Errorf("checkouts %v, want %v", got, want)
	}
}

func TestRevive_runsTheDueExpiriesItself(t *testing.T) {
	t.Run("due dormant, no sweep", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		r := registration(sessA, "subflux-dev", envA())
		r.TTL = 15 * time.Minute
		old := mustRegister(t, s, r)
		clk.advance(15 * time.Minute)
		if got := mustRevive(t, s, revival(sessA, "", envA())); got.Identity.ID != old.ID {
			t.Errorf("revived %d, want %d", got.Identity.ID, old.ID)
		}
	})
	t.Run("past the purge, no sweep", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		r := registration(sessA, "subflux-dev", envA())
		r.TTL = 15 * time.Minute
		mustRegister(t, s, r)
		clk.advance(15*time.Minute + 72*time.Hour)
		_, err := s.Revive(t.Context(), new(revival(sessB, "subflux-dev", envA())))
		wantRefusal(t, err, KindNotFound, "", "no dormant identity subflux-dev")
	})
}

func TestTakeOverHint(t *testing.T) {
	const proc = "4242@1730700000"
	tests := []struct {
		caller  Where
		name    string
		session string
		proc    string
		want    string
	}{
		{name: "same proc, environment and provider", session: sessB, proc: proc, caller: envA(), want: "subflux-dev"},
		{name: "the caller's machine unknown", session: sessB, proc: proc, caller: Where{Environment: hex32("a")}, want: "subflux-dev"},
		{name: "another proc", session: sessB, proc: "4243@1730700000", caller: envA()},
		{name: "a proc the identity's starts with", session: sessB, proc: "4242@173070000", caller: envA()},
		{name: "no proc", session: sessB, caller: envA()},
		{name: "another environment", session: sessB, proc: proc, caller: Where{Machine: dmi("1"), Environment: hex32("b")}},
		{name: "a copied environment on another machine", session: sessB, proc: proc, caller: Where{Machine: dmi("2"), Environment: hex32("a")}},
		{name: "no environment", session: sessB, proc: proc, caller: Where{Machine: dmi("1")}},
		{name: "another provider", session: "claude-code:session-b", proc: proc, caller: envA()},
		{name: "the identity's own session", session: sessA, proc: proc, caller: envA()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, _ := openIdentityStore(t)
			r := registration(sessA, "subflux-dev", envA())
			r.Proc = proc
			mustRegister(t, s, r)
			got, found, err := s.TakeOverHint(t.Context(), tt.session, tt.proc, tt.caller)
			if err != nil {
				t.Fatal(err)
			}
			if found != (tt.want != "") || got.Name != tt.want {
				t.Errorf("hint %q found %v, want %q", got.Name, found, tt.want)
			}
		})
	}
}

func TestTakeOverHint_dormantAndLive(t *testing.T) {
	const proc = "4242@1730700000"
	s, clk, _ := openIdentityStore(t)
	r := registration(sessA, "subflux-dev", envA())
	r.Proc, r.TTL = proc, 15*time.Minute
	mustRegister(t, s, r)
	clk.advance(15 * time.Minute)

	got, found, err := s.TakeOverHint(t.Context(), sessB, proc, envA())
	if err != nil || !found || got.Name != "subflux-dev" || got.State != stateDormant {
		t.Errorf("hint %q state %q found %v err %v, want the dormant subflux-dev", got.Name, got.State, found, err)
	}

	fresh := registration(sessB, "subflux-two", envA())
	fresh.Proc = proc
	mustRegister(t, s, fresh)
	got, found, err = s.TakeOverHint(t.Context(), sessC, proc, envA())
	if err != nil || !found || got.Name != "subflux-two" {
		t.Errorf("hint %q found %v err %v, want the live subflux-two before the dormant one", got.Name, found, err)
	}

	clk.advance(72 * time.Hour)
	if got, found, err = s.TakeOverHint(t.Context(), sessC, proc, envA()); err != nil || found && got.Name == "subflux-dev" {
		t.Errorf("hint %q found %v err %v, want the purged identity never offered", got.Name, found, err)
	}
}
