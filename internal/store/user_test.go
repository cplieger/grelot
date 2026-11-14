//go:build !windows

package store

import (
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

// userRow reads @user's identity row by name.
func userRow(t *testing.T, s *Store) identityRow {
	t.Helper()
	var id uint64
	var found bool
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		var err error
		id, _, found, err = s.t.byName.Lookup(r, []byte(UserName))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("no @user identity")
	}
	return getIdentity(t, s, id)
}

func userPosting(to Target) Posting {
	return Posting{Caller: Caller{User: true}, To: to, Subject: "from the user", Body: "body"}
}

func TestUser_isCreatedOnceAtOpenAndNeverExpires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	var first uint64
	for i := range 2 {
		s, err := Open(path, &Options{Now: func() time.Time { return testNow }})
		if err != nil {
			t.Fatal(err)
		}
		u := userRow(t, s)
		if u.State != stateLive || !u.Lease.Expires.IsZero() || !u.Lease.WarnAt.IsZero() || u.Session != "" || u.Provider != "web" {
			t.Errorf("@user row %+v, want live, no lease, no session, provider web", u)
		}
		if i == 0 {
			first = u.ID
		} else if u.ID != first {
			t.Errorf("@user's id %d after a reopen, want %d", u.ID, first)
		}
		if n := countRows(t, s, s.t.identities.table); n != 1 {
			t.Errorf("%d identity rows, want @user's alone", n)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUser_aResetStoreHasItAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	writeOtherFormat(t, path)
	s, err := openAt(t, path, newLogSink())
	if err != nil {
		t.Fatal(err)
	}
	if u := userRow(t, s); u.State != stateLive {
		t.Errorf("@user is %s after the reset, want live", u.State)
	}
}

func TestUser_staysLivePastEverySweep(t *testing.T) {
	s, clk := openClockStore(t)
	clk.advance(400 * 24 * time.Hour)
	mustSweep(t, s)
	if u := userRow(t, s); u.State != stateLive {
		t.Errorf("@user is %s a year on, want live", u.State)
	}
}

func TestUser_noAgentMayTakeTheName(t *testing.T) {
	const reserved = "reserved for the user"
	t.Run("register", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		_, _, err := s.Register(t.Context(), Keyed{}, new(registration(sessA, UserName, envA())))
		wantRefusal(t, err, KindInvalid, "", reserved)
	})
	t.Run("take-over", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		r := registration(sessA, UserName, envA())
		r.TakeOver = true
		_, _, err := s.Register(t.Context(), Keyed{}, &r)
		wantRefusal(t, err, KindInvalid, "", reserved)
	})
	t.Run("set", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		regAt(t, s, "alpha")
		_, err := s.Set(t.Context(), Keyed{}, Caller{Session: sess("alpha")}, Change{Fields: Fields{Name: UserName}})
		wantRefusal(t, err, KindInvalid, "", reserved)
	})
	t.Run("post name", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		regAt(t, s, "alpha")
		p := posting(sess("alpha"), Target{Announce: true})
		p.Fields.Name = UserName
		_, _, err := s.Post(t.Context(), Keyed{}, &p)
		wantRefusal(t, err, KindInvalid, "", reserved)
	})
	t.Run("revive by name", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		_, _, err := s.Revive(t.Context(), Keyed{}, new(revival(sessA, UserName, envA())))
		wantRefusal(t, err, KindInvalid, "", reserved)
	})
	t.Run("the row is left as it was", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		before := userRow(t, s)
		_, _, _ = s.Register(t.Context(), Keyed{}, new(registration(sessA, UserName, envA())))
		if after := userRow(t, s); after.Session != before.Session || after.ID != before.ID {
			t.Errorf("@user row %+v after a refused register, want %+v", after, before)
		}
	})
}

func TestUser_noAgentRequestActsAsUser(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	t.Run("a session of that name has no identity", func(t *testing.T) {
		_, _, err := s.Post(t.Context(), Keyed{}, new(posting("web:user", Target{Announce: true})))
		wantRefusal(t, err, KindGone, "none")
	})
	t.Run("the name is no delegate handle", func(t *testing.T) {
		p := posting(sessA, Target{Announce: true})
		p.Caller.As = UserName
		_, _, err := s.Post(t.Context(), Keyed{}, &p)
		wantRefusal(t, err, KindInvalid, "", "not a delegate handle")
	})
	t.Run("a User caller naming a session is refused", func(t *testing.T) {
		p := userPosting(Target{Announce: true})
		p.Caller.Session = sessA
		_, _, err := s.Post(t.Context(), Keyed{}, &p)
		wantRefusal(t, err, KindInvalid, "", "@user")
	})
}

