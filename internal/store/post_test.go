//go:build !windows

package store

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

func sess(n string) string { return "kiro-cli:session-" + n }

// keepFields is a post's identity fields, every one "no change".
func keepFields() Fields {
	return Fields{Name: NoChange, Description: NoChange, Paths: []string{NoChange}}
}

func posting(session string, to Target) Posting {
	return Posting{Caller: Caller{Session: session}, To: to, Subject: "Start: one-engine refactor", Body: "body", Fields: keepFields()}
}

func mustPost(t *testing.T, s *Store, p Posting) Posted {
	t.Helper()
	got, _, err := s.Post(t.Context(), Keyed{}, &p)
	if err != nil {
		t.Fatalf("Post(%s, %+v): %v", p.Caller.Session, p.To, err)
	}
	return got
}

// regAt registers name from its own session in envA on paths.
func regAt(t *testing.T, s *Store, name string, paths ...string) Identity {
	t.Helper()
	return mustRegister(t, s, registration(sess(name), name, envA(), paths...))
}

// deliveriesTo lists identity id's delivery rows in seq order, leaving out
// the join and left cards that registrations beside it write.
func deliveriesTo(t *testing.T, s *Store, id uint64) []deliveryRow {
	t.Helper()
	var out []deliveryRow
	for _, d := range allDeliveriesTo(t, s, id) {
		if d.Kind != kindJoin && d.Kind != kindLeft {
			out = append(out, d)
		}
	}
	return out
}

