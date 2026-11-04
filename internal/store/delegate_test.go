package store

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// grantFrom is a grant of label from name's own session, with a 2 h ttl.
func grantFrom(name, label string) Grant {
	return Grant{Caller: Caller{Session: sess(name), Self: true}, Label: label, TTL: 2 * time.Hour}
}

func mustGrant(t *testing.T, s *Store, g Grant) Granted {
	t.Helper()
	got, err := s.Delegate(t.Context(), &g)
	if err != nil {
		t.Fatalf("Delegate(%s, %s): %v", g.Caller.Session, g.Label, err)
	}
	return got
}

// delegateReg is a register --delegate request from session.
func delegateReg(session, link string) Registration {
	return Registration{Session: session, Link: link, Fields: Fields{Description: "review the diff"}}
}

func mustRegisterDelegate(t *testing.T, s *Store, session, link string) Identity {
	t.Helper()
	r := delegateReg(session, link)
	got, err := s.Register(t.Context(), &r)
	if err != nil {
		t.Fatalf("Register(--delegate from %s): %v", session, err)
	}
	return got.Identity
}

// delegateOf registers parent and grants and registers its delegate label
// from parent's session; it returns both identities.
func delegateOf(t *testing.T, s *Store, parent, label string) (p, d Identity) {
	t.Helper()
	p = regAt(t, s, parent, repoSubflux)
	g := mustGrant(t, s, grantFrom(parent, label))
	return p, mustRegisterDelegate(t, s, sess(parent), g.Link)
}

// as is the caller that acts as handle from session.
func as(session, handle string) Caller { return Caller{Session: session, As: handle} }

// endedWhy lists the why of each delegate ended line for handle.
func endedWhy(sink logSink, handle string) []string {
	var out []string
	for _, r := range sink.lines(slog.LevelInfo, "delegate ended") {
		if attr(r, "handle") == handle {
			out = append(out, attr(r, "why"))
		}
	}
	return out
}

// outliveParent moves delegate id's expiry past its parent's, which no
// verb does, so only the parent's end can end it.
func outliveParent(t *testing.T, s *Store, id uint64) {
	t.Helper()
	editIdentity(t, s, id, func(r *identityRow) { r.Lease.Expires = r.Lease.Expires.Add(time.Hour) })
}

func identityExists(t *testing.T, s *Store, id uint64) bool {
	t.Helper()
	_, found := findIdentity(t, s, id)
	return found
}

func findIdentity(t *testing.T, s *Store, id uint64) (identityRow, bool) {
	t.Helper()
	var row identityRow
	var found bool
	putRows(t, s, func(w *wtx) error {
		var err error
		row, found, err = s.t.identities.table.Get(w.r, id)
		return err
	})
	return row, found
}

func TestDelegate_handleOfTwoFullNamesRegistersAndIsAddressable(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	parentName, label := strings.Repeat("p", 48), strings.Repeat("l", 48)
	parent := regAt(t, s, parentName, repoSubflux)
	g := mustGrant(t, s, grantFrom(parentName, label))
	handle := parentName + "/" + label
	if g.Handle != handle || len(handle) != 97 {
		t.Fatalf("handle %q (%d chars), want %q of 97", g.Handle, len(g.Handle), handle)
	}
	if !strings.HasPrefix(g.Link, "grelot-link:") || len(strings.TrimPrefix(g.Link, "grelot-link:")) != 26 {
		t.Errorf("link %q, want grelot-link: and 26 base32 characters", g.Link)
	}
	d := mustRegisterDelegate(t, s, sess(parentName), g.Link)
	if d.Name != handle {
		t.Errorf("delegate name %q, want %q", d.Name, handle)
	}
	p := posting(sess(parentName), Target{Agent: handle})
	p.Caller.Self = true
	got := mustPost(t, s, p)
	if got.Delivered != 1 {
		t.Errorf("delivered %d, want 1 to @%s", got.Delivered, handle)
	}
	if ds := deliveriesTo(t, s, d.ID); len(ds) != 1 || ds[0].Sender != parent.ID {
		t.Errorf("delegate deliveries %+v, want one from its parent", ds)
	}
}

