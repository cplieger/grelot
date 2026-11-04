package store

import (
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

const (
	pathMarotte = "/workspace/marotte"
	pathWeb     = "/workspace/marotte/web"
	pathAPI     = "/workspace/marotte/api"
)

func reserving(name, path string, ttl time.Duration) Reserving {
	return Reserving{Caller: Caller{Session: sess(name)}, Path: path, Reason: "reason of " + name, TTL: ttl}
}

func mustReserve(t *testing.T, s *Store, r Reserving) Reserved {
	t.Helper()
	got, err := s.Reserve(t.Context(), &r)
	if err != nil {
		t.Fatalf("Reserve(%s, %s): %v", r.Caller.Session, r.Path, err)
	}
	return got
}

func mustQueue(t *testing.T, s *Store, r Reserving) Reserved {
	t.Helper()
	r.Queue = true
	got := mustReserve(t, s, r)
	if got.Reservation.State != reservationQueued {
		t.Fatalf("Reserve(%s, %s, queue) is %s, want queued", r.Caller.Session, r.Path, got.Reservation.State)
	}
	return got
}

func mustRelease(t *testing.T, s *Store, name string, id uint64) {
	t.Helper()
	if _, err := s.Release(t.Context(), Caller{Session: sess(name)}, id); err != nil {
		t.Fatalf("Release(%s, %d): %v", name, id, err)
	}
}

// getReservation reads reservation row id.
func getReservation(t *testing.T, s *Store, id uint64) (reservationRow, bool) {
	t.Helper()
	var row reservationRow
	var found bool
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		var err error
		row, found, err = s.t.resv.table.Get(r, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return row, found
}

// wantState fails unless reservation id is there in state.
func wantState(t *testing.T, s *Store, id uint64, state string) reservationRow {
	t.Helper()
	row, found := getReservation(t, s, id)
	if !found || row.State != state {
		t.Fatalf("reservation %d found %v state %q, want %s", id, found, row.State, state)
	}
	return row
}

// eventsOf lists identity id's deliveries of kind, in seq order.
func eventsOf(t *testing.T, s *Store, id uint64, kind string) []deliveryRow {
	t.Helper()
	var out []deliveryRow
	for _, d := range allDeliveriesTo(t, s, id) {
		if d.Kind == kind {
			out = append(out, d)
		}
	}
	return out
}

func openReservationStore(t *testing.T, maxTTL time.Duration) (*Store, *clock) {
	t.Helper()
	clk := &clock{t: testNow}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), Options{
		Logger: slog.New(newLogSink()), Now: clk.now, ReservationMaxTTL: maxTTL,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

func TestReserve_warningLeadScalesWithTheTTL(t *testing.T) {
	for _, tc := range []struct {
		ttl, lead time.Duration
	}{{8 * time.Hour, 5 * time.Minute}, {2 * time.Minute, time.Minute}, {time.Minute, 30 * time.Second}} {
		t.Run(tc.ttl.String(), func(t *testing.T) {
			s, _, _ := openIdentityStore(t)
			regAt(t, s, "holder")
			got := mustReserve(t, s, reserving("holder", pathMarotte, tc.ttl))
			row := wantState(t, s, got.Reservation.ID, reservationHeld)
			if want := testNow.Add(tc.ttl); !row.Lease.Expires.Equal(want) || !got.Reservation.Expires.Equal(want) {
				t.Errorf("expires %v (answered %v), want %v", row.Lease.Expires, got.Reservation.Expires, want)
			}
			if lead := row.Lease.Expires.Sub(row.Lease.WarnAt); lead != tc.lead {
				t.Errorf("warning lead %v, want %v", lead, tc.lead)
			}
		})
	}
}

func TestReserve_ttlBounds(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "holder")
	for _, ttl := range []time.Duration{0, 59 * time.Second, 8*time.Hour + time.Second} {
		_, err := s.Reserve(t.Context(), new(reserving("holder", pathMarotte, ttl)))
		wantRefusal(t, err, KindInvalid, "", "--ttl", "1m", "8h")
	}
	mustReserve(t, s, reserving("holder", pathMarotte, 8*time.Hour))
	queued := reserving("holder", pathWeb, 0)
	queued.Queue = true
	_, err := s.Reserve(t.Context(), &queued)
	wantRefusal(t, err, KindInvalid, "", "a ttl is required")

	short, _ := openReservationStore(t, 2*time.Hour)
	regAt(t, short, "holder")
	_, err = short.Reserve(t.Context(), new(reserving("holder", pathMarotte, 3*time.Hour)))
	wantRefusal(t, err, KindInvalid, "", "1m to 2h")
}

func TestReserve_reasonAndPathChecked(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "holder")
	for name, edit := range map[string]func(*Reserving){
		"no reason":       func(r *Reserving) { r.Reason = "" },
		"two-line reason": func(r *Reserving) { r.Reason = "a\nb" },
		"long reason":     func(r *Reserving) { r.Reason = strings.Repeat("é", 281) },
	} {
		t.Run(name, func(t *testing.T) {
			r := reserving("holder", pathMarotte, time.Hour)
			edit(&r)
			_, err := s.Reserve(t.Context(), &r)
			wantRefusal(t, err, KindInvalid, "", "--reason")
		})
	}
	_, err := s.Reserve(t.Context(), new(reserving("holder", "workspace/marotte", time.Hour)))
	wantRefusal(t, err, KindInvalid, "", "canonical")
	_, err = s.Reserve(t.Context(), new(reserving("nobody", pathMarotte, time.Hour)))
	wantRefusal(t, err, KindGone, "none")
}

func TestReserve_conflictsWithAnOverlappingGrantEitherWay(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "holder")
	regAt(t, s, "other")
	mustReserve(t, s, reserving("holder", pathMarotte, time.Hour))
	for _, p := range []string{pathMarotte, pathWeb, "/workspace", "/"} {
		_, err := s.Reserve(t.Context(), new(reserving("other", p, time.Hour)))
		wantRefusal(t, err, KindConflict, "reserved", pathMarotte)
	}
	for _, p := range []string{"/workspace/marotte-old", repoSubflux} {
		if got := mustReserve(t, s, reserving("other", p, time.Hour)); got.Reservation.State != reservationHeld {
			t.Errorf("%s is %s, want held: it overlaps nothing", p, got.Reservation.State)
		}
	}
}