// allDeliveriesTo lists identity id's delivery rows in seq order.
func allDeliveriesTo(t *testing.T, s *Store, id uint64) []deliveryRow {
	t.Helper()
	var out []deliveryRow
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		return s.t.deliveries.table.Scan(r, bolttable.Range{Prefix: be64(id)}, func(_ []byte, d *deliveryRow) (bool, error) {
			out = append(out, *d)
			return true, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func findMessage(t *testing.T, s *Store, id uint64) (messageRow, bool) {
	t.Helper()
	var out messageRow
	var found bool
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		return s.t.messages.table.Scan(r, bolttable.Range{}, func(_ []byte, m *messageRow) (bool, error) {
			if m.ID == id {
				out, found = *m, true
			}
			return !found, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return out, found
}

func findThread(t *testing.T, s *Store, id uint64) (threadRow, bool) {
	t.Helper()
	var out threadRow
	var found bool
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		var err error
		out, found, err = s.t.threads.table.Get(r, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out, found
}

func TestOpen_createsTheServerThreadsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	for range 2 {
		s, err := Open(path, &Options{Now: func() time.Time { return testNow }})
		if err != nil {
			t.Fatal(err)
		}
		byKind := map[string][]threadRow{}
		if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
			return s.t.threads.table.Scan(r, bolttable.Range{}, func(_ uint64, v *threadRow) (bool, error) {
				byKind[v.Kind] = append(byKind[v.Kind], *v)
				return true, nil
			})
		}); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{kindAnnounce, kindCards} {
			ths := byKind[kind]
			if len(ths) != 1 {
				t.Fatalf("%d %s threads, want 1", len(ths), kind)
			}
			if !ths[0].Lease.Expires.IsZero() {
				t.Errorf("%s thread expires %v, want never", kind, ths[0].Lease.Expires)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPost_announcementReachesEveryLiveAndDormantIdentityButTheSender(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	a := regAt(t, s, "alpha")
	b := regAt(t, s, "bravo")
	rc := registration(sess("charlie"), "charlie", envA())
	rc.TTL = 15 * time.Minute
	c := mustRegister(t, s, rc)
	d := regAt(t, s, "delta")
	editIdentity(t, s, d.ID, func(r *identityRow) {
		r.State, r.DormantSince, r.Lease = stateDormant, testNow, lease{Expires: testNow.Add(time.Minute)}
	})
	// charlie is past its expiry and delta past its purge; no sweep ran.
	clk.advance(20 * time.Minute)
	got := mustPost(t, s, posting(sess("alpha"), Target{Announce: true}))
	if got.Delivered != 2 {
		t.Errorf("delivered %d, want 2 (bravo and the dormant charlie)", got.Delivered)
	}
	if st := getIdentity(t, s, c.ID).State; st != stateDormant {
		t.Errorf("charlie is %s at the post, want dormant", st)
	}
	wantExpiry := clk.now().Add(7 * 24 * time.Hour)
	for _, id := range []uint64{b.ID, c.ID} {
		ds := deliveriesTo(t, s, id)
		if len(ds) != 1 {
			t.Fatalf("identity %d has %d deliveries, want 1", id, len(ds))
		}
		dv := ds[0]
		if dv.Kind != kindAnnounce || dv.Message != got.Message || dv.Thread != got.Thread || dv.Sender != a.ID {
			t.Errorf("delivery %+v, want an announcement of message %d in thread %d from %d", dv, got.Message, got.Thread, a.ID)
		}
		if !dv.Lease.Expires.Equal(wantExpiry) {
			t.Errorf("delivery expires %v, want its message's, 7 days on: %v", dv.Lease.Expires, wantExpiry)
		}
	}
	if ds := deliveriesTo(t, s, a.ID); len(ds) != 0 {
		t.Errorf("the sender got %d deliveries of its own post", len(ds))
	}
	if ds := deliveriesTo(t, s, d.ID); len(ds) != 0 {
		t.Errorf("a purged identity got %d deliveries", len(ds))
	}
	msg, found := findMessage(t, s, got.Message)
	if !found || !msg.Lease.Expires.Equal(wantExpiry) {
		t.Errorf("message found %v expires %v, want %v", found, msg.Lease.Expires, wantExpiry)
	}
	th, _ := findThread(t, s, got.Thread)
	if th.Kind != kindAnnounce || !th.Lease.Expires.IsZero() {
		t.Errorf("thread kind %q expires %v, want the announcements, never expiring", th.Kind, th.Lease.Expires)
	}
}

func TestPost_pathReachesWorkPathsAtOrAboveItOnce(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	const p = "/workspace/marotte"
	a := regAt(t, s, "alpha", "/workspace")
	b := regAt(t, s, "bravo", p)
	c := regAt(t, s, "charlie", "/workspace")
	d := regAt(t, s, "delta", p+"/web")
	e := regAt(t, s, "echo", "/workspace/marotte-old")
	f := regAt(t, s, "foxtrot", "/workspace", p)
	got := mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	if got.Delivered != 3 {
		t.Errorf("delivered %d, want 3", got.Delivered)
	}
	for id, label := range map[uint64]string{b.ID: p, c.ID: "/workspace", f.ID: p} {
		ds := deliveriesTo(t, s, id)
		if len(ds) != 1 {
			t.Errorf("identity %d has %d deliveries, want 1", id, len(ds))
			continue
		}
		if ds[0].Kind != kindPath || ds[0].Path != label || ds[0].Thread != got.Thread {
			t.Errorf("identity %d: kind %q via %q in thread %d, want path via %q in %d", id, ds[0].Kind, ds[0].Path, ds[0].Thread, label, got.Thread)
		}
	}
	for name, id := range map[string]uint64{"the sender": a.ID, "a subpath": d.ID, "a name prefix": e.ID} {
		if ds := deliveriesTo(t, s, id); len(ds) != 0 {
			t.Errorf("%s got %d deliveries", name, len(ds))
		}
	}
	if th, _ := findThread(t, s, got.Thread); th.Kind != kindPath || th.Path != p {
		t.Errorf("thread kind %q path %q, want a path thread at %s", th.Kind, th.Path, p)
	}
}

func TestPost_followCoversItsPathAndTheThreadsUnderIt(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	const p = "/workspace/marotte/web"
	regAt(t, s, "alpha", repoGrelot)
	b := regAt(t, s, "bravo", "/workspace/marotte")
	c := regAt(t, s, "charlie", repoGrelot)
	d := regAt(t, s, "delta", repoGrelot)
	e := regAt(t, s, "echo", repoGrelot)
	f := regAt(t, s, "foxtrot", repoGrelot)
	for id, follows := range map[uint64][]string{b.ID: {p}, c.ID: {p + "/sub"}, d.ID: {"/workspace/marotte"}, e.ID: {p}, f.ID: {"/workspace/marot"}} {
		editIdentity(t, s, id, func(r *identityRow) { r.Follows = follows })
	}
	got := mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	if got.Delivered != 3 {
		t.Errorf("delivered %d, want 3", got.Delivered)
	}
	for id, via := range map[uint64]string{b.ID: p, e.ID: p, d.ID: "/workspace/marotte"} {
		ds := deliveriesTo(t, s, id)
		if len(ds) != 1 || ds[0].Path != via {
			t.Errorf("identity %d: %+v, want one delivery labelled with the followed %s", id, ds, via)
		}
	}
	for name, id := range map[string]uint64{"a subpath's follower": c.ID, "a name-prefix sibling's follower": f.ID} {
		if ds := deliveriesTo(t, s, id); len(ds) != 0 {
			t.Errorf("%s got %d deliveries", name, len(ds))
		}
	}
}

func TestPost_inAPathThreadFollowsIt(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	const p = "/workspace/marotte"
	a := regAt(t, s, "alpha", repoGrelot)
	regAt(t, s, "bravo", p)
	mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	if got := getIdentity(t, s, a.ID).Follows; !slices.Equal(got, []string{p}) {
		t.Errorf("follows %v, want [%s] once", got, p)
	}
	reply := mustPost(t, s, posting(sess("bravo"), Target{Path: p}))
	ds := deliveriesTo(t, s, a.ID)
	if len(ds) != 1 || ds[0].Message != reply.Message || ds[0].Path != p {
		t.Errorf("the poster's deliveries %+v, want the reply %d via %s", ds, reply.Message, p)
	}
}

func TestPost_aFollowWiderThanItsThreadKeepsTheFollowsUnderItAndSaysSo(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	const lib, wide = "/workspace/bolttable", "/workspace"
	a := regAt(t, s, "alpha", repoGrelot)
	regAt(t, s, "bravo", "/workspace/marotte")
	chosen := []string{lib, repoSubflux + "/internal"}
	editIdentity(t, s, a.ID, func(r *identityRow) { r.Follows = chosen })
	leaf := mustPost(t, s, posting(sess("alpha"), Target{Path: "/workspace/marotte/web"}))
	if len(leaf.Notices) != 0 {
		t.Errorf("a post at a path with no thread or follow under it: notices %q, want none", leaf.Notices)
	}
	got := mustPost(t, s, posting(sess("alpha"), Target{Path: wide}))
	if f := getIdentity(t, s, a.ID).Follows; !slices.Equal(f, []string{lib, repoSubflux + "/internal", "/workspace/marotte/web", wide}) {
		t.Errorf("follows %v, want the chosen ones and the leaf kept, then %s", f, wide)
	}
	if len(got.Notices) != 1 || !strings.Contains(got.Notices[0], "grelot unfollow '"+wide+"'") ||
		!strings.Contains(got.Notices[0], lib+", "+repoSubflux+"/internal, /workspace/marotte/web") {
		t.Errorf("notices %q, want one naming unfollow %s and the follows kept under it", got.Notices, wide)
	}
	mustUnfollow(t, s, "alpha", wide)
	if f := getIdentity(t, s, a.ID).Follows; !slices.Equal(f, []string{lib, repoSubflux + "/internal", "/workspace/marotte/web"}) {
		t.Errorf("follows after unfollowing %s: %v, want the earlier ones back", wide, f)
	}
}

func TestPost_aWideFollowNoticePrintsAParseSafeUnfollow(t *testing.T) {
	for _, tt := range []struct{ name, path, want string }{
		{"a path with a space", "/workspace/my notes", "grelot unfollow '/workspace/my notes' to stop"},
		{"a path with a quote", "/workspace/o'reilly", ": unfollow it to stop"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _, _ := openIdentityStore(t)
			a := regAt(t, s, "alpha", repoGrelot)
			editIdentity(t, s, a.ID, func(r *identityRow) { r.Follows = []string{tt.path + "/sub"} })
			got := mustPost(t, s, posting(sess("alpha"), Target{Path: tt.path}))
			if len(got.Notices) != 1 || !strings.Contains(got.Notices[0], tt.want) {
				t.Errorf("notices %q, want one holding %q", got.Notices, tt.want)
			}
		})
	}
}

func TestPost_aFollowOverALiveThreadSaysSo(t *testing.T) {
	s, clk := openShortThreadStore(t)
	const lib = "/workspace/bolttable"
	regAt(t, s, "alpha", repoGrelot)
	regAt(t, s, "bravo", repoSubflux)
	regAt(t, s, "charlie", "/workspace/marotte")
	mustPost(t, s, posting(sess("bravo"), Target{Path: lib + "/index.go"}))
	clk.advance(shortThreadTTL + time.Minute)
	if over := mustPost(t, s, posting(sess("charlie"), Target{Path: lib})); len(over.Notices) != 0 {
		t.Errorf("a post over an expired thread: notices %q, want none", over.Notices)
	}
	mustPost(t, s, posting(sess("bravo"), Target{Path: lib + "/index.go"}))
	got := mustPost(t, s, posting(sess("alpha"), Target{Path: lib}))
	if len(got.Notices) != 1 || !strings.Contains(got.Notices[0], "every thread under it") || strings.Contains(got.Notices[0], "stay") {
		t.Errorf("notices %q, want one saying the follow reaches the thread under %s, naming no kept follow", got.Notices, lib)
	}
}

func TestPost_pastThirtyTwoFollowsStillFollowsItsPath(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	a := regAt(t, s, "alpha", repoGrelot)
	full := make([]string, 32)
	for i := range full {
		full[i] = fmt.Sprintf("/workspace/thread-%02d", i)
	}
	editIdentity(t, s, a.ID, func(r *identityRow) { r.Follows = full })
	const p = "/workspace/one-more"
	got := mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	if f := getIdentity(t, s, a.ID).Follows; !slices.Equal(f, append(slices.Clone(full), p)) {
		t.Errorf("follows %v, want the 32 and %s", f, p)
	}
	if len(got.Notices) != 0 {
		t.Errorf("notices %q, want none", got.Notices)
	}
}

func TestPost_directThreadIsOneForBothDirections(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	a := regAt(t, s, "alpha")
	b := regAt(t, s, "bravo")
	c := regAt(t, s, "charlie")
	ab := mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	ba := mustPost(t, s, posting(sess("bravo"), Target{Agent: "alpha"}))
	toC := posting(sess("alpha"), Target{Agent: "charlie"})
	toC.Subject = "another subject"
	ac := mustPost(t, s, toC)
	if ab.Thread != ba.Thread {
		t.Errorf("A to B in thread %d, B to A in %d, want one thread", ab.Thread, ba.Thread)
	}
	if ac.Thread == ab.Thread {
		t.Error("A to C shares A and B's thread")
	}
	for _, p := range []Posted{ab, ba, ac} {
		if p.Delivered != 1 || len(p.Notices) != 0 {
			t.Errorf("delivered %d notices %q, want 1 and none", p.Delivered, p.Notices)
		}
	}
	for id, msg := range map[uint64]uint64{a.ID: ba.Message, b.ID: ab.Message, c.ID: ac.Message} {
		ds := deliveriesTo(t, s, id)
		if len(ds) != 1 || ds[0].Kind != kindDirect || ds[0].Message != msg {
			t.Errorf("identity %d: %+v, want the one direct message %d", id, ds, msg)
		}
	}
	if th, _ := findThread(t, s, ab.Thread); th.Kind != kindDirect {
		t.Errorf("thread kind %q, want direct", th.Kind)
	}
	p := posting(sess("alpha"), Target{Agent: "zulu"})
	_, _, err := s.Post(t.Context(), Keyed{}, &p)
	wantRefusal(t, err, KindNotFound, "", "no identity zulu")
	p = posting(sess("alpha"), Target{Agent: "alpha"})
	_, _, err = s.Post(t.Context(), Keyed{}, &p)
	wantRefusal(t, err, KindInvalid, "", "yourself")
}

func TestPost_dormantRecipientGetsItsMailAndTheSenderIsTold(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha", repoGrelot)
	rb := registration(sessB, "bravo", envA(), "/workspace/marotte")
	rb.TTL = 15 * time.Minute
	b := mustRegister(t, s, rb)
	clk.advance(20 * time.Minute)
	mustSweep(t, s)
	direct := mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	want := "@bravo is dormant since 2026-11-04 05:54 UTC; it gets this if it revives before 2026-11-07 05:54 UTC"
	if direct.Delivered != 1 || !slices.Equal(direct.Notices, []string{want}) {
		t.Errorf("delivered %d notices %q, want 1 and %q", direct.Delivered, direct.Notices, want)
	}
	mustPost(t, s, posting(sess("alpha"), Target{Path: "/workspace/marotte"}))
	mustPost(t, s, posting(sess("alpha"), Target{Announce: true}))
	var kinds []string
	for _, d := range deliveriesTo(t, s, b.ID) {
		kinds = append(kinds, d.Kind)
	}
	if !slices.Equal(kinds, []string{kindDirect, kindPath, kindAnnounce}) {
		t.Errorf("the dormant identity got %v, want direct, path and announce", kinds)
	}
	w := envA()
	got, _, err := s.Revive(t.Context(), Keyed{}, &Revival{Session: sessB, Where: &w, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got.Unread != 3 {
		t.Errorf("unread after the revive %d, want the 3 posted while dormant", got.Unread)
	}
}

func TestPost_countsRecipientsBySharedEnvironment(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	mine := Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "c")}}
	mustRegister(t, s, registration(sess("alpha"), "alpha", mine))
	for name, w := range map[string]Where{
		"same-checkout":   mine,
		"other-checkout":  {Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoSubflux, "d")}},
		"other-env":       {Machine: dmi("1"), Environment: hex32("b")},
		"other-machine":   {Machine: dmi("2"), Environment: hex32("a")},
		"unknown-machine": {},
	} {
		mustRegister(t, s, registration(sess(name), name, w))
	}
	got := mustPost(t, s, posting(sess("alpha"), Target{Announce: true}))
	if got.Delivered != 5 || got.SameEnvironment != 2 || got.Other != 3 {
		t.Errorf("delivered %d: %d same environment, %d other; want 5: 2 and 3", got.Delivered, got.SameEnvironment, got.Other)
	}
}

func TestPost_messageKeepsTheSendersWhereAsItStoodAtThePost(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	a := regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	atPost := Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoGrelot, "e")}}
	p := posting(sess("alpha"), Target{Announce: true})
	p.Fields.Paths, p.Where = []string{repoGrelot}, &atPost
	p.Subject, p.Body = "For subflux: bolttable renames Scan", "the body\nin markdown"
	got := mustPost(t, s, p)
	check := func(when string) {
		t.Helper()
		m, found := findMessage(t, s, got.Message)
		if !found {
			t.Fatalf("%s: message %d not found", when, got.Message)
		}
		if m.Where.Machine != atPost.Machine || m.Where.Environment != atPost.Environment || !slices.Equal(m.Where.Checkouts, atPost.Checkouts) {
			t.Errorf("%s: stored where %+v, want the one sent with the post %+v", when, m.Where, atPost)
		}
		if m.Sender != a.ID || m.SenderName != "alpha" || m.Host != "host-a/app" || m.OS != "linux" || m.Arch != "amd64" {
			t.Errorf("%s: sender %d %q %q %s/%s, want alpha's labels", when, m.Sender, m.SenderName, m.Host, m.OS, m.Arch)
		}
		if m.Subject != p.Subject || m.Body != p.Body || m.Kind != kindAnnounce || !m.PostedAt.Equal(testNow) {
			t.Errorf("%s: subject %q body %q kind %q posted %v", when, m.Subject, m.Body, m.Kind, m.PostedAt)
		}
	}
	check("at the post")
	moved := Where{Machine: dmi("2"), Environment: hex32("b")}
	if _, err := s.Set(t.Context(), Keyed{}, Caller{Session: sess("alpha")}, Change{TTL: time.Hour, Where: &moved}); err != nil {
		t.Fatal(err)
	}
	check("after the sender renewed from elsewhere")
}

