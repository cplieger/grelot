//go:build !windows

package store

import (
	"strings"
	"testing"
	"time"
)

func TestDelegate_parentSessionEndEndsTheDelegatesAndKeepsTheParent(t *testing.T) {
	s, _, sink := openIdentityStore(t)
	p, shared := delegateOf(t, s, "parent", "review")
	own := mustRegisterDelegate(t, s, "codex:thread-1", mustGrant(t, s, grantFrom("parent", "codex")).Link)
	if err := s.EndSession(t.Context(), sess("parent")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []Identity{shared, own} {
		if identityExists(t, s, d.ID) {
			t.Errorf("delegate %s kept after its parent's SessionEnd, want it deleted", d.Name)
		}
		if got := endedWhy(sink, d.Name); len(got) != 1 || got[0] != "session_end" {
			t.Errorf("delegate ended lines for %s %v, want one session_end", d.Name, got)
		}
	}
	if row, found := findIdentity(t, s, p.ID); !found || row.State != stateLive || !row.Ended {
		t.Errorf("parent found %v state %q ended %v, want it live and ended", found, row.State, row.Ended)
	}
}

func TestDelegate_revoke(t *testing.T) {
	t.Run("ends the delegate and the unused link of its label", func(t *testing.T) {
		s, _, sink := openIdentityStore(t)
		p, d := delegateOf(t, s, "parent", "review")
		link := mustGrant(t, s, Grant{Caller: self("parent"), Label: "next", TTL: time.Hour}).Link
		got, err := s.Revoke(t.Context(), Keyed{}, self("parent"), "review")
		if err != nil || len(got.Handles) != 1 || got.Handles[0] != d.Name || got.Link {
			t.Fatalf("Revoke(review): %+v, %v; want the delegate %s and no link", got, err, d.Name)
		}
		if identityExists(t, s, d.ID) {
			t.Error("revoked delegate's row kept, want it deleted, never dormant")
		}
		if why := endedWhy(sink, d.Name); len(why) != 1 || why[0] != "revoke" {
			t.Errorf("delegate ended lines %v, want one revoke", why)
		}
		got, err = s.Revoke(t.Context(), Keyed{}, self("parent"), "next")
		if err != nil || len(got.Handles) != 0 || !got.Link {
			t.Fatalf("Revoke(next): %+v, %v; want only the unused link", got, err)
		}
		_, _, err = s.Register(t.Context(), Keyed{}, new(delegateReg(sess("parent"), link)))
		wantRefusal(t, err, KindGone, "link_gone", "")
		if !identityExists(t, s, p.ID) {
			t.Error("parent deleted by a revoke")
		}
	})
	t.Run("a label with a delegate and an unused link", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		_, d := delegateOf(t, s, "parent", "review")
		if _, err := s.Set(t.Context(), Keyed{}, self("parent"), Change{Fields: Fields{Name: "renamed"}}); err != nil {
			t.Fatal(err)
		}
		link := mustGrant(t, s, Grant{Caller: self("parent"), Label: "review", TTL: time.Hour}).Link
		got, err := s.Revoke(t.Context(), Keyed{}, self("parent"), "review")
		if err != nil || len(got.Handles) != 1 || got.Handles[0] != d.Name || !got.Link {
			t.Fatalf("Revoke(review): %+v, %v; want %s and the unused link", got, err, d.Name)
		}
		_, _, err = s.Register(t.Context(), Keyed{}, new(delegateReg(sess("parent"), link)))
		wantRefusal(t, err, KindGone, "link_gone", "")
	})
	t.Run("a label with neither", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		regAt(t, s, "parent", repoSubflux)
		_, err := s.Revoke(t.Context(), Keyed{}, self("parent"), "review")
		wantRefusal(t, err, KindNotFound, "", "review")
	})
	t.Run("from a delegate", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		_, d := delegateOf(t, s, "parent", "review")
		_, err := s.Revoke(t.Context(), Keyed{}, as(sess("parent"), d.Name), "review")
		wantRefusal(t, err, KindForbidden, "", "")
	})
}

