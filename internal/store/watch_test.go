package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

const subject = "Start: one-engine refactor"

// watchedPost is a post from name to to with a read notification.
func watchedPost(name string, to Target, n NotifyRead) Posting {
	p := posting(sess(name), to)
	p.NotifyRead = n
	return p
}

// eventsTo lists the texts of identity id's deliveries of kind.
func eventsTo(t *testing.T, s *Store, id uint64, kind string) []string {
	t.Helper()
	var out []string
	for _, d := range deliveriesTo(t, s, id) {
		if d.Kind == kind {
			out = append(out, d.Text)
		}
	}
	return out
}

func wantEvents(t *testing.T, s *Store, id uint64, kind string, want ...string) {
	t.Helper()
	if got := eventsTo(t, s, id, kind); !slices.Equal(got, want) {
		t.Errorf("%s events %q, want %q", kind, got, want)
	}
}

func watchCount(t *testing.T, s *Store) int {
	t.Helper()
	return countRows(t, s, s.t.watches.table)
}

func TestNotifyRead_allFiresReceiptsCompleteOnceWhenTheLastRecipientReads(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	regAt(t, s, "charlie")
	regAt(t, s, "delta")
	msg := mustPost(t, s, watchedPost("alpha", Target{Path: repoSubflux}, NotifyRead{All: true})).Message

	readAll(t, s, "bravo")
	readAll(t, s, "charlie")
	t.Run("nothing while one is still to read", func(t *testing.T) {
		wantEvents(t, s, sender.ID, kindReceiptsComplete)
		if n := watchCount(t, s); n != 1 {
			t.Errorf("%d watches, want 1", n)
		}
	})
	readAll(t, s, "delta")
	want := fmt.Sprintf("message %d: all 3 have read %s", msg, subject)
	t.Run("one event when the last reads, and the watch is gone", func(t *testing.T) {
		wantEvents(t, s, sender.ID, kindReceiptsComplete, want)
		if n := watchCount(t, s); n != 0 {
			t.Errorf("%d watches after completion, want 0", n)
		}
	})
	t.Run("the event wakes the sender", func(t *testing.T) {
		page := mustInbox(t, s, sess("alpha"), 0)
		i := slices.IndexFunc(page.Entries, func(e Entry) bool { return e.Kind == kindReceiptsComplete })
		if page.Unread.Receipts != 1 || page.Unread.Wake() != 1 || i < 0 || !page.Entries[i].Wake || page.Entries[i].Text != want {
			t.Errorf("sender's inbox %+v, want one waking receipts event", page.Unread)
		}
	})
	t.Run("a later ack changes nothing", func(t *testing.T) {
		mustPost(t, s, posting(sess("charlie"), Target{Agent: "bravo"}))
		readAll(t, s, "bravo")
		wantEvents(t, s, sender.ID, kindReceiptsComplete, want)
	})
}

func TestNotifyRead_retiredRecipientsCountApartAndComplete(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	regAt(t, s, "charlie")
	regAt(t, s, "delta")
	msg := mustPost(t, s, watchedPost("alpha", Target{Path: repoSubflux}, NotifyRead{All: true})).Message
	readAll(t, s, "bravo")
	readAll(t, s, "delta")
	if err := s.Retire(t.Context(), plain("charlie")); err != nil {
		t.Fatal(err)
	}
	wantEvents(t, s, sender.ID, kindReceiptsComplete,
		fmt.Sprintf("message %d: 2 of 3 have read %s; 1 retired without reading", msg, subject))
}

