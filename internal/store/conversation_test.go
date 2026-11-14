//go:build !windows

package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

func mustConversations(t *testing.T, s *Store, q ConversationsQuery) ConversationPage {
	t.Helper()
	p, err := s.Conversations(t.Context(), q)
	if err != nil {
		t.Fatalf("Conversations(%+v): %v", q, err)
	}
	return p
}

func mustConversation(t *testing.T, s *Store, id, before uint64, limit int) Conversation {
	t.Helper()
	c, err := s.Conversation(t.Context(), id, before, limit)
	if err != nil {
		t.Fatalf("Conversation(%d, %d, %d): %v", id, before, limit, err)
	}
	return c
}

func titles(p ConversationPage) []string {
	out := make([]string, 0, len(p.Rows))
	for i := range p.Rows {
		out = append(out, p.Rows[i].Title)
	}
	return out
}

func postSubjects(ps []ConversationPost) []string {
	out := make([]string, 0, len(ps))
	for i := range ps {
		out = append(out, ps[i].Subject)
	}
	return out
}

// rowFor is the row of thread id in p, failing the test when p has none.
func rowFor(t *testing.T, p ConversationPage, id uint64) ConversationSummary {
	t.Helper()
	for i := range p.Rows {
		if p.Rows[i].ID == id {
			return p.Rows[i]
		}
	}
	t.Fatalf("no row for thread %d in %q", id, titles(p))
	return ConversationSummary{}
}

// threeKinds posts an announcement, a path post and a direct post a minute
// apart; charlie works on the path.
func threeKinds(t *testing.T) (s *Store, clk *clock, announce, path, direct Posted) {
	t.Helper()
	s, clk, _ = openIdentityStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	regAt(t, s, "charlie", pathMarotte)
	announce = say(t, s, "alpha", Target{Announce: true}, "a1")
	clk.advance(time.Minute)
	path = say(t, s, "bravo", Target{Path: pathMarotte}, "p1")
	clk.advance(time.Minute)
	direct = say(t, s, "alpha", Target{Agent: "bravo"}, "d1")
	return s, clk, announce, path, direct
}

func TestConversations_newestActivityFirstUnderEachFilter(t *testing.T) {
	s, _, _, _, _ := threeKinds(t)
	for _, tc := range []struct {
		name string
		want []string
		show Show
	}{
		{"all", []string{"@alpha and @bravo", pathMarotte, "announce"}, ShowAll},
		{"threads", []string{pathMarotte, "announce"}, ShowThreads},
		{"direct", []string{"@alpha and @bravo"}, ShowDirect},
	} {
		t.Run("show "+tc.name, func(t *testing.T) {
			p := mustConversations(t, s, ConversationsQuery{Show: tc.show})
			if got := titles(p); !slices.Equal(got, tc.want) {
				t.Errorf("titles %q, want %q", got, tc.want)
			}
			if p.Next != 0 {
				t.Errorf("next %d on the only page, want 0", p.Next)
			}
		})
	}
}

func TestConversations_rowsCarryKindLastPostAndParticipants(t *testing.T) {
	s, clk, announce, path, direct := threeKinds(t)
	p := mustConversations(t, s, ConversationsQuery{})
	for _, tc := range []struct {
		name         string
		thread       uint64
		kind, from   string
		subject      string
		at           time.Time
		participants int
		agents       []string
	}{
		// alpha sends; bravo and charlie receive; @user reads it in the UI.
		{"announce", announce.Thread, kindAnnounce, "alpha", "a1", testNow, 3, nil},
		{"path", path.Thread, kindPath, "bravo", "p1", testNow.Add(time.Minute), 2, nil},
		{"direct", direct.Thread, kindDirect, "alpha", "d1", clk.now(), 2, []string{"alpha", "bravo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := rowFor(t, p, tc.thread)
			if row.Kind != tc.kind || row.Last.From != tc.from || row.Last.Subject != tc.subject || !row.Last.At.Equal(tc.at) {
				t.Errorf("row %+v, want kind %s, last %s from %s at %v", row, tc.kind, tc.subject, tc.from, tc.at)
			}
			if row.Participants != tc.participants {
				t.Errorf("participants %d, want %d", row.Participants, tc.participants)
			}
			if !slices.Equal(row.Agents, tc.agents) {
				t.Errorf("agents %q, want %q", row.Agents, tc.agents)
			}
		})
	}
	if row := rowFor(t, p, path.Thread); row.Path != pathMarotte {
		t.Errorf("path row's path %q, want %s", row.Path, pathMarotte)
	}
}

