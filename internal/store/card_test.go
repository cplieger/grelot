//go:build !windows

package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

// cardsAbout lists the join and left cards reader holds about identity
// about, in seq order.
func cardsAbout(t *testing.T, s *Store, reader, about uint64) []deliveryRow {
	t.Helper()
	var out []deliveryRow
	for _, d := range allDeliveriesTo(t, s, reader) {
		if (d.Kind == kindJoin || d.Kind == kindLeft) && d.Sender == about {
			out = append(out, d)
		}
	}
	return out
}

func cardPaths(ds []deliveryRow) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Kind+" "+d.Path)
	}
	return out
}

func TestJoinCard_onRegisterReachesLiveIdentitiesOverlappingThePath(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	const p = "/workspace/marotte"
	readers := map[string]uint64{}
	for name, path := range map[string]string{"below": p + "/web", "above": "/workspace", "equal": p} {
		readers[name] = regAt(t, s, name, path).ID
	}
	sibling := regAt(t, s, "sibling", "/workspace/marotte-old")
	other := regAt(t, s, "other", repoSubflux)
	rd := registration(sess("dormant"), "dormant", envA(), p)
	rd.TTL = 15 * time.Minute
	dormant := mustRegister(t, s, rd)
	clk.advance(15 * time.Minute)
	mustSweep(t, s)

	elsewhere := Where{Machine: dmi("2"), Environment: hex32("b")}
	r := registration(sess("newbie"), "newbie", elsewhere, p)
	r.Host, r.Fields.Description = "laptop/wsl", "taking all of marotte"
	newbie := mustRegister(t, s, r)

	for name, id := range readers {
		ds := cardsAbout(t, s, id, newbie.ID)
		if len(ds) != 1 {
			t.Errorf("%s holds %d cards about newbie, want 1", name, len(ds))
			continue
		}
		d := ds[0]
		if d.Kind != kindJoin || d.Path != p || d.Name != name {
			t.Errorf("%s's card: kind %q via %q named %q, want a join card via %s named %s", name, d.Kind, d.Path, d.Name, p, name)
		}
		if want := clk.now().Add(72 * time.Hour); !d.Lease.Expires.Equal(want) {
			t.Errorf("%s's card expires %v, want THREAD_TTL on: %v", name, d.Lease.Expires, want)
		}
		th, _ := findThread(t, s, d.Thread)
		if th.Kind != kindCards || !th.Lease.Expires.IsZero() {
			t.Errorf("card thread kind %q expires %v, want the cards thread, never expiring", th.Kind, th.Lease.Expires)
		}
		m, found := findMessage(t, s, d.Message)
		if !found {
			t.Fatalf("%s's card message %d is missing", name, d.Message)
		}
		if m.Kind != kindJoin || m.Path != p || m.Sender != newbie.ID || m.SenderName != "newbie" || m.Host != "laptop/wsl" {
			t.Errorf("card message %+v, want a join card at %s about newbie on laptop/wsl", m, p)
		}
		if m.Where.Environment != elsewhere.Environment || m.Where.Machine != elsewhere.Machine {
			t.Errorf("card where %+v, want newbie's %+v", m.Where, elsewhere)
		}
		for _, part := range []string{"provider kiro-cli", "paths: " + p, "description: taking all of marotte"} {
			if !strings.Contains(m.Subject+"\n"+m.Body, part) {
				t.Errorf("card %q / %q lacks %q", m.Subject, m.Body, part)
			}
		}
		if !strings.Contains(m.Subject, "newbie") {
			t.Errorf("card subject %q does not name newbie", m.Subject)
		}
	}
	for name, id := range map[string]uint64{"a name-prefix sibling": sibling.ID, "an unrelated path": other.ID, "a dormant identity": dormant.ID, "the newcomer": newbie.ID} {
		if ds := cardsAbout(t, s, id, newbie.ID); len(ds) != 0 {
			t.Errorf("%s got %d cards about newbie", name, len(ds))
		}
	}
}