func TestDelegate_registrationTakesTheGrantAndThePartsThatAreItsParents(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	parent := regAt(t, s, "subflux-dev", repoSubflux)
	g := mustGrant(t, s, grantFrom("subflux-dev", "review"))
	clk.advance(time.Minute)
	r := delegateReg(sess("subflux-dev"), g.Link)
	other := Where{Machine: dmi("9"), Environment: hex32("9")}
	r.Where = &other
	got, err := s.Register(t.Context(), &r)
	if err != nil {
		t.Fatal(err)
	}
	d := got.Identity
	row, _ := findIdentity(t, s, d.ID)
	t.Run("handle and description", func(t *testing.T) {
		if d.Name != "subflux-dev/review" || d.Description != "review the diff" || len(d.Paths) != 0 {
			t.Errorf("delegate %q %q paths %v, want the handle, the description and no paths", d.Name, d.Description, d.Paths)
		}
	})
	t.Run("where ignored", func(t *testing.T) {
		if row.Where.Machine != "" || row.Where.Environment != "" || len(row.Where.Checkouts) != 0 {
			t.Errorf("stored where %+v, want none: a delegate reads its parent's", row.Where)
		}
	})
	t.Run("parent and scope stored", func(t *testing.T) {
		if row.Parent != parent.ID || row.Scope == nil {
			t.Errorf("parent %d scope %v, want %d and a scope", row.Parent, row.Scope, parent.ID)
		}
	})
	t.Run("lease from registration", func(t *testing.T) {
		if want := clk.now().Add(2 * time.Hour); !d.Expires.Equal(want) {
			t.Errorf("expires %v, want now + the grant's 2 h, %v", d.Expires, want)
		}
	})
	t.Run("no session bound from the parent's", func(t *testing.T) {
		if row.Session != "" {
			t.Errorf("session %q, want none: the parent's session stays the parent's", row.Session)
		}
		me, err := s.Seen(t.Context(), Caller{Session: sess("subflux-dev"), Self: true})
		if err != nil || me.ID != parent.ID {
			t.Errorf("parent's session acts as %d (%v), want the parent %d", me.ID, err, parent.ID)
		}
	})
}

func TestDelegate_leaseNeverPassesTheParents(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	r := registration(sess("parent"), "parent", envA(), repoSubflux)
	r.TTL = time.Hour
	parent := mustRegister(t, s, r)
	g := grantFrom("parent", "worker")
	g.TTL = 3 * time.Hour
	got := mustGrant(t, s, g)
	d := mustRegisterDelegate(t, s, sess("parent"), got.Link)
	if !d.Expires.Equal(parent.Expires) {
		t.Errorf("delegate expires %v, want its parent's %v", d.Expires, parent.Expires)
	}
}

func TestDelegate_grantRefusals(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "parent", repoSubflux)
	regAt(t, s, "peer", repoGrelot)
	cases := []struct {
		edit func(*Grant)
		name string
		why  string
		part string
		kind Kind
	}{
		{name: "no ttl", edit: func(g *Grant) { g.TTL = 0 }, kind: KindInvalid, part: "a ttl is required"},
		{name: "ttl under 15 min", edit: func(g *Grant) { g.TTL = 14 * time.Minute }, kind: KindInvalid, part: "15m"},
		{name: "ttl over 24 h", edit: func(g *Grant) { g.TTL = 25 * time.Hour }, kind: KindInvalid, part: "24h"},
		{name: "bad label", edit: func(g *Grant) { g.Label = "Review" }, kind: KindInvalid, part: "label"},
		{name: "deny of the parent", edit: func(g *Grant) { g.DenyAgents = []string{"parent"} }, kind: KindInvalid, part: "deny"},
		{name: "unknown agent", edit: func(g *Grant) { g.AllowAgents = []string{"nobody"} }, kind: KindNotFound, part: "nobody"},
		{name: "bad thread", edit: func(g *Grant) { g.AllowThreads = []string{"relative/path"} }, kind: KindInvalid, part: "canonical"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := grantFrom("parent", "worker")
			c.edit(&g)
			_, err := s.Delegate(t.Context(), &g)
			wantRefusal(t, err, c.kind, c.why, c.part)
		})
	}
	t.Run("allow and deny resolved", func(t *testing.T) {
		g := grantFrom("parent", "scoped")
		g.AllowThreads = []string{"announce", repoGrelot}
		g.AllowAgents = []string{"peer"}
		mustGrant(t, s, g)
	})
}

