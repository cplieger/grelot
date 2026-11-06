//go:build !windows

package store

import (
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// openDormancyStore opens a store whose dormant identities purge an hour
// after their expiry, well inside a post's retention.
func openDormancyStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	clk := &clock{t: testNow}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), Options{Logger: slog.New(newLogSink()), Now: clk.now, DormantTTL: time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

func mustReceipts(t *testing.T, s *Store, c Caller, msg uint64) Receipts {
	t.Helper()
	got, err := s.Receipts(t.Context(), c, msg)
	if err != nil {
		t.Fatalf("Receipts(%+v, %d): %v", c, msg, err)
	}
	return got
}

// receiptStates is each listed recipient as "name state".
func receiptStates(rs Receipts) []string {
	var out []string
	for _, r := range rs.Recipients {
		out = append(out, r.Name+" "+r.State)
	}
	return out
}

func wantStates(t *testing.T, rs Receipts, want ...string) {
	t.Helper()
	if got := receiptStates(rs); !slices.Equal(got, want) {
		t.Errorf("receipts %q, want %q", got, want)
	}
}

// readAll reads and acks every unread delivery of name.
func readAll(t *testing.T, s *Store, name string) {
	t.Helper()
	page := mustInbox(t, s, sess(name), MaxInboxLimit)
	if len(page.Entries) > 0 {
		mustAck(t, s, sess(name), page.Entries[len(page.Entries)-1].Seq)
	}
}

func TestReceipts_listsTheRecipientsAsThePostFannedOut(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "subflux-dev", repoSubflux)
	regAt(t, s, "cloudy-lake", repoSubflux)
	regAt(t, s, "quiet-glacier", repoSubflux)
	msg := mustPost(t, s, posting(sess("subflux-dev"), Target{Path: repoSubflux})).Message

	got := mustReceipts(t, s, plain("subflux-dev"), msg)
	wantStates(t, got, "cloudy-lake unread", "quiet-glacier unread")
	if got.Message != msg || got.Subject != "Start: one-engine refactor" || got.Kind != kindPath || got.RegisteredSince != 0 {
		t.Errorf("receipts header %+v, want message %d, its subject and kind", got, msg)
	}
	for _, r := range got.Recipients {
		if r.Host != "host-a/app" || r.Relation != "same environment" || !r.ReadAt.IsZero() {
			t.Errorf("recipient %+v, want host host-a/app, same environment, no read time", r)
		}
	}

	t.Run("read once the ack passes the delivery, at the ack's time", func(t *testing.T) {
		clk.advance(time.Minute)
		ackedAt := clk.now()
		readAll(t, s, "cloudy-lake")
		got := mustReceipts(t, s, plain("subflux-dev"), msg)
		wantStates(t, got, "cloudy-lake read", "quiet-glacier unread")
		if !got.Recipients[0].ReadAt.Equal(ackedAt) {
			t.Errorf("read_at %v, want the ack's time %v", got.Recipients[0].ReadAt, ackedAt)
		}
	})
	t.Run("a later ack leaves read_at alone", func(t *testing.T) {
		first := mustReceipts(t, s, plain("subflux-dev"), msg).Recipients[0].ReadAt
		clk.advance(time.Hour)
		mustPost(t, s, posting(sess("subflux-dev"), Target{Agent: "cloudy-lake"}))
		readAll(t, s, "cloudy-lake")
		if got := mustReceipts(t, s, plain("subflux-dev"), msg).Recipients[0].ReadAt; !got.Equal(first) {
			t.Errorf("read_at %v after a later ack, want %v", got, first)
		}
	})
	t.Run("a recipient sees them too", func(t *testing.T) {
		wantStates(t, mustReceipts(t, s, plain("quiet-glacier"), msg), "cloudy-lake read", "quiet-glacier unread")
	})
}

