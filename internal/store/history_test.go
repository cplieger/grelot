package store

import (
	"slices"
	"testing"
	"time"
)

func mustHistory(t *testing.T, s *Store, session string, q HistoryQuery) History {
	t.Helper()
	h, err := s.History(t.Context(), session, &q)
	if err != nil {
		t.Fatalf("History(%s, %+v): %v", session, q, err)
	}
	return h
}

// historySubjects is the subjects a history read returns, oldest first.
func historySubjects(t *testing.T, s *Store, session string, q HistoryQuery) []string {
	t.Helper()
	return subjects(mustHistory(t, s, session, q).Entries)
}

func wantSubjects(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s: subjects %q, want %q", label, got, want)
	}
}

// say posts subject from name's session to.
func say(t *testing.T, s *Store, name string, to Target, subject string) Posted {
	t.Helper()
	p := posting(sess(name), to)
	p.Subject = subject
	return mustPost(t, s, p)
}

// keepAlive moves each identity's lease far out, so a test can step the
// clock past a day without the identities going dormant.
func keepAlive(t *testing.T, s *Store, ids ...uint64) {
	t.Helper()
	for _, id := range ids {
		editIdentity(t, s, id, func(r *identityRow) { r.Lease = lease{Expires: testNow.Add(30 * 24 * time.Hour)} })
	}
}

func TestHistory_readAndUnreadMailWithOwnPostsInTimeOrder(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	regAt(t, s, "charlie", "/workspace/charlie")
	say(t, s, "alpha", Target{Agent: "bravo"}, "a1")
	clk.advance(time.Minute)
	say(t, s, "bravo", Target{Agent: "alpha"}, "b1")
	clk.advance(time.Minute)
	say(t, s, "charlie", Target{Announce: true}, "c1")
	page := mustInbox(t, s, sess("bravo"), 1)
	mustAck(t, s, sess("bravo"), page.Entries[0].Seq)
	clk.advance(time.Minute)

	h := mustHistory(t, s, sess("bravo"), HistoryQuery{})
	if got := subjects(h.Entries); !slices.Equal(got, []string{"a1", "b1", "c1"}) {
		t.Fatalf("subjects %q, want [a1 b1 c1]", got)
	}
	read, own, unread := h.Entries[0], h.Entries[1], h.Entries[2]
	if !read.ReadAt.Equal(testNow.Add(2*time.Minute)) || read.From != "alpha" || read.Seq == 0 {
		t.Errorf("read entry %+v, want alpha's delivery with the ack's read_at", read)
	}
	if own.From != "bravo" || own.Relation != "you" || own.Seq != 0 || own.Kind != kindDirect ||
		!own.PostedAt.Equal(testNow.Add(time.Minute)) || own.Thread != read.Thread || own.Message == 0 {
		t.Errorf("own post %+v, want bravo's post in the direct thread, relation you, no seq", own)
	}
	if !unread.ReadAt.IsZero() || unread.Kind != kindAnnounce || unread.Relation != "same environment" {
		t.Errorf("unread entry %+v, want charlie's unread announcement, same environment", unread)
	}
	if h.Identity.ID != bravo.ID {
		t.Errorf("identity %d, want bravo's %d", h.Identity.ID, bravo.ID)
	}
}

