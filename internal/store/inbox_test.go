//go:build !windows

package store

import (
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// direct posts n messages from one registered identity to another, the
// subjects "1" to "n".
func direct(t *testing.T, s *Store, from, to string, n int) {
	t.Helper()
	for i := range n {
		p := posting(sess(from), Target{Agent: to})
		p.Subject = string(rune('1' + i))
		mustPost(t, s, p)
	}
}

func mustInbox(t *testing.T, s *Store, session string, limit int) Page {
	t.Helper()
	p, err := s.Inbox(t.Context(), Caller{Session: session}, limit)
	if err != nil {
		t.Fatalf("Inbox(%s): %v", session, err)
	}
	return p
}

func mustAck(t *testing.T, s *Store, session string, upto uint64) Acked {
	t.Helper()
	a, _, err := s.Ack(t.Context(), Keyed{}, Caller{Session: session}, upto)
	if err != nil {
		t.Fatalf("Ack(%s, %d): %v", session, upto, err)
	}
	return a
}

// total counts every delivery in c, quiet ones included.
func total(c Counts) int {
	return c.Direct + c.Path + c.Announce + c.Identity + c.Reservation + c.Receipts + c.Cards + c.Copies + c.QuietPath
}

func subjects(es []Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Subject)
	}
	return out
}

func TestInbox_showsUnreadMailOnceItIsAcked(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	direct(t, s, "alpha", "bravo", 2)

	page := mustInbox(t, s, sess("bravo"), 0)
	if got := subjects(page.Entries); len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Fatalf("subjects %q, want [1 2]", got)
	}
	e := page.Entries[0]
	if e.Kind != kindDirect || e.Body != "body" || e.From != "alpha" || e.Host != "host-a/app" ||
		e.OS != "linux" || e.Arch != "amd64" || e.Relation != "same environment" || !e.Wake || e.Message == 0 {
		t.Errorf("entry %+v, want a wake direct message from alpha, same environment", e)
	}
	if page.Unread.Direct != 2 || page.Unread.Wake() != 2 || total(page.Unread) != 2 {
		t.Errorf("unread %+v, want 2 direct, both wake", page.Unread)
	}

	clk.advance(time.Minute)
	acked := mustAck(t, s, sess("bravo"), page.Entries[1].Seq)
	if acked.Read != page.Entries[1].Seq || total(acked.Left) != 0 {
		t.Errorf("ack %+v, want read %d and nothing left", acked, page.Entries[1].Seq)
	}
	if again := mustInbox(t, s, sess("bravo"), 0); len(again.Entries) != 0 || total(again.Unread) != 0 {
		t.Errorf("second inbox %+v, want nothing new", again)
	}
	row := getIdentity(t, s, bravo.ID)
	if want := (seqRange{From: page.Entries[0].Seq, To: page.Entries[1].Seq}); row.LastPage != want {
		t.Errorf("last_page %+v, want %+v", row.LastPage, want)
	}
	for _, d := range deliveriesTo(t, s, bravo.ID) {
		if !d.ReadAt.Equal(testNow.Add(time.Minute)) {
			t.Errorf("delivery %d read_at %v, want the ack's time", d.Seq, d.ReadAt)
		}
	}
	if n := len(deliveriesTo(t, s, bravo.ID)); n != 2 {
		t.Errorf("%d delivery rows after the read, want both kept", n)
	}
}

func TestAck_neverRewindsAndStopsAtTheHighestSeq(t *testing.T) {
	s, _ := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	direct(t, s, "alpha", "bravo", 3)
	seqs := []uint64{}
	for _, e := range mustInbox(t, s, sess("bravo"), 0).Entries {
		seqs = append(seqs, e.Seq)
	}

	if a := mustAck(t, s, sess("bravo"), seqs[1]); a.Read != seqs[1] || total(a.Left) != 1 {
		t.Errorf("ack to the second: %+v, want read %d, 1 left", a, seqs[1])
	}
	for _, stale := range []uint64{0, seqs[0]} {
		if a := mustAck(t, s, sess("bravo"), stale); a.Read != seqs[1] {
			t.Errorf("stale ack %d moved read to %d, want %d", stale, a.Read, seqs[1])
		}
	}
	if row := getIdentity(t, s, bravo.ID); row.LastPage != (seqRange{From: seqs[0], To: seqs[1]}) {
		t.Errorf("last_page %+v after stale acks, want the first page's", row.LastPage)
	}
	if a := mustAck(t, s, sess("bravo"), seqs[2]+50); a.Read != seqs[2] {
		t.Errorf("ack past the highest seq: read %d, want %d", a.Read, seqs[2])
	}
}

