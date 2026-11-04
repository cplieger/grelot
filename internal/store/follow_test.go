package store

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// shortThreadTTL lets a thread expire inside the 6-hour leases the test
// identities register with.
const shortThreadTTL = 2 * time.Hour

func openShortThreadStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	clk := &clock{t: testNow}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), Options{Logger: slog.New(newLogSink()), Now: clk.now, ThreadTTL: shortThreadTTL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

func mustFollow(t *testing.T, s *Store, session, path string) Identity {
	t.Helper()
	got, err := s.Follow(t.Context(), Caller{Session: session}, path)
	if err != nil {
		t.Fatalf("Follow(%s, %s): %v", session, path, err)
	}
	return got
}

func TestFollow_aLiveThreadAddsItOnce(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	const p = "/workspace/marotte/web"
	regAt(t, s, "alpha", repoGrelot)
	b := regAt(t, s, "bravo", repoSubflux)
	mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	got := mustFollow(t, s, sess("bravo"), p)
	if !slices.Equal(got.Follows, []string{p}) {
		t.Errorf("follows %v, want [%s]", got.Follows, p)
	}
	mustFollow(t, s, sess("bravo"), p)
	if f := getIdentity(t, s, b.ID).Follows; !slices.Equal(f, []string{p}) {
		t.Errorf("follows after a second follow %v, want [%s] once", f, p)
	}
	reply := mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	ds := deliveriesTo(t, s, b.ID)
	if len(ds) != 1 || ds[0].Message != reply.Message || ds[0].Path != p {
		t.Errorf("the follower's deliveries %+v, want the later post %d via %s", ds, reply.Message, p)
	}
}

func TestFollow_needsALiveThread(t *testing.T) {
	s, clk := openShortThreadStore(t)
	const p = "/workspace/marotte/web"
	regAt(t, s, "alpha", repoGrelot)
	b := regAt(t, s, "bravo", repoSubflux)
	_, err := s.Follow(t.Context(), Caller{Session: sess("bravo")}, p)
	wantRefusal(t, err, KindNotFound, "", "no thread at "+p+"; post there to start one")
	// A direct thread's key is no path, and the announcements are no path
	// thread either.
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	_, err = s.Follow(t.Context(), Caller{Session: sess("bravo")}, "/workspace")
	wantRefusal(t, err, KindNotFound, "", "no thread at /workspace")
	mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	// The thread is past its expiry, unswept.
	clk.advance(shortThreadTTL)
	_, err = s.Follow(t.Context(), Caller{Session: sess("bravo")}, p)
	wantRefusal(t, err, KindNotFound, "", "no thread at "+p)
	if f := getIdentity(t, s, b.ID).Follows; len(f) != 0 {
		t.Errorf("follows %v after refusals, want none", f)
	}
	_, err = s.Follow(t.Context(), Caller{Session: sess("bravo")}, "workspace/x")
	wantRefusal(t, err, KindInvalid, "", "canonical")
}

func TestFollow_refusedAtTheCap(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	const p = "/workspace/one-more"
	regAt(t, s, "alpha", repoGrelot)
	b := regAt(t, s, "bravo", repoSubflux)
	mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	full := make([]string, 32)
	for i := range full {
		full[i] = fmt.Sprintf("/workspace/thread-%02d", i)
	}
	editIdentity(t, s, b.ID, func(r *identityRow) { r.Follows = full })
	_, err := s.Follow(t.Context(), Caller{Session: sess("bravo")}, p)
	wantRefusal(t, err, KindConflict, "", "32", "grelot unfollow")
	if f := getIdentity(t, s, b.ID).Follows; !slices.Equal(f, full) {
		t.Errorf("follows changed to %d entries, want the 32 kept", len(f))
	}
}

func TestFollowAndUnfollow_needALiveIdentity(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha", repoGrelot)
	mustPost(t, s, posting(sess("alpha"), Target{Path: repoSubflux}))
	_, err := s.Follow(t.Context(), Caller{Session: sess("nobody")}, repoSubflux)
	wantRefusal(t, err, KindGone, "none")
	_, err = s.Unfollow(t.Context(), Caller{Session: sess("nobody")}, repoSubflux)
	wantRefusal(t, err, KindGone, "none")
	expireToDormant(t, s, clk, "subflux-dev")
	_, err = s.Follow(t.Context(), Caller{Session: sessA}, repoSubflux)
	wantRefusal(t, err, KindGone, "dormant", "grelot revive --ttl <d>")
	_, err = s.Unfollow(t.Context(), Caller{Session: sessA}, repoSubflux)
	wantRefusal(t, err, KindGone, "dormant", "grelot revive --ttl <d>")
}

func TestUnfollow_stopsTheThread(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	const p = "/workspace/marotte/web"
	regAt(t, s, "alpha", repoGrelot)
	b := regAt(t, s, "bravo", repoSubflux)
	mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	mustPost(t, s, posting(sess("alpha"), Target{Path: repoGrelot}))
	mustFollow(t, s, sess("bravo"), p)
	mustFollow(t, s, sess("bravo"), repoGrelot)
	got, err := s.Unfollow(t.Context(), Caller{Session: sess("bravo")}, p)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Follows, []string{repoGrelot}) {
		t.Errorf("follows %v, want only %s left", got.Follows, repoGrelot)
	}
	mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	if ds := deliveriesTo(t, s, b.ID); len(ds) != 0 {
		t.Errorf("an unfollowed thread delivered %d posts", len(ds))
	}
	if _, err := s.Unfollow(t.Context(), Caller{Session: sess("bravo")}, p); err != nil {
		t.Errorf("unfollowing a thread not followed: %v, want nothing to do", err)
	}
	if f := getIdentity(t, s, b.ID).Follows; !slices.Equal(f, []string{repoGrelot}) {
		t.Errorf("follows %v, want [%s]", f, repoGrelot)
	}
}