func TestHistory_neverMovesTheCursorsOrTheNudgeState(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	direct(t, s, "alpha", "bravo", 3)
	page := mustInbox(t, s, sess("bravo"), 1)
	mustAck(t, s, sess("bravo"), page.Entries[0].Seq)
	editIdentity(t, s, bravo.ID, func(r *identityRow) {
		r.Shown, r.Nudged, r.Nudges, r.NudgedAt = 2, 2, 1, testNow
	})
	before := getIdentity(t, s, bravo.ID)
	clk.advance(time.Minute)

	for _, q := range []HistoryQuery{{}, {LastRead: true}, {FromMe: true}, {Thread: page.Entries[0].Thread}, {Grep: "3"}, {Full: true}} {
		mustHistory(t, s, sess("bravo"), q)
	}
	after := getIdentity(t, s, bravo.ID)
	if after.Read != before.Read || after.Shown != before.Shown || after.LastPage != before.LastPage {
		t.Errorf("read %d shown %d last_page %+v, want %d %d %+v", after.Read, after.Shown, after.LastPage,
			before.Read, before.Shown, before.LastPage)
	}
	if after.Nudged != before.Nudged || after.Nudges != before.Nudges || !after.NudgedAt.Equal(before.NudgedAt) {
		t.Errorf("nudged %d nudges %d nudged_at %v, want %d %d %v", after.Nudged, after.Nudges, after.NudgedAt,
			before.Nudged, before.Nudges, before.NudgedAt)
	}
	for _, d := range deliveriesTo(t, s, bravo.ID)[1:] {
		if !d.ReadAt.IsZero() {
			t.Errorf("delivery %d read_at %v after history, want unread", d.Seq, d.ReadAt)
		}
	}
	if got := subjects(mustInbox(t, s, sess("bravo"), 0).Entries); !slices.Equal(got, []string{"2", "3"}) {
		t.Errorf("inbox after history %q, want [2 3] still new", got)
	}
}

func TestHistory_threadFilterReadsOneThreadBothSides(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	regAt(t, s, "charlie", "/workspace/charlie")
	a1 := say(t, s, "alpha", Target{Agent: "bravo"}, "a1")
	clk.advance(time.Minute)
	say(t, s, "bravo", Target{Agent: "alpha"}, "b1")
	clk.advance(time.Minute)
	p1 := say(t, s, "alpha", Target{Path: "/workspace/bravo/x"}, "p1")
	clk.advance(time.Minute)
	say(t, s, "charlie", Target{Agent: "bravo"}, "c1")
	clk.advance(time.Minute)
	say(t, s, "bravo", Target{Path: "/workspace/bravo/x"}, "p2")

	wantSubjects(t, "direct thread", historySubjects(t, s, sess("bravo"), HistoryQuery{Thread: a1.Thread}), "a1", "b1")
	wantSubjects(t, "path thread", historySubjects(t, s, sess("bravo"), HistoryQuery{Thread: p1.Thread}), "p1", "p2")
	wantSubjects(t, "no such thread", historySubjects(t, s, sess("bravo"), HistoryQuery{Thread: 9999}))
}

func TestHistory_pathFilterReadsThePathsThreadAndItsSubtree(t *testing.T) {
	s, clk := openClockStore(t)
	bravo := regAt(t, s, "bravo", "/workspace")
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "delta", "/workspace/bravo/sub")
	for _, p := range []struct{ to, subject string }{
		{"/workspace/bravo", "p-root"},
		{"/workspace/bravo/sub", "p-sub"},
		{"/workspace/bravo-old", "p-sibling"},
		{"/workspace", "p-parent"},
	} {
		say(t, s, "alpha", Target{Path: p.to}, p.subject)
		clk.advance(time.Minute)
	}
	say(t, s, "bravo", Target{Path: "/workspace/bravo/sub"}, "own-sub")
	clk.advance(time.Minute)
	say(t, s, "bravo", Target{Path: "/workspace/bravo-old"}, "own-sibling")
	clk.advance(time.Minute)
	say(t, s, "alpha", Target{Agent: "bravo"}, "d")
	deliverEvents(t, s, bravo.ID, 1)

	wantSubjects(t, "path", historySubjects(t, s, sess("bravo"), HistoryQuery{Path: "/workspace/bravo"}),
		"p-root", "p-sub", "own-sub")
	h := mustHistory(t, s, sess("bravo"), HistoryQuery{})
	if !slices.ContainsFunc(h.Entries, func(e Entry) bool { return e.Kind == kindJoin && e.ThreadPath == "/workspace/bravo/sub" }) {
		t.Errorf("unfiltered history %q lacks delta's join card, which the path filter must leave out", subjects(h.Entries))
	}
	for _, bad := range []string{"workspace/bravo", "/workspace/../bravo", "/workspace/bravo/"} {
		_, err := s.History(t.Context(), sess("bravo"), &HistoryQuery{Path: bad})
		wantRefusal(t, err, KindInvalid, "", "canonical path")
	}
}

