//go:build !windows

package store

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// shortThreadTTL lets a thread expire inside the 6-hour leases the test
// identities register with.
const shortThreadTTL = 2 * time.Hour

func openShortThreadStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	clk := &clock{t: testNow}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{Logger: slog.New(newLogSink()), Now: clk.now, ThreadTTL: shortThreadTTL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

func mustFollow(t *testing.T, s *Store, session, path string) Identity {
	t.Helper()
	return mustFollowed(t, s, session, path).Identity
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

func TestFollow_withNoThreadYetReceivesTheFirstPost(t *testing.T) {
	s, clk := openShortThreadStore(t)
	const p = "/workspace/marotte/web"
	regAt(t, s, "alpha", repoGrelot)
	b := regAt(t, s, "bravo", repoSubflux)
	got, err := s.Follow(t.Context(), Keyed{}, Caller{Session: sess("bravo")}, p)
	if err != nil {
		t.Fatalf("follow with no thread: %v, want it followed", err)
	}
	if len(got.Notices) != 1 || !strings.Contains(got.Notices[0], "no thread lives at "+p) || !strings.Contains(got.Notices[0], "grelot threads") {
		t.Errorf("notices %q, want one saying no thread lives at %s and naming grelot threads", got.Notices, p)
	}
	if !slices.Equal(got.Identity.Follows, []string{p}) {
		t.Errorf("follows %v, want [%s]", got.Identity.Follows, p)
	}
	first := mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	if ds := deliveriesTo(t, s, b.ID); len(ds) != 1 || ds[0].Message != first.Message {
		t.Errorf("the follower's deliveries %+v, want the thread's first post %d", ds, first.Message)
	}
	if again := mustFollowed(t, s, sess("bravo"), p); len(again.Notices) != 0 {
		t.Errorf("following a live thread: notices %q, want none", again.Notices)
	}
	// The thread is past its expiry, unswept.
	clk.advance(shortThreadTTL)
	if after := mustFollowed(t, s, sess("bravo"), p); len(after.Notices) != 1 {
		t.Errorf("following an expired thread: notices %q, want the no-thread line", after.Notices)
	}
	_, err = s.Follow(t.Context(), Keyed{}, Caller{Session: sess("bravo")}, "workspace/x")
	wantRefusal(t, err, KindInvalid, "", "canonical")
}

func TestThreaded_setForGoodByAFollowOrAPathPost(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	const p = "/workspace/marotte"
	follower := regAt(t, s, "alpha", repoGrelot)
	poster := regAt(t, s, "bravo", repoGrelot)
	other := regAt(t, s, "charlie", repoGrelot)
	d := scopedDelegate(t, s, "charlie", "review", func(g *Grant) { g.AllowThreads = []string{p} })
	if got := mustFollow(t, s, sess("alpha"), p); !got.Threaded {
		t.Errorf("the follow's identity %+v, want it threaded", got)
	}
	if _, err := s.Unfollow(t.Context(), Keyed{}, Caller{Session: sess("alpha")}, p); err != nil {
		t.Fatal(err)
	}
	mustPost(t, s, posting(sess("bravo"), Target{Path: p}))
	mustPost(t, s, selfPost("charlie", Target{Announce: true}))
	mustPost(t, s, selfPost("charlie", Target{Agent: "alpha"}))
	mustPost(t, s, asPost("charlie", d.Name, Target{Path: p}))
	for name, tc := range map[string]struct {
		id   uint64
		want bool
	}{
		"a follower after its unfollow": {follower.ID, true},
		"a path poster":                 {poster.ID, true},
		"an announce and direct poster": {other.ID, false},
		"a delegate's path post":        {d.ID, false},
	} {
		if got := getIdentity(t, s, tc.id).Threaded; got != tc.want {
			t.Errorf("%s: threaded %v, want %v", name, got, tc.want)
		}
	}
}

func mustFollowed(t *testing.T, s *Store, session, path string) Followed {
	t.Helper()
	got, err := s.Follow(t.Context(), Keyed{}, Caller{Session: session}, path)
	if err != nil {
		t.Fatalf("Follow(%s, %s): %v", session, path, err)
	}
	return got
}

func TestFollow_aPathItsFollowsCoverIsNotAdded(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	const lib = "/workspace/bolttable"
	b := regAt(t, s, "bravo", repoSubflux)
	mustFollow(t, s, sess("bravo"), lib)
	got := mustFollowed(t, s, sess("bravo"), lib+"/table.go")
	if !slices.Equal(got.Identity.Follows, []string{lib}) {
		t.Errorf("follows %v, want [%s] alone", got.Identity.Follows, lib)
	}
	if !slices.ContainsFunc(got.Notices, func(n string) bool { return strings.Contains(n, "your follow of "+lib) }) {
		t.Errorf("notices %q, want one naming the follow of %s that covers it", got.Notices, lib)
	}
	mustPost(t, s, posting(sess("bravo"), Target{Path: lib + "/index.go"}))
	if f := getIdentity(t, s, b.ID).Follows; !slices.Equal(f, []string{lib}) {
		t.Errorf("follows after a post under %s: %v, want [%s] alone", lib, f, lib)
	}
}

func TestFollow_aPathItsWorkPathReachesIsNotAdded(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	b := regAt(t, s, "bravo", repoGrelot)
	got := mustFollowed(t, s, sess("bravo"), repoGrelot+"/internal")
	if len(got.Identity.Follows) != 0 {
		t.Errorf("follows %v, want none", got.Identity.Follows)
	}
	if !slices.ContainsFunc(got.Notices, func(n string) bool { return strings.Contains(n, "your work path "+repoGrelot) }) {
		t.Errorf("notices %q, want one naming the work path %s that reaches it", got.Notices, repoGrelot)
	}
	for i := range maxFollows + 1 {
		mustPost(t, s, posting(sess("bravo"), Target{Path: fmt.Sprintf("%s/pkg%d", repoGrelot, i)}))
	}
	if row := getIdentity(t, s, b.ID); len(row.Follows) != 0 || !row.Threaded {
		t.Errorf("after posts under its own work path: follows %v, threaded %v; want none and threaded", row.Follows, row.Threaded)
	}
	if _, err := s.Follow(t.Context(), Keyed{}, Caller{Session: sess("bravo")}, "/workspace/bolttable"); err != nil {
		t.Errorf("follow of a library after those posts: %v, want it added", err)
	}
}

func TestFollow_replacesTheFollowsUnderIt(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	const lib = "/workspace/bolttable"
	b := regAt(t, s, "bravo", repoSubflux)
	full := []string{lib + "/table.go", lib + "/index"}
	for i := len(full); i < maxFollows; i++ {
		full = append(full, fmt.Sprintf("/workspace/thread-%02d", i))
	}
	editIdentity(t, s, b.ID, func(r *identityRow) { r.Follows = full })
	got := mustFollowed(t, s, sess("bravo"), lib)
	if want := append(slices.Clone(full[2:]), lib); !slices.Equal(got.Identity.Follows, want) {
		t.Errorf("follows %v, want %v", got.Identity.Follows, want)
	}
	if !slices.ContainsFunc(got.Notices, func(n string) bool {
		return strings.Contains(n, "replaces") && strings.Contains(n, full[0]+", "+full[1])
	}) {
		t.Errorf("notices %q, want one naming the replaced %s and %s", got.Notices, full[0], full[1])
	}
	editIdentity(t, s, b.ID, func(r *identityRow) { r.Follows = []string{"/workspace/other"} })
	mustPost(t, s, posting(sess("bravo"), Target{Path: lib + "/table.go"}))
	mustPost(t, s, posting(sess("bravo"), Target{Path: lib}))
	if f := getIdentity(t, s, b.ID).Follows; !slices.Equal(f, []string{"/workspace/other", lib}) {
		t.Errorf("follows after posts at %s/table.go then %s: %v, want the post at %s to replace the subpath's", lib, lib, f, lib)
	}
}

func TestUnfollow_removesAFollowAnotherFollowCovers(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	const lib = "/workspace/bolttable"
	b := regAt(t, s, "bravo", repoSubflux)
	editIdentity(t, s, b.ID, func(r *identityRow) { r.Follows = []string{lib, lib + "/table.go"} })
	got := mustUnfollow(t, s, "bravo", lib+"/table.go")
	if !slices.Equal(got.Identity.Follows, []string{lib}) {
		t.Errorf("follows %v, want [%s]", got.Identity.Follows, lib)
	}
	if len(got.Notices) != 1 || !strings.Contains(got.Notices[0], "your follow of "+lib) {
		t.Errorf("notices %q, want one naming the follow of %s that still delivers it", got.Notices, lib)
	}
	_, err := s.Unfollow(t.Context(), Keyed{}, Caller{Session: sess("bravo")}, lib+"/never-followed")
	wantRefusal(t, err, KindConflict, "", lib+"/never-followed", "your follow of "+lib)
	if got := mustUnfollow(t, s, "bravo", lib); len(got.Identity.Follows) != 0 || len(got.Notices) != 0 {
		t.Errorf("unfollowing the last follow: follows %v, notices %q; want neither", got.Identity.Follows, got.Notices)
	}
}

func mustUnfollow(t *testing.T, s *Store, name, path string) Followed {
	t.Helper()
	got, err := s.Unfollow(t.Context(), Keyed{}, Caller{Session: sess(name)}, path)
	if err != nil {
		t.Fatalf("Unfollow(%s, %s): %v", name, path, err)
	}
	return got
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
	_, err := s.Follow(t.Context(), Keyed{}, Caller{Session: sess("bravo")}, p)
	wantRefusal(t, err, KindConflict, "", "32", "grelot unfollow")
	if f := getIdentity(t, s, b.ID).Follows; !slices.Equal(f, full) {
		t.Errorf("follows changed to %d entries, want the 32 kept", len(f))
	}
}

func TestFollowAndUnfollow_needALiveIdentity(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha", repoGrelot)
	mustPost(t, s, posting(sess("alpha"), Target{Path: repoSubflux}))
	_, err := s.Follow(t.Context(), Keyed{}, Caller{Session: sess("nobody")}, repoSubflux)
	wantRefusal(t, err, KindGone, "none")
	_, err = s.Unfollow(t.Context(), Keyed{}, Caller{Session: sess("nobody")}, repoSubflux)
	wantRefusal(t, err, KindGone, "none")
	expireToDormant(t, s, clk, "subflux-dev")
	_, err = s.Follow(t.Context(), Keyed{}, Caller{Session: sessA}, repoSubflux)
	wantRefusal(t, err, KindGone, "dormant", "grelot revive --ttl=<d>")
	_, err = s.Unfollow(t.Context(), Keyed{}, Caller{Session: sessA}, repoSubflux)
	wantRefusal(t, err, KindGone, "dormant", "grelot revive --ttl=<d>")
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
	if got := mustUnfollow(t, s, "bravo", p).Identity.Follows; !slices.Equal(got, []string{repoGrelot}) {
		t.Errorf("follows %v, want only %s left", got, repoGrelot)
	}
	mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	if ds := deliveriesTo(t, s, b.ID); len(ds) != 0 {
		t.Errorf("an unfollowed thread delivered %d posts", len(ds))
	}
	if _, err := s.Unfollow(t.Context(), Keyed{}, Caller{Session: sess("bravo")}, p); err != nil {
		t.Errorf("unfollowing a thread not followed: %v, want nothing to do", err)
	}
	if f := getIdentity(t, s, b.ID).Follows; !slices.Equal(f, []string{repoGrelot}) {
		t.Errorf("follows %v, want [%s]", f, repoGrelot)
	}
}

func TestUnfollow_underAWorkPath(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	const work = "/workspace/marotte"
	b := regAt(t, s, "bravo", work)
	editIdentity(t, s, b.ID, func(r *identityRow) { r.Follows = []string{work, work + "/web", "/workspace/marotte-old"} })
	for _, p := range []string{work, work + "/web"} {
		got := mustUnfollow(t, s, "bravo", p)
		if slices.Contains(got.Identity.Follows, p) {
			t.Errorf("unfollow %s: follows %v, want it removed", p, got.Identity.Follows)
		}
		if len(got.Notices) != 1 || !strings.Contains(got.Notices[0], "your work path "+work) || !strings.Contains(got.Notices[0], "narrow your paths") {
			t.Errorf("unfollow %s: notices %q, want one naming the work path that still delivers it", p, got.Notices)
		}
	}
	_, err := s.Unfollow(t.Context(), Keyed{}, Caller{Session: sess("bravo")}, work+"/never-followed")
	wantRefusal(t, err, KindConflict, "", work+"/never-followed", work, "narrow your paths")
	got := mustUnfollow(t, s, "bravo", "/workspace/marotte-old")
	if len(got.Identity.Follows) != 0 || len(got.Notices) != 0 {
		t.Errorf("a name-prefix sibling: follows %v, notices %q; want it removed with no notice", got.Identity.Follows, got.Notices)
	}
	if f := getIdentity(t, s, b.ID).Follows; len(f) != 0 {
		t.Errorf("stored follows %v, want none", f)
	}
}

// followThreadsFixture posts a thread at /workspace/shared/x that expires
// first, a subpath thread and a later unrelated thread that outlive it,
// and has bravo follow x, charlie the subpath and delta the later thread.
// alpha's posts follow x and y; its follow of x covers the subpath.
func followThreadsFixture(t *testing.T) (s *Store, ids map[string]uint64) {
	t.Helper()
	s, clk := openShortThreadStore(t)
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
	return s, ids
}

const sharedX = "/workspace/shared/x"

// wantFollowsKept checks that every fixture identity still follows what
// it followed before x expired, and that the next post at x reaches x's
// followers, alpha and bravo.
func wantFollowsKept(t *testing.T, s *Store, ids map[string]uint64) {
	t.Helper()
	want := map[string][]string{
		"alpha":   {sharedX, "/workspace/shared/y"},
		"bravo":   {sharedX},
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
	got := mustPost(t, s, posting(sess("charlie"), Target{Path: sharedX}))
	if got.Delivered != 2 {
		t.Errorf("the recreated thread's first post delivered to %d, want alpha and bravo", got.Delivered)
	}
	for _, n := range []string{"alpha", "bravo"} {
		if ds := deliveriesTo(t, s, ids[n]); len(ds) == 0 || ds[len(ds)-1].Message != got.Message {
			t.Errorf("%s, a follower of x, got %+v, want the new post %d last", n, ds, got.Message)
		}
	}
}

func TestThreadExpiry_keepsItsFollows(t *testing.T) {
	s, ids := followThreadsFixture(t)
	mustSweep(t, s)
	if ths, err := s.Threads(t.Context(), Caller{Session: sess("alpha")}, sharedX); err != nil || len(ths) != 1 || ths[0].Path != sharedX+"/sub" {
		t.Fatalf("threads under x after the sweep %+v, %v: want only the subpath's, x expired", ths, err)
	}
	wantFollowsKept(t, s, ids)
}

func TestThreadExpiry_unsweptKeepsItsFollows(t *testing.T) {
	s, ids := followThreadsFixture(t)
	// charlie's post in wantFollowsKept meets the expired thread before any sweep.
	wantFollowsKept(t, s, ids)
}