func TestDelegate_subagentStop(t *testing.T) {
	t.Run("the session's own delegate", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		regAt(t, s, "parent", repoSubflux)
		d := mustRegisterDelegate(t, s, "codex:thread-1", mustGrant(t, s, grantFrom("parent", "codex")).Link)
		if err := s.SubagentStop(t.Context(), "codex:thread-1"); err != nil {
			t.Fatal(err)
		}
		if identityExists(t, s, d.ID) {
			t.Error("delegate kept after its own session's SubagentStop")
		}
	})
	t.Run("the parent's session ends none of its delegates", func(t *testing.T) {
		s, sink, _, _ := gateFixture(t)
		d := mustRegisterDelegate(t, s, sess("parent"), mustGrant(t, s, grantFrom("parent", "bound")).Link)
		lines := len(*sink.records)
		if err := s.SubagentStop(t.Context(), sess("parent")); err != nil {
			t.Errorf("SubagentStop from the parent's session: %v, want no refusal", err)
		}
		if n := len(*sink.records) - lines; n != 0 {
			t.Errorf("%d log lines, want none", n)
		}
		if !identityExists(t, s, d.ID) {
			t.Error("the parent's SubagentStop ended its delegate")
		}
	})
}

func TestDelegate_idleForAnHourSurvivesToItsLease(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "parent", repoSubflux)
	g := grantFrom("parent", "review")
	g.TTL = 3 * time.Hour
	d := mustRegisterDelegate(t, s, sess("parent"), mustGrant(t, s, g).Link)
	clk.advance(time.Hour + time.Minute)
	mustSweep(t, s)
	if !identityExists(t, s, d.ID) {
		t.Fatal("delegate ended after an hour with no request, want it kept to its lease")
	}
	mustPost(t, s, asPost("parent", d.Name, Target{Agent: "parent"}))
	if _, err := s.Inbox(t.Context(), as(sess("parent"), d.Name), 0); err != nil {
		t.Fatal(err)
	}
	if row, _ := findIdentity(t, s, d.ID); !row.Lease.Expires.Equal(d.Expires) {
		t.Errorf("expires %v after a post and an inbox read, want %v unmoved", row.Lease.Expires, d.Expires)
	}
	clk.set(d.Expires)
	mustSweep(t, s)
	if identityExists(t, s, d.ID) {
		t.Error("delegate kept past its lease")
	}
}