func TestUnfollow_refusedUnderAWorkPath(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	const work = "/workspace/marotte"
	b := regAt(t, s, "bravo", work)
	follows := []string{work, work + "/web", "/workspace/marotte-old"}
	editIdentity(t, s, b.ID, func(r *identityRow) { r.Follows = follows })
	for _, p := range []string{work, work + "/web", work + "/never-followed"} {
		_, err := s.Unfollow(t.Context(), Caller{Session: sess("bravo")}, p)
		wantRefusal(t, err, KindConflict, "", p, work, "narrow your paths")
	}
	if f := getIdentity(t, s, b.ID).Follows; !slices.Equal(f, follows) {
		t.Errorf("follows %v after refusals, want %v", f, follows)
	}
	got, err := s.Unfollow(t.Context(), Caller{Session: sess("bravo")}, "/workspace/marotte-old")
	if err != nil {
		t.Fatalf("a name-prefix sibling is not under the work path: %v", err)
	}
	if !slices.Equal(got.Follows, follows[:2]) {
		t.Errorf("follows %v, want %v", got.Follows, follows[:2])
	}
}

// followThreadsFixture posts a thread at /workspace/shared/x that expires
// first, a subpath thread and a later unrelated thread that outlive it,
// and has bravo follow x, charlie the subpath and delta the later thread.
// alpha posted at x, so it follows x too.
func followThreadsFixture(t *testing.T) (s *Store, clk *clock, ids map[string]uint64) {
	t.Helper()
	s, clk = openShortThreadStore(t)
	ids = map[string]uint64{}
	for _, n := range []string{"alpha", "bravo", "charlie", "delta"} {
		ids[n] = regAt(t, s, n, "/workspace/"+n).ID
	}
	mustPost(t, s, posting(sess("alpha"), Target{Path: sharedX}))
	mustFollow(t, s, sess("bravo"), sharedX)
	clk.advance(30 * time.Minute)
	mustPost(t, s, posting(sess("alpha"), Target{Path: sharedX + "/sub"}))
	mustFollow(t, s, sess("charlie"), sharedX+"/sub")
	mustPost(t, s, posting(sess("alpha"), Target{Path: "/workspace/shared/y"}))
	mustFollow(t, s, sess("delta"), "/workspace/shared/y")
	// x expires at its last post plus the thread TTL, half an hour before
	// the others.
	clk.advance(shortThreadTTL - 30*time.Minute)
	return s, clk, ids
}

const sharedX = "/workspace/shared/x"

func TestThreadExpiry_dropsExactlyItsFollows(t *testing.T) {
	s, _, ids := followThreadsFixture(t)
	mustSweep(t, s)
	want := map[string][]string{
		"alpha":   {sharedX + "/sub", "/workspace/shared/y"},
		"bravo":   nil,
		"charlie": {sharedX + "/sub"},
		"delta":   {"/workspace/shared/y"},
	}
	for n, f := range want {
		row := getIdentity(t, s, ids[n])
		if !slices.Equal(row.Follows, f) {
			t.Errorf("%s follows %v, want %v", n, row.Follows, f)
		}
		if row.State != stateLive {
			t.Errorf("%s is %s, want the follower identity kept live", n, row.State)
		}
	}
	// A thread recreated at the same path starts with no followers.
	mustPost(t, s, posting(sess("charlie"), Target{Path: sharedX}))
	for _, n := range []string{"alpha", "bravo"} {
		if ds := deliveriesTo(t, s, ids[n]); len(ds) != 0 {
			t.Errorf("%s, a follower of the expired thread, got %d deliveries of the new one", n, len(ds))
		}
	}
}

func TestThreadExpiry_dropsItsFollowsEagerly(t *testing.T) {
	s, _, ids := followThreadsFixture(t)
	// charlie's post meets the expired thread before any sweep.
	got := mustPost(t, s, posting(sess("charlie"), Target{Path: sharedX}))
	if got.Delivered != 0 {
		t.Errorf("delivered %d, want 0: the old thread's followers are gone", got.Delivered)
	}
	if f := getIdentity(t, s, ids["bravo"]).Follows; len(f) != 0 {
		t.Errorf("bravo follows %v, want none", f)
	}
	if f := getIdentity(t, s, ids["charlie"]).Follows; !slices.Equal(f, []string{sharedX + "/sub", sharedX}) {
		t.Errorf("charlie follows %v, want its subpath and the new thread it posted in", f)
	}
}