func TestDelegate_handleHeldIsRefusedAtTheGrant(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	delegateOf(t, s, "parent", "review")
	_, err := s.Delegate(t.Context(), new(grantFrom("parent", "review")))
	wantRefusal(t, err, KindConflict, "", "parent/review", "held")
	t.Run("an unused link of the label", func(t *testing.T) {
		mustGrant(t, s, grantFrom("parent", "build"))
		_, err := s.Delegate(t.Context(), new(grantFrom("parent", "build")))
		wantRefusal(t, err, KindConflict, "", "unused link", "revoke build")
	})
}

func TestDelegate_aDelegateCannotGrant(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	_, d := delegateOf(t, s, "parent", "review")
	g := Grant{Caller: as(sess("parent"), d.Name), Label: "nested", TTL: time.Hour}
	_, err := s.Delegate(t.Context(), &g)
	wantRefusal(t, err, KindForbidden, "", "cannot grant")
}

func TestDelegate_linkIsSingleUse(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "parent", repoSubflux)
	g := mustGrant(t, s, grantFrom("parent", "review"))
	mustRegisterDelegate(t, s, sess("parent"), g.Link)
	_, err := s.Register(t.Context(), new(delegateReg(sess("parent"), g.Link)))
	wantRefusal(t, err, KindGone, "link_gone", "link already used")
	if n := countRows(t, s, s.t.links.table); n != 0 {
		t.Errorf("%d links stored, want the used one deleted", n)
	}
}

func TestDelegate_linkGoneAnswers410(t *testing.T) {
	t.Run("expired at the grant's ttl", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		regAt(t, s, "parent", repoSubflux)
		g := mustGrant(t, s, grantFrom("parent", "review"))
		if want := clk.now().Add(2 * time.Hour); !g.Expires.Equal(want) {
			t.Errorf("link expires %v, want the grant's 2 h, %v", g.Expires, want)
		}
		clk.advance(2 * time.Hour)
		_, err := s.Register(t.Context(), new(delegateReg(sess("parent"), g.Link)))
		wantRefusal(t, err, KindGone, "link_gone", "link already used")
	})
	t.Run("expired by the sweep with no warning", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		p := regAt(t, s, "parent", repoSubflux)
		mustGrant(t, s, grantFrom("parent", "review"))
		clk.advance(2*time.Hour - time.Second)
		mustSweep(t, s)
		if n := countRows(t, s, s.t.links.table); n != 1 {
			t.Fatalf("%d links before the expiry, want 1", n)
		}
		clk.advance(time.Second)
		mustSweep(t, s)
		if n := countRows(t, s, s.t.links.table); n != 0 {
			t.Errorf("%d links after the expiry, want 0", n)
		}
		if ds := deliveriesTo(t, s, p.ID); len(ds) != 0 {
			t.Errorf("parent deliveries %+v, want no warning for a link", ds)
		}
	})
	t.Run("parent retired", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		regAt(t, s, "parent", repoSubflux)
		g := mustGrant(t, s, grantFrom("parent", "review"))
		if err := s.Retire(t.Context(), Caller{Session: sess("parent"), Self: true}); err != nil {
			t.Fatal(err)
		}
		_, err := s.Register(t.Context(), new(delegateReg(sess("other"), g.Link)))
		wantRefusal(t, err, KindGone, "link_gone", "link already used")
	})
	t.Run("parent expired", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		r := registration(sess("parent"), "parent", envA(), repoSubflux)
		r.TTL = time.Hour
		mustRegister(t, s, r)
		g := mustGrant(t, s, grantFrom("parent", "review"))
		clk.advance(time.Hour)
		_, err := s.Register(t.Context(), new(delegateReg(sess("other"), g.Link)))
		wantRefusal(t, err, KindGone, "link_gone", "link already used")
	})
	t.Run("not a link", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		_, err := s.Register(t.Context(), new(delegateReg(sess("other"), "grelot-link:short")))
		wantRefusal(t, err, KindInvalid, "", "not a delegate link")
	})
}

func TestDelegate_registrationRefusesTheFieldsAGrantSets(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "parent", repoSubflux)
	g := mustGrant(t, s, grantFrom("parent", "review"))
	cases := []struct {
		edit func(*Registration)
		name string
		part string
	}{
		{name: "a name", edit: func(r *Registration) { r.Fields.Name = "mine" }, part: "handle"},
		{name: "paths", edit: func(r *Registration) { r.Fields.Paths = []string{repoSubflux} }, part: "scope replaces paths"},
		{name: "a ttl", edit: func(r *Registration) { r.TTL = time.Hour }, part: "grant's"},
		{name: "no description", edit: func(r *Registration) { r.Fields.Description = "" }, part: "description is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := delegateReg(sess("parent"), g.Link)
			c.edit(&r)
			_, err := s.Register(t.Context(), &r)
			wantRefusal(t, err, KindInvalid, "", c.part)
		})
	}
	t.Run("the link survives a refused registration", func(t *testing.T) {
		mustRegisterDelegate(t, s, sess("parent"), g.Link)
	})
}