func TestHistory_withFilterReadsTheDirectThreadWithThatAgent(t *testing.T) {
	s, clk := openClockStore(t)
	for _, n := range []string{"alpha", "bravo", "charlie", "dave"} {
		regAt(t, s, n, "/workspace/"+n)
	}
	for _, m := range []struct {
		from, to, subject string
	}{{"alpha", "bravo", "a1"}, {"bravo", "alpha", "b1"}, {"charlie", "bravo", "c1"}, {"bravo", "charlie", "b2"}} {
		say(t, s, m.from, Target{Agent: m.to}, m.subject)
		clk.advance(time.Minute)
	}
	say(t, s, "alpha", Target{Announce: true}, "ann")

	wantSubjects(t, "with alpha", historySubjects(t, s, sess("bravo"), HistoryQuery{With: "alpha"}), "a1", "b1")
	wantSubjects(t, "with charlie", historySubjects(t, s, sess("bravo"), HistoryQuery{With: "charlie"}), "c1", "b2")
	wantSubjects(t, "with dave, no thread", historySubjects(t, s, sess("bravo"), HistoryQuery{With: "dave"}))
	_, err := s.History(t.Context(), sess("bravo"), &HistoryQuery{With: "bravo"})
	wantRefusal(t, err, KindInvalid, "", "yourself")
	_, err = s.History(t.Context(), sess("bravo"), &HistoryQuery{With: "nobody"})
	wantRefusal(t, err, KindNotFound, "", "nobody")
}

func TestHistory_announceFilterReadsAnnouncementsBothSides(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	say(t, s, "alpha", Target{Announce: true}, "ann1")
	clk.advance(time.Minute)
	say(t, s, "alpha", Target{Agent: "bravo"}, "d1")
	clk.advance(time.Minute)
	say(t, s, "bravo", Target{Announce: true}, "ann2")
	clk.advance(time.Minute)
	say(t, s, "bravo", Target{Agent: "alpha"}, "own-direct")

	wantSubjects(t, "announce", historySubjects(t, s, sess("bravo"), HistoryQuery{Announce: true}), "ann1", "ann2")
}

func TestHistory_fromFilter(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace")
	regAt(t, s, "charlie", "/workspace/charlie")
	for _, m := range []struct {
		from    string
		to      Target
		subject string
	}{
		{"alpha", Target{Agent: "bravo"}, "a1"},
		{"bravo", Target{Agent: "alpha"}, "b1"},
		{"alpha", Target{Path: "/workspace/alpha/x"}, "a2"},
		{"charlie", Target{Agent: "bravo"}, "c1"},
		{"alpha", Target{Announce: true}, "a3"},
		{"bravo", Target{Announce: true}, "b2"},
	} {
		say(t, s, m.from, m.to, m.subject)
		clk.advance(time.Minute)
	}

	wantSubjects(t, "from alpha", historySubjects(t, s, sess("bravo"), HistoryQuery{From: "alpha"}), "a1", "a2", "a3")
	wantSubjects(t, "from charlie, not its join card", historySubjects(t, s, sess("bravo"), HistoryQuery{From: "charlie"}), "c1")
	wantSubjects(t, "from @me, not the card about bravo", historySubjects(t, s, sess("bravo"), HistoryQuery{FromMe: true}), "b1", "b2")
	wantSubjects(t, "from its own name", historySubjects(t, s, sess("bravo"), HistoryQuery{From: "bravo"}), "b1", "b2")
	_, err := s.History(t.Context(), sess("bravo"), &HistoryQuery{From: "alpha", FromMe: true})
	wantRefusal(t, err, KindInvalid, "", "@me")
	_, err = s.History(t.Context(), sess("bravo"), &HistoryQuery{From: "nobody"})
	wantRefusal(t, err, KindNotFound, "", "nobody")
}

