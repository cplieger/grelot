//go:build !windows

package store

import (
	"slices"
	"testing"
	"time"
)

const (
	pathInternal = repoSubflux + "/internal"
	pathLegacy   = pathInternal + "/legacy"
)

// scopedDelegate grants parent's delegate label under the scope edit sets
// and registers it from parent's session.
func scopedDelegate(t *testing.T, s *Store, parent, label string, edit func(*Grant)) Identity {
	t.Helper()
	g := grantFrom(parent, label)
	if edit != nil {
		edit(&g)
	}
	return mustRegisterDelegate(t, s, sess(parent), mustGrant(t, s, g).Link)
}

// asPost is a post as handle from its parent's session, every field kept.
func asPost(parent, handle string, to Target) Posting {
	p := posting(sess(parent), to)
	p.Caller = as(sess(parent), handle)
	p.Fields = Fields{Description: NoChange}
	return p
}

// selfPost is a post from name's own session that names itself.
func selfPost(name string, to Target) Posting {
	p := posting(sess(name), to)
	p.Caller.Self = true
	return p
}

func postErr(t *testing.T, s *Store, p Posting) error {
	t.Helper()
	_, err := s.Post(t.Context(), &p)
	return err
}

func TestScope_defaultIsTheParentAndTheSiblings(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "subflux-dev", repoGrelot)
	regAt(t, s, "cloudy-lake", repoSubflux)
	review := scopedDelegate(t, s, "subflux-dev", "review", nil)
	tests := scopedDelegate(t, s, "subflux-dev", "tests", nil)

	t.Run("direct to the parent and a sibling", func(t *testing.T) {
		mustPost(t, s, asPost("subflux-dev", review.Name, Target{Agent: "subflux-dev"}))
		mustPost(t, s, asPost("subflux-dev", review.Name, Target{Agent: tests.Name}))
	})
	t.Run("nothing else", func(t *testing.T) {
		for _, to := range []Target{{Agent: "cloudy-lake"}, {Path: repoSubflux}, {Announce: true}} {
			err := postErr(t, s, asPost("subflux-dev", review.Name, to))
			wantRefusal(t, err, KindForbidden, "", "outside @subflux-dev/review's scope")
		}
	})
	t.Run("a direct post to it from outside its scope", func(t *testing.T) {
		err := postErr(t, s, posting(sess("cloudy-lake"), Target{Agent: review.Name}))
		wantRefusal(t, err, KindForbidden, "", "outside @subflux-dev/review's scope")
		if ds := deliveriesTo(t, s, review.ID); len(ds) != 0 {
			t.Errorf("review's deliveries %+v, want none", ds)
		}
	})
	t.Run("the default gets no announcement", func(t *testing.T) {
		mustPost(t, s, posting(sess("cloudy-lake"), Target{Announce: true}))
		if ds := deliveriesTo(t, s, tests.ID); len(ds) != 1 || ds[0].Kind != kindDirect {
			t.Errorf("tests' deliveries %+v, want only its sibling's direct post", ds)
		}
	})
}

func TestScope_allowedThreadAndItsSubtreeToPostAndReceive(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "subflux-dev", repoGrelot)
	regAt(t, s, "cloudy-lake", repoSubflux)
	d := scopedDelegate(t, s, "subflux-dev", "review", func(g *Grant) {
		g.AllowThreads = []string{pathInternal}
		g.DenyThreads = []string{pathLegacy}
	})

	t.Run("posts at and under the allowed thread", func(t *testing.T) {
		mustPost(t, s, asPost("subflux-dev", d.Name, Target{Path: pathInternal}))
		mustPost(t, s, asPost("subflux-dev", d.Name, Target{Path: pathInternal + "/store"}))
		if row, _ := findIdentity(t, s, d.ID); len(row.Follows) != 0 {
			t.Errorf("delegate follows %v, want none: its scope delivers what it posts in", row.Follows)
		}
	})
	t.Run("a deny wins over the allow, naming both", func(t *testing.T) {
		err := postErr(t, s, asPost("subflux-dev", d.Name, Target{Path: pathLegacy + "/old.go"}))
		wantRefusal(t, err, KindForbidden, "", "outside @subflux-dev/review's scope", "allowed", pathInternal, "denied", pathLegacy)
	})
	t.Run("above the allowed thread is outside", func(t *testing.T) {
		err := postErr(t, s, asPost("subflux-dev", d.Name, Target{Path: repoSubflux}))
		wantRefusal(t, err, KindForbidden, "", "outside @subflux-dev/review's scope")
	})

	before := len(deliveriesTo(t, s, d.ID))
	for _, p := range []string{pathInternal + "/x.go", pathLegacy, repoSubflux} {
		mustPost(t, s, posting(sess("cloudy-lake"), Target{Path: p}))
	}
	got := deliveriesTo(t, s, d.ID)[before:]
	t.Run("receives under the allowed thread, labelled with it", func(t *testing.T) {
		if len(got) != 1 || got[0].Kind != kindPath || got[0].Path != pathInternal {
			t.Fatalf("deliveries %+v, want one path delivery via %s", got, pathInternal)
		}
		m, _ := findMessage(t, s, got[0].Message)
		if m.Path != pathInternal+"/x.go" {
			t.Errorf("delivered the post at %s, want the one at %s/x.go", m.Path, pathInternal)
		}
	})
}