func TestDelegate_caps(t *testing.T) {
	t.Run("32 unused links", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		regAt(t, s, "parent", repoSubflux)
		for i := range 32 {
			mustGrant(t, s, grantFrom("parent", fmt.Sprintf("w%02d", i)))
		}
		_, err := s.Delegate(t.Context(), new(grantFrom("parent", "w32")))
		wantRefusal(t, err, KindConflict, "", "32 unused delegate links")
	})
	t.Run("32 live delegates", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		regAt(t, s, "parent", repoSubflux)
		for i := range 32 {
			g := mustGrant(t, s, grantFrom("parent", fmt.Sprintf("w%02d", i)))
			mustRegisterDelegate(t, s, sess("parent"), g.Link)
		}
		_, err := s.Delegate(t.Context(), new(grantFrom("parent", "w32")))
		wantRefusal(t, err, KindConflict, "", "32 live delegates")
	})
}

func TestDelegate_capsCountNothingPastItsLeaseBeforeTheSweep(t *testing.T) {
	hourGrant := func(label string) Grant {
		g := grantFrom("parent", label)
		g.TTL = time.Hour
		return g
	}
	t.Run("an expired link of the label", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		regAt(t, s, "parent", repoSubflux)
		mustGrant(t, s, hourGrant("review"))
		clk.advance(time.Hour)
		mustGrant(t, s, hourGrant("review"))
	})
	t.Run("32 expired links", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		regAt(t, s, "parent", repoSubflux)
		for i := range 32 {
			mustGrant(t, s, hourGrant(fmt.Sprintf("w%02d", i)))
		}
		clk.advance(time.Hour)
		mustGrant(t, s, hourGrant("w32"))
	})
	t.Run("32 delegates past their lease, at the grant", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		regAt(t, s, "parent", repoSubflux)
		for i := range 32 {
			mustRegisterDelegate(t, s, sess("parent"), mustGrant(t, s, hourGrant(fmt.Sprintf("w%02d", i))).Link)
		}
		clk.advance(time.Hour)
		mustGrant(t, s, hourGrant("w32"))
	})
	t.Run("32 delegates past their lease, at the registration", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		regAt(t, s, "parent", repoSubflux)
		spare := grantFrom("parent", "spare")
		link := mustGrant(t, s, spare).Link
		for i := range 32 {
			mustRegisterDelegate(t, s, sess("parent"), mustGrant(t, s, hourGrant(fmt.Sprintf("w%02d", i))).Link)
		}
		clk.advance(time.Hour)
		if d := mustRegisterDelegate(t, s, sess("parent"), link); d.Name != "parent/spare" {
			t.Errorf("registered %q, want parent/spare", d.Name)
		}
	})
}

func TestDelegate_asIsAcceptedOnlyFromTheParentsSessionOrItsOwn(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "stranger", repoGrelot)
	_, d := delegateOf(t, s, "parent", "review")
	t.Run("parent's session", func(t *testing.T) {
		got, err := s.Seen(t.Context(), as(sess("parent"), d.Name))
		if err != nil || got.ID != d.ID {
			t.Errorf("Seen as %s from the parent's session: %d, %v; want the delegate %d", d.Name, got.ID, err, d.ID)
		}
	})
	t.Run("another session", func(t *testing.T) {
		_, err := s.Seen(t.Context(), as(sess("stranger"), d.Name))
		wantRefusal(t, err, KindForbidden, "", "parent's session or its own")
		_, err = s.Inbox(t.Context(), as(sess("stranger"), d.Name), 0)
		wantRefusal(t, err, KindForbidden, "", "parent's session or its own")
	})
	t.Run("with self", func(t *testing.T) {
		_, err := s.Seen(t.Context(), Caller{Session: sess("parent"), As: d.Name, Self: true})
		wantRefusal(t, err, KindInvalid, "", "--self")
	})
	t.Run("not a handle", func(t *testing.T) {
		_, err := s.Seen(t.Context(), as(sess("parent"), "stranger"))
		wantRefusal(t, err, KindInvalid, "", "not a delegate handle")
	})
	t.Run("a handle no delegate holds", func(t *testing.T) {
		_, err := s.Seen(t.Context(), as(sess("parent"), "parent/gone"))
		wantRefusal(t, err, KindGone, "delegate_ended", "ask your parent for a new link")
	})
}