func TestReserve_ownGrantsNeverConflict(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "holder")
	for _, p := range []string{pathMarotte, pathWeb, pathMarotte, "/workspace"} {
		if got := mustReserve(t, s, reserving("holder", p, time.Hour)); got.Reservation.State != reservationHeld {
			t.Errorf("own %s is %s, want held", p, got.Reservation.State)
		}
	}
}

func TestReserve_conflictNamesTheHolder(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	r := registration(sess("holder"), "holder", Where{Machine: dmi("2"), Environment: hex32("b")})
	r.Host = "laptop/wsl"
	mustRegister(t, s, r)
	regAt(t, s, "other")
	res := reserving("holder", pathMarotte, 2*time.Hour)
	res.Reason = "rebasing the web tree"
	mustReserve(t, s, res)
	_, err := s.Reserve(t.Context(), new(reserving("other", pathWeb, time.Hour)))
	wantRefusal(t, err, KindConflict, "reserved",
		"holder", "laptop/wsl", "other machine", "rebasing the web tree", "2026-11-04 07:39 UTC", "--queue")
}

func TestReserve_newcomerSubpathWaitsBehindAQueuedParent(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	for _, n := range []string{"web-holder", "parent", "newcomer"} {
		regAt(t, s, n)
	}
	web := mustReserve(t, s, reserving("web-holder", pathWeb, time.Hour))
	parent := mustQueue(t, s, reserving("parent", pathMarotte, time.Hour))
	if parent.Behind == nil || parent.Behind.ID != web.Reservation.ID || parent.Behind.Holder != "web-holder" {
		t.Errorf("the parent waits behind %+v, want web-holder's grant", parent.Behind)
	}
	_, err := s.Reserve(t.Context(), new(reserving("newcomer", pathAPI, time.Hour)))
	wantRefusal(t, err, KindConflict, "queued", pathMarotte, "parent", "queued first", "reason of parent", "--queue")
	got := mustQueue(t, s, reserving("newcomer", pathAPI, time.Hour))
	if got.Behind == nil || got.Behind.ID != parent.Reservation.ID {
		t.Errorf("the newcomer waits behind %+v, want the parent's queue entry", got.Behind)
	}
	// A queued request holds back its own identity's newer ones too.
	_, err = s.Reserve(t.Context(), new(reserving("parent", pathAPI, time.Hour)))
	wantRefusal(t, err, KindConflict, "queued", "you")
}