func TestScope_announcementsOnlyWhereAllowedAndNotDenied(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "subflux-dev", repoGrelot)
	regAt(t, s, "cloudy-lake", repoSubflux)
	allowed := scopedDelegate(t, s, "subflux-dev", "allowed", func(g *Grant) { g.AllowThreads = []string{scopeAnnounce} })
	denied := scopedDelegate(t, s, "subflux-dev", "denied", func(g *Grant) {
		g.AllowThreads = []string{scopeAnnounce}
		g.DenyThreads = []string{scopeAnnounce}
	})

	mustPost(t, s, posting(sess("cloudy-lake"), Target{Announce: true}))
	if ds := deliveriesTo(t, s, allowed.ID); len(ds) != 1 || ds[0].Kind != kindAnnounce {
		t.Errorf("allowed delegate's deliveries %+v, want the announcement", ds)
	}
	if ds := deliveriesTo(t, s, denied.ID); len(ds) != 0 {
		t.Errorf("denied delegate's deliveries %+v, want none", ds)
	}
	mustPost(t, s, asPost("subflux-dev", allowed.Name, Target{Announce: true}))
	err := postErr(t, s, asPost("subflux-dev", denied.Name, Target{Announce: true}))
	wantRefusal(t, err, KindForbidden, "", "outside @subflux-dev/denied's scope")
}

func TestScope_allowedAndDeniedAgents(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "subflux-dev", repoGrelot)
	regAt(t, s, "cloudy-lake", repoSubflux)
	regAt(t, s, "quiet-glacier", repoSubflux)
	sibling := scopedDelegate(t, s, "subflux-dev", "tests", nil)
	d := scopedDelegate(t, s, "subflux-dev", "review", func(g *Grant) {
		g.AllowAgents = []string{"cloudy-lake", "quiet-glacier"}
		g.DenyAgents = []string{"quiet-glacier", sibling.Name}
	})

	t.Run("an allowed agent both ways", func(t *testing.T) {
		mustPost(t, s, asPost("subflux-dev", d.Name, Target{Agent: "cloudy-lake"}))
		mustPost(t, s, posting(sess("cloudy-lake"), Target{Agent: d.Name}))
	})
	t.Run("a deny wins over the allow, both ways", func(t *testing.T) {
		err := postErr(t, s, asPost("subflux-dev", d.Name, Target{Agent: "quiet-glacier"}))
		wantRefusal(t, err, KindForbidden, "", "outside @subflux-dev/review's scope", "denied", "@quiet-glacier")
		err = postErr(t, s, posting(sess("quiet-glacier"), Target{Agent: d.Name}))
		wantRefusal(t, err, KindForbidden, "", "outside @subflux-dev/review's scope")
	})
	t.Run("a denied sibling both ways", func(t *testing.T) {
		err := postErr(t, s, asPost("subflux-dev", d.Name, Target{Agent: sibling.Name}))
		wantRefusal(t, err, KindForbidden, "", "outside @subflux-dev/review's scope")
		err = postErr(t, s, asPost("subflux-dev", sibling.Name, Target{Agent: d.Name}))
		wantRefusal(t, err, KindForbidden, "", "outside @subflux-dev/review's scope")
	})
	t.Run("the parent always", func(t *testing.T) {
		mustPost(t, s, selfPost("subflux-dev", Target{Agent: d.Name}))
		mustPost(t, s, asPost("subflux-dev", d.Name, Target{Agent: "subflux-dev"}))
	})
}