func TestConversations_previewIsTheBodysFirstLineCut(t *testing.T) {
	long := strings.Repeat("é", 250)
	for _, tc := range []struct{ name, body, want string }{
		{"first line", "first line\r\nsecond line", "first line"},
		{"long line", long, strings.Repeat("é", 200) + "…"},
		{"exactly the cut", strings.Repeat("x", 200), strings.Repeat("x", 200)},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := openIdentityStore(t)
			regAt(t, s, "alpha")
			regAt(t, s, "bravo")
			p := posting(sess("alpha"), Target{Agent: "bravo"})
			p.Body = tc.body
			mustPost(t, s, p)
			rows := mustConversations(t, s, ConversationsQuery{}).Rows
			if len(rows) != 1 || rows[0].Last.Preview != tc.want {
				t.Errorf("rows %+v, want one with preview %q", rows, tc.want)
			}
		})
	}
}

func TestConversations_pagesByLastActivityWithoutRepeats(t *testing.T) {
	s, clk, _, _, _ := threeKinds(t)
	clk.advance(time.Minute)
	say(t, s, "charlie", Target{Announce: true}, "a2")

	first := mustConversations(t, s, ConversationsQuery{Limit: 2})
	if got, want := titles(first), []string{"announce", "@alpha and @bravo"}; !slices.Equal(got, want) {
		t.Fatalf("first page %q, want %q", got, want)
	}
	if first.Next == 0 {
		t.Fatal("first page has no next cursor, want one")
	}
	second := mustConversations(t, s, ConversationsQuery{Limit: 2, Before: first.Next, BeforeThread: first.NextThread})
	if got, want := titles(second), []string{pathMarotte}; !slices.Equal(got, want) {
		t.Errorf("second page %q, want %q", got, want)
	}
	if second.Next != 0 {
		t.Errorf("next %d on the last page, want 0", second.Next)
	}
}

// subpathReply is @user's post at pathMarotte answered a minute later by
// alpha, who works on pathWeb, in pathWeb's thread.
func subpathReply(t *testing.T) (s *Store, clk *clock, parent, child Posted) {
	t.Helper()
	s, clk, _ = openIdentityStore(t)
	regAt(t, s, "alpha", pathWeb)
	parent = mustPost(t, s, userPosting(Target{Path: pathMarotte}))
	clk.advance(time.Minute)
	child = say(t, s, "alpha", Target{Path: pathWeb}, "child")
	return s, clk, parent, child
}

func TestConversations_aPathRowSummarizesTheConversationItOpens(t *testing.T) {
	s, clk, parent, child := subpathReply(t)

	p := mustConversations(t, s, ConversationsQuery{})
	row := rowFor(t, p, parent.Thread)
	if row.Last.ID != child.Message || row.Last.From != "alpha" || !row.Last.At.Equal(clk.now()) {
		t.Errorf("parent row's last %+v, want alpha's subpath post %d, as opening it shows", row.Last, child.Message)
	}
	if row.Participants != 2 {
		t.Errorf("parent row's participants %d, want @user and alpha", row.Participants)
	}
	// The two rows share their newest post, so the newer thread leads.
	if got, want := titles(p), []string{pathWeb, pathMarotte}; !slices.Equal(got, want) {
		t.Errorf("titles %q, want %q", got, want)
	}
}