func TestHistory_sinceAndUntil(t *testing.T) {
	s, clk := openClockStore(t)
	alpha := regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	keepAlive(t, s, alpha.ID, bravo.ID)
	say(t, s, "alpha", Target{Agent: "bravo"}, "m1")
	clk.advance(time.Minute)
	say(t, s, "bravo", Target{Agent: "alpha"}, "b1")
	clk.set(testNow.Add(25 * time.Hour))
	say(t, s, "alpha", Target{Agent: "bravo"}, "m2")
	clk.advance(time.Minute)
	say(t, s, "bravo", Target{Agent: "alpha"}, "b2")

	wantSubjects(t, "default: the last 24 h", historySubjects(t, s, sess("bravo"), HistoryQuery{}), "m2", "b2")
	wantSubjects(t, "since the first post", historySubjects(t, s, sess("bravo"), HistoryQuery{Since: testNow}), "m1", "b1", "m2", "b2")
	wantSubjects(t, "since and until", historySubjects(t, s, sess("bravo"),
		HistoryQuery{Since: testNow, Until: testNow.Add(time.Hour)}), "m1", "b1")
	wantSubjects(t, "until the second own post's time", historySubjects(t, s, sess("bravo"),
		HistoryQuery{Since: testNow.Add(time.Minute), Until: testNow.Add(25 * time.Hour)}), "b1", "m2")
	_, err := s.History(t.Context(), sess("bravo"), &HistoryQuery{Since: testNow.Add(time.Hour), Until: testNow})
	wantRefusal(t, err, KindInvalid, "", "since", "until")
	wantSubjects(t, "until alone, inside the default window", historySubjects(t, s, sess("bravo"),
		HistoryQuery{Until: testNow.Add(25 * time.Hour)}), "m2")
	_, err = s.History(t.Context(), sess("bravo"), &HistoryQuery{Until: testNow.Add(time.Hour)})
	wantRefusal(t, err, KindInvalid, "", "--until", "default 24 h window", "--since")
}

func TestHistory_grepIgnoresCaseOverSubjectAndBody(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	for _, m := range []struct{ from, to, subject, body string }{
		{"alpha", "bravo", "Fix NOTIFY path", "x"},
		{"alpha", "bravo", "other", "the notify hook\nline 2"},
		{"alpha", "bravo", "nothing", "none"},
		{"bravo", "alpha", "mine", "Notify me"},
	} {
		p := posting(sess(m.from), Target{Agent: m.to})
		p.Subject, p.Body = m.subject, m.body
		mustPost(t, s, p)
		clk.advance(time.Minute)
	}
	deliverEvents(t, s, bravo.ID, 1)

	wantSubjects(t, "notify", historySubjects(t, s, sess("bravo"), HistoryQuery{Grep: "notify"}), "Fix NOTIFY path", "other", "mine")
	wantSubjects(t, "a body's second line", historySubjects(t, s, sess("bravo"), HistoryQuery{Grep: "LINE 2"}), "other")
	h := mustHistory(t, s, sess("bravo"), HistoryQuery{Grep: "EVENT"})
	if len(h.Entries) != 1 || h.Entries[0].Kind != kindIdentityExpiring || h.Entries[0].Text != "event" {
		t.Errorf("grep EVENT %+v, want the one event, matched on its text", h.Entries)
	}
}