func TestUser_receivesAnAgentsDirectPost(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	got := mustPost(t, s, posting(sess("alpha"), Target{Agent: UserName}))
	if got.Delivered != 1 {
		t.Errorf("delivered %d, want 1", got.Delivered)
	}
	u := userRow(t, s)
	ds := deliveriesTo(t, s, u.ID)
	if len(ds) != 1 || ds[0].Kind != kindDirect || ds[0].Message != got.Message {
		t.Errorf("@user's deliveries %+v, want the direct post %d", ds, got.Message)
	}
	if u.Unread.Counts.Direct != 1 {
		t.Errorf("@user's unread %+v, want 1 direct", u.Unread.Counts)
	}
}

func TestUser_postsReachTheirRecipientsLikeAnAgentsPost(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	alpha := regAt(t, s, "alpha", repoSubflux)
	bravo := regAt(t, s, "bravo", repoGrelot)
	t.Run("announce", func(t *testing.T) {
		got := mustPost(t, s, userPosting(Target{Announce: true}))
		if got.Delivered != 2 {
			t.Errorf("delivered %d, want alpha and bravo", got.Delivered)
		}
	})
	t.Run("path", func(t *testing.T) {
		got := mustPost(t, s, userPosting(Target{Path: repoSubflux + "/README.md"}))
		if got.Delivered != 1 {
			t.Errorf("delivered %d, want alpha, whose work path is above it", got.Delivered)
		}
	})
	t.Run("direct", func(t *testing.T) {
		got := mustPost(t, s, userPosting(Target{Agent: "bravo"}))
		if got.Delivered != 1 {
			t.Errorf("delivered %d, want 1", got.Delivered)
		}
		p := mustInbox(t, s, sess("bravo"), 0)
		last := p.Entries[len(p.Entries)-1]
		if last.Kind != kindDirect || last.From != UserName || last.Message != got.Message {
			t.Errorf("bravo's last entry %+v, want the direct post from user", last)
		}
	})
	for _, id := range []uint64{alpha.ID, bravo.ID} {
		if ds := deliveriesTo(t, s, id); len(ds) != 2 {
			t.Errorf("identity %d has %d deliveries, want the announcement and its one other post", id, len(ds))
		}
	}
	if u := userRow(t, s); u.Name != UserName || u.Description != "the user, through grelot's web UI" {
		t.Errorf("@user's fields %q %q after posts that set none", u.Name, u.Description)
	}
}