func TestInbox_aPageSmallerThanTheUnreadAcksOnlyWhatItShowed(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	direct(t, s, "alpha", "bravo", 3)

	page := mustInbox(t, s, sess("bravo"), 2)
	if got := subjects(page.Entries); len(got) != 2 || got[1] != "2" || total(page.Unread) != 3 {
		t.Fatalf("page %q with %d unread, want [1 2] of 3", got, total(page.Unread))
	}
	acked := mustAck(t, s, sess("bravo"), page.Entries[1].Seq)
	if total(acked.Left) != 1 || acked.Left.Direct != 1 {
		t.Errorf("left %+v, want the one direct message not shown", acked.Left)
	}
	if d := deliveriesTo(t, s, bravo.ID)[2]; !d.ReadAt.IsZero() {
		t.Errorf("the unshown delivery has read_at %v, want none until its own ack", d.ReadAt)
	}
	clk.advance(time.Hour)
	rest := mustInbox(t, s, sess("bravo"), 2)
	if got := subjects(rest.Entries); len(got) != 1 || got[0] != "3" {
		t.Fatalf("next page %q, want [3]", got)
	}
	mustAck(t, s, sess("bravo"), rest.Entries[0].Seq)
	ds := deliveriesTo(t, s, bravo.ID)
	if !ds[0].ReadAt.Equal(testNow) || !ds[2].ReadAt.Equal(testNow.Add(time.Hour)) {
		t.Errorf("read_at %v and %v, want each row's own ack time, kept by the later ack", ds[0].ReadAt, ds[2].ReadAt)
	}
	if row := getIdentity(t, s, bravo.ID); row.LastPage != (seqRange{From: ds[2].Seq, To: ds[2].Seq}) {
		t.Errorf("last_page %+v, want the last page alone", row.LastPage)
	}
}

func TestInbox_limit(t *testing.T) {
	s, _ := openClockStore(t)
	me := regAt(t, s, "bravo", "/workspace/bravo")
	deliverEvents(t, s, me.ID, defaultInboxLimit+5)
	if p := mustInbox(t, s, sess("bravo"), 0); len(p.Entries) != defaultInboxLimit || total(p.Unread) != defaultInboxLimit+5 {
		t.Errorf("default page %d of %d, want %d of %d", len(p.Entries), total(p.Unread), defaultInboxLimit, defaultInboxLimit+5)
	}
	if p := mustInbox(t, s, sess("bravo"), maxInboxLimit); len(p.Entries) != defaultInboxLimit+5 {
		t.Errorf("page of %d at the largest limit, want every one", len(p.Entries))
	}
	for _, bad := range []int{-1, maxInboxLimit + 1} {
		_, err := s.Inbox(t.Context(), Caller{Session: sess("bravo")}, bad)
		wantRefusal(t, err, KindInvalid, "", "limit", "1 to 100")
	}
}

func TestInbox_anEventCarriesItsTextAndNoSender(t *testing.T) {
	s, _ := openClockStore(t)
	me := regAt(t, s, "bravo", "/workspace/bravo")
	putRows(t, s, func(w *wtx) error { return w.writeEvent(me.ID, kindIdentityExpiring, "your identity bravo expires") })
	e := mustInbox(t, s, sess("bravo"), 0).Entries[0]
	if e.Kind != kindIdentityExpiring || e.Text != "your identity bravo expires" || e.From != "" ||
		e.Relation != "" || e.Message != 0 || !e.Wake {
		t.Errorf("event entry %+v, want its text, a wake event with no sender or relation", e)
	}
}