func TestRelease_handsOffInQueueOrder(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "first")
	secondID, thirdID := regAt(t, s, "second").ID, regAt(t, s, "third").ID
	held := mustReserve(t, s, reserving("first", pathMarotte, time.Hour))
	second := mustQueue(t, s, reserving("second", pathMarotte, time.Hour))
	third := mustQueue(t, s, reserving("third", pathMarotte, time.Hour))

	mustRelease(t, s, "first", held.Reservation.ID)
	if _, found := getReservation(t, s, held.Reservation.ID); found {
		t.Error("the released grant is still there")
	}
	wantState(t, s, second.Reservation.ID, reservationHeld)
	wantState(t, s, third.Reservation.ID, reservationQueued)

	mustRelease(t, s, "second", second.Reservation.ID)
	wantState(t, s, third.Reservation.ID, reservationHeld)
	for name, id := range map[string]uint64{"second": secondID, "third": thirdID} {
		if n := len(eventsOf(t, s, id, kindReservationGranted)); n != 1 {
			t.Errorf("%s got %d reservation_granted, want 1", name, n)
		}
	}
}

func TestRelease_grantsEveryRequestNoLongerHeldBack(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	for _, n := range []string{"parent", "web", "api", "late"} {
		regAt(t, s, n)
	}
	parent := mustReserve(t, s, reserving("parent", pathMarotte, time.Hour))
	web := mustQueue(t, s, reserving("web", pathWeb, time.Hour))
	api := mustQueue(t, s, reserving("api", pathAPI, time.Hour))
	late := mustQueue(t, s, reserving("late", pathMarotte, time.Hour))

	mustRelease(t, s, "parent", parent.Reservation.ID)
	wantState(t, s, web.Reservation.ID, reservationHeld)
	wantState(t, s, api.Reservation.ID, reservationHeld)
	wantState(t, s, late.Reservation.ID, reservationQueued)
}

func TestRelease_cancellingTheOnlyBlockerGrantsTheRequestBehindIt(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	for _, n := range []string{"web", "parent", "api"} {
		regAt(t, s, n)
	}
	web := mustReserve(t, s, reserving("web", pathWeb, time.Hour))
	parent := mustQueue(t, s, reserving("parent", pathMarotte, time.Hour))
	api := mustQueue(t, s, reserving("api", pathAPI, time.Hour))

	got, err := s.Release(t.Context(), Caller{Session: sess("parent")}, parent.Reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != reservationQueued || got.ID != parent.Reservation.ID {
		t.Errorf("release answered %+v, want the cancelled queue entry", got)
	}
	if _, found := getReservation(t, s, parent.Reservation.ID); found {
		t.Error("the cancelled queue entry is still there")
	}
	wantState(t, s, api.Reservation.ID, reservationHeld)
	wantState(t, s, web.Reservation.ID, reservationHeld)
}

func TestHandOff_expiryCountsTheQueuedTTLFromTheGrant(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "holder")
	waiter := regAt(t, s, "waiter")
	held := mustReserve(t, s, reserving("holder", pathMarotte, 4*time.Hour))
	queued := mustQueue(t, s, reserving("waiter", pathMarotte, 2*time.Hour))
	if !queued.Reservation.Expires.IsZero() || queued.Reservation.TTL != 2*time.Hour {
		t.Errorf("queued entry expires %v ttl %v, want no expiry and the 2h duration", queued.Reservation.Expires, queued.Reservation.TTL)
	}
	clk.advance(3 * time.Hour)
	mustRelease(t, s, "holder", held.Reservation.ID)

	row := wantState(t, s, queued.Reservation.ID, reservationHeld)
	want := testNow.Add(5 * time.Hour)
	if !row.Lease.Expires.Equal(want) || !row.Since.Equal(testNow.Add(3*time.Hour)) {
		t.Errorf("granted at %v until %v, want %v until %v", row.Since, row.Lease.Expires, testNow.Add(3*time.Hour), want)
	}
	if lead := row.Lease.Expires.Sub(row.Lease.WarnAt); lead != 5*time.Minute {
		t.Errorf("warning lead %v, want 5m", lead)
	}
	ev := eventsOf(t, s, waiter.ID, kindReservationGranted)
	if len(ev) != 1 || !strings.Contains(ev[0].Text, pathMarotte) || !strings.Contains(ev[0].Text, "2026-11-04 10:39 UTC") {
		t.Errorf("reservation_granted %+v, want one naming %s and its expiry", ev, pathMarotte)
	}
}