// openRetainingStore opens a store on a test clock whose posts and
// threads live for retention.
func openRetainingStore(t *testing.T, retention time.Duration) (*Store, *clock) {
	t.Helper()
	clk := &clock{t: testNow}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{
		Logger: slog.New(newLogSink()), Now: clk.now, Retention: retainFor(retention),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

// reached reports whether message m was delivered to identity id.
func reached(t *testing.T, s *Store, id, m uint64) bool {
	t.Helper()
	return slices.ContainsFunc(deliveriesTo(t, s, id), func(d deliveryRow) bool { return d.Message == m })
}

func TestUser_aPathPostsFollowEndsWithItsThread(t *testing.T) {
	const retention = 30 * time.Minute
	s, clk := openRetainingStore(t, retention)
	regAt(t, s, "alpha", repoSubflux)
	bravo := regAt(t, s, "bravo", repoGrelot)

	sent := mustPost(t, s, userPosting(Target{Path: repoSubflux}))
	if f := userRow(t, s).Follows; !slices.Equal(f, []string{repoSubflux}) {
		t.Fatalf("@user follows %q after its path post, want %s", f, repoSubflux)
	}
	mustFollow(t, s, sess("bravo"), repoSubflux)
	reply := say(t, s, "alpha", Target{Path: repoSubflux}, "reply")
	if reply.Delivered != 2 {
		t.Errorf("the reply delivered %d, want @user and bravo", reply.Delivered)
	}
	if n := rowFor(t, mustConversations(t, s, ConversationsQuery{}), sent.Thread).Unread; n != 1 {
		t.Errorf("@user's unread in the thread %d, want the reply", n)
	}

	clk.advance(retention)
	mustSweep(t, s)
	u := userRow(t, s)
	if len(u.Follows) != 0 {
		t.Errorf("@user follows %q once the thread expired, want none", u.Follows)
	}
	later := say(t, s, "alpha", Target{Path: repoSubflux}, "later")
	if later.Delivered != 1 {
		t.Errorf("a later post delivered %d, want bravo alone", later.Delivered)
	}
	if reached(t, s, u.ID, later.Message) {
		t.Errorf("the later post %d reached @user", later.Message)
	}
	if f := getIdentity(t, s, bravo.ID).Follows; !slices.Equal(f, []string{repoSubflux}) {
		t.Errorf("bravo follows %q after the thread expired, want %s kept", f, repoSubflux)
	}
}

func TestUser_followsEachPathItPostsAtUntilThatThreadExpires(t *testing.T) {
	const retention = 30 * time.Minute
	sub := repoSubflux + "/web"
	s, clk := openRetainingStore(t, retention)
	regAt(t, s, "alpha", repoSubflux)
	mustPost(t, s, userPosting(Target{Path: repoSubflux}))
	clk.advance(retention / 2)
	mine := mustPost(t, s, userPosting(Target{Path: sub}))
	if f := userRow(t, s).Follows; !slices.Equal(f, []string{repoSubflux, sub}) {
		t.Fatalf("@user follows %q after posting at both paths, want both", f)
	}

	clk.advance(retention / 2)
	mustSweep(t, s)
	if f := userRow(t, s).Follows; !slices.Equal(f, []string{sub}) {
		t.Fatalf("@user follows %q once the parent thread expired, want %s alone", f, sub)
	}
	reply := say(t, s, "alpha", Target{Path: sub}, "reply")
	if !reached(t, s, userRow(t, s).ID, reply.Message) {
		t.Errorf("the reply %d in the thread @user posted in did not reach @user", reply.Message)
	}
	if n := rowFor(t, mustConversations(t, s, ConversationsQuery{}), mine.Thread).Unread; n != 1 {
		t.Errorf("@user's unread in the subpath thread %d, want the reply", n)
	}

	clk.advance(retention)
	mustSweep(t, s)
	if f := userRow(t, s).Follows; len(f) != 0 {
		t.Errorf("@user follows %q once the subpath thread expired too, want none", f)
	}
}

func TestUser_aChildThreadItNeverPostedInEndsWithTheParentsFollow(t *testing.T) {
	const retention = 30 * time.Minute
	child := repoSubflux + "/web"
	s, clk := openRetainingStore(t, retention)
	regAt(t, s, "alpha", repoSubflux)
	mustPost(t, s, userPosting(Target{Path: repoSubflux}))
	clk.advance(retention / 2)
	first := say(t, s, "alpha", Target{Path: child}, "child")
	u := userRow(t, s)
	if !reached(t, s, u.ID, first.Message) {
		t.Fatalf("the child post %d did not reach @user through its parent follow", first.Message)
	}

	clk.advance(retention / 2)
	mustSweep(t, s)
	if f := userRow(t, s).Follows; len(f) != 0 {
		t.Errorf("@user follows %q once the parent thread expired, want none", f)
	}
	later := say(t, s, "alpha", Target{Path: child}, "later")
	if reached(t, s, u.ID, later.Message) {
		t.Errorf("a later child post %d reached @user after the parent thread expired", later.Message)
	}
}

func TestUser_isLeftOutOfCensusWhoAndPush(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	mustPost(t, s, posting(sess("alpha"), Target{Agent: UserName}))
	u := userRow(t, s)
	t.Run("census", func(t *testing.T) {
		c := mustCensus(t, s, func(*Identity) bool { return true })
		if c.Live != 1 || c.Online != 1 || c.Unread != 0 {
			t.Errorf("live %d online %d unread %d, want alpha alone and nothing unread", c.Live, c.Online, c.Unread)
		}
	})
	t.Run("who", func(t *testing.T) {
		peers, err := s.Who(t.Context(), Caller{Session: sess("alpha")}, &WhoQuery{All: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(peers) != 1 || peers[0].Name != "alpha" {
			t.Errorf("who lists %v, want alpha alone", peers)
		}
	})
	t.Run("push", func(t *testing.T) {
		as, err := s.Addressees(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(as, func(a Addressee) bool { return a.ID == u.ID }) {
			t.Errorf("addressees %v include @user", as)
		}
		if _, found, _ := s.Addressee(t.Context(), u.ID); found {
			t.Error("Addressee found @user")
		}
	})
	t.Run("nudge", func(t *testing.T) {
		st, found, err := s.Nudging(t.Context(), u.ID)
		if err != nil || !found || st.Due != nil {
			t.Errorf("nudging @user: %+v found %v err %v, want live and nothing due", st, found, err)
		}
	})
}

func TestReadThread_marksTheThreadsPostsRead(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	alpha := regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	watched := posting(sess("alpha"), Target{Agent: UserName})
	watched.NotifyRead = NotifyRead{Names: []string{UserName}}
	first := mustPost(t, s, watched)
	mustPost(t, s, posting(sess("alpha"), Target{Agent: UserName}))
	other := mustPost(t, s, posting(sess("bravo"), Target{Agent: UserName}))
	mustPost(t, s, posting(sess("alpha"), Target{Agent: UserName}))
	u := userRow(t, s)
	clk.advance(time.Minute)

	n, err := s.ReadThread(t.Context(), Caller{User: true}, first.Thread, everyPost)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("stamped %d, want alpha's 3", n)
	}
	for _, d := range deliveriesTo(t, s, u.ID) {
		if read := !d.ReadAt.IsZero(); read != (d.Thread == first.Thread) {
			t.Errorf("delivery %d in thread %d read %v, want read only in thread %d", d.Seq, d.Thread, read, first.Thread)
		}
		if d.Thread == first.Thread && !d.ReadAt.Equal(clk.now()) {
			t.Errorf("delivery %d read at %v, want %v", d.Seq, d.ReadAt, clk.now())
		}
	}
	if row := userRow(t, s); row.Read != 2 {
		t.Errorf("read %d, want 2: bravo's unread post at 3 holds it", row.Read)
	}
	rc, err := s.Receipts(t.Context(), Caller{Session: sess("alpha")}, first.Message)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Recipients) != 1 || rc.Recipients[0].State != receiptRead {
		t.Errorf("receipts %+v, want user read", rc.Recipients)
	}
	if !slices.ContainsFunc(deliveriesTo(t, s, alpha.ID), func(d deliveryRow) bool { return d.Kind == kindReceiptsComplete }) {
		t.Error("alpha got no receipts_complete for the watched post")
	}

	n, err = s.ReadThread(t.Context(), Caller{User: true}, other.Thread, everyPost)
	if err != nil {
		t.Fatal(err)
	}
	if row := userRow(t, s); n != 1 || row.Read != row.Seq {
		t.Errorf("stamped %d, read %d of %d, want 1 and read at the last delivery", n, row.Read, row.Seq)
	}
	if row := userRow(t, s); row.Unread.Counts.Total() != 0 {
		t.Errorf("unread %+v after every thread was read, want none", row.Unread.Counts)
	}
}

func TestReadThread_leavesPostsAfterTheBoundUnread(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	shown := mustPost(t, s, posting(sess("alpha"), Target{Agent: UserName}))
	later := mustPost(t, s, posting(sess("alpha"), Target{Agent: UserName}))
	u := userRow(t, s)

	n, err := s.ReadThread(t.Context(), Caller{User: true}, shown.Thread, shown.Message)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("stamped %d, want only the post shown", n)
	}
	for _, d := range deliveriesTo(t, s, u.ID) {
		if read := !d.ReadAt.IsZero(); read != (d.Message == shown.Message) {
			t.Errorf("delivery of message %d read %v, want read only for message %d", d.Message, read, shown.Message)
		}
	}
	row := userRow(t, s)
	if row.Read >= row.Seq {
		t.Errorf("read %d of %d, want message %d's unread delivery to hold read behind it", row.Read, row.Seq, later.Message)
	}
	if row.Unread.Counts.Total() != 1 {
		t.Errorf("unread %+v, want the later post alone", row.Unread.Counts)
	}
}

func TestReadThread_isOnlyForUser(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	got := mustPost(t, s, posting(sess("alpha"), Target{Agent: UserName}))
	_, err := s.ReadThread(t.Context(), Caller{Session: sess("alpha")}, got.Thread, everyPost)
	wantRefusal(t, err, KindForbidden, "", "only @user")
}