func TestPost_threadsAndMessagesExpireFromTheirPosts(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	const p = "/workspace/marotte"
	ra := registration(sess("alpha"), "alpha", envA(), repoGrelot)
	ra.TTL = 24 * time.Hour
	mustRegister(t, s, ra)
	rb := registration(sess("bravo"), "bravo", envA(), p)
	rb.TTL = 24 * time.Hour
	b := mustRegister(t, s, rb)
	first := mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	dm := mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	clk.advance(time.Hour)
	second := mustPost(t, s, posting(sess("alpha"), Target{Path: p}))
	if second.Thread != first.Thread {
		t.Fatalf("second post in thread %d, want the first's %d", second.Thread, first.Thread)
	}
	if th, _ := findThread(t, s, first.Thread); !th.Lease.Expires.Equal(testNow.Add(73 * time.Hour)) {
		t.Errorf("path thread expires %v, want THREAD_TTL after its last post", th.Lease.Expires)
	}
	m1, _ := findMessage(t, s, first.Message)
	m2, _ := findMessage(t, s, second.Message)
	if !m1.Lease.Expires.Equal(testNow.Add(72*time.Hour)) || !m2.Lease.Expires.Equal(testNow.Add(73*time.Hour)) {
		t.Errorf("messages expire %v and %v, want THREAD_TTL after each post", m1.Lease.Expires, m2.Lease.Expires)
	}
	for _, d := range deliveriesTo(t, s, b.ID) {
		m, _ := findMessage(t, s, d.Message)
		if !d.Lease.Expires.Equal(m.Lease.Expires) {
			t.Errorf("delivery %d expires %v, want its message's %v", d.Seq, d.Lease.Expires, m.Lease.Expires)
		}
	}

	clk.set(testNow.Add(72 * time.Hour))
	mustSweep(t, s)
	if _, found := findMessage(t, s, first.Message); found {
		t.Error("the first message outlived its expiry")
	}
	if _, found := findMessage(t, s, second.Message); !found {
		t.Error("the second message went before its expiry")
	}
	if _, found := findThread(t, s, first.Thread); !found {
		t.Error("the path thread went before its last post's expiry")
	}
	if _, found := findThread(t, s, dm.Thread); found {
		t.Error("the direct thread outlived its only post")
	}
	if ds := deliveriesTo(t, s, b.ID); len(ds) != 1 || ds[0].Message != second.Message {
		t.Errorf("deliveries left %+v, want only the second post's", ds)
	}

	clk.set(testNow.Add(73 * time.Hour))
	mustSweep(t, s)
	if _, found := findThread(t, s, first.Thread); found {
		t.Error("the path thread outlived its last post")
	}
	if _, found := findMessage(t, s, second.Message); found {
		t.Error("the second message outlived its expiry")
	}
}