func TestJoinCard_onePerAddedPathAndNoneOnARepeatedRegister(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	reader := regAt(t, s, "reader", "/workspace")
	r := registration(sess("newbie"), "newbie", envA(), repoSubflux, repoGrelot)
	newbie := mustRegister(t, s, r)
	mustRegister(t, s, r)
	want := []string{"join " + repoSubflux, "join " + repoGrelot}
	if got := cardPaths(cardsAbout(t, s, reader.ID, newbie.ID)); !slices.Equal(got, want) {
		t.Errorf("cards %v, want %v", got, want)
	}
}

func TestJoinCard_onlyForThePathsASetOrAPostAdds(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	reader := regAt(t, s, "reader", "/workspace")
	newbie := regAt(t, s, "newbie", repoSubflux)
	w := envA()
	if _, err := s.Set(t.Context(), Keyed{}, Caller{Session: sess("newbie")}, Change{Where: &w, Fields: Fields{Paths: []string{repoSubflux, repoGrelot}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(t.Context(), Keyed{}, Caller{Session: sess("newbie")}, Change{Fields: Fields{Description: "a new description"}}); err != nil {
		t.Fatal(err)
	}
	p := posting(sess("newbie"), Target{Announce: true})
	p.Where, p.Fields.Paths = &w, []string{repoSubflux, repoGrelot, "/workspace/sse"}
	mustPost(t, s, p)
	mustPost(t, s, posting(sess("newbie"), Target{Announce: true}))
	want := []string{"join " + repoSubflux, "join " + repoGrelot, "join /workspace/sse"}
	if got := cardPaths(cardsAbout(t, s, reader.ID, newbie.ID)); !slices.Equal(got, want) {
		t.Errorf("cards %v, want %v", got, want)
	}
}

func TestJoinCard_aTakeOverCardsOnlyThePathsItAdds(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	reader := regAt(t, s, "reader", "/workspace")
	holder := mustRegister(t, s, registration(sessA, "subflux-dev", envA(), repoSubflux))
	r := registration(sessB, "subflux-dev", envA(), repoSubflux, repoGrelot)
	r.TakeOver = true
	mustRegister(t, s, r)
	want := []string{"join " + repoSubflux, "join " + repoGrelot}
	if got := cardPaths(cardsAbout(t, s, reader.ID, holder.ID)); !slices.Equal(got, want) {
		t.Errorf("cards %v, want %v", got, want)
	}
}

func TestLeftCard_onExpiryThenAJoinCardOnRevive(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	reader := regAt(t, s, "reader", "/workspace")
	leaver := expireToDormant(t, s, clk, "subflux-dev")
	afterExpiry := []string{"join " + repoSubflux, "join " + repoGrelot, "left " + repoSubflux, "left " + repoGrelot}
	ds := cardsAbout(t, s, reader.ID, leaver.ID)
	if got := cardPaths(ds); !slices.Equal(got, afterExpiry) {
		t.Fatalf("cards %v, want %v", got, afterExpiry)
	}
	if m, _ := findMessage(t, s, ds[2].Message); m.Kind != kindLeft || !strings.Contains(m.Subject, "subflux-dev") || !strings.Contains(m.Subject, "left") {
		t.Errorf("left card message kind %q subject %q, want a left card saying subflux-dev left", m.Kind, m.Subject)
	}
	mustRevive(t, s, revival(sessA, "", envA()))
	want := append(afterExpiry, "join "+repoSubflux, "join "+repoGrelot)
	if got := cardPaths(cardsAbout(t, s, reader.ID, leaver.ID)); !slices.Equal(got, want) {
		t.Errorf("cards after the revive %v, want %v", got, want)
	}
}

func TestLeftCard_noneAtThePurge(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	reader := regAt(t, s, "reader", "/workspace")
	leaver := expireToDormant(t, s, clk, "subflux-dev")
	before := len(cardsAbout(t, s, reader.ID, leaver.ID))
	editIdentity(t, s, leaver.ID, func(r *identityRow) { r.Lease.Expires = clk.now().Add(time.Minute) })
	clk.advance(time.Minute)
	mustSweep(t, s)
	if got := len(cardsAbout(t, s, reader.ID, leaver.ID)); got != before {
		t.Errorf("%d cards after the purge, want the %d from before it", got, before)
	}
}

// A left card can be due inside a request whose own identity is the card's
// reader; the reader's later deliveries must still take fresh seqs.
func TestLeftCard_writtenEagerlyToTheRequestsOwnIdentity(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	rl := registration(sess("leaver"), "leaver", envA(), repoSubflux)
	rl.TTL = 15 * time.Minute
	leaver := mustRegister(t, s, rl)
	reader := regAt(t, s, "reader", "/workspace")
	regAt(t, s, "poster", "/srv/poster")
	clk.advance(15 * time.Minute)
	// reader's direct post meets leaver past its expiry, unswept.
	got := mustPost(t, s, posting(sess("reader"), Target{Agent: "leaver"}))
	if len(got.Notices) != 1 || !strings.Contains(got.Notices[0], "leaver is dormant") {
		t.Errorf("notices %q, want the dormancy notice", got.Notices)
	}
	ds := cardsAbout(t, s, reader.ID, leaver.ID)
	if len(ds) != 1 || ds[0].Kind != kindLeft || ds[0].Seq != 1 {
		t.Fatalf("reader's cards about leaver %+v, want one left card at seq 1", ds)
	}
	news := mustPost(t, s, posting(sess("poster"), Target{Path: "/workspace"}))
	all := allDeliveriesTo(t, s, reader.ID)
	if len(all) != 2 || all[0].Kind != kindLeft || all[1].Message != news.Message || all[1].Seq != 2 {
		t.Errorf("reader's deliveries %+v, want the left card at 1, then the post at 2", all)
	}
	if seq := getIdentity(t, s, reader.ID).Seq; seq != 2 {
		t.Errorf("reader's highest seq %d, want 2", seq)
	}
}

func TestRetire_deletesTheIdentityAndWritesLeftCards(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	reader := regAt(t, s, "reader", "/workspace")
	leaver := regAt(t, s, "leaver", repoSubflux, repoGrelot)
	mustPost(t, s, posting(sess("reader"), Target{Agent: "leaver"}))
	if _, _, err := s.Retire(t.Context(), Keyed{}, Caller{Session: sess("leaver")}); err != nil {
		t.Fatal(err)
	}
	want := []string{"join " + repoSubflux, "join " + repoGrelot, "left " + repoSubflux, "left " + repoGrelot}
	if got := cardPaths(cardsAbout(t, s, reader.ID, leaver.ID)); !slices.Equal(got, want) {
		t.Errorf("reader's cards about leaver %v, want %v", got, want)
	}
	var found bool
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		var err error
		_, found, err = s.t.identities.table.Get(r, leaver.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("the retired identity's row is still there")
	}
	if ds := allDeliveriesTo(t, s, leaver.ID); len(ds) != 1 {
		t.Errorf("the retired identity has %d delivery rows, want its 1 kept until its expiry", len(ds))
	}
	_, err := s.Seen(t.Context(), Caller{Session: sess("leaver")})
	wantRefusal(t, err, KindGone, "none")
	again := regAt(t, s, "leaver", repoSubflux)
	if again.ID == leaver.ID {
		t.Errorf("the freed name registered under the retired id %d", again.ID)
	}
}

func TestRetire_needsALiveIdentity(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	_, _, err := s.Retire(t.Context(), Keyed{}, Caller{Session: sess("nobody")})
	wantRefusal(t, err, KindGone, "none")
	expireToDormant(t, s, clk, "subflux-dev")
	_, _, err = s.Retire(t.Context(), Keyed{}, Caller{Session: sessA})
	wantRefusal(t, err, KindGone, "dormant", "grelot revive --ttl <d>")
}

func TestCards_neverCreateOrMoveAPathThread(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "reader", "/workspace")
	thread := mustPost(t, s, posting(sess("reader"), Target{Path: repoSubflux}))
	before, _ := findThread(t, s, thread.Thread)
	clk.advance(time.Hour)
	regAt(t, s, "newbie", repoSubflux, "/workspace/sse")
	if after, _ := findThread(t, s, thread.Thread); !after.Lease.Expires.Equal(before.Lease.Expires) {
		t.Errorf("the path thread now expires %v, want %v: only a post moves it", after.Lease.Expires, before.Lease.Expires)
	}
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		_, _, found, err := s.t.threadKey.Lookup(r, []byte("/workspace/sse"))
		if found {
			t.Error("a card created a path thread at /workspace/sse")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
