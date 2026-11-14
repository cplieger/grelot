//go:build !windows

package store

import (
	"slices"
	"testing"
	"time"
)

func mustAgentHistory(t *testing.T, s *Store, name string, before uint64, limit int) AgentHistoryPage {
	t.Helper()
	h, err := s.AgentHistory(t.Context(), name, before, limit)
	if err != nil {
		t.Fatalf("AgentHistory(%s, %d, %d): %v", name, before, limit, err)
	}
	return h
}

func historyPostSubjects(h AgentHistoryPage) []string {
	out := make([]string, 0, len(h.Posts))
	for i := range h.Posts {
		out = append(out, h.Posts[i].Subject)
	}
	return out
}

func TestAgentCard_carriesTheCardReservationsAndDelegates(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha", pathWeb)
	regAt(t, s, "bravo", "/workspace/bravo")
	mustReserve(t, s, reserving("bravo", pathAPI, time.Hour))
	mustQueue(t, s, reserving("alpha", pathAPI, time.Hour))
	mustReserve(t, s, reserving("alpha", pathWeb, time.Hour))
	review := scopedDelegate(t, s, "alpha", "review", nil)

	c, err := s.AgentCard(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "alpha" || c.State != stateLive || c.Host != "host-a/app" || c.Provider != "kiro-cli" || !slices.Equal(c.Paths, []string{pathWeb}) {
		t.Errorf("card %+v, want alpha live on host-a/app through kiro-cli, working on %s", c.Identity, pathWeb)
	}
	type row struct {
		path, state string
		place       int
	}
	var rows []row
	for _, e := range c.Reservations {
		rows = append(rows, row{e.Path, e.State, e.Place})
	}
	// The grant comes first though the queue entry is older.
	if want := []row{{pathWeb, reservationHeld, 0}, {pathAPI, reservationQueued, 1}}; !slices.Equal(rows, want) {
		t.Errorf("reservations %+v, want %+v", rows, want)
	}
	if !slices.Equal(c.Delegates, []string{review.Name}) {
		t.Errorf("delegates %q, want %s", c.Delegates, review.Name)
	}
}

func TestAgentCard_aDormantAgentReadsDormantWithNoDelegates(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	scopedDelegate(t, s, "alpha", "review", nil)
	clk.advance(7 * time.Hour)

	c, err := s.AgentCard(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if c.State != stateDormant || len(c.Delegates) != 0 {
		t.Errorf("card state %s, delegates %q, want dormant and none", c.State, c.Delegates)
	}
}

func TestAgents_listsLiveAndDormantAgentsByNameWithoutUserOrDelegates(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "charlie")
	clk.advance(7 * time.Hour)
	regAt(t, s, "alpha")
	review := scopedDelegate(t, s, "alpha", "review", nil)
	if c, err := s.AgentCard(t.Context(), "alpha"); err != nil || !slices.Equal(c.Delegates, []string{review.Name}) {
		t.Fatalf("alpha's delegates %q (%v), want %s live", c.Delegates, err, review.Name)
	}

	got, err := s.Agents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ name, state string }
	var rows []row
	for i := range got {
		rows = append(rows, row{got[i].Name, got[i].State})
	}
	if want := []row{{"alpha", stateLive}, {"charlie", stateDormant}}; !slices.Equal(rows, want) {
		t.Errorf("agents %+v, want %+v", rows, want)
	}
}

func TestAgentCard_anUnknownNameIsNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.AgentCard(t.Context(), "nobody")
	wantRefusal(t, err, KindNotFound, "", "no agent called @nobody")
	_, err = s.AgentHistory(t.Context(), "nobody", 0, 0)
	wantRefusal(t, err, KindNotFound, "", "no agent called @nobody")
}