func TestNotifyRead_aNamedList(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	charlie := regAt(t, s, "charlie")
	delta := regAt(t, s, "delta")
	msg := mustPost(t, s, watchedPost("alpha", Target{Path: repoSubflux}, NotifyRead{Names: []string{"charlie", "delta", "charlie"}})).Message

	t.Run("only the named recipients' rows carry the watch", func(t *testing.T) {
		for id, watched := range map[uint64]bool{bravo.ID: false, charlie.ID: true, delta.ID: true} {
			ds := deliveriesTo(t, s, id)
			if len(ds) != 1 || (ds[0].Watch != 0) != watched {
				t.Errorf("identity %d deliveries %+v, want one, watched %v", id, ds, watched)
			}
		}
	})
	readAll(t, s, "charlie")
	readAll(t, s, "bravo")
	wantEvents(t, s, sender.ID, kindReceiptsComplete)
	readAll(t, s, "delta")
	wantEvents(t, s, sender.ID, kindReceiptsComplete, fmt.Sprintf("message %d: all 2 have read %s", msg, subject))
}

func TestNotifyRead_aNameThatIsNotARecipientIsRefusedAndNothingPosted(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	regAt(t, s, "outsider", repoGrelot)
	before := getIdentity(t, s, bravo.ID).Seq
	messages := countRows(t, s, s.t.messages.table)

	p := watchedPost("alpha", Target{Path: repoSubflux}, NotifyRead{Names: []string{"bravo", "outsider", "nobody"}})
	_, err := s.Post(t.Context(), &p)
	wantRefusal(t, err, KindInvalid, "", "@outsider", "@nobody")
	if strings.Contains(err.Error(), "@bravo") {
		t.Errorf("refusal %q names bravo, a recipient", err)
	}
	if got := getIdentity(t, s, bravo.ID).Seq; got != before {
		t.Errorf("bravo's seq %d, want %d: nothing delivered", got, before)
	}
	if n := countRows(t, s, s.t.messages.table); n != messages {
		t.Errorf("%d messages, want %d: none stored", n, messages)
	}
	if n := watchCount(t, s); n != 0 {
		t.Errorf("%d watches, want 0", n)
	}
	if follows := getIdentity(t, s, sender.ID).Follows; len(follows) != 0 {
		t.Errorf("sender follows %q, want nothing: the post was refused", follows)
	}
}

func TestNotifyRead_requestChecks(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	for name, tc := range map[string]struct {
		n    NotifyRead
		want string
	}{
		"all and names":    {n: NotifyRead{All: true, Names: []string{"bravo"}}, want: "all or a list"},
		"by with no watch": {n: NotifyRead{By: time.Hour}, want: "--by needs --notify-read"},
		"a negative by":    {n: NotifyRead{All: true, By: -time.Minute}, want: "--by"},
		"a bad name":       {n: NotifyRead{Names: []string{"Bad Name"}}, want: `"Bad Name" is not an identity name`},
	} {
		t.Run(name, func(t *testing.T) {
			p := watchedPost("alpha", Target{Agent: "bravo"}, tc.n)
			_, err := s.Post(t.Context(), &p)
			wantRefusal(t, err, KindInvalid, "", tc.want)
		})
	}
	t.Run("all with no recipients puts no watch", func(t *testing.T) {
		got := mustPost(t, s, watchedPost("alpha", Target{Path: "/workspace/empty"}, NotifyRead{All: true}))
		if n := watchCount(t, s); n != 0 || !slices.ContainsFunc(got.Notices, func(l string) bool { return strings.Contains(l, "no recipients") }) {
			t.Errorf("%d watches, notices %q: want none and a line saying so", n, got.Notices)
		}
	})
}

func TestNotifyRead_byFiresReceiptsDueWithTheStragglers(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	regAt(t, s, "charlie")
	short := registration(sess("delta"), "delta", envA(), repoSubflux)
	short.TTL = 15 * time.Minute
	mustRegister(t, s, short)
	regAt(t, s, "echo")
	msg := mustPost(t, s, watchedPost("alpha", Target{Path: repoSubflux}, NotifyRead{All: true, By: time.Hour})).Message
	readAll(t, s, "bravo")
	if err := s.Retire(t.Context(), plain("echo")); err != nil {
		t.Fatal(err)
	}

	clk.advance(59 * time.Minute)
	mustSweep(t, s)
	wantEvents(t, s, sender.ID, kindReceiptsDue)
	clk.advance(time.Minute)
	mustSweep(t, s)
	want := fmt.Sprintf("message %d: 1 of 4 have read %s by the deadline; still unread: @charlie; dormant: @delta; 1 retired without reading", msg, subject)
	t.Run("at the deadline", func(t *testing.T) {
		wantEvents(t, s, sender.ID, kindReceiptsDue, want)
		if n := watchCount(t, s); n != 0 {
			t.Errorf("%d watches, want 0", n)
		}
	})
	t.Run("a read after it changes nothing", func(t *testing.T) {
		readAll(t, s, "charlie")
		wantEvents(t, s, sender.ID, kindReceiptsDue, want)
		wantEvents(t, s, sender.ID, kindReceiptsComplete)
	})
}

