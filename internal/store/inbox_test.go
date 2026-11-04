package store

import (
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
	p, err := s.Inbox(t.Context(), session, limit)
	if err != nil {
		t.Fatalf("Inbox(%s): %v", session, err)
	}
	return p
}

func mustAck(t *testing.T, s *Store, session string, upto uint64) Acked {
	t.Helper()
	a, err := s.Ack(t.Context(), session, upto)
	if err != nil {
		t.Fatalf("Ack(%s, %d): %v", session, upto, err)
	}
	return a
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
	if e.Kind != kindDirect || e.Body != "body" || e.From != "alpha" || e.Host != "borgcube/marotte" ||
		e.OS != "linux" || e.Arch != "amd64" || e.Relation != "same environment" || !e.Wake || e.Message == 0 {
		t.Errorf("entry %+v, want a wake direct message from alpha, same environment", e)
	}
	if page.Unread.Direct != 2 || page.Unread.Wake() != 2 || page.Unread.Total() != 2 {
		t.Errorf("unread %+v, want 2 direct, both wake", page.Unread)
	}

	clk.advance(time.Minute)
	acked := mustAck(t, s, sess("bravo"), page.Entries[1].Seq)
	if acked.Read != page.Entries[1].Seq || acked.Left.Total() != 0 {
		t.Errorf("ack %+v, want read %d and nothing left", acked, page.Entries[1].Seq)
	}
	if again := mustInbox(t, s, sess("bravo"), 0); len(again.Entries) != 0 || again.Unread.Total() != 0 {
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

	if a := mustAck(t, s, sess("bravo"), seqs[1]); a.Read != seqs[1] || a.Left.Total() != 1 {
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
	if got := subjects(page.Entries); len(got) != 2 || got[1] != "2" || page.Unread.Total() != 3 {
		t.Fatalf("page %q with %d unread, want [1 2] of 3", got, page.Unread.Total())
	}
	acked := mustAck(t, s, sess("bravo"), page.Entries[1].Seq)
	if acked.Left.Total() != 1 || acked.Left.Direct != 1 {
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
	deliverEvents(t, s, me.ID, DefaultInboxLimit+5)
	if p := mustInbox(t, s, sess("bravo"), 0); len(p.Entries) != DefaultInboxLimit || p.Unread.Total() != DefaultInboxLimit+5 {
		t.Errorf("default page %d of %d, want %d of %d", len(p.Entries), p.Unread.Total(), DefaultInboxLimit, DefaultInboxLimit+5)
	}
	if p := mustInbox(t, s, sess("bravo"), MaxInboxLimit); len(p.Entries) != DefaultInboxLimit+5 {
		t.Errorf("page of %d at the largest limit, want every one", len(p.Entries))
	}
	for _, bad := range []int{-1, MaxInboxLimit + 1} {
		_, err := s.Inbox(t.Context(), sess("bravo"), bad)
		wantRefusal(t, err, KindInvalid, "", "limit", "1 to 100")
	}
}

func TestInbox_anEventCarriesItsTextAndNoSender(t *testing.T) {
	s, _ := openClockStore(t)
	me := regAt(t, s, "bravo", "/workspace/bravo")
	putRows(t, s, func(w *wtx) error { return w.deliverEvent(me.ID, kindIdentityExpiring, "your identity bravo expires") })
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
	if len(p.Entries) != 1 || p.Entries[0].Seq != 2 || p.Unread.Total() != 1 {
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
	if err := s.Retire(t.Context(), sess("alpha")); err != nil {
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
	if p.Unread.Cards != 1 || p.Unread.Wake() != 0 {
		t.Errorf("unread %+v, want one card and no wake event", p.Unread)
	}

	if err := s.Retire(t.Context(), sess("charlie")); err != nil {
		t.Fatal(err)
	}
	p = mustInbox(t, s, sess("bravo"), 0)
	if len(p.Entries) != 2 || p.Entries[1].Kind != kindLeft || p.Entries[1].Wake || p.Unread.Cards != 2 || p.Unread.Wake() != 0 {
		t.Errorf("page %+v, want the join and left cards, both quiet", p)
	}
}

func TestHold_advancesShownOnlyOverAWakeEvent(t *testing.T) {
	s, _ := openClockStore(t)
	mustRegister(t, s, registration(sess("bravo"), "bravo", envA(), repoSubflux))
	bravo := getIdentity(t, s, 1)
	mustRegister(t, s, registration(sess("charlie"), "charlie", envA(), repoSubflux))

	h, err := s.Hold(t.Context(), sess("bravo"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Counts.Cards != 1 || h.Counts.Wake() != 0 || h.Shown != 0 {
		t.Errorf("hold over a card alone %+v, want the card counted and shown left at 0", h)
	}
	direct(t, s, "charlie", "bravo", 1)
	h, err = s.Hold(t.Context(), sess("bravo"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Counts.Direct != 1 || h.Counts.Cards != 1 || h.Shown != 2 {
		t.Errorf("hold %+v, want 1 direct and 1 card, shown 2", h)
	}
	h, err = s.Hold(t.Context(), sess("bravo"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Counts.Total() != 0 || h.Shown != 2 {
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

func TestInbox_aDormantSessionGetsTheReviveOffer(t *testing.T) {
	s, clk := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/alpha")
	bravo := regAt(t, s, "bravo", "/workspace/bravo")
	direct(t, s, "alpha", "bravo", 2)
	clk.set(bravo.Expires.Add(3*time.Hour + 59*time.Minute))
	want := "your identity bravo expired 3 h ago with 2 unread; `grelot revive --ttl <d>` restores it"

	_, err := s.Inbox(t.Context(), sess("bravo"), 0)
	wantRefusal(t, err, KindGone, "dormant", want)
	_, err = s.Ack(t.Context(), sess("bravo"), 1)
	wantRefusal(t, err, KindGone, "dormant", want)
	_, err = s.Hold(t.Context(), sess("bravo"))
	wantRefusal(t, err, KindGone, "dormant", want)
	_, err = s.Inbox(t.Context(), sess("nobody"), 0)
	wantRefusal(t, err, KindGone, "none")
}

func TestInbox_clearsEnded(t *testing.T) {
	for _, call := range []string{"inbox", "ack", "hold"} {
		s, _ := openClockStore(t)
		me := regAt(t, s, "bravo", "/workspace/bravo")
		if err := s.EndSession(t.Context(), sess("bravo")); err != nil {
			t.Fatal(err)
		}
		var err error
		switch call {
		case "inbox":
			_, err = s.Inbox(t.Context(), sess("bravo"), 0)
		case "ack":
			_, err = s.Ack(t.Context(), sess("bravo"), 0)
		case "hold":
			_, err = s.Hold(t.Context(), sess("bravo"))
		}
		if err != nil {
			t.Fatal(err)
		}
		if getIdentity(t, s, me.ID).Ended {
			t.Errorf("%s left ended set, want it cleared", call)
		}
	}
}