func TestScope_aDelegatePostCarriesOnlyItsDescription(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	_, d := delegateOf(t, s, "subflux-dev", "review")
	to := Target{Agent: "subflux-dev"}
	t.Run("a name or paths refused", func(t *testing.T) {
		for _, f := range []Fields{
			{Name: "renamed", Description: NoChange},
			{Description: NoChange, Paths: []string{repoSubflux}},
		} {
			p := asPost("subflux-dev", d.Name, to)
			p.Fields = f
			wantRefusal(t, postErr(t, s, p), KindInvalid, "", "handle")
		}
	})
	t.Run("description required", func(t *testing.T) {
		p := asPost("subflux-dev", d.Name, to)
		p.Fields = Fields{}
		wantRefusal(t, postErr(t, s, p), KindInvalid, "", "description")
	})
	t.Run("name and paths as no change, a new description and a where ignored", func(t *testing.T) {
		p := asPost("subflux-dev", d.Name, to)
		other := Where{Machine: dmi("9"), Environment: hex32("9")}
		p.Fields, p.Where = Fields{Name: NoChange, Description: "reviewing the store", Paths: []string{NoChange}}, &other
		mustPost(t, s, p)
		row, _ := findIdentity(t, s, d.ID)
		if row.Name != d.Name || len(row.Paths) != 0 || row.Description != "reviewing the store" || row.Where.Environment != "" {
			t.Errorf("row name %q paths %v desc %q where %+v, want the handle, no paths, the new description, no where",
				row.Name, row.Paths, row.Description, row.Where)
		}
	})
}