func TestHistory_limitKeepsTheNewestWithTheNewestLast(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	var all []string
	for i := 1; i <= 25; i++ {
		subject := string(rune('A'+i/10)) + string(rune('0'+i%10))
		say(t, s, "alpha", Target{Agent: "bravo"}, subject)
		all = append(all, subject)
		clk.advance(time.Minute)
	}

	wantSubjects(t, "default 20", historySubjects(t, s, sess("bravo"), HistoryQuery{}), all[5:]...)
	wantSubjects(t, "limit 2", historySubjects(t, s, sess("bravo"), HistoryQuery{Limit: 2}), all[23:]...)
	wantSubjects(t, "limit 100", historySubjects(t, s, sess("bravo"), HistoryQuery{Limit: 100}), all...)
	say(t, s, "bravo", Target{Agent: "alpha"}, "own")
	wantSubjects(t, "limit 2 with an own post", historySubjects(t, s, sess("bravo"), HistoryQuery{Limit: 2}), all[24], "own")
	for _, bad := range []int{-1, MaxHistoryLimit + 1} {
		_, err := s.History(t.Context(), sess("bravo"), &HistoryQuery{Limit: bad})
		wantRefusal(t, err, KindInvalid, "", "limit")
	}
}

func TestHistory_fullBodiesOnlyWhenAsked(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	for _, m := range []struct{ from, to, subject, body string }{
		{"alpha", "bravo", "two lines", "line one\nline two"},
		{"alpha", "bravo", "one line", "only line"},
		{"bravo", "alpha", "own", "mine one\nmine two"},
	} {
		p := posting(sess(m.from), Target{Agent: m.to})
		p.Subject, p.Body = m.subject, m.body
		mustPost(t, s, p)
		clk.advance(time.Minute)
	}

	short := mustHistory(t, s, sess("bravo"), HistoryQuery{}).Entries
	type cut struct {
		body      string
		truncated bool
	}
	var got []cut
	for _, e := range short {
		got = append(got, cut{e.Body, e.Truncated})
	}
	if want := []cut{{"line one", true}, {"only line", false}, {"mine one", true}}; !slices.Equal(got, want) {
		t.Errorf("default bodies %+v, want %+v", got, want)
	}
	got = got[:0]
	for _, e := range mustHistory(t, s, sess("bravo"), HistoryQuery{Full: true}).Entries {
		got = append(got, cut{e.Body, e.Truncated})
	}
	if want := []cut{{"line one\nline two", false}, {"only line", false}, {"mine one\nmine two", false}}; !slices.Equal(got, want) {
		t.Errorf("full bodies %+v, want %+v", got, want)
	}
}

func TestHistory_lastReadIsTheLastAckedInboxPage(t *testing.T) {
	s, clk := openClockStore(t)
	alpha := regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	keepAlive(t, s, alpha.ID, bravo.ID)
	direct(t, s, "alpha", "bravo", 4)
	say(t, s, "bravo", Target{Agent: "alpha"}, "own")

	wantSubjects(t, "before any ack", historySubjects(t, s, sess("bravo"), HistoryQuery{LastRead: true}))
	first := mustInbox(t, s, sess("bravo"), 1)
	mustAck(t, s, sess("bravo"), first.Entries[0].Seq)
	page := mustInbox(t, s, sess("bravo"), 2)
	mustAck(t, s, sess("bravo"), page.Entries[1].Seq)
	clk.advance(30 * time.Hour)

	wantSubjects(t, "last read, past the default window", historySubjects(t, s, sess("bravo"), HistoryQuery{LastRead: true}), "2", "3")
	wantSubjects(t, "last read with grep", historySubjects(t, s, sess("bravo"), HistoryQuery{LastRead: true, Grep: "3"}), "3")
	wantSubjects(t, "last read with a later since", historySubjects(t, s, sess("bravo"),
		HistoryQuery{LastRead: true, Since: testNow.Add(time.Hour)}))
}