func TestConversations_aPageBoundaryBetweenRowsSharingTheirNewestPost(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha", pathWeb)
	say(t, s, "alpha", Target{Announce: true}, "a1")
	clk.advance(time.Minute)
	mustPost(t, s, userPosting(Target{Path: pathMarotte}))
	clk.advance(time.Minute)
	say(t, s, "alpha", Target{Path: pathWeb}, "child")

	var got []string
	q := ConversationsQuery{Limit: 1}
	for range 4 {
		p := mustConversations(t, s, q)
		got = append(got, titles(p)...)
		if p.Next == 0 {
			break
		}
		q.Before, q.BeforeThread = p.Next, p.NextThread
	}
	if want := []string{pathWeb, pathMarotte, "announce"}; !slices.Equal(got, want) {
		t.Errorf("pages of one %q, want %q, none repeated or skipped", got, want)
	}
}

func TestConversations_countsEachFiltersRowsWhateverThePage(t *testing.T) {
	s, clk, _, _, _ := threeKinds(t)
	first := mustConversations(t, s, ConversationsQuery{Show: ShowThreads, Limit: 1})
	second := mustConversations(t, s, ConversationsQuery{Show: ShowThreads, Limit: 1, Before: first.Next, BeforeThread: first.NextThread})
	direct := mustConversations(t, s, ConversationsQuery{Show: ShowDirect})
	want := ConversationCounts{Threads: 2, Direct: 1}
	for name, p := range map[string]ConversationPage{"first threads page": first, "second threads page": second, "direct": direct} {
		if p.Counts != want {
			t.Errorf("%s counts %+v, want %+v", name, p.Counts, want)
		}
	}
	clk.advance(DefaultRetention)
	if got, want := mustConversations(t, s, ConversationsQuery{}).Counts, (ConversationCounts{Threads: 1}); got != want {
		t.Errorf("counts %+v after the retention, want %+v: the announcement alone lives on", got, want)
	}
}

func TestConversations_expiredThreadsLeaveTheList(t *testing.T) {
	s, clk, _, _, direct := threeKinds(t)
	clk.advance(DefaultRetention)
	if got, want := titles(mustConversations(t, s, ConversationsQuery{})), []string{"announce"}; !slices.Equal(got, want) {
		t.Errorf("titles %q after the retention, want the announcement alone, which lives a week", got)
	}
	_, err := s.Conversation(t.Context(), direct.Thread, 0, 0)
	wantRefusal(t, err, KindNotFound, "", "has expired")
}

func TestConversations_unreadCountsUsersUnreadPosts(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	fromAlpha := mustPost(t, s, posting(sess("alpha"), Target{Agent: userName}))
	mustPost(t, s, posting(sess("alpha"), Target{Agent: userName}))
	fromBravo := mustPost(t, s, posting(sess("bravo"), Target{Agent: userName}))

	p := mustConversations(t, s, ConversationsQuery{})
	if a, b := rowFor(t, p, fromAlpha.Thread).Unread, rowFor(t, p, fromBravo.Thread).Unread; a != 2 || b != 1 {
		t.Errorf("unread alpha %d, bravo %d, want 2 and 1", a, b)
	}
	if _, err := s.ReadThread(t.Context(), Caller{User: true}, fromAlpha.Thread, everyPost); err != nil {
		t.Fatal(err)
	}
	p = mustConversations(t, s, ConversationsQuery{})
	if a, b := rowFor(t, p, fromAlpha.Thread).Unread, rowFor(t, p, fromBravo.Thread).Unread; a != 0 || b != 1 {
		t.Errorf("after reading alpha's thread unread alpha %d, bravo %d, want 0 and 1", a, b)
	}
}

func TestConversations_aRetiredSenderKeepsItsNameAtThePost(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	sent := say(t, s, "alpha", Target{Agent: "bravo"}, "d1")
	if _, _, err := s.Retire(t.Context(), Keyed{}, Caller{Session: sess("alpha")}); err != nil {
		t.Fatal(err)
	}

	if got := titles(mustConversations(t, s, ConversationsQuery{})); !slices.Equal(got, []string{"@alpha and @bravo"}) {
		t.Errorf("titles %q, want the retired alpha still named", got)
	}
	c := mustConversation(t, s, sent.Thread, 0, 0)
	if len(c.Posts) != 1 || c.Posts[0].From != "alpha" {
		t.Errorf("posts %+v, want alpha's post from alpha", c.Posts)
	}
	if len(c.Cards) != 1 || c.Cards[0].Name != "bravo" {
		t.Errorf("cards %+v, want bravo's alone: alpha's row is gone", c.Cards)
	}
	regAt(t, s, "alpha")
	c = mustConversation(t, s, sent.Thread, 0, 0)
	if len(c.Cards) != 1 || c.Cards[0].Name != "bravo" {
		t.Errorf("cards %+v after a new identity took the name alpha, want bravo's alone", c.Cards)
	}
}