func TestPost_identityFieldRule(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	b := regAt(t, s, "bravo")
	for name, tc := range map[string]struct {
		edit func(*Fields)
		flag string
	}{
		"no name":        {func(f *Fields) { f.Name = "" }, "--name"},
		"no description": {func(f *Fields) { f.Description = "" }, "--desc"},
		"no paths":       {func(f *Fields) { f.Paths = nil }, "--paths"},
	} {
		t.Run(name, func(t *testing.T) {
			p := posting(sess("alpha"), Target{Announce: true})
			tc.edit(&p.Fields)
			_, _, err := s.Post(t.Context(), Keyed{}, &p)
			wantRefusal(t, err, KindInvalid, "", "required on every post", tc.flag, `"no change"`)
		})
	}
	if ds := deliveriesTo(t, s, b.ID); len(ds) != 0 {
		t.Errorf("refused posts delivered %d messages", len(ds))
	}
}

func TestPost_changesTheIdentityFieldsItCarries(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	a := regAt(t, s, "alpha", repoSubflux)
	regAt(t, s, "bravo")
	clk.advance(10 * time.Minute)
	p := posting(sess("alpha"), Target{Announce: true})
	p.Fields = Fields{Name: "alpha-two", Description: "now moving the run surface", Paths: []string{repoSubflux, repoGrelot}}
	_, _, err := s.Post(t.Context(), Keyed{}, &p)
	wantRefusal(t, err, KindInvalid, "", "where is required")

	w := Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repoGrelot, "e")}}
	p.Where = &w
	got := mustPost(t, s, p)
	if got.Identity.Name != "alpha-two" || got.Identity.Description != p.Fields.Description || !slices.Equal(got.Identity.Paths, p.Fields.Paths) {
		t.Errorf("identity %+v, want the post's fields", got.Identity)
	}
	row := getIdentity(t, s, a.ID)
	if !slices.Equal(row.Where.Checkouts, w.Checkouts) {
		t.Errorf("checkouts %+v, want the post's %+v", row.Where.Checkouts, w.Checkouts)
	}
	if !row.Lease.Expires.Equal(testNow.Add(6 * time.Hour)) {
		t.Errorf("expires %v, want the registration's: a post never moves the lease", row.Lease.Expires)
	}

	p = posting(sess("alpha"), Target{Announce: true})
	p.Fields.Name = "bravo"
	_, _, err = s.Post(t.Context(), Keyed{}, &p)
	wantRefusal(t, err, KindConflict, "", "bravo is held")
}

