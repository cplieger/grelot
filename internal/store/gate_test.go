//go:build !windows

package store

import (
	"testing"
	"time"
)

// plain is a request from name's own session that names neither a handle
// nor --self, as a subagent that forgot `as` sends it.
func plain(name string) Caller { return Caller{Session: sess(name)} }

func self(name string) Caller { return Caller{Session: sess(name), Self: true} }

// wantGate fails unless err is the delegate gate's 409 naming handle.
func wantGate(t *testing.T, err error, handle string) {
	t.Helper()
	wantRefusal(t, err, KindConflict, "delegate_gate", "this session has delegates (@"+handle, "grelot as <its handle>", "--self")
}

// gateFixture registers parent and peer, with peer's direct post waiting
// in parent's inbox, and grants parent the delegate label review, leaving
// the link unused.
func gateFixture(t *testing.T) (s *Store, sink logSink, parent Identity, link string) {
	t.Helper()
	s, _, sink = openIdentityStore(t)
	parent = regAt(t, s, "parent", repoSubflux)
	regAt(t, s, "peer", repoGrelot)
	direct(t, s, "peer", "parent", 1)
	return s, sink, parent, mustGrant(t, s, grantFrom("parent", "review")).Link
}

func TestGate_unnamedRequestsFromASessionHoldingALinkMoveNothing(t *testing.T) {
	s, _, parent, _ := gateFixture(t)
	if err := s.EndSession(t.Context(), sess("parent")); err != nil {
		t.Fatal(err)
	}
	before := getIdentity(t, s, parent.ID)
	t.Run("inbox", func(t *testing.T) {
		_, err := s.Inbox(t.Context(), plain("parent"), 0)
		wantGate(t, err, "parent/review")
		_, _, err = s.Ack(t.Context(), Keyed{}, plain("parent"), before.Seq)
		wantGate(t, err, "parent/review")
	})
	t.Run("post", func(t *testing.T) {
		_, _, err := s.Post(t.Context(), Keyed{}, new(posting(sess("parent"), Target{Agent: "peer"})))
		wantGate(t, err, "parent/review")
	})
	t.Run("set", func(t *testing.T) {
		_, _, err := s.Set(t.Context(), Keyed{}, plain("parent"), Change{Fields: Fields{Description: "a subagent's task"}})
		wantGate(t, err, "parent/review")
	})
	t.Run("bye", func(t *testing.T) {
		_, err := s.Retire(t.Context(), Keyed{}, plain("parent"))
		wantGate(t, err, "parent/review")
	})
	t.Run("register", func(t *testing.T) {
		_, _, err := s.Register(t.Context(), Keyed{}, new(registration(sess("parent"), "parent", envA())))
		wantGate(t, err, "parent/review")
	})
	t.Run("nothing moved", func(t *testing.T) {
		after := getIdentity(t, s, parent.ID)
		if after.Read != before.Read || after.Description != before.Description || !after.Ended || !after.LastSeen.Equal(before.LastSeen) {
			t.Errorf("parent row read %d desc %q ended %v last seen %v, want %d %q true %v as before",
				after.Read, after.Description, after.Ended, after.LastSeen, before.Read, before.Description, before.LastSeen)
		}
		peer, _ := s.Seen(t.Context(), self("peer"))
		if ds := deliveriesTo(t, s, peer.ID); len(ds) != 0 {
			t.Errorf("peer has %d deliveries, want none from a refused post", len(ds))
		}
	})
}

