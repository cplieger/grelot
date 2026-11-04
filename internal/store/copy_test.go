package store

import "testing"

// copiesTo lists identity id's deliveries that are a delegate's copies.
func copiesTo(t *testing.T, s *Store, id uint64) []deliveryRow {
	t.Helper()
	var out []deliveryRow
	for _, d := range deliveriesTo(t, s, id) {
		if d.Via != "" {
			out = append(out, d)
		}
	}
	return out
}

func TestCopy_noneForTheDirectThreadWithTheParent(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	parent, d := delegateOf(t, s, "subflux-dev", "review")
	mustPost(t, s, asPost("subflux-dev", d.Name, Target{Agent: "subflux-dev"}))
	mustPost(t, s, selfPost("subflux-dev", Target{Agent: d.Name}))
	ds := deliveriesTo(t, s, parent.ID)
	if len(ds) != 1 || ds[0].Via != "" || ds[0].Kind != kindDirect {
		t.Errorf("parent's deliveries %+v, want its own direct delivery and no copy", ds)
	}
}

func TestCopy_noneForAPathPostTheParentReceivesItself(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	parent := regAt(t, s, "subflux-dev", repoSubflux)
	regAt(t, s, "cloudy-lake", repoGrelot)
	d := scopedDelegate(t, s, "subflux-dev", "review", func(g *Grant) { g.AllowThreads = []string{pathInternal} })
	mustPost(t, s, posting(sess("cloudy-lake"), Target{Path: pathInternal + "/x.go"}))
	mustPost(t, s, asPost("subflux-dev", d.Name, Target{Path: pathInternal}))
	ds := deliveriesTo(t, s, parent.ID)
	if len(ds) != 2 || ds[0].Via != "" || ds[1].Via != "" {
		t.Errorf("parent's deliveries %+v, want its own two path deliveries and no copy", ds)
	}
}

func TestCopy_quietCopyOfAPostTheDelegateReceivesUnderASubtreeTheParentDoesNotFollow(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	parent := regAt(t, s, "subflux-dev", repoGrelot)
	regAt(t, s, "cloudy-lake", repoSubflux)
	d := scopedDelegate(t, s, "subflux-dev", "review", func(g *Grant) { g.AllowThreads = []string{pathInternal} })
	got := mustPost(t, s, posting(sess("cloudy-lake"), Target{Path: pathInternal + "/x.go"}))
	if got.Delivered != 1 {
		t.Errorf("delivered %d, want 1: the copy is not a recipient", got.Delivered)
	}
	cs := copiesTo(t, s, parent.ID)
	if len(cs) != 1 || cs[0].Via != d.Name || cs[0].Message != got.Message || cs[0].Kind != kindPath {
		t.Fatalf("parent's copies %+v, want one of message %d via %s", cs, got.Message, d.Name)
	}

	t.Run("quiet in the inbox and the hold", func(t *testing.T) {
		self := Caller{Session: sess("subflux-dev"), Self: true}
		page, err := s.Inbox(t.Context(), self, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Entries) != 1 || page.Entries[0].Via != d.Name || page.Entries[0].Wake || page.Unread.Copies != 1 || page.Unread.Wake() != 0 {
			t.Errorf("page %+v, want one quiet copy via %s", page, d.Name)
		}
		h, err := s.Hold(t.Context(), self)
		if err != nil {
			t.Fatal(err)
		}
		if h.Counts.Copies != 1 || h.Counts.Wake() != 0 || h.Shown != 0 {
			t.Errorf("hold %+v, want the copy counted and shown not moved", h)
		}
	})
	t.Run("and of the delegate's own post there", func(t *testing.T) {
		own := mustPost(t, s, asPost("subflux-dev", d.Name, Target{Path: pathInternal}))
		cs := copiesTo(t, s, parent.ID)
		if len(cs) != 2 || cs[1].Message != own.Message || cs[1].Via != d.Name {
			t.Errorf("parent's copies %+v, want the delegate's post %d via %s", cs, own.Message, d.Name)
		}
	})
}

func TestCopy_oneCopyOfADirectMessageBetweenSiblings(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	parent := regAt(t, s, "subflux-dev", repoGrelot)
	review := scopedDelegate(t, s, "subflux-dev", "review", nil)
	tests := scopedDelegate(t, s, "subflux-dev", "tests", nil)
	got := mustPost(t, s, asPost("subflux-dev", review.Name, Target{Agent: tests.Name}))
	cs := copiesTo(t, s, parent.ID)
	if len(cs) != 1 || cs[0].Message != got.Message || cs[0].Via != review.Name || cs[0].Kind != kindDirect {
		t.Errorf("parent's copies %+v, want one of message %d via %s", cs, got.Message, review.Name)
	}
}

func TestCopy_onePerParentWhenTwoDelegatesReceiveOnePost(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	parent := regAt(t, s, "subflux-dev", repoGrelot)
	regAt(t, s, "cloudy-lake", repoGrelot)
	allow := func(g *Grant) { g.AllowThreads = []string{pathInternal} }
	scopedDelegate(t, s, "subflux-dev", "review", allow)
	scopedDelegate(t, s, "subflux-dev", "tests", allow)
	got := mustPost(t, s, posting(sess("cloudy-lake"), Target{Path: pathInternal}))
	if got.Delivered != 2 {
		t.Errorf("delivered %d, want the two delegates", got.Delivered)
	}
	if cs := copiesTo(t, s, parent.ID); len(cs) != 1 {
		t.Errorf("parent's copies %+v, want one", cs)
	}
}