func TestPost_needsALiveIdentity(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	p := posting(sess("alpha"), Target{Announce: true})
	_, _, err := s.Post(t.Context(), Keyed{}, &p)
	wantRefusal(t, err, KindGone, "none", "grelot register")

	r := registration(sess("alpha"), "alpha", envA())
	r.TTL = 15 * time.Minute
	mustRegister(t, s, r)
	clk.advance(15 * time.Minute)
	_, _, err = s.Post(t.Context(), Keyed{}, &p)
	wantRefusal(t, err, KindGone, "dormant", "grelot revive")
}

func TestPost_clearsEnded(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	a := regAt(t, s, "alpha")
	if err := s.EndSession(t.Context(), sess("alpha")); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	mustPost(t, s, posting(sess("alpha"), Target{Announce: true}))
	row := getIdentity(t, s, a.ID)
	if row.Ended || !row.LastSeen.Equal(clk.now()) {
		t.Errorf("ended %v last seen %v, want cleared and now", row.Ended, row.LastSeen)
	}
	if !row.Lease.Expires.Equal(testNow.Add(time.Hour)) {
		t.Errorf("expires %v, want the SessionEnd cap kept: only a renewal moves it", row.Lease.Expires)
	}
}

func TestPost_messageLimits(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	for name, tc := range map[string]struct {
		edit func(*Posting)
		want string
	}{
		"no target":          {func(p *Posting) { p.To = Target{} }, "exactly one"},
		"two targets":        {func(p *Posting) { p.To.Agent = "bravo" }, "exactly one"},
		"no subject":         {func(p *Posting) { p.Subject = "" }, "subject"},
		"121-rune subject":   {func(p *Posting) { p.Subject = strings.Repeat("é", 121) }, "1 to 120"},
		"two-line subject":   {func(p *Posting) { p.Subject = "one\ntwo" }, "one line"},
		"body over 64 KiB":   {func(p *Posting) { p.Body = strings.Repeat("a", 64<<10+1) }, "64 KiB"},
		"body not UTF-8":     {func(p *Posting) { p.Body = "\xff" }, "UTF-8"},
		"relative path":      {func(p *Posting) { p.To = Target{Path: "workspace/x"} }, "canonical"},
		"path trailing /":    {func(p *Posting) { p.To = Target{Path: "/workspace/x/"} }, "canonical"},
		"path with dot-dots": {func(p *Posting) { p.To = Target{Path: "/workspace/../x"} }, "canonical"},
	} {
		t.Run(name, func(t *testing.T) {
			p := posting(sess("alpha"), Target{Announce: true})
			tc.edit(&p)
			_, _, err := s.Post(t.Context(), Keyed{}, &p)
			wantRefusal(t, err, KindInvalid, "", tc.want)
		})
	}
	p := posting(sess("alpha"), Target{Announce: true})
	p.Subject, p.Body = strings.Repeat("é", 120), strings.Repeat("a", 64<<10)
	mustPost(t, s, p)
}

func TestPost_seqIsPerRecipientAfterItsEvents(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	// Paths apart, so no join card takes a seq.
	regAt(t, s, "alpha", "/workspace/a")
	b := regAt(t, s, "bravo", "/workspace/b")
	c := regAt(t, s, "charlie", "/workspace/c")
	deliverEvents(t, s, b.ID, 1)
	mustPost(t, s, posting(sess("alpha"), Target{Announce: true}))
	seqs := func(id uint64) []uint64 {
		var out []uint64
		for _, d := range deliveriesTo(t, s, id) {
			out = append(out, d.Seq)
		}
		return out
	}
	if got := seqs(b.ID); !slices.Equal(got, []uint64{1, 2}) {
		t.Errorf("bravo's seqs %v, want [1 2]", got)
	}
	if got := seqs(c.ID); !slices.Equal(got, []uint64{1}) {
		t.Errorf("charlie's seqs %v, want [1]", got)
	}
	if got := getIdentity(t, s, b.ID).Seq; got != 2 {
		t.Errorf("bravo's highest seq %d, want 2", got)
	}
	if ds := deliveriesTo(t, s, b.ID); ds[1].Name != "bravo" {
		t.Errorf("recipient name at fan-out %q, want bravo", ds[1].Name)
	}
}