// historyFixture has alpha send, receive and announce-receive, a minute
// apart: d1 to bravo, d2 from bravo, a1 from charlie, p1 on a path.
func historyFixture(t *testing.T) (*Store, *clock) {
	t.Helper()
	s, clk, _ := openIdentityStore(t)
	alpha := regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	regAt(t, s, "charlie", "/workspace/charlie")
	for _, p := range []struct {
		from    string
		to      Target
		subject string
	}{
		{"alpha", Target{Agent: "bravo"}, "d1"},
		{"bravo", Target{Agent: "alpha"}, "d2"},
		{"charlie", Target{Announce: true}, "a1"},
		{"alpha", Target{Path: pathMarotte}, "p1"},
		{"bravo", Target{Agent: "charlie"}, "not alpha's"},
	} {
		say(t, s, p.from, p.to, p.subject)
		clk.advance(time.Minute)
	}
	keepAlive(t, s, alpha.ID)
	return s, clk
}

func TestAgentHistory_sentAndReceivedNewestFirstWithTheirConversation(t *testing.T) {
	s, _ := historyFixture(t)
	h := mustAgentHistory(t, s, "alpha", 0, 0)
	if got, want := historyPostSubjects(h), []string{"p1", "a1", "d2", "d1"}; !slices.Equal(got, want) {
		t.Fatalf("subjects %q, want %q", got, want)
	}
	var convs []string
	for i := range h.Posts {
		convs = append(convs, h.Posts[i].Conversation.Title)
	}
	if want := []string{pathMarotte, "announce", "@alpha and @bravo", "@alpha and @bravo"}; !slices.Equal(convs, want) {
		t.Errorf("conversations %q, want %q", convs, want)
	}
	if h.Posts[0].From != "alpha" || h.Posts[2].From != "bravo" {
		t.Errorf("senders %s and %s, want alpha and bravo", h.Posts[0].From, h.Posts[2].From)
	}
}

func TestAgentHistory_pagesOlderPosts(t *testing.T) {
	s, _ := historyFixture(t)
	first := mustAgentHistory(t, s, "alpha", 0, 3)
	if got, want := historyPostSubjects(first), []string{"p1", "a1", "d2"}; !slices.Equal(got, want) {
		t.Fatalf("first page %q, want %q", got, want)
	}
	if first.Next != first.Posts[2].ID {
		t.Errorf("next %d, want the oldest shown post's id %d", first.Next, first.Posts[2].ID)
	}
	second := mustAgentHistory(t, s, "alpha", first.Next, 3)
	if got := historyPostSubjects(second); !slices.Equal(got, []string{"d1"}) || second.Next != 0 {
		t.Errorf("second page %q next %d, want d1 and no next", got, second.Next)
	}
}

func TestAgentHistory_expiredPostsLeave(t *testing.T) {
	s, clk := historyFixture(t)
	clk.advance(DefaultRetention)
	if got := historyPostSubjects(mustAgentHistory(t, s, "alpha", 0, 0)); !slices.Equal(got, []string{"a1"}) {
		t.Errorf("subjects %q after the retention, want the announcement alone, which lives a week", got)
	}
}

func TestAgentHistory_aDelegatesMailIsItsOwnNotItsParents(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	review := scopedDelegate(t, s, "alpha", "review", func(g *Grant) { g.AllowAgents = []string{"bravo"} })
	mustPost(t, s, asPost("alpha", review.Name, Target{Agent: "bravo"}))
	say(t, s, "bravo", Target{Agent: review.Name}, "for review")

	if got := historyPostSubjects(mustAgentHistory(t, s, review.Name, 0, 0)); len(got) != 2 || got[0] != "for review" {
		t.Errorf("delegate's history %q, want bravo's reply then its own post", got)
	}
	if got := historyPostSubjects(mustAgentHistory(t, s, "alpha", 0, 0)); len(got) != 0 {
		t.Errorf("parent's history %q, want none: its copies are not its mail", got)
	}
	h := mustAgentHistory(t, s, "bravo", 0, 0)
	if len(h.Posts) != 2 || h.Posts[1].From != review.Name || h.Posts[1].Conversation.Title != "@"+review.Name+" and @bravo" {
		t.Errorf("bravo's history %+v, want the delegate's post in their direct thread", h.Posts)
	}
}