func TestScope_aDelegatePostStoresItsParentsWhereAndItsRelationFollowsIt(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "subflux-dev", repoGrelot)
	reader := mustRegister(t, s, registration(sess("cloudy-lake"), "cloudy-lake", Where{Machine: dmi("1"), Environment: hex32("b")}))
	d := scopedDelegate(t, s, "subflux-dev", "review", func(g *Grant) { g.AllowAgents = []string{"cloudy-lake"} })

	first := mustPost(t, s, asPost("subflux-dev", d.Name, Target{Agent: "cloudy-lake"}))
	if m, _ := findMessage(t, s, first.Message); m.Where.Environment != hex32("a") || m.Where.Machine != dmi("1") {
		t.Errorf("message where %+v, want the parent's", m.Where)
	}
	if first.Other != 1 || first.SameEnvironment != 0 {
		t.Errorf("counted %d same, %d other, want the reader other to the parent's environment", first.SameEnvironment, first.Other)
	}
	page, err := s.Inbox(t.Context(), Caller{Session: sess("cloudy-lake")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Relation != "same machine, other environment" {
		t.Errorf("reader's entries %+v, want the delegate at the parent's relation", page.Entries)
	}

	moved := Where{Machine: dmi("1"), Environment: hex32("b")}
	if _, err := s.Set(t.Context(), Caller{Session: sess("subflux-dev"), Self: true}, Change{Where: &moved, TTL: 6 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	second := mustPost(t, s, asPost("subflux-dev", d.Name, Target{Agent: "cloudy-lake"}))
	if second.SameEnvironment != 1 {
		t.Errorf("after the parent moved, counted %d same, want 1", second.SameEnvironment)
	}
	toDelegate := mustPost(t, s, posting(sess("cloudy-lake"), Target{Agent: d.Name}))
	if toDelegate.SameEnvironment != 1 {
		t.Errorf("a post to the delegate counted %d same, want 1 by the parent's new where", toDelegate.SameEnvironment)
	}
	got, err := s.Inbox(t.Context(), as(sess("subflux-dev"), d.Name), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 || got.Entries[0].From != reader.Name || got.Entries[0].Relation != "same environment" {
		t.Errorf("delegate's entries %+v, want the reader at same environment by the parent's new where", got.Entries)
	}
}

func TestScope_aDelegateWritesAndReceivesNoCards(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "subflux-dev", repoSubflux)
	watcher := regAt(t, s, "cloudy-lake", repoSubflux)
	d := scopedDelegate(t, s, "subflux-dev", "review", func(g *Grant) { g.AllowThreads = []string{repoSubflux} })
	regAt(t, s, "quiet-glacier", pathInternal)
	if n := len(allDeliveriesTo(t, s, d.ID)); n != 0 {
		t.Errorf("delegate got %d deliveries, want no card", n)
	}
	if err := s.Retire(t.Context(), as(sess("subflux-dev"), d.Name)); err != nil {
		t.Fatal(err)
	}
	cards := 0
	for _, r := range allDeliveriesTo(t, s, watcher.ID) {
		if r.Kind == kindJoin || r.Kind == kindLeft {
			cards++
		}
	}
	if cards != 1 {
		t.Errorf("watcher got %d cards, want only quiet-glacier's join card", cards)
	}
}

func TestScope_aDelegateCannotReserveQueueOrFollow(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	_, d := delegateOf(t, s, "subflux-dev", "review")
	for _, queue := range []bool{false, true} {
		r := Reserving{Caller: as(sess("subflux-dev"), d.Name), Path: repoSubflux, Reason: "review", TTL: time.Hour, Queue: queue}
		_, err := s.Reserve(t.Context(), &r)
		wantRefusal(t, err, KindForbidden, "", "delegate")
	}
	mustPost(t, s, selfPost("subflux-dev", Target{Path: repoSubflux}))
	_, err := s.Follow(t.Context(), as(sess("subflux-dev"), d.Name), repoSubflux)
	wantRefusal(t, err, KindForbidden, "", "scope")
}

func TestScope_whoNestsADelegateUnderItsParent(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "subflux-dev", repoSubflux)
	mustRegister(t, s, registration(sess("cloudy-lake"), "cloudy-lake", Where{Machine: dmi("1"), Environment: hex32("b")}))
	d := scopedDelegate(t, s, "subflux-dev", "review", nil)
	peers, err := s.Who(t.Context(), Caller{Session: sess("cloudy-lake")}, &WhoQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range peers {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"cloudy-lake", "subflux-dev"}) {
		t.Fatalf("top-level %v, want the two identities and no delegate", names)
	}
	parent := peers[1]
	if len(parent.Delegates) != 1 || parent.Delegates[0].Name != d.Name || parent.Delegates[0].Relation != "same machine, other environment" {
		t.Errorf("parent's delegates %+v, want %s at the parent's relation", parent.Delegates, d.Name)
	}
}

func TestScope_historyStaysInsideTheScope(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "subflux-dev", repoSubflux)
	regAt(t, s, "cloudy-lake", repoSubflux)
	d := scopedDelegate(t, s, "subflux-dev", "review", func(g *Grant) { g.AllowThreads = []string{pathInternal} })
	mustPost(t, s, posting(sess("cloudy-lake"), Target{Path: repoSubflux}))
	mustPost(t, s, posting(sess("cloudy-lake"), Target{Path: pathInternal}))
	mustPost(t, s, asPost("subflux-dev", d.Name, Target{Agent: "subflux-dev"}))
	h, err := s.History(t.Context(), as(sess("subflux-dev"), d.Name), &HistoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Entries) != 2 || h.Entries[0].ThreadPath != pathInternal || h.Entries[1].Relation != "you" {
		t.Errorf("history %+v, want the post at %s and its own post", h.Entries, pathInternal)
	}
}

func TestScope_aDelegateIsShownWithItsParentsWhere(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	_, d := delegateOf(t, s, "subflux-dev", "review")
	known := func(name string, v Identity) {
		t.Helper()
		if !v.EnvironmentKnown || v.MachineKind != "dmi" {
			t.Errorf("%s: environment known %v, machine kind %q, want the parent's: known, dmi", name, v.EnvironmentKnown, v.MachineKind)
		}
	}
	known("register", d)
	posted := mustPost(t, s, asPost("subflux-dev", d.Name, Target{Agent: "subflux-dev"}))
	known("post", posted.Identity)
	acked, err := s.Ack(t.Context(), as(sess("subflux-dev"), d.Name), 0)
	if err != nil {
		t.Fatal(err)
	}
	known("ack", acked.Identity)
}