func TestConversations_refusesABadQuery(t *testing.T) {
	s := openTestStore(t)
	_, err := s.Conversations(t.Context(), ConversationsQuery{Limit: maxWebLimit + 1})
	wantRefusal(t, err, KindInvalid, "", "page limit")
	_, err = s.Conversations(t.Context(), ConversationsQuery{BeforeThread: 3})
	wantRefusal(t, err, KindInvalid, "", "cursor needs both")
	_, err = s.Conversations(t.Context(), ConversationsQuery{Before: 3})
	wantRefusal(t, err, KindInvalid, "", "cursor needs both")
	_, err = s.Conversation(t.Context(), 1, 0, -1)
	wantRefusal(t, err, KindInvalid, "", "page limit")
}

// pathFixture is a path thread at pathMarotte with a post from its subpath
// pathWeb and one from the name-prefix sibling pathMarotte-old. dave works
// on /workspace and charlie on pathMarotte; alpha on pathWeb, under it.
func pathFixture(t *testing.T) (s *Store, parent, child Posted) {
	t.Helper()
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha", pathWeb)
	regAt(t, s, "bravo", "/workspace/bravo")
	regAt(t, s, "charlie", pathMarotte)
	regAt(t, s, "dave", "/workspace")
	parent = say(t, s, "bravo", Target{Path: pathMarotte}, "parent")
	clk.advance(time.Minute)
	child = say(t, s, "alpha", Target{Path: pathWeb}, "child")
	clk.advance(time.Minute)
	say(t, s, "dave", Target{Path: pathMarotte + "-old"}, "sibling")
	return s, parent, child
}

func TestConversation_aPathThreadTakesInItsSubpathsWithTheirOrigin(t *testing.T) {
	s, parent, child := pathFixture(t)

	c := mustConversation(t, s, parent.Thread, 0, 0)
	if got, want := postSubjects(c.Posts), []string{"parent", "child"}; !slices.Equal(got, want) {
		t.Fatalf("parent thread posts %q, want %q", got, want)
	}
	if c.Posts[0].Origin != "" || c.Posts[1].Origin != pathWeb {
		t.Errorf("origins %q and %q, want none and %s", c.Posts[0].Origin, c.Posts[1].Origin, pathWeb)
	}
	if c.Thread.Title != pathMarotte || c.Thread.Kind != kindPath {
		t.Errorf("thread %+v, want the path thread %s", c.Thread, pathMarotte)
	}

	c = mustConversation(t, s, child.Thread, 0, 0)
	if got := postSubjects(c.Posts); !slices.Equal(got, []string{"child"}) || c.Posts[0].Origin != "" {
		t.Errorf("child thread posts %+v, want child alone with no origin", c.Posts)
	}
}

func TestConversation_aPathThreadsFollowersReachItsPosts(t *testing.T) {
	s, parent, _ := pathFixture(t)
	// bravo follows by posting; charlie and dave work at or above the path;
	// alpha works under it, which a post at the path does not reach.
	want := []string{"bravo", "charlie", "dave"}
	if got := mustConversation(t, s, parent.Thread, 0, 0).Followers; !slices.Equal(got, want) {
		t.Errorf("followers %q, want %q", got, want)
	}
}