func TestInbox_skipsAnExpiredDelivery(t *testing.T) {
	s, clk := openClockStore(t)
	me := regAt(t, s, "bravo", "/workspace/bravo")
	putRows(t, s, func(w *wtx) error {
		row, _, err := s.t.identities.table.Get(w.r, me.ID)
		if err != nil {
			return err
		}
		return w.deliver(&row, &deliveryRow{Kind: kindIdentityExpiring, Text: "x", Lease: lease{Expires: w.now.Add(time.Minute)}})
	})
	deliverEvents(t, s, me.ID, 1)
	clk.advance(time.Minute)
	p := mustInbox(t, s, sess("bravo"), 0)
	if len(p.Entries) != 1 || p.Entries[0].Seq != 2 || total(p.Unread) != 1 {
		t.Errorf("page %+v, want only seq 2: a delivery at its expiry is gone, swept or not", p)
	}
}

func TestInbox_relationIsReadFromTheMessageSnapshot(t *testing.T) {
	s, _ := openClockStore(t)
	sender := envA()
	sender.Checkouts = []Checkout{co(repoSubflux, "d")}
	reader := envA()
	reader.Checkouts = []Checkout{co(repoSubflux, "c")}
	mustRegister(t, s, registration(sess("alpha"), "alpha", sender, repoSubflux+"/cmd"))
	mustRegister(t, s, registration(sess("bravo"), "bravo", reader, repoSubflux))
	mustPost(t, s, posting(sess("alpha"), Target{Path: repoSubflux + "/internal"}))
	if _, _, err := s.Retire(t.Context(), Keyed{}, Caller{Session: sess("alpha")}); err != nil {
		t.Fatal(err)
	}

	e := mustInbox(t, s, sess("bravo"), 0).Entries[0]
	if e.Kind != kindPath || e.From != "alpha" || e.ThreadPath != repoSubflux+"/internal" || e.Matched != repoSubflux {
		t.Errorf("entry %+v, want alpha's post in the internal thread, matched by the work path", e)
	}
	if want := "same environment, other checkout of " + repoSubflux; e.Relation != want {
		t.Errorf("relation %q after the sender retired, want %q", e.Relation, want)
	}
}

func TestInbox_aCardIsQuietAndRelatedAtItsPath(t *testing.T) {
	s, _ := openClockStore(t)
	w := envA()
	w.Checkouts = []Checkout{co(repoSubflux, "c")}
	mustRegister(t, s, registration(sess("bravo"), "bravo", w, repoSubflux))
	mustRegister(t, s, registration(sess("charlie"), "charlie", w, repoSubflux))

	p := mustInbox(t, s, sess("bravo"), 0)
	if len(p.Entries) != 1 {
		t.Fatalf("%d entries, want charlie's join card", len(p.Entries))
	}
	e := p.Entries[0]
	if e.Kind != kindJoin || e.Wake || e.From != "charlie" || e.Relation != "same checkout" || e.ThreadPath != repoSubflux {
		t.Errorf("card %+v, want a quiet join card from charlie, same checkout", e)
	}
	if want := "@charlie is now active on " + repoSubflux; e.Subject != want {
		t.Errorf("join card subject %q, want %q", e.Subject, want)
	}
	if p.Unread.Cards != 1 || p.Unread.Wake() != 0 {
		t.Errorf("unread %+v, want one card and no wake event", p.Unread)
	}

	if _, _, err := s.Retire(t.Context(), Keyed{}, Caller{Session: sess("charlie")}); err != nil {
		t.Fatal(err)
	}
	p = mustInbox(t, s, sess("bravo"), 0)
	if len(p.Entries) != 2 || p.Entries[1].Kind != kindLeft || p.Entries[1].Wake || p.Unread.Cards != 2 || p.Unread.Wake() != 0 {
		t.Fatalf("page %+v, want the join and left cards, both quiet", p)
	}
	if want := "@charlie has left " + repoSubflux; p.Entries[1].Subject != want {
		t.Errorf("left card subject %q, want %q", p.Entries[1].Subject, want)
	}
}