func TestRelease_aStaleIDCannotTouchALaterGrant(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "holder")
	regAt(t, s, "other")
	old := mustReserve(t, s, reserving("holder", pathMarotte, time.Hour))
	mustRelease(t, s, "holder", old.Reservation.ID)
	later := mustReserve(t, s, reserving("holder", pathMarotte, time.Hour))
	if later.Reservation.ID == old.Reservation.ID {
		t.Fatal("the later grant reused the old id")
	}
	_, err := s.Release(t.Context(), Caller{Session: sess("holder")}, old.Reservation.ID)
	wantRefusal(t, err, KindNotFound, "", "no reservation")
	_, err = s.RenewReservation(t.Context(), Caller{Session: sess("holder")}, old.Reservation.ID, 8*time.Hour)
	wantRefusal(t, err, KindNotFound, "", "no reservation")
	_, err = s.Release(t.Context(), Caller{Session: sess("other")}, later.Reservation.ID)
	wantRefusal(t, err, KindNotFound, "", "no reservation")
	_, err = s.RenewReservation(t.Context(), Caller{Session: sess("other")}, later.Reservation.ID, 8*time.Hour)
	wantRefusal(t, err, KindNotFound, "", "no reservation")
	row := wantState(t, s, later.Reservation.ID, reservationHeld)
	if !row.Lease.Expires.Equal(testNow.Add(time.Hour)) {
		t.Errorf("the later grant expires %v, want its own hour", row.Lease.Expires)
	}
}

func TestRenewReservation_refusesAQueueEntry(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "holder")
	regAt(t, s, "waiter")
	mustReserve(t, s, reserving("holder", pathMarotte, time.Hour))
	q := mustQueue(t, s, reserving("waiter", pathMarotte, time.Hour))
	_, err := s.RenewReservation(t.Context(), Caller{Session: sess("waiter")}, q.Reservation.ID, 2*time.Hour)
	wantRefusal(t, err, KindInvalid, "", "queued")
}

func TestReservationWarning_onceAndRenewRearms(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	holder := regAt(t, s, "holder")
	res := mustReserve(t, s, reserving("holder", pathMarotte, time.Hour))
	id := res.Reservation.ID
	clk.advance(54 * time.Minute)
	mustSweep(t, s)
	if n := len(eventsOf(t, s, holder.ID, kindReservationExpiring)); n != 0 {
		t.Fatalf("%d warnings 6 min before expiry, want none until 5 min", n)
	}
	clk.advance(time.Minute)
	mustSweep(t, s)
	mustSweep(t, s)
	ev := eventsOf(t, s, holder.ID, kindReservationExpiring)
	if len(ev) != 1 {
		t.Fatalf("%d warnings at 5 min left, want exactly one", len(ev))
	}
	for _, part := range []string{pathMarotte, "5 min", "grelot renew"} {
		if !strings.Contains(ev[0].Text, part) {
			t.Errorf("warning %q lacks %q", ev[0].Text, part)
		}
	}
	got, err := s.RenewReservation(t.Context(), Caller{Session: sess("holder")}, id, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if want := clk.now().Add(time.Hour); !got.Expires.Equal(want) {
		t.Errorf("renewed to %v, want %v", got.Expires, want)
	}
	clk.advance(55 * time.Minute)
	mustSweep(t, s)
	if n := len(eventsOf(t, s, holder.ID, kindReservationExpiring)); n != 2 {
		t.Errorf("%d warnings after the renewed lease reached its lead, want 2", n)
	}
	if n := len(eventsOf(t, s, holder.ID, kindReservationExpired)); n != 0 {
		t.Errorf("%d expiries before the renewed expiry", n)
	}
}

func TestReservationExpiry_freesThePathAndHandsItOff(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	holder := regAt(t, s, "holder")
	waiter := regAt(t, s, "waiter")
	res := mustReserve(t, s, reserving("holder", pathMarotte, 10*time.Minute))
	q := mustQueue(t, s, reserving("waiter", pathMarotte, time.Hour))
	clk.advance(5 * time.Minute)
	mustSweep(t, s)
	clk.advance(5 * time.Minute)
	mustSweep(t, s)

	if _, found := getReservation(t, s, res.Reservation.ID); found {
		t.Error("the expired grant is still there")
	}
	ev := eventsOf(t, s, holder.ID, kindReservationExpired)
	if len(ev) != 1 || !strings.Contains(ev[0].Text, pathMarotte) || strings.Contains(ev[0].Text, "down") {
		t.Errorf("reservation_expired %+v, want one naming %s, after its warning", ev, pathMarotte)
	}
	row := wantState(t, s, q.Reservation.ID, reservationHeld)
	if want := clk.now().Add(time.Hour); !row.Lease.Expires.Equal(want) {
		t.Errorf("handed-off grant expires %v, want %v", row.Lease.Expires, want)
	}
	if n := len(eventsOf(t, s, waiter.ID, kindReservationGranted)); n != 1 {
		t.Errorf("%d reservation_granted for the waiter, want 1", n)
	}
}

func TestReserve_racingAnExpiredGrantNeverBeatsTheQueue(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "holder")
	waiter := regAt(t, s, "waiter")
	regAt(t, s, "racer")
	mustReserve(t, s, reserving("holder", pathMarotte, 10*time.Minute))
	q := mustQueue(t, s, reserving("waiter", pathMarotte, time.Hour))
	clk.advance(10 * time.Minute)

	_, err := s.Reserve(t.Context(), new(reserving("racer", pathWeb, time.Hour)))
	wantRefusal(t, err, KindConflict, "reserved", "waiter")
	wantState(t, s, q.Reservation.ID, reservationHeld)
	if n := len(eventsOf(t, s, waiter.ID, kindReservationGranted)); n != 1 {
		t.Errorf("%d reservation_granted for the waiter, want 1", n)
	}
}