func TestDelegate_renewal(t *testing.T) {
	setup := func(t *testing.T) (*Store, *clock, Identity, Identity) {
		t.Helper()
		s, clk, _ := openIdentityStore(t)
		p := regAt(t, s, "parent", repoSubflux)
		g := grantFrom("parent", "review")
		g.TTL = time.Hour
		d := mustRegisterDelegate(t, s, sess("parent"), mustGrant(t, s, g).Link)
		clk.advance(30 * time.Minute)
		return s, clk, p, d
	}
	t.Run("by its parent through as", func(t *testing.T) {
		s, clk, _, d := setup(t)
		got, err := s.Set(t.Context(), Keyed{}, as(sess("parent"), d.Name), Change{TTL: 2 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if want := clk.now().Add(2 * time.Hour); !got.Expires.Equal(want) || got.Notice != "" {
			t.Errorf("expires %v notice %q, want now + 2 h, %v, and no notice", got.Expires, got.Notice, want)
		}
		if warn, want := getIdentity(t, s, got.ID).Lease.WarnAt, got.Expires.Add(-30*time.Minute); !warn.Equal(want) {
			t.Errorf("warn_at %v, want min(30 min, ttl/2) before expiry, %v", warn, want)
		}
	})
	t.Run("by itself, capped at its parent's expiry", func(t *testing.T) {
		s, _, p, _ := setup(t)
		own := mustRegisterDelegate(t, s, "codex:thread-1", mustGrant(t, s, grantFrom("parent", "codex")).Link)
		got, err := s.Set(t.Context(), Keyed{}, Caller{Session: "codex:thread-1"}, Change{TTL: 24 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if !got.Expires.Equal(p.Expires) {
			t.Errorf("expires %v, want capped at the parent's %v", got.Expires, p.Expires)
		}
		if !strings.Contains(got.Notice, "@parent renews first") {
			t.Errorf("notice %q, want it to say the parent renews first", got.Notice)
		}
		if got.ID != own.ID {
			t.Errorf("renewed %d, want the delegate %d", got.ID, own.ID)
		}
	})
	t.Run("outside the bounds", func(t *testing.T) {
		s, _, _, d := setup(t)
		_, err := s.Set(t.Context(), Keyed{}, as(sess("parent"), d.Name), Change{TTL: 14 * time.Minute})
		wantRefusal(t, err, KindInvalid, "", "15m")
	})
}

func TestDelegate_aParentsShorterLeaseCapsItsDelegates(t *testing.T) {
	t.Run("a renewal", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		p, d := delegateOf(t, s, "parent", "review")
		w := envA()
		renewed, err := s.Set(t.Context(), Keyed{}, self("parent"), Change{TTL: time.Hour, Where: &w})
		if err != nil {
			t.Fatal(err)
		}
		row := getIdentity(t, s, d.ID)
		if !row.Lease.Expires.Equal(renewed.Expires) {
			t.Errorf("delegate expires %v, want the parent's %v", row.Lease.Expires, renewed.Expires)
		}
		if want := renewed.Expires.Add(-30 * time.Minute); !row.Lease.WarnAt.Equal(want) {
			t.Errorf("delegate warn_at %v, want min(30 min, ttl/2) before the new expiry, %v", row.Lease.WarnAt, want)
		}
		clk.advance(30*time.Minute - time.Second)
		mustSweep(t, s)
		if got := eventsOf(t, s, d.ID, kindIdentityExpiring); len(got) != 0 {
			t.Errorf("warned a second early: %+v", got)
		}
		clk.advance(time.Second)
		mustSweep(t, s)
		if got := eventsOf(t, s, d.ID, kindIdentityExpiring); len(got) != 1 {
			t.Errorf("delegate got %d identity_expiring, want 1", len(got))
		}
		if got := eventsOf(t, s, p.ID, kindIdentityExpiring); len(got) != 1 {
			t.Errorf("parent got %d identity_expiring, want 1", len(got))
		}
	})
	t.Run("a longer renewal moves nothing", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		_, d := delegateOf(t, s, "parent", "review")
		before := getIdentity(t, s, d.ID).Lease
		w := envA()
		if _, err := s.Set(t.Context(), Keyed{}, self("parent"), Change{TTL: 24 * time.Hour, Where: &w}); err != nil {
			t.Fatal(err)
		}
		if got := getIdentity(t, s, d.ID).Lease; got != before {
			t.Errorf("delegate lease %+v, want it unchanged, %+v", got, before)
		}
	})
	t.Run("a take-over's lease", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		_, d := delegateOf(t, s, "parent", "review")
		r := registration(sessB, "parent", envA(), repoSubflux)
		r.TakeOver, r.TTL = true, 20*time.Minute
		moved := mustRegister(t, s, r)
		row := getIdentity(t, s, d.ID)
		if !row.Lease.Expires.Equal(moved.Expires) {
			t.Errorf("delegate expires %v, want the parent's %v", row.Lease.Expires, moved.Expires)
		}
		if !row.Lease.WarnAt.Equal(clk.now()) {
			t.Errorf("delegate warn_at %v, want now, since its lead passes the new expiry", row.Lease.WarnAt)
		}
	})
}

func TestDelegate_expiryWarningNamesTheRenewalAsItsHandle(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	_, d := delegateOf(t, s, "parent", "review")
	clk.advance(90 * time.Minute)
	mustSweep(t, s)
	got := eventsOf(t, s, d.ID, kindIdentityExpiring)
	if len(got) != 1 {
		t.Fatalf("%d identity_expiring, want 1", len(got))
	}
	if want := "run `grelot as parent/review renew --ttl=2h`"; !strings.Contains(got[0].Text, want) {
		t.Errorf("text %q lacks %q", got[0].Text, want)
	}
}

func TestDelegate_standingWaitIsRefused(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	p, d := delegateOf(t, s, "parent", "review")
	mustRegisterDelegate(t, s, "codex:thread-1", mustGrant(t, s, grantFrom("parent", "codex")).Link)
	for name, c := range map[string]Caller{"as its handle": as(sess("parent"), d.Name), "from its own session": {Session: "codex:thread-1"}} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Watcher(t.Context(), c)
			wantRefusal(t, err, KindInvalid, "", "a delegate is pull only; use grelot wait")
		})
	}
	t.Run("the parent's wait holds, and leaves ended set", func(t *testing.T) {
		if err := s.EndSession(t.Context(), sess("parent")); err != nil {
			t.Fatal(err)
		}
		got, err := s.Watcher(t.Context(), self("parent"))
		if err != nil || got.ID != p.ID || !got.Ended {
			t.Errorf("Watcher(parent): %d ended %v, %v; want %d with ended still set", got.ID, got.Ended, err, p.ID)
		}
		if row, _ := findIdentity(t, s, p.ID); !row.Ended {
			t.Error("a standing wait cleared ended")
		}
	})
}