func TestHold_advancesShownOnlyOverAWakeEvent(t *testing.T) {
	s, _ := openClockStore(t)
	bravo := mustRegister(t, s, registration(sess("bravo"), "bravo", envA(), repoSubflux))
	mustRegister(t, s, registration(sess("charlie"), "charlie", envA(), repoSubflux))

	h, err := s.Hold(t.Context(), Caller{Session: sess("bravo")})
	if err != nil {
		t.Fatal(err)
	}
	if h.Counts.Cards != 1 || h.Counts.Wake() != 0 || h.Shown != 0 {
		t.Errorf("hold over a card alone %+v, want the card counted and shown left at 0", h)
	}
	direct(t, s, "charlie", "bravo", 1)
	h, err = s.Hold(t.Context(), Caller{Session: sess("bravo")})
	if err != nil {
		t.Fatal(err)
	}
	if h.Counts.Direct != 1 || h.Counts.Cards != 1 || h.Shown != 2 {
		t.Errorf("hold %+v, want 1 direct and 1 card, shown 2", h)
	}
	h, err = s.Hold(t.Context(), Caller{Session: sess("bravo")})
	if err != nil {
		t.Fatal(err)
	}
	if total(h.Counts) != 0 || h.Shown != 2 {
		t.Errorf("second hold %+v, want nothing new", h)
	}
	row := getIdentity(t, s, bravo.ID)
	if row.Read != 0 || row.Shown != 2 {
		t.Errorf("read %d shown %d, want a hold to leave read alone", row.Read, row.Shown)
	}
	if p := mustInbox(t, s, sess("bravo"), 0); len(p.Entries) != 2 {
		t.Errorf("inbox after the holds shows %d, want both: shown is not delivery", len(p.Entries))
	}
}

func TestHold_writesTheRowOnlyWhenItMovesSomething(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	before := getIdentity(t, s, bravo.ID)
	start := commitCount(t, s)

	for range 2 {
		clk.advance(time.Minute)
		h, err := s.Hold(t.Context(), Caller{Session: sess("bravo")})
		if err != nil {
			t.Fatal(err)
		}
		if !h.Identity.LastSeen.Equal(clk.now()) {
			t.Errorf("hold reports last seen %v, want the request at %v", h.Identity.LastSeen, clk.now())
		}
	}
	if n := commitCount(t, s) - start; n != 0 {
		t.Errorf("two holds with nothing new committed %d times, want none", n)
	}
	if got := getIdentity(t, s, bravo.ID); !reflect.DeepEqual(got, before) {
		t.Errorf("two holds with nothing new rewrote the row:\n got %+v\nwant %+v", got, before)
	}

	clk.advance(time.Minute)
	direct(t, s, "alpha", "bravo", 1)
	if _, err := s.Hold(t.Context(), Caller{Session: sess("bravo")}); err != nil {
		t.Fatal(err)
	}
	if row := getIdentity(t, s, bravo.ID); row.Shown != 1 || !row.LastSeen.Equal(testNow) {
		t.Errorf("shown %d last seen %v, want shown 1 and last seen still %v", row.Shown, row.LastSeen, testNow)
	}

	clk.set(testNow.Add(5 * time.Minute))
	if _, err := s.Hold(t.Context(), Caller{Session: sess("bravo")}); err != nil {
		t.Fatal(err)
	}
	if row := getIdentity(t, s, bravo.ID); !row.LastSeen.Equal(clk.now()) {
		t.Errorf("stored last seen %v after 5 minutes, want %v", row.LastSeen, clk.now())
	}
}