func TestReceipts_onlyForTheSenderAndTheRecipients(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "subflux-dev", repoSubflux)
	regAt(t, s, "cloudy-lake", repoSubflux)
	regAt(t, s, "outsider", repoGrelot)
	msg := mustPost(t, s, posting(sess("subflux-dev"), Target{Agent: "cloudy-lake"})).Message

	t.Run("another agent", func(t *testing.T) {
		_, err := s.Receipts(t.Context(), plain("outsider"), msg)
		wantRefusal(t, err, KindForbidden, "", "only the sender and the recipients")
	})
	t.Run("an unknown message", func(t *testing.T) {
		_, err := s.Receipts(t.Context(), plain("subflux-dev"), msg+1)
		wantRefusal(t, err, KindNotFound, "", "no message")
	})
	t.Run("a session with no identity", func(t *testing.T) {
		_, err := s.Receipts(t.Context(), plain("nobody"), msg)
		wantRefusal(t, err, KindGone, "")
	})
}

func TestReceipts_dormantAndRetiredRecipientsKeptApart(t *testing.T) {
	s, clk := openDormancyStore(t)
	regAt(t, s, "subflux-dev", repoSubflux)
	short := registration(sess("cloudy-lake"), "cloudy-lake", envA(), repoSubflux)
	short.TTL = 15 * time.Minute
	mustRegister(t, s, short)
	regAt(t, s, "quiet-glacier", repoSubflux)
	regAt(t, s, "rainy-condor", repoSubflux)
	msg := mustPost(t, s, posting(sess("subflux-dev"), Target{Path: repoSubflux})).Message
	readAll(t, s, "rainy-condor")
	if err := s.Retire(t.Context(), plain("quiet-glacier")); err != nil {
		t.Fatal(err)
	}
	if err := s.Retire(t.Context(), plain("rainy-condor")); err != nil {
		t.Fatal(err)
	}

	clk.advance(15 * time.Minute)
	t.Run("dormant before and after the sweep", func(t *testing.T) {
		wantStates(t, mustReceipts(t, s, plain("subflux-dev"), msg),
			"cloudy-lake dormant", "quiet-glacier retired", "rainy-condor read")
		mustSweep(t, s)
		wantStates(t, mustReceipts(t, s, plain("subflux-dev"), msg),
			"cloudy-lake dormant", "quiet-glacier retired", "rainy-condor read")
	})
	t.Run("retired with no host and relation unknown", func(t *testing.T) {
		r := mustReceipts(t, s, plain("subflux-dev"), msg).Recipients[1]
		if r.Host != "" || r.Relation != "unknown" {
			t.Errorf("retired recipient %+v, want no host and relation unknown", r)
		}
	})
	t.Run("a purged recipient reads retired", func(t *testing.T) {
		clk.advance(time.Hour)
		mustSweep(t, s)
		r := mustReceipts(t, s, plain("subflux-dev"), msg).Recipients[0]
		if r.Name != "cloudy-lake" || r.State != ReceiptRetired || r.Relation != "unknown" {
			t.Errorf("purged recipient %+v, want cloudy-lake retired, relation unknown", r)
		}
	})
}

func TestReceipts_aDelegateRecipientListedOnlyToItsParentAndTheSender(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "subflux-dev", repoGrelot)
	regAt(t, s, "cloudy-lake", repoSubflux)
	regAt(t, s, "quiet-glacier", repoSubflux)
	d := scopedDelegate(t, s, "subflux-dev", "review", func(g *Grant) { g.AllowThreads = []string{pathInternal} })
	msg := mustPost(t, s, posting(sess("cloudy-lake"), Target{Path: pathInternal + "/x.go"})).Message

	t.Run("to the sender", func(t *testing.T) {
		wantStates(t, mustReceipts(t, s, plain("cloudy-lake"), msg), "quiet-glacier unread", d.Name+" unread")
	})
	t.Run("to the parent, whose copy is no receipt", func(t *testing.T) {
		wantStates(t, mustReceipts(t, s, self("subflux-dev"), msg), "quiet-glacier unread", d.Name+" unread")
	})
	t.Run("not to another recipient", func(t *testing.T) {
		wantStates(t, mustReceipts(t, s, plain("quiet-glacier"), msg), "quiet-glacier unread")
	})
	t.Run("not to the delegate itself", func(t *testing.T) {
		wantStates(t, mustReceipts(t, s, as(sess("subflux-dev"), d.Name), msg), "quiet-glacier unread")
	})
	t.Run("its relation follows its parent's where", func(t *testing.T) {
		r := mustReceipts(t, s, plain("cloudy-lake"), msg).Recipients[1]
		if r.Relation != "same environment" || r.Host != "host-a/app" {
			t.Errorf("delegate recipient %+v, want its parent's relation and host", r)
		}
	})
	t.Run("retired once it ends", func(t *testing.T) {
		if err := s.Retire(t.Context(), as(sess("subflux-dev"), d.Name)); err != nil {
			t.Fatal(err)
		}
		wantStates(t, mustReceipts(t, s, plain("cloudy-lake"), msg), "quiet-glacier unread", d.Name+" retired")
		wantStates(t, mustReceipts(t, s, plain("quiet-glacier"), msg), "quiet-glacier unread")
	})
}