func TestReservationExpiry_whileTheServerWasDown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	clk := &clock{t: testNow}
	open := func() *Store {
		s, err := Open(path, Options{Logger: slog.New(newLogSink()), Now: clk.now})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return s
	}
	s := open()
	holder := regAt(t, s, "holder")
	res := mustReserve(t, s, reserving("holder", pathMarotte, time.Hour))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	clk.advance(2 * time.Hour)
	s = open()
	t.Cleanup(func() { _ = s.Close() })
	mustSweep(t, s)

	if _, found := getReservation(t, s, res.Reservation.ID); found {
		t.Error("the grant survived its expiry across the restart")
	}
	if n := len(eventsOf(t, s, holder.ID, kindReservationExpiring)); n != 0 {
		t.Errorf("%d warnings for a reservation that expired while the server was down, want none", n)
	}
	ev := eventsOf(t, s, holder.ID, kindReservationExpired)
	if len(ev) != 1 || !strings.Contains(ev[0].Text, "server was down") || !strings.Contains(ev[0].Text, "no warning") {
		t.Errorf("reservation_expired %+v, want one saying no warning went out while the server was down", ev)
	}
}

func TestInbox_showsAReservationEventsText(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "holder")
	regAt(t, s, "waiter")
	res := mustReserve(t, s, reserving("holder", pathMarotte, time.Hour))
	q := mustQueue(t, s, reserving("waiter", pathMarotte, time.Hour))
	mustRelease(t, s, "holder", res.Reservation.ID)

	page := mustInbox(t, s, sess("waiter"), 0)
	var granted []Entry
	for _, e := range page.Entries {
		if e.Kind == kindReservationGranted {
			granted = append(granted, e)
		}
	}
	if len(granted) != 1 {
		t.Fatalf("inbox %+v, want one reservation_granted", page.Entries)
	}
	e := granted[0]
	if !e.Wake || !strings.Contains(e.Text, pathMarotte) || !strings.Contains(e.Text, "release") || e.From != "" || e.Relation != "" {
		t.Errorf("entry %+v, want a waking server event whose text names the path and the release", e)
	}
	if !strings.Contains(e.Text, "reservation "+strconv.FormatUint(q.Reservation.ID, 10)) {
		t.Errorf("entry text %q does not name reservation %d", e.Text, q.Reservation.ID)
	}
	if page.Unread.Reservation != 1 || page.Unread.Wake() != 1 {
		t.Errorf("unread %+v, want one waking reservation event", page.Unread)
	}
}