func TestReads_neverMoveTheLease(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	mustPost(t, s, posting(sess("alpha"), Target{Path: "/workspace/alpha"}))
	direct(t, s, "alpha", "bravo", 1)
	want := getIdentity(t, s, bravo.ID).Lease
	ctx, me := t.Context(), sess("bravo")
	calls := []struct {
		name string
		call func() error
	}{
		{"inbox", func() error { _, err := s.Inbox(ctx, Caller{Session: me}, 0); return err }},
		{"ack", func() error { _, _, err := s.Ack(ctx, Keyed{}, Caller{Session: me}, 1); return err }},
		{"hold", func() error { _, err := s.Hold(ctx, Caller{Session: me}); return err }},
		{"history", func() error { _, err := s.History(ctx, Caller{Session: me}, &HistoryQuery{}); return err }},
		{"follow", func() error { _, err := s.Follow(ctx, Keyed{}, Caller{Session: me}, "/workspace/alpha"); return err }},
		{"unfollow", func() error {
			_, err := s.Unfollow(ctx, Keyed{}, Caller{Session: me}, "/workspace/alpha")
			return err
		}},
	}
	for _, c := range calls {
		clk.advance(10 * time.Minute)
		if err := c.call(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := getIdentity(t, s, bravo.ID).Lease; !got.Expires.Equal(want.Expires) || !got.WarnAt.Equal(want.WarnAt) {
			t.Errorf("%s moved the lease to %+v, want the registration's %+v", c.name, got, want)
		}
	}
}

func TestInbox_aDormantSessionGetsTheReviveOffer(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	direct(t, s, "alpha", "bravo", 2)
	clk.set(bravo.Expires.Add(3*time.Hour + 59*time.Minute))
	want := "your identity @bravo expired 3 h ago with 2 unread; `grelot revive --ttl=<d>` restores it; if reviving is refused or against the user's instructions, finish the user's task and tell the user"

	_, err := s.Inbox(t.Context(), Caller{Session: sess("bravo")}, 0)
	wantRefusal(t, err, KindGone, "dormant", want)
	_, _, err = s.Ack(t.Context(), Keyed{}, Caller{Session: sess("bravo")}, 1)
	wantRefusal(t, err, KindGone, "dormant", want)
	_, err = s.Hold(t.Context(), Caller{Session: sess("bravo")})
	wantRefusal(t, err, KindGone, "dormant", want)
	_, err = s.Inbox(t.Context(), Caller{Session: sess("nobody")}, 0)
	wantRefusal(t, err, KindGone, "none")
}

func TestInbox_clearsEnded(t *testing.T) {
	for _, call := range []string{"inbox", "ack", "hold"} {
		s, _ := openClockStore(t)
		me := regAt(t, s, "bravo", "/workspace/bravo")
		if err := s.EndSession(t.Context(), sess("bravo"), 0); err != nil {
			t.Fatal(err)
		}
		var err error
		switch call {
		case "inbox":
			_, err = s.Inbox(t.Context(), Caller{Session: sess("bravo")}, 0)
		case "ack":
			_, _, err = s.Ack(t.Context(), Keyed{}, Caller{Session: sess("bravo")}, 0)
		case "hold":
			_, err = s.Hold(t.Context(), Caller{Session: sess("bravo")})
		}
		if err != nil {
			t.Fatal(err)
		}
		if getIdentity(t, s, me.ID).Ended {
			t.Errorf("%s left ended set, want it cleared", call)
		}
	}
}

func TestAck_pastOnlyExpiredMailRecordsTheRangeFromRead(t *testing.T) {
	clk := &clock{t: testNow}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{
		Logger: slog.New(newLogSink()), Now: clk.now, Retention: retainFor(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	direct(t, s, "alpha", "bravo", 2)
	ds := deliveriesTo(t, s, bravo.ID)
	clk.advance(11 * time.Minute)

	acked := mustAck(t, s, sess("bravo"), ds[1].Seq)
	if acked.Count != 0 || acked.Read != ds[1].Seq {
		t.Errorf("count %d read %d, want no unexpired row passed and read %d", acked.Count, acked.Read, ds[1].Seq)
	}
	if row := getIdentity(t, s, bravo.ID); row.LastPage != (seqRange{From: 1, To: ds[1].Seq}) {
		t.Errorf("last_page %+v, want from the old read plus one to %d", row.LastPage, ds[1].Seq)
	}
}