func TestHistory_filtersCombine(t *testing.T) {
	s, clk := openClockStore(t)
	alpha := regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace")
	charlie := regAt(t, s, "charlie", "/workspace/charlie")
	keepAlive(t, s, alpha.ID, bravo.ID, charlie.ID)
	d := say(t, s, "alpha", Target{Agent: "bravo"}, "a-direct")
	clk.advance(time.Minute)
	say(t, s, "bravo", Target{Agent: "alpha"}, "b-direct")
	clk.advance(time.Minute)
	p := say(t, s, "charlie", Target{Path: "/workspace/x"}, "c-path notify")
	clk.advance(time.Minute)
	say(t, s, "alpha", Target{Path: "/workspace/x"}, "a-path")
	clk.advance(time.Minute)
	say(t, s, "alpha", Target{Announce: true}, "a-ann")
	clk.advance(time.Minute)
	say(t, s, "charlie", Target{Agent: "bravo"}, "c-direct")
	clk.set(testNow.Add(25 * time.Hour))
	say(t, s, "charlie", Target{Announce: true}, "c-ann")
	since := testNow

	for _, c := range []struct {
		name string
		q    HistoryQuery
		want []string
	}{
		{"thread and from", HistoryQuery{Thread: p.Thread, From: "charlie", Since: since}, []string{"c-path notify"}},
		{"with and from me", HistoryQuery{With: "alpha", FromMe: true, Since: since}, []string{"b-direct"}},
		{"path and grep", HistoryQuery{Path: "/workspace/x", Grep: "NOTIFY", Since: since}, []string{"c-path notify"}},
		{"announce in the default window", HistoryQuery{Announce: true}, []string{"c-ann"}},
		{"announce since", HistoryQuery{Announce: true, Since: since}, []string{"a-ann", "c-ann"}},
		{"thread and announce", HistoryQuery{Thread: d.Thread, Announce: true, Since: since}, nil},
		{"thread and another with", HistoryQuery{Thread: d.Thread, With: "charlie", Since: since}, nil},
		{"from and limit", HistoryQuery{From: "alpha", Since: since, Limit: 2}, []string{"a-path", "a-ann"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantSubjects(t, c.name, historySubjects(t, s, sess("bravo"), c.q), c.want...)
		})
	}
}

func TestHistory_skipsExpiredRowsTheSweepHasNotReached(t *testing.T) {
	s, clk := openShortThreadStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	say(t, s, "alpha", Target{Agent: "bravo"}, "old")
	say(t, s, "bravo", Target{Path: "/workspace/bravo/x"}, "own-old")
	clk.advance(shortThreadTTL)
	say(t, s, "alpha", Target{Agent: "bravo"}, "new")

	wantSubjects(t, "after the first two expired", historySubjects(t, s, sess("bravo"), HistoryQuery{Since: testNow}), "new")
}

func TestHistory_keepsARetiredSendersMessagesAndRelation(t *testing.T) {
	s, _ := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	regAt(t, s, "bravo", "/workspace/bravo")
	say(t, s, "alpha", Target{Agent: "bravo"}, "a1")
	if err := s.Retire(t.Context(), sess("alpha")); err != nil {
		t.Fatal(err)
	}

	h := mustHistory(t, s, sess("bravo"), HistoryQuery{})
	if len(h.Entries) != 1 || h.Entries[0].From != "alpha" || h.Entries[0].Relation != "same environment" {
		t.Errorf("entries %+v, want alpha's message, same environment, from its snapshot", h.Entries)
	}
}

func TestHistory_needsALiveIdentity(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "bravo", "/workspace/bravo")
	_, err := s.History(t.Context(), sess("nobody"), &HistoryQuery{})
	wantRefusal(t, err, KindGone, "none")
	clk.advance(6 * time.Hour)
	_, err = s.History(t.Context(), sess("bravo"), &HistoryQuery{})
	wantRefusal(t, err, KindGone, "dormant", "grelot revive")
}

func TestHistory_clearsEnded(t *testing.T) {
	s, _ := openClockStore(t)
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	if err := s.EndSession(t.Context(), sess("bravo")); err != nil {
		t.Fatal(err)
	}
	mustHistory(t, s, sess("bravo"), HistoryQuery{})
	if getIdentity(t, s, bravo.ID).Ended {
		t.Error("ended still set after a history read, want it cleared like any identity request")
	}
}