func TestPost_reachesAHolderOnAPathItDidNotList(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	holder := regAt(t, s, "holder", repoSubflux)
	queued := regAt(t, s, "queued", repoSubflux)
	regAt(t, s, "poster", repoGrelot)
	res := mustReserve(t, s, reserving("holder", pathMarotte, time.Hour))
	mustQueue(t, s, reserving("queued", pathMarotte, time.Hour))

	mustPost(t, s, posting(sess("poster"), Target{Path: pathWeb}))
	ds := deliveriesTo(t, s, holder.ID)
	if len(ds) != 1 || ds[0].Kind != kindPath || ds[0].Path != pathMarotte {
		t.Fatalf("holder's deliveries %+v, want one path delivery matched by %s", ds, pathMarotte)
	}
	if ds := deliveriesTo(t, s, queued.ID); len(ds) != 0 {
		t.Errorf("a queue entry subscribed its identity: %+v", ds)
	}
	mustRelease(t, s, "holder", res.Reservation.ID)
	// The release handed the path to the queued identity, which hears the
	// next post, while the releaser no longer does.
	mustPost(t, s, posting(sess("poster"), Target{Path: pathWeb}))
	if n := len(deliveriesTo(t, s, holder.ID)); n != 1 {
		t.Errorf("holder has %d deliveries after releasing, want still 1", n)
	}
	got := deliveriesTo(t, s, queued.ID)
	if n := len(slices.DeleteFunc(got, func(d deliveryRow) bool { return d.Kind != kindPath })); n != 1 {
		t.Errorf("the new holder has %d path deliveries, want 1", n)
	}
}

func TestUnfollow_refusedUnderAHeldReservation(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "holder", repoSubflux)
	regAt(t, s, "poster", repoGrelot)
	mustPost(t, s, posting(sess("poster"), Target{Path: pathWeb}))
	mustFollow(t, s, sess("holder"), pathWeb)
	res := mustReserve(t, s, reserving("holder", pathMarotte, time.Hour))

	_, err := s.Unfollow(t.Context(), Caller{Session: sess("holder")}, pathWeb)
	id := strconv.FormatUint(res.Reservation.ID, 10)
	wantRefusal(t, err, KindConflict, "", "release "+id+" first", pathMarotte)
	mustRelease(t, s, "holder", res.Reservation.ID)
	got, err := s.Unfollow(t.Context(), Caller{Session: sess("holder")}, pathWeb)
	if err != nil || slices.Contains(got.Follows, pathWeb) {
		t.Errorf("unfollow after the release: follows %v, err %v; want it gone", got.Follows, err)
	}
}

func TestJoinCard_reachesAReservationHolder(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	holder := regAt(t, s, "holder", repoSubflux)
	queued := regAt(t, s, "queued", repoSubflux)
	mustReserve(t, s, reserving("holder", pathMarotte, time.Hour))
	mustQueue(t, s, reserving("queued", pathMarotte, time.Hour))
	newbie := regAt(t, s, "newbie", pathWeb)

	ds := cardsAbout(t, s, holder.ID, newbie.ID)
	if len(ds) != 1 || ds[0].Kind != kindJoin || ds[0].Path != pathWeb {
		t.Errorf("holder's cards about newbie %+v, want one join card for %s", ds, pathWeb)
	}
	if ds := cardsAbout(t, s, queued.ID, newbie.ID); len(ds) != 0 {
		t.Errorf("a queue entry got its identity %d cards", len(ds))
	}
}

func TestReservations_listsGrantsAndQueueEntries(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "holder")
	regAt(t, s, "waiter")
	regAt(t, s, "elsewhere")
	held := mustReserve(t, s, reserving("holder", pathMarotte, time.Hour))
	queued := mustQueue(t, s, reserving("waiter", pathMarotte, time.Hour))
	other := mustReserve(t, s, reserving("elsewhere", repoGrelot, 10*time.Minute))

	ids := func(rs []Reservation) []uint64 {
		var out []uint64
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return out
	}
	list := func(session string, q ReservationQuery) []Reservation {
		t.Helper()
		rs, err := s.Reservations(t.Context(), Caller{Session: session}, q)
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	all := list(sess("waiter"), ReservationQuery{})
	if want := []uint64{held.Reservation.ID, queued.Reservation.ID, other.Reservation.ID}; !slices.Equal(ids(all), want) {
		t.Fatalf("all %v, want %v", ids(all), want)
	}
	if r := all[0]; r.Holder != "holder" || r.State != reservationHeld || r.Relation != "same environment" || r.Reason != "reason of holder" {
		t.Errorf("held entry %+v, want holder's grant read from the same environment", r)
	}
	if r := all[1]; r.State != reservationQueued || r.Relation != "you" || !r.Expires.IsZero() {
		t.Errorf("queued entry %+v, want the caller's own, with no expiry", r)
	}
	if got := ids(list(sess("waiter"), ReservationQuery{Path: pathWeb})); !slices.Equal(got, []uint64{held.Reservation.ID, queued.Reservation.ID}) {
		t.Errorf("under %s: %v", pathWeb, got)
	}
	if got := ids(list(sess("waiter"), ReservationQuery{Mine: true})); !slices.Equal(got, []uint64{queued.Reservation.ID}) {
		t.Errorf("mine: %v", got)
	}
	if got := list(sess("nobody"), ReservationQuery{}); len(got) != 3 || got[0].Relation != "unknown" {
		t.Errorf("a session with no identity sees %+v, want all three, relation unknown", got)
	}
	if got := list(sess("nobody"), ReservationQuery{Mine: true}); len(got) != 0 {
		t.Errorf("a session with no identity owns %+v", got)
	}
	clk.advance(10 * time.Minute)
	if got := ids(list(sess("waiter"), ReservationQuery{})); slices.Contains(got, other.Reservation.ID) {
		t.Errorf("an expired grant is still listed before the sweep: %v", got)
	}
}