func TestNotifyRead_aReadPastTheDeadlineBeforeTheSweepIsDue(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	msg := mustPost(t, s, watchedPost("alpha", Target{Agent: "bravo"}, NotifyRead{All: true, By: time.Hour})).Message
	clk.advance(time.Hour)
	readAll(t, s, "bravo")
	wantEvents(t, s, sender.ID, kindReceiptsComplete)
	wantEvents(t, s, sender.ID, kindReceiptsDue,
		fmt.Sprintf("message %d: 0 of 1 have read %s by the deadline; still unread: @bravo", msg, subject))
}

func TestNotifyRead_aCompletedWatchFiresNoReceiptsDue(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	mustPost(t, s, watchedPost("alpha", Target{Agent: "bravo"}, NotifyRead{All: true, By: time.Hour}))
	readAll(t, s, "bravo")
	clk.advance(2 * time.Hour)
	mustSweep(t, s)
	if got := eventsTo(t, s, sender.ID, kindReceiptsComplete); len(got) != 1 {
		t.Errorf("receipts_complete %q, want one", got)
	}
	wantEvents(t, s, sender.ID, kindReceiptsDue)
}

func TestNotifyRead_byIsCappedAtTheMessagesExpiry(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	keepAlive(t, s, sender.ID, bravo.ID)
	mustPost(t, s, watchedPost("alpha", Target{Agent: "bravo"}, NotifyRead{All: true, By: 30 * 24 * time.Hour}))
	clk.advance(DefaultThreadTTL - time.Minute)
	mustSweep(t, s)
	wantEvents(t, s, sender.ID, kindReceiptsDue)
	clk.advance(time.Minute)
	mustSweep(t, s)
	if got := eventsTo(t, s, sender.ID, kindReceiptsDue); len(got) != 1 || !strings.Contains(got[0], "still unread: @bravo") {
		t.Errorf("receipts_due %q at the message's expiry, want one naming bravo", got)
	}
}

func TestNotifyRead_withoutByEndsSilentlyWithTheMessage(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	keepAlive(t, s, sender.ID, bravo.ID)
	mustPost(t, s, watchedPost("alpha", Target{Agent: "bravo"}, NotifyRead{All: true}))
	clk.advance(DefaultThreadTTL)
	mustSweep(t, s)
	if n := watchCount(t, s); n != 0 {
		t.Errorf("%d watches after the message expired, want 0", n)
	}
	wantEvents(t, s, sender.ID, kindReceiptsDue)
	wantEvents(t, s, sender.ID, kindReceiptsComplete)
}

func TestNotifyRead_endsWithItsSendersRetirementOrPurge(t *testing.T) {
	t.Run("retirement", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		regAt(t, s, "alpha")
		regAt(t, s, "bravo")
		mustPost(t, s, watchedPost("alpha", Target{Agent: "bravo"}, NotifyRead{All: true, By: time.Hour}))
		if err := s.Retire(t.Context(), plain("alpha")); err != nil {
			t.Fatal(err)
		}
		if n := watchCount(t, s); n != 0 {
			t.Errorf("%d watches after the sender retired, want 0", n)
		}
		readAll(t, s, "bravo")
	})
	t.Run("purge", func(t *testing.T) {
		s, clk := openDormancyStore(t)
		short := registration(sess("alpha"), "alpha", envA(), repoSubflux)
		short.TTL = 15 * time.Minute
		mustRegister(t, s, short)
		bravo := regAt(t, s, "bravo")
		keepAlive(t, s, bravo.ID)
		mustPost(t, s, watchedPost("alpha", Target{Agent: "bravo"}, NotifyRead{All: true}))
		clk.advance(15 * time.Minute)
		mustSweep(t, s)
		if n := watchCount(t, s); n != 1 {
			t.Errorf("%d watches while the sender is dormant, want 1", n)
		}
		clk.advance(time.Hour)
		mustSweep(t, s)
		if n := watchCount(t, s); n != 0 {
			t.Errorf("%d watches after the sender's purge, want 0", n)
		}
	})
}