func TestDelegate_registeredFromItsOwnSessionBindsIt(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "parent", repoSubflux)
	g := mustGrant(t, s, grantFrom("parent", "codex"))
	own := "codex:thread-1"
	d := mustRegisterDelegate(t, s, own, g.Link)
	got, err := s.Seen(t.Context(), Caller{Session: own})
	if err != nil || got.ID != d.ID {
		t.Errorf("Seen from the delegate's own session: %d, %v; want %d", got.ID, err, d.ID)
	}
	if got, err := s.Seen(t.Context(), as(own, d.Name)); err != nil || got.ID != d.ID {
		t.Errorf("Seen as its handle from its own session: %d, %v; want %d", got.ID, err, d.ID)
	}
	t.Run("not from a session with another live identity", func(t *testing.T) {
		g := mustGrant(t, s, grantFrom("parent", "other"))
		regAt(t, s, "busy", repoGrelot)
		_, err := s.Register(t.Context(), new(delegateReg(sess("busy"), g.Link)))
		wantRefusal(t, err, KindConflict, "", "busy")
	})
}

func TestDelegate_endsWithItsParent(t *testing.T) {
	t.Run("retirement, in that transaction", func(t *testing.T) {
		s, _, sink := openIdentityStore(t)
		p, d := delegateOf(t, s, "parent", "review")
		mustGrant(t, s, grantFrom("parent", "unused"))
		before := commitCount(t, s)
		if err := s.Retire(t.Context(), Caller{Session: sess("parent"), Self: true}); err != nil {
			t.Fatal(err)
		}
		if n := commitCount(t, s) - before; n != 1 {
			t.Errorf("%d commits, want the retirement in one", n)
		}
		if identityExists(t, s, d.ID) || identityExists(t, s, p.ID) {
			t.Error("parent or delegate row kept, want both deleted")
		}
		if got := endedWhy(sink, d.Name); len(got) != 1 || got[0] != "parent" {
			t.Errorf("delegate ended lines %v, want one parent_retired", got)
		}
		if n := countRows(t, s, s.t.links.table); n != 0 {
			t.Errorf("%d links kept, want the unused one deleted", n)
		}
	})
	t.Run("expiry, which keeps the parent dormant", func(t *testing.T) {
		s, clk, sink := openIdentityStore(t)
		r := registration(sess("parent"), "parent", envA(), repoSubflux)
		r.TTL = time.Hour
		p := mustRegister(t, s, r)
		g := grantFrom("parent", "review")
		g.TTL = 3 * time.Hour
		d := mustRegisterDelegate(t, s, sess("parent"), mustGrant(t, s, g).Link)
		outliveParent(t, s, d.ID)
		mustGrant(t, s, grantFrom("parent", "unused"))
		clk.advance(time.Hour)
		mustSweep(t, s)
		if got := endedWhy(sink, d.Name); len(got) != 1 || got[0] != "parent" {
			t.Errorf("delegate ended lines %v, want one parent_expired", got)
		}
		if row, found := findIdentity(t, s, p.ID); !found || row.State != stateDormant {
			t.Errorf("parent found %v state %q, want dormant", found, row.State)
		}
		if identityExists(t, s, d.ID) {
			t.Error("delegate row kept, want it deleted, never dormant")
		}
		if n := countRows(t, s, s.t.links.table); n != 0 {
			t.Errorf("%d links kept, want the unused one deleted", n)
		}
		t.Run("revival restores none", func(t *testing.T) {
			mustRevive(t, s, revival(sess("parent"), "", envA()))
			if identityExists(t, s, d.ID) {
				t.Error("delegate back after the revival")
			}
			_, err := s.Seen(t.Context(), as(sess("parent"), d.Name))
			wantRefusal(t, err, KindGone, "delegate_ended", "")
		})
	})
	t.Run("expiry seen before the sweep", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		r := registration(sess("parent"), "parent", envA(), repoSubflux)
		r.TTL = time.Hour
		mustRegister(t, s, r)
		g := grantFrom("parent", "review")
		g.TTL = 3 * time.Hour
		d := mustRegisterDelegate(t, s, sess("parent"), mustGrant(t, s, g).Link)
		outliveParent(t, s, d.ID)
		// Seen a minute before, so the next requests read without writing.
		clk.advance(59 * time.Minute)
		if _, err := s.Seen(t.Context(), as(sess("parent"), d.Name)); err != nil {
			t.Fatal(err)
		}
		clk.advance(time.Minute)
		_, err := s.Inbox(t.Context(), as(sess("parent"), d.Name), 0)
		wantRefusal(t, err, KindGone, "delegate_ended", "")
		_, err = s.Set(t.Context(), as(sess("parent"), d.Name), Change{Fields: Fields{Description: "x"}})
		wantRefusal(t, err, KindGone, "delegate_ended", "")
	})
	t.Run("expiry seen from the delegate's own session", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		r := registration(sess("parent"), "parent", envA(), repoSubflux)
		r.TTL = time.Hour
		mustRegister(t, s, r)
		g := grantFrom("parent", "codex")
		g.TTL = 3 * time.Hour
		own := "codex:thread-1"
		d := mustRegisterDelegate(t, s, own, mustGrant(t, s, g).Link)
		outliveParent(t, s, d.ID)
		clk.advance(59 * time.Minute)
		if _, err := s.Seen(t.Context(), Caller{Session: own}); err != nil {
			t.Fatal(err)
		}
		clk.advance(time.Minute)
		_, err := s.Inbox(t.Context(), Caller{Session: own}, 0)
		wantRefusal(t, err, KindGone, "delegate_ended", "")
		p := posting(own, Target{Agent: "parent"})
		p.Fields = Fields{Description: NoChange}
		_, err = s.Post(t.Context(), &p)
		wantRefusal(t, err, KindGone, "delegate_ended", "")
	})
}