func TestPost_afterAnUnsweptExpiryReachesTheNewHolder(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	holder := regAt(t, s, "holder", repoSubflux)
	waiter := regAt(t, s, "waiter", repoSubflux)
	regAt(t, s, "poster", repoGrelot)
	mustReserve(t, s, reserving("holder", pathMarotte, 10*time.Minute))
	mustQueue(t, s, reserving("waiter", pathMarotte, time.Hour))
	clk.advance(10 * time.Minute)

	mustPost(t, s, posting(sess("poster"), Target{Path: pathWeb}))
	if ds := eventsOf(t, s, holder.ID, kindPath); len(ds) != 0 {
		t.Errorf("the expired holder got %+v", ds)
	}
	if ds := eventsOf(t, s, waiter.ID, kindPath); len(ds) != 1 || ds[0].Path != pathMarotte {
		t.Errorf("the new holder got %+v, want one path delivery matched by %s", ds, pathMarotte)
	}
}

func TestPost_bySenderAHandOffGrantsKeepsEverySeq(t *testing.T) {
	// The waiter's own post settles the expired grant, which hands the
	// path to the waiter: its event and its row must keep distinct seqs.
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "holder", repoSubflux)
	waiter := regAt(t, s, "waiter", repoSubflux)
	regAt(t, s, "reader", pathMarotte)
	mustReserve(t, s, reserving("holder", pathMarotte, 10*time.Minute))
	mustQueue(t, s, reserving("waiter", pathMarotte, time.Hour))
	clk.advance(10 * time.Minute)
	direct(t, s, "reader", "waiter", 1)

	mustPost(t, s, posting(sess("waiter"), Target{Path: pathWeb}))
	ds := allDeliveriesTo(t, s, waiter.ID)
	kinds := make([]string, 0, len(ds))
	for i, d := range ds {
		kinds = append(kinds, d.Kind)
		if d.Seq != uint64(i+1) {
			t.Errorf("delivery %d has seq %d, want %d", i, d.Seq, i+1)
		}
	}
	if !slices.Contains(kinds, kindReservationGranted) || !slices.Contains(kinds, kindDirect) {
		t.Errorf("waiter's deliveries %v, want the direct message and the grant", kinds)
	}
	if row := getIdentity(t, s, waiter.ID); row.Seq != uint64(len(ds)) {
		t.Errorf("waiter's seq %d, want %d", row.Seq, len(ds))
	}
}

func TestUnfollow_passesOnceTheReservationExpired(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "holder", repoSubflux)
	regAt(t, s, "poster", repoGrelot)
	mustPost(t, s, posting(sess("poster"), Target{Path: pathWeb}))
	mustFollow(t, s, sess("holder"), pathWeb)
	mustReserve(t, s, reserving("holder", pathMarotte, 10*time.Minute))
	clk.advance(10 * time.Minute)
	if _, err := s.Unfollow(t.Context(), Caller{Session: sess("holder")}, pathWeb); err != nil {
		t.Errorf("unfollow after the reservation expired: %v", err)
	}
}