func TestConversation_aPathThreadShowsOverlappingGrantsThenTheQueue(t *testing.T) {
	s, parent, _ := pathFixture(t)
	mustReserve(t, s, reserving("alpha", pathWeb, time.Hour))
	mustReserve(t, s, reserving("charlie", "/workspace/other", time.Hour))
	mustQueue(t, s, reserving("bravo", pathMarotte, time.Hour))
	mustQueue(t, s, reserving("dave", pathMarotte, time.Hour))

	got := mustConversation(t, s, parent.Thread, 0, 0).Reservations
	type row struct {
		holder, path, state string
		place               int
	}
	want := []row{
		{"alpha", pathWeb, reservationHeld, 0},
		{"bravo", pathMarotte, reservationQueued, 1},
		{"dave", pathMarotte, reservationQueued, 2},
	}
	var rows []row
	for _, e := range got {
		rows = append(rows, row{e.Holder, e.Path, e.State, e.Place})
	}
	if !slices.Equal(rows, want) {
		t.Errorf("reservations %+v, want %+v", rows, want)
	}
}

func TestConversation_postsCountTheirReaders(t *testing.T) {
	s, parent, _ := pathFixture(t)
	mustAck(t, s, sess("charlie"), 1<<62)

	p := mustConversation(t, s, parent.Thread, 0, 0).Posts[0]
	// charlie and dave work at or above the path; charlie has read it.
	if p.Recipients != 2 || p.Read != 1 {
		t.Errorf("read %d of %d, want 1 of 2", p.Read, p.Recipients)
	}
}

func TestConversation_aPostTheUserReadInTheUIReadsRead(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	mustPost(t, s, posting(sess("alpha"), Target{Agent: userName}))
	mustPost(t, s, posting(sess("bravo"), Target{Agent: userName}))
	second := mustPost(t, s, posting(sess("alpha"), Target{Agent: userName}))
	if _, err := s.ReadThread(t.Context(), Caller{User: true}, second.Thread, everyPost); err != nil {
		t.Fatal(err)
	}

	// bravo's unread post holds @user's read cursor below alpha's second.
	if p := mustConversation(t, s, second.Thread, 0, 0).Posts[1]; p.Read != 1 || p.Recipients != 1 {
		t.Errorf("alpha's second post read %d of %d, want 1 of 1", p.Read, p.Recipients)
	}
	rc, err := s.MessageReceipts(t.Context(), second.Message)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Recipients) != 1 || rc.Recipients[0].State != receiptRead {
		t.Errorf("receipts %+v, want @user read", rc.Recipients)
	}
}

func TestConversation_marksOnlyThePostsTheUserHasNotRead(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	toUser := mustPost(t, s, posting(sess("alpha"), Target{Agent: userName}))
	mustPost(t, s, userPosting(Target{Agent: "alpha"}))

	unread := func() []bool {
		var out []bool
		for _, p := range mustConversation(t, s, toUser.Thread, 0, 0).Posts {
			out = append(out, p.Unread)
		}
		return out
	}
	// alpha's post reached @user unread; @user's own post went to alpha.
	if got, want := unread(), []bool{true, false}; !slices.Equal(got, want) {
		t.Errorf("unread %v, want %v", got, want)
	}
	if _, err := s.ReadThread(t.Context(), Caller{User: true}, toUser.Thread, everyPost); err != nil {
		t.Fatal(err)
	}
	if got, want := unread(), []bool{false, false}; !slices.Equal(got, want) {
		t.Errorf("unread after ReadThread %v, want %v", got, want)
	}
}

func TestConversation_pagesOlderPostsOldestFirst(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	var th uint64
	for _, subject := range []string{"d1", "d2", "d3"} {
		th = say(t, s, "alpha", Target{Agent: "bravo"}, subject).Thread
		clk.advance(time.Minute)
	}

	newest := mustConversation(t, s, th, 0, 2)
	if got, want := postSubjects(newest.Posts), []string{"d2", "d3"}; !slices.Equal(got, want) {
		t.Fatalf("newest page %q, want %q", got, want)
	}
	if newest.Next != newest.Posts[0].ID {
		t.Errorf("next %d, want the oldest shown post's id %d", newest.Next, newest.Posts[0].ID)
	}
	older := mustConversation(t, s, th, newest.Next, 2)
	if got := postSubjects(older.Posts); !slices.Equal(got, []string{"d1"}) || older.Next != 0 {
		t.Errorf("older page %q next %d, want d1 and no next", got, older.Next)
	}
}