func TestNotifyRead_aPurgedRecipientCountsRetiredAndADormantOneStaysPending(t *testing.T) {
	s, clk := openDormancyStore(t)
	sender := regAt(t, s, "alpha")
	short := registration(sess("bravo"), "bravo", envA(), repoSubflux)
	short.TTL = 15 * time.Minute
	mustRegister(t, s, short)
	msg := mustPost(t, s, watchedPost("alpha", Target{Agent: "bravo"}, NotifyRead{All: true})).Message
	clk.advance(15 * time.Minute)
	mustSweep(t, s)
	wantEvents(t, s, sender.ID, kindReceiptsComplete)
	clk.advance(time.Hour)
	mustSweep(t, s)
	wantEvents(t, s, sender.ID, kindReceiptsComplete,
		fmt.Sprintf("message %d: 0 of 1 have read %s; 1 retired without reading", msg, subject))
}

func TestNotifyRead_aDormantSenderGetsTheEventAndReadsItAfterRevival(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	short := registration(sess("alpha"), "alpha", envA(), repoSubflux)
	short.TTL = 15 * time.Minute
	sender := mustRegister(t, s, short)
	bravo := regAt(t, s, "bravo")
	msg := mustPost(t, s, watchedPost("alpha", Target{Agent: "bravo"}, NotifyRead{All: true})).Message
	clk.advance(15 * time.Minute)
	mustSweep(t, s)
	keepAlive(t, s, bravo.ID)
	readAll(t, s, "bravo")
	want := fmt.Sprintf("message %d: all 1 have read %s", msg, subject)
	wantEvents(t, s, sender.ID, kindReceiptsComplete, want)
	if st := getIdentity(t, s, sender.ID).State; st != stateDormant {
		t.Fatalf("sender is %s, want dormant", st)
	}

	mustRevive(t, s, revival(sess("alpha"), "", envA()))
	page := mustInbox(t, s, sess("alpha"), 0)
	if !slices.ContainsFunc(page.Entries, func(e Entry) bool { return e.Kind == kindReceiptsComplete && e.Text == want }) {
		t.Errorf("inbox after revival %+v, want the receipts_complete event", page.Entries)
	}
}

func TestNotifyRead_anAnnouncementWhileDormantIsInTheInboxAfterRevival(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha")
	keepAlive(t, s, sender.ID)
	sleeper := expireToDormant(t, s, clk, "bravo")
	msg := mustPost(t, s, watchedPost("alpha", Target{Announce: true}, NotifyRead{All: true})).Message
	wantStates(t, mustReceipts(t, s, plain("alpha"), msg), "bravo dormant")

	mustRevive(t, s, revival(sessA, "", envA()))
	page := mustInbox(t, s, sessA, 0)
	if !slices.ContainsFunc(page.Entries, func(e Entry) bool { return e.Message == msg }) {
		t.Errorf("inbox of %d after revival %+v, want the announcement", sleeper.ID, page.Entries)
	}
	mustAck(t, s, sessA, page.Entries[len(page.Entries)-1].Seq)
	wantEvents(t, s, sender.ID, kindReceiptsComplete, fmt.Sprintf("message %d: all 1 have read %s", msg, subject))
}