func TestGate_selfPasses(t *testing.T) {
	s, _, parent, _ := gateFixture(t)
	page, err := s.Inbox(t.Context(), self("parent"), 0)
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("Inbox with --self: %d entries, %v; want peer's one post", len(page.Entries), err)
	}
	if _, _, err := s.Ack(t.Context(), Keyed{}, self("parent"), page.Entries[0].Seq); err != nil {
		t.Errorf("Ack with --self: %v", err)
	}
	if got := mustPost(t, s, selfPost("parent", Target{Agent: "peer"})); got.Delivered != 1 {
		t.Errorf("post with --self delivered %d, want 1", got.Delivered)
	}
	if got, _, err := s.Register(t.Context(), Keyed{}, &Registration{
		Session: sess("parent"), Self: true, Where: new(envA()), TTL: time.Hour,
		Fields: Fields{Name: "parent", Description: "moving the run surface", Paths: []string{repoSubflux}},
	}); err != nil || !got.Existing || got.Identity.ID != parent.ID {
		t.Errorf("register with --self: %+v, %v; want the parent returned unchanged", got, err)
	}
	if _, err := s.Retire(t.Context(), Keyed{}, self("parent")); err != nil {
		t.Errorf("bye with --self: %v", err)
	}
}

func TestGate_aDelegateRegisteredFromTheSessionHoldsIt(t *testing.T) {
	s, _, _, link := gateFixture(t)
	d := mustRegisterDelegate(t, s, sess("parent"), link)
	_, err := s.Inbox(t.Context(), plain("parent"), 0)
	wantGate(t, err, d.Name)
	if got, err := s.Seen(t.Context(), as(sess("parent"), d.Name)); err != nil || got.ID != d.ID {
		t.Errorf("as %s: %d, %v; want the delegate %d", d.Name, got.ID, err, d.ID)
	}
	t.Run("lifts when the last delegate ends", func(t *testing.T) {
		if _, err := s.Retire(t.Context(), Keyed{}, as(sess("parent"), d.Name)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Inbox(t.Context(), plain("parent"), 0); err != nil {
			t.Errorf("Inbox once the delegate ended: %v, want the gate lifted", err)
		}
	})
}

func TestGate_registerDelegatePasses(t *testing.T) {
	s, _, _, link := gateFixture(t)
	mustGrant(t, s, grantFrom("parent", "second"))
	if d := mustRegisterDelegate(t, s, sess("parent"), link); d.Name != "parent/review" {
		t.Errorf("registered %q from the gated session, want parent/review", d.Name)
	}
}

func TestGate_liftsWhenADelegateUsesTheLastLinkFromItsOwnSession(t *testing.T) {
	s, _, _, link := gateFixture(t)
	d := mustRegisterDelegate(t, s, "codex:thread-1", link)
	if _, err := s.Inbox(t.Context(), plain("parent"), 0); err != nil {
		t.Errorf("Inbox with the only delegate on its own session: %v, want no gate", err)
	}
	if got, err := s.Seen(t.Context(), Caller{Session: "codex:thread-1"}); err != nil || got.ID != d.ID {
		t.Errorf("the delegate's own session: %d, %v; want %d with no header", got.ID, err, d.ID)
	}
}

func TestGate_anExpiredLinkHoldsNothing(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "parent", repoSubflux)
	g := grantFrom("parent", "review")
	g.TTL = 30 * time.Minute
	mustGrant(t, s, g)
	clk.advance(30 * time.Minute)
	if _, err := s.Inbox(t.Context(), plain("parent"), 0); err != nil {
		t.Errorf("Inbox once the only link expired, before the sweep: %v, want no gate", err)
	}
}

func TestGate_aDelegatePastItsLeaseHoldsNothing(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "parent", repoSubflux)
	g := grantFrom("parent", "review")
	g.TTL = 30 * time.Minute
	mustRegisterDelegate(t, s, sess("parent"), mustGrant(t, s, g).Link)
	clk.advance(30 * time.Minute)
	if _, err := s.Inbox(t.Context(), plain("parent"), 0); err != nil {
		t.Errorf("Inbox once the only delegate's lease ran out, before the sweep: %v, want no gate", err)
	}
}

func TestGate_aSessionThatNeverGrantedPaysNothing(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "parent", repoSubflux)
	if _, err := s.Inbox(t.Context(), plain("parent"), 0); err != nil {
		t.Errorf("Inbox: %v", err)
	}
	if _, err := s.Seen(t.Context(), self("parent")); err != nil {
		t.Errorf("Seen with --self and no delegates: %v, want it to act as the identity", err)
	}
}