func TestConversation_aDirectThreadCarriesBothCardsAndMarksUsersPosts(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	sent := mustPost(t, s, userPosting(Target{Agent: "alpha"}))
	mustPost(t, s, posting(sess("alpha"), Target{Agent: userName}))

	c := mustConversation(t, s, sent.Thread, 0, 0)
	if c.Thread.Title != "@alpha and @user" {
		t.Errorf("title %q, want @alpha and @user", c.Thread.Title)
	}
	if len(c.Posts) != 2 || !c.Posts[0].FromUser || c.Posts[1].FromUser {
		t.Errorf("posts %+v, want @user's first and marked, alpha's unmarked", c.Posts)
	}
	var names []string
	for i := range c.Cards {
		names = append(names, c.Cards[i].Name)
	}
	if !slices.Equal(names, []string{"alpha", userName}) {
		t.Errorf("cards %q, want alpha and user", names)
	}
	if len(c.Reservations) != 0 || len(c.Followers) != 0 {
		t.Errorf("a direct thread with reservations %+v, followers %q, want neither", c.Reservations, c.Followers)
	}
}

func TestConversation_theCardsThreadAndUnknownIDsAreNotFound(t *testing.T) {
	s := openTestStore(t)
	var cards uint64
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		var err error
		cards, _, _, err = s.t.threadKey.Lookup(r, []byte(cardsKey))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint64{cards, 9999} {
		_, err := s.Conversation(t.Context(), id, 0, 0)
		wantRefusal(t, err, KindNotFound, "", "no conversation")
		_, err = s.ReadThread(t.Context(), Caller{User: true}, id, everyPost)
		wantRefusal(t, err, KindNotFound, "", "no conversation")
	}
}

func TestMessageReceipts_listsADelegateButNotItsParentsCopy(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	review := scopedDelegate(t, s, "alpha", "review", func(g *Grant) { g.AllowAgents = []string{"bravo"} })
	sent := say(t, s, "bravo", Target{Agent: review.Name}, "for review")

	rc, err := s.MessageReceipts(t.Context(), sent.Message)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Recipients) != 1 || rc.Recipients[0].Name != review.Name || rc.Recipients[0].State != receiptUnread {
		t.Errorf("recipients %+v, want %s unread alone", rc.Recipients, review.Name)
	}
	if p := mustConversation(t, s, sent.Thread, 0, 0).Posts[0]; p.Recipients != 1 {
		t.Errorf("post counts %d recipients, want the delegate alone", p.Recipients)
	}
	if n := rowFor(t, mustConversations(t, s, ConversationsQuery{}), sent.Thread).Participants; n != 2 {
		t.Errorf("%d participants, want bravo and the delegate: alpha holds only a copy", n)
	}
	_, err = s.MessageReceipts(t.Context(), 9999)
	wantRefusal(t, err, KindNotFound, "", "no message")
}

func TestReadThread_aPathConversationTakesInItsSubpaths(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha", pathWeb)
	parent := mustPost(t, s, userPosting(Target{Path: pathMarotte}))
	child := say(t, s, "alpha", Target{Path: pathWeb}, "child")

	p := mustConversations(t, s, ConversationsQuery{})
	if a, b := rowFor(t, p, parent.Thread).Unread, rowFor(t, p, child.Thread).Unread; a != 1 || b != 1 {
		t.Errorf("unread parent %d, child %d, want the child's post in both", a, b)
	}
	n, err := s.ReadThread(t.Context(), Caller{User: true}, parent.Thread, everyPost)
	if err != nil || n != 1 {
		t.Fatalf("ReadThread stamped %d (%v), want the child's post", n, err)
	}
	p = mustConversations(t, s, ConversationsQuery{})
	if a, b := rowFor(t, p, parent.Thread).Unread, rowFor(t, p, child.Thread).Unread; a != 0 || b != 0 {
		t.Errorf("after reading the parent unread parent %d, child %d, want 0 and 0", a, b)
	}
}