func TestNotifyRead_aDelegateRecipientsEndCountsRetired(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha", repoGrelot)
	regAt(t, s, "subflux-dev", repoGrelot)
	scopedDelegate(t, s, "subflux-dev", "review", func(g *Grant) { g.AllowThreads = []string{pathInternal} })
	msg := mustPost(t, s, watchedPost("alpha", Target{Path: pathInternal}, NotifyRead{All: true})).Message
	if _, err := s.Revoke(t.Context(), self("subflux-dev"), "review"); err != nil {
		t.Fatal(err)
	}
	wantEvents(t, s, sender.ID, kindReceiptsComplete,
		fmt.Sprintf("message %d: 0 of 1 have read %s; 1 retired without reading", msg, subject))
}

func TestNotifyRead_aParentKeepsContiguousSeqsWhenItsDelegatesEndCompletesItsWatch(t *testing.T) {
	for name, end := range map[string]func(t *testing.T, s *Store){
		"session end": func(t *testing.T, s *Store) {
			if err := s.EndSession(t.Context(), sess("subflux-dev")); err != nil {
				t.Fatal(err)
			}
		},
		"revoke": func(t *testing.T, s *Store) {
			if _, err := s.Revoke(t.Context(), self("subflux-dev"), "review"); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := openIdentityStore(t)
			p, d := delegateOf(t, s, "subflux-dev", "review")
			post := watchedPost("subflux-dev", Target{Agent: d.Name}, NotifyRead{All: true})
			post.Caller.Self = true
			mustPost(t, s, post)
			end(t, s)
			row := getIdentity(t, s, p.ID)
			ds := allDeliveriesTo(t, s, p.ID)
			if len(ds) == 0 || ds[len(ds)-1].Seq != row.Seq || ds[len(ds)-1].Kind != kindReceiptsComplete {
				t.Errorf("parent seq %d, deliveries %+v: want the receipts event under the row's newest seq", row.Seq, ds)
			}
			for i, dl := range ds {
				if dl.Seq != uint64(i+1) {
					t.Errorf("delivery %d has seq %d, want contiguous seqs", i, dl.Seq)
				}
			}
		})
	}
}

func TestNotifyRead_aCopyIsNotWatched(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha", repoGrelot)
	parent := regAt(t, s, "subflux-dev", repoGrelot)
	d := scopedDelegate(t, s, "subflux-dev", "review", func(g *Grant) { g.AllowThreads = []string{pathInternal} })
	msg := mustPost(t, s, watchedPost("alpha", Target{Path: pathInternal}, NotifyRead{All: true})).Message
	for _, dl := range deliveriesTo(t, s, parent.ID) {
		if dl.Watch != 0 {
			t.Errorf("parent's copy %+v carries the watch", dl)
		}
	}
	if _, err := s.Ack(t.Context(), self("subflux-dev"), getIdentity(t, s, parent.ID).Seq); err != nil {
		t.Fatal(err)
	}
	wantEvents(t, s, sender.ID, kindReceiptsComplete)
	if _, err := s.Ack(t.Context(), as(sess("subflux-dev"), d.Name), getIdentity(t, s, d.ID).Seq); err != nil {
		t.Fatal(err)
	}
	wantEvents(t, s, sender.ID, kindReceiptsComplete, fmt.Sprintf("message %d: all 1 have read %s", msg, subject))
}

func TestNotifyRead_receiptsDueCountsADelegateEndedBeforeTheSweepAsRetired(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha", repoGrelot)
	bravo := regAt(t, s, "bravo", repoSubflux)
	regAt(t, s, "subflux-dev", repoGrelot)
	scopedDelegate(t, s, "subflux-dev", "review", func(g *Grant) { g.AllowThreads = []string{pathInternal} })
	msg := mustPost(t, s, watchedPost("alpha", Target{Path: pathInternal}, NotifyRead{All: true, By: 3 * time.Hour})).Message
	keepAlive(t, s, sender.ID, bravo.ID)
	clk.advance(3 * time.Hour)
	readAll(t, s, "bravo")
	wantEvents(t, s, sender.ID, kindReceiptsDue,
		fmt.Sprintf("message %d: 0 of 2 have read %s by the deadline; still unread: @bravo; 1 retired without reading", msg, subject))
}