func TestReceipts_anAnnouncementCountsTheIdentitiesRegisteredAfterIt(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "subflux-dev", repoSubflux)
	regAt(t, s, "cloudy-lake", repoSubflux)
	expireToDormant(t, s, clk, "quiet-glacier")
	keepAlive(t, s, 1, 2)
	msg := mustPost(t, s, posting(sess("subflux-dev"), Target{Announce: true})).Message
	clk.advance(time.Minute)
	regAt(t, s, "late-one", repoGrelot)
	regAt(t, s, "late-two", repoGrelot)

	got := mustReceipts(t, s, plain("subflux-dev"), msg)
	wantStates(t, got, "cloudy-lake unread", "quiet-glacier dormant")
	if got.RegisteredSince != 2 {
		t.Errorf("registered since %d, want 2", got.RegisteredSince)
	}
}

func TestReceipts_registeredSinceCountsOnlyLiveAndDormantIdentitiesNotDelegates(t *testing.T) {
	s, clk := openDormancyStore(t)
	regAt(t, s, "subflux-dev", repoSubflux)
	msg := mustPost(t, s, posting(sess("subflux-dev"), Target{Announce: true})).Message
	clk.advance(time.Minute)
	regAt(t, s, "late-one", repoGrelot)
	mustRegisterDelegate(t, s, sess("subflux-dev"), mustGrant(t, s, grantFrom("subflux-dev", "review")).Link)
	short := registration(sess("late-short"), "late-short", envA(), repoGrelot)
	short.TTL = 15 * time.Minute
	mustRegister(t, s, short)
	if got := mustReceipts(t, s, self("subflux-dev"), msg).RegisteredSince; got != 2 {
		t.Errorf("registered since %d, want 2: late-one and late-short, not the delegate", got)
	}
	clk.advance(15*time.Minute + time.Hour)
	if got := mustReceipts(t, s, self("subflux-dev"), msg).RegisteredSince; got != 1 {
		t.Errorf("registered since %d once late-short's purge is due, before the sweep, want 1", got)
	}
}

func TestReceipts_anExpiredMessageIsNotFoundBeforeTheSweep(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	a := regAt(t, s, "subflux-dev", repoSubflux)
	b := regAt(t, s, "cloudy-lake", repoSubflux)
	keepAlive(t, s, a.ID, b.ID)
	msg := mustPost(t, s, posting(sess("subflux-dev"), Target{Agent: "cloudy-lake"})).Message
	clk.advance(DefaultThreadTTL)
	_, err := s.Receipts(t.Context(), plain("subflux-dev"), msg)
	wantRefusal(t, err, KindNotFound, "", "no message")
}

func TestReceipts_aDelegateWhoseParentExpiredReadsRetiredBeforeTheSweep(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after time.Duration
	}{
		{name: "parent dormant", after: 0},
		{name: "parent's purge due", after: time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, clk := openDormancyStore(t)
			parent := regAt(t, s, "subflux-dev", repoGrelot)
			sender := regAt(t, s, "cloudy-lake", repoSubflux)
			keepAlive(t, s, sender.ID)
			d := scopedDelegate(t, s, "subflux-dev", "review", func(g *Grant) { g.AllowThreads = []string{pathInternal} })
			end := getIdentity(t, s, parent.ID).Lease.Expires
			editIdentity(t, s, d.ID, func(r *identityRow) { r.Lease = lease{Expires: end.Add(24 * time.Hour)} })
			msg := mustPost(t, s, posting(sess("cloudy-lake"), Target{Path: pathInternal})).Message
			clk.set(end.Add(tc.after))
			wantStates(t, mustReceipts(t, s, plain("cloudy-lake"), msg), d.Name+" retired")
		})
	}
}