func TestDelegate_ownLeaseDeletesItNeverDormant(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	p := regAt(t, s, "parent", repoSubflux)
	g := grantFrom("parent", "review")
	g.TTL = 30 * time.Minute
	d := mustRegisterDelegate(t, s, sess("parent"), mustGrant(t, s, g).Link)
	clk.advance(30 * time.Minute)
	if _, err := s.Seen(t.Context(), as(sess("parent"), d.Name)); !isKind(err, KindGone) {
		t.Errorf("Seen past its lease before the sweep: %v, want KindGone", err)
	}
	mustSweep(t, s)
	if identityExists(t, s, d.ID) {
		t.Error("delegate row kept past its lease, want it deleted")
	}
	if row, found := findIdentity(t, s, p.ID); !found || row.State != stateLive {
		t.Errorf("parent found %v state %q, want it live", found, row.State)
	}
	t.Run("the handle is free again", func(t *testing.T) {
		mustRegisterDelegate(t, s, sess("parent"), mustGrant(t, s, grantFrom("parent", "review")).Link)
	})
}

func TestDelegate_byeEndsOnlyTheDelegate(t *testing.T) {
	s, _, sink := openIdentityStore(t)
	p, d := delegateOf(t, s, "parent", "review")
	if err := s.Retire(t.Context(), as(sess("parent"), d.Name)); err != nil {
		t.Fatal(err)
	}
	if identityExists(t, s, d.ID) {
		t.Error("delegate row kept after its bye")
	}
	if !identityExists(t, s, p.ID) {
		t.Error("parent row deleted by its delegate's bye")
	}
	if got := endedWhy(sink, d.Name); len(got) != 1 || got[0] != "bye" {
		t.Errorf("delegate ended lines %v, want one bye", got)
	}
}

func TestDelegate_takeOverKeepsDelegatesAndLinks(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	p, d := delegateOf(t, s, "parent", "review")
	g := mustGrant(t, s, grantFrom("parent", "later"))
	r := registration(sessB, "parent", envA(), repoSubflux)
	r.TakeOver = true
	if got := mustRegister(t, s, r); got.ID != p.ID {
		t.Fatalf("take-over moved %d, want %d", got.ID, p.ID)
	}
	if got, err := s.Seen(t.Context(), as(sessB, d.Name)); err != nil || got.ID != d.ID {
		t.Errorf("delegate from the new session: %d, %v; want %d", got.ID, err, d.ID)
	}
	_, err := s.Seen(t.Context(), as(sess("parent"), d.Name))
	if err == nil {
		t.Error("delegate still acts from the old session")
	}
	if got := mustRegisterDelegate(t, s, sessB, g.Link); got.Name != "parent/later" {
		t.Errorf("link from before the take-over registered %q, want parent/later", got.Name)
	}
}
