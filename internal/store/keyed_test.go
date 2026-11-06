//go:build !windows

package store

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
	"go.etcd.io/bbolt"
)

// keyFor is the Idempotency-Key key sent by c with the request named
// request.
func keyFor(c Caller, key, request string) Keyed {
	return NewKeyed(c, key, sha256.Sum256([]byte(request)))
}

// countPosts counts the agents' posts, leaving out the cards
// registrations write.
func countPosts(t *testing.T, s *Store) int {
	t.Helper()
	n := 0
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		return s.t.messages.table.Scan(r, bolttable.Range{}, func(_ []byte, m *messageRow) (bool, error) {
			if isPost(m.Kind) {
				n++
			}
			return true, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func countRequests(t *testing.T, s *Store) int {
	t.Helper()
	n := 0
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		return s.t.requests.table.Scan(r, bolttable.Range{}, func([]byte, *requestRow) (bool, error) {
			n++
			return true, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPost_keyedReSendAnswersTheFirstResultAndStoresOnce(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	b := regAt(t, s, "bravo")
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	k := keyFor(plain("alpha"), "key-one", "post to bravo")

	first, replayed, err := s.Post(t.Context(), k, &p)
	if err != nil || replayed {
		t.Fatalf("first Post = replayed %v, %v; want a fresh post", replayed, err)
	}
	commits := commitCount(t, s)
	again, replayed, err := s.Post(t.Context(), k, &p)
	if err != nil {
		t.Fatalf("re-sent Post: %v", err)
	}
	if !replayed {
		t.Error("re-sent Post did not report a replay")
	}
	if !reflect.DeepEqual(again, first) {
		t.Errorf("re-sent Post = %+v, want the first result %+v", again, first)
	}
	if got := commitCount(t, s); got != commits {
		t.Errorf("the replay committed %d transactions, want none", got-commits)
	}
	if n := countPosts(t, s); n != 1 {
		t.Errorf("%d messages, want 1", n)
	}
	if n := len(deliveriesTo(t, s, b.ID)); n != 1 {
		t.Errorf("bravo has %d deliveries, want 1", n)
	}
	checkSummaries(t, s, testNow, "after the replay")
}

func TestPost_keySentWithAnotherRequestIsRefusedAndStoresNothing(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	mustKeyedPost(t, s, keyFor(plain("alpha"), "key-one", "post to bravo"), p)

	p.Body = "another body"
	_, _, err := s.Post(t.Context(), keyFor(plain("alpha"), "key-one", "another post to bravo"), &p)
	wantRefusal(t, err, KindKeyReused, "", "Idempotency-Key", "send a new key")
	if n := countPosts(t, s); n != 1 {
		t.Errorf("%d messages, want 1", n)
	}
}

func TestPost_refusedKeyedPostRecordsNothingSoItsReSendRuns(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	k := keyFor(plain("alpha"), "key-one", "post to bravo")

	_, _, err := s.Post(t.Context(), k, &p)
	wantRefusal(t, err, KindNotFound, "")
	if n := countRequests(t, s); n != 0 {
		t.Errorf("%d request rows after a refusal, want 0", n)
	}
	regAt(t, s, "bravo")
	if _, replayed, err := s.Post(t.Context(), k, &p); err != nil || replayed {
		t.Errorf("re-sent Post = replayed %v, %v; want a fresh post", replayed, err)
	}
	if n := countPosts(t, s); n != 1 {
		t.Errorf("%d messages, want 1", n)
	}
}

func TestPost_reSentKeyAnswersItsFirstResultPastNinetyPercent(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	k := keyFor(plain("alpha"), "key-one", "post to bravo")
	first := mustKeyedPost(t, s, k, p)

	s.space = inUse(DefaultMaxBytes)
	again, replayed, err := s.Post(t.Context(), k, &p)
	if err != nil || !replayed || !reflect.DeepEqual(again, first) {
		t.Errorf("re-sent Post past 90 %% = %+v, replayed %v, %v; want the first result %+v", again, replayed, err, first)
	}
	_, _, err = s.Post(t.Context(), keyFor(plain("alpha"), "key-two", "post to bravo"), &p)
	wantRefusal(t, err, KindFull, "", "store nearly full")
}

func TestPost_keyRunsAfreshOnceItsFiveMinutesPassed(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	k := keyFor(plain("alpha"), "key-one", "post to bravo")
	mustKeyedPost(t, s, k, p)

	clk.advance(5*time.Minute - time.Second)
	mustSweep(t, s)
	if _, replayed, err := s.Post(t.Context(), k, &p); err != nil || !replayed {
		t.Errorf("Post 1 s before the expiry = replayed %v, %v; want a replay", replayed, err)
	}
	clk.advance(time.Second)
	mustSweep(t, s)
	if n := countRequests(t, s); n != 0 {
		t.Errorf("%d request rows after the sweep, want 0", n)
	}
	if _, replayed, err := s.Post(t.Context(), k, &p); err != nil || replayed {
		t.Errorf("Post after the expiry = replayed %v, %v; want a fresh post", replayed, err)
	}
	if n := countPosts(t, s); n != 2 {
		t.Errorf("%d messages, want 2", n)
	}
}

func TestPost_keyIsScopedToItsCaller(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	mustKeyedPost(t, s, keyFor(plain("alpha"), "key-one", "a post"), posting(sess("alpha"), Target{Agent: "bravo"}))

	p := posting(sess("bravo"), Target{Agent: "alpha"})
	got, replayed, err := s.Post(t.Context(), keyFor(plain("bravo"), "key-one", "a post"), &p)
	if err != nil || replayed {
		t.Fatalf("bravo's Post with alpha's key = replayed %v, %v; want bravo's own post", replayed, err)
	}
	if got.Identity.Name != "bravo" {
		t.Errorf("bravo's Post answered %s's identity", got.Identity.Name)
	}
	if n := countPosts(t, s); n != 2 {
		t.Errorf("%d messages, want 2", n)
	}
}

func TestReplayPost_readsTheStoredFirstResult(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	k := keyFor(plain("alpha"), "key-one", "post to bravo")

	if _, found, err := s.ReplayPost(t.Context(), k); err != nil || found {
		t.Fatalf("ReplayPost before the post = found %v, %v; want nothing", found, err)
	}
	first := mustKeyedPost(t, s, k, posting(sess("alpha"), Target{Agent: "bravo"}))
	got, found, err := s.ReplayPost(t.Context(), k)
	if err != nil || !found || !reflect.DeepEqual(got, first) {
		t.Errorf("ReplayPost = %+v, found %v, %v; want the first result %+v", got, found, err, first)
	}
	_, _, err = s.ReplayPost(t.Context(), keyFor(plain("alpha"), "key-one", "another post"))
	wantRefusal(t, err, KindKeyReused, "")
	clk.advance(5 * time.Minute)
	if _, found, err := s.ReplayPost(t.Context(), k); err != nil || found {
		t.Errorf("ReplayPost past the expiry = found %v, %v; want nothing", found, err)
	}
}

func TestKeyedWrites_reSendAnswersTheFirstResultWhereARunWouldRefuse(t *testing.T) {
	t.Run("retire", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		alpha := regAt(t, s, "alpha").ID
		k := keyFor(plain("alpha"), "key-one", "retire")
		if id, replayed, err := s.Retire(t.Context(), k, plain("alpha")); err != nil || replayed || id != alpha {
			t.Fatalf("Retire = %d, replayed %v, %v; want a fresh retirement of %d", id, replayed, err, alpha)
		}
		if id, replayed, err := s.Retire(t.Context(), k, plain("alpha")); err != nil || !replayed || id != alpha {
			t.Errorf("re-sent Retire = %d, replayed %v, %v; want a replay of %d, not KindGone", id, replayed, err, alpha)
		}
	})
	t.Run("delegate link", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		regAt(t, s, "alpha")
		g := grantFrom("alpha", "review")
		link, _, err := s.Delegate(t.Context(), keyFor(g.Caller, "key-one", "grant"), &g)
		if err != nil {
			t.Fatal(err)
		}
		r := delegateReg(sess("alpha"), link.Link)
		k := keyFor(plain("alpha"), "key-two", "register the delegate")
		first, _, err := s.Register(t.Context(), k, &r)
		if err != nil {
			t.Fatal(err)
		}
		again, replayed, err := s.Register(t.Context(), k, &r)
		if err != nil || !replayed || !reflect.DeepEqual(again, first) {
			t.Errorf("re-sent Register with a used link = %+v, replayed %v, %v; want the first %+v", again, replayed, err, first)
		}
	})
	t.Run("reserve", func(t *testing.T) {
		s, _ := openReservationStore(t, 0)
		regAt(t, s, "holder")
		r := reserving("holder", pathMarotte, time.Hour)
		k := keyFor(r.Caller, "key-one", "reserve")
		first, _, err := s.Reserve(t.Context(), k, &r)
		if err != nil {
			t.Fatal(err)
		}
		again, replayed, err := s.Reserve(t.Context(), k, &r)
		if err != nil || !replayed || !reflect.DeepEqual(again, first) {
			t.Errorf("re-sent Reserve = %+v, replayed %v, %v; want the first %+v", again, replayed, err, first)
		}
		got, err := s.Reservations(t.Context(), plain("holder"), ReservationQuery{Mine: true})
		if err != nil || len(got) != 1 {
			t.Errorf("Reservations = %d rows, %v; want 1", len(got), err)
		}
	})
}

func TestReserve_conflictRecordsNothingSoItsReSendRuns(t *testing.T) {
	s, _ := openReservationStore(t, 0)
	regAt(t, s, "holder")
	regAt(t, s, "other")
	held := mustReserve(t, s, reserving("holder", pathMarotte, time.Hour))
	r := reserving("other", pathMarotte, time.Hour)
	k := keyFor(r.Caller, "key-one", "reserve")

	_, _, err := s.Reserve(t.Context(), k, &r)
	wantRefusal(t, err, KindConflict, "")
	if _, err := s.Release(t.Context(), Keyed{}, plain("holder"), held.Reservation.ID); err != nil {
		t.Fatal(err)
	}
	got, replayed, err := s.Reserve(t.Context(), k, &r)
	if err != nil || replayed || got.Reservation.State != reservationHeld {
		t.Errorf("re-sent Reserve = %+v, replayed %v, %v; want a fresh grant", got, replayed, err)
	}
}

func TestDelegate_keyedLinkIsNeverStoredInTheClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	s, err := Open(path, &Options{Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	regAt(t, s, "alpha")
	g := grantFrom("alpha", "review")
	k := keyFor(g.Caller, "key-one", "grant")
	first, _, err := s.Delegate(t.Context(), k, &g)
	if err != nil {
		t.Fatal(err)
	}
	again, replayed, err := s.Delegate(t.Context(), k, &g)
	if err != nil || !replayed || again.Link != first.Link {
		t.Fatalf("re-sent Delegate = %q, replayed %v, %v; want the first link %q", again.Link, replayed, err, first.Link)
	}
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte(first.Link[len(linkPrefix):])
	if bytes.Contains(file, secret) {
		t.Error("the store file holds the delegate link in the clear")
	}
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		return s.t.requests.table.Scan(r, bolttable.Range{}, func(_ []byte, row *requestRow) (bool, error) {
			if bytes.Contains(row.Sealed, secret) {
				t.Error("the request row holds the delegate link in the clear")
			}
			return true, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestKeyedWrites_Concurrent(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	const goroutines, shared, own = 32, 8, 8
	type result struct {
		v        any
		replayed bool
	}
	results := make([][]result, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range shared + own {
				name := fmt.Sprintf("shared-%d", i)
				if i >= shared {
					name = fmt.Sprintf("own-%d-%d", g, i)
				}
				p := posting(sess("alpha"), Target{Agent: "bravo"})
				p.Subject = name
				posted, pr, err := s.Post(t.Context(), keyFor(plain("alpha"), "post-"+name, "post "+name), &p)
				if err != nil {
					t.Errorf("Post %s: %v", name, err)
				}
				acked, ar, err := s.Ack(t.Context(), keyFor(plain("bravo"), "ack-"+name, "ack "+name), plain("bravo"), uint64(i+1))
				if err != nil {
					t.Errorf("Ack %s: %v", name, err)
				}
				results[g] = append(results[g], result{posted, pr}, result{acked, ar})
			}
		})
	}
	wg.Wait()

	if n := countPosts(t, s); n != shared+goroutines*own {
		t.Errorf("%d messages, want %d: one per key", n, shared+goroutines*own)
	}
	for i := range 2 * shared {
		fresh := 0
		for g := range goroutines {
			if !results[g][i].replayed {
				fresh++
			}
			if !reflect.DeepEqual(results[g][i].v, results[0][i].v) {
				t.Errorf("shared call %d: goroutine %d got %+v, goroutine 0 got %+v", i, g, results[g][i].v, results[0][i].v)
			}
		}
		if fresh != 1 {
			t.Errorf("shared call %d ran %d times, want once", i, fresh)
		}
	}
	checkSummaries(t, s, testNow, "after the concurrent writes")
}

func TestOpen_fileWithoutTheRequestsNamespaceKeepsItsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	s, err := Open(path, &Options{Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	a := regAt(t, s, "alpha")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket([]byte(requestsNamespace)) }); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	sink := newLogSink()
	s, err = Open(path, &Options{Logger: slog.New(sink), Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if lines := sink.lines(slog.LevelWarn, msgStoreReset); len(lines) != 0 {
		t.Errorf("opening the file reset namespaces %q, want none", attr(lines[0], "namespaces"))
	}
	if row := getIdentity(t, s, a.ID); row.Name != "alpha" {
		t.Errorf("identity %d is %q, want alpha", a.ID, row.Name)
	}
	mustKeyedPost(t, s, keyFor(plain("alpha"), "key-one", "announce"), posting(sess("alpha"), Target{Announce: true}))
}

func mustKeyedPost(t *testing.T, s *Store, k Keyed, p Posting) Posted {
	t.Helper()
	got, replayed, err := s.Post(t.Context(), k, &p)
	if err != nil || replayed {
		t.Fatalf("Post = replayed %v, %v; want a fresh post", replayed, err)
	}
	return got
}