// rowsHeldBy lists every grant and queue entry of identity holder.
func rowsHeldBy(t *testing.T, s *Store, holder uint64) []reservationRow {
	t.Helper()
	var out []reservationRow
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		return s.t.resv.table.Scan(r, bolttable.Range{}, func(_ uint64, v *reservationRow) (bool, error) {
			if v.Holder == holder {
				out = append(out, *v)
			}
			return true, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRetire_freesEveryOwnRowBeforeTheQueueWalk(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "holder")
	leaving := regAt(t, s, "leaving")
	regAt(t, s, "third")
	mustReserve(t, s, reserving("holder", pathWeb, time.Hour))
	mustQueue(t, s, reserving("leaving", pathMarotte, time.Hour))
	// Held back only by leaving's own older entry on the parent.
	mustQueue(t, s, reserving("leaving", pathAPI, time.Hour))
	third := mustQueue(t, s, reserving("third", pathAPI, time.Hour))

	if err := s.Retire(t.Context(), Caller{Session: sess("leaving")}); err != nil {
		t.Fatal(err)
	}
	if rows := rowsHeldBy(t, s, leaving.ID); len(rows) != 0 {
		t.Errorf("the retired identity still has %+v, want none granted back to it", rows)
	}
	if ev := eventsOf(t, s, leaving.ID, kindReservationGranted); len(ev) != 0 {
		t.Errorf("the retired identity got %+v, want no grant back to it", ev)
	}
	wantState(t, s, third.Reservation.ID, reservationHeld)
}

func TestRetire_handsItsGrantToTheAgentQueuedBehindIt(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "leaving")
	waiter := regAt(t, s, "waiter")
	held := mustReserve(t, s, reserving("leaving", pathMarotte, time.Hour))
	q := mustQueue(t, s, reserving("waiter", pathMarotte, 2*time.Hour))

	if err := s.Retire(t.Context(), Caller{Session: sess("leaving")}); err != nil {
		t.Fatal(err)
	}
	if _, found := getReservation(t, s, held.Reservation.ID); found {
		t.Error("the retired identity's grant is still there")
	}
	row := wantState(t, s, q.Reservation.ID, reservationHeld)
	if want := testNow.Add(2 * time.Hour); !row.Lease.Expires.Equal(want) {
		t.Errorf("handed-off grant expires %v, want %v", row.Lease.Expires, want)
	}
	if n := len(eventsOf(t, s, waiter.ID, kindReservationGranted)); n != 1 {
		t.Errorf("%d reservation_granted for the waiter, want 1", n)
	}
}

func TestIdentityExpiry_handsItsGrantsOffAndRevivalRestoresNone(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	r := registration(sessA, "leaving", envA())
	r.TTL = 15 * time.Minute
	leaving := mustRegister(t, s, r)
	waiter := regAt(t, s, "waiter")
	regAt(t, s, "other")
	mustReserve(t, s, reserving("other", "/workspace/sse", time.Hour))
	held := mustReserve(t, s, Reserving{Caller: Caller{Session: sessA}, Path: pathMarotte, Reason: "build", TTL: time.Hour})
	own := mustQueue(t, s, Reserving{Caller: Caller{Session: sessA}, Path: "/workspace/sse", Reason: "after", TTL: time.Hour})
	q := mustQueue(t, s, reserving("waiter", pathMarotte, time.Hour))

	clk.advance(15 * time.Minute)
	mustSweep(t, s)
	if row := getIdentity(t, s, leaving.ID); row.State != stateDormant {
		t.Fatalf("state %q, want dormant", row.State)
	}
	for _, id := range []uint64{held.Reservation.ID, own.Reservation.ID} {
		if _, found := getReservation(t, s, id); found {
			t.Errorf("reservation %d of the expired identity is still there", id)
		}
	}
	wantState(t, s, q.Reservation.ID, reservationHeld)
	if n := len(eventsOf(t, s, waiter.ID, kindReservationGranted)); n != 1 {
		t.Errorf("%d reservation_granted for the waiter, want 1", n)
	}

	mustRevive(t, s, revival(sessA, "", envA()))
	if rows := rowsHeldBy(t, s, leaving.ID); len(rows) != 0 {
		t.Errorf("the revived identity holds %+v, want no reservation restored", rows)
	}
}

func TestTakeOver_keepsReservationIDsWorkingFromTheNewSession(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	mustRegister(t, s, registration(sessA, "subflux-dev", envA()))
	held := mustReserve(t, s, Reserving{Caller: Caller{Session: sessA}, Path: pathMarotte, Reason: "build", TTL: time.Hour})
	r := registration(sessB, "subflux-dev", envA())
	r.TakeOver = true
	mustRegister(t, s, r)

	if _, err := s.RenewReservation(t.Context(), Caller{Session: sessB}, held.Reservation.ID, 2*time.Hour); err != nil {
		t.Errorf("renew from the new session: %v", err)
	}
	_, err := s.Release(t.Context(), Caller{Session: sessA}, held.Reservation.ID)
	wantRefusal(t, err, KindGone, "")
	if _, err := s.Release(t.Context(), Caller{Session: sessB}, held.Reservation.ID); err != nil {
		t.Errorf("release from the new session: %v", err)
	}
}
