//go:build !windows

package store

import (
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// editIdentity rewrites identity row id in place, for state no verb sets
// yet (acks, nudges) or that takes many requests to reach (32 follows).
func editIdentity(t *testing.T, s *Store, id uint64, edit func(*identityRow)) {
	t.Helper()
	putRows(t, s, func(w *wtx) error {
		row, _, err := s.t.identities.table.Get(w.r, id)
		if err != nil {
			return err
		}
		edit(&row)
		// An edited read cursor moves what the unread summary counts.
		w.recounts = append(w.recounts, id)
		return s.t.identities.table.Put(w.tx, id, &row)
	})
}

// deliverEvents writes n server events to identity id.
func deliverEvents(t *testing.T, s *Store, id uint64, n int) {
	t.Helper()
	putRows(t, s, func(w *wtx) error {
		for range n {
			if err := w.writeEvent(id, kindIdentityExpiring, "event"); err != nil {
				return err
			}
		}
		return nil
	})
}

func TestExpiry_makesTheIdentityDormantKeepingItsRow(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	r := registration(sessA, "subflux-dev", envA(), repoSubflux, repoGrelot)
	r.TTL = 15 * time.Minute
	id := mustRegister(t, s, r)
	deliverEvents(t, s, id.ID, 2)
	editIdentity(t, s, id.ID, func(row *identityRow) {
		row.Follows = []string{"/workspace/docs"}
		row.Read, row.Shown = 1, 2
	})
	clk.advance(15 * time.Minute)
	mustSweep(t, s)

	row := getIdentity(t, s, id.ID)
	expiry := testNow.Add(15 * time.Minute)
	if row.State != stateDormant || !row.DormantSince.Equal(expiry) {
		t.Errorf("state %q since %v, want dormant since the expiry %v", row.State, row.DormantSince, expiry)
	}
	if want := expiry.Add(72 * time.Hour); !row.Lease.Expires.Equal(want) || !row.Lease.WarnAt.IsZero() {
		t.Errorf("lease %+v, want a purge at %v with no warning", row.Lease, want)
	}
	if row.Name != "subflux-dev" || row.Session != sessA || row.Description != "moving the run surface" ||
		!slices.Equal(row.Paths, []string{repoSubflux, repoGrelot}) || !slices.Equal(row.Follows, []string{"/workspace/docs"}) {
		t.Errorf("row %+v lost its name, session, description, paths or follows", row)
	}
	if row.Read != 1 || row.Shown != 2 || row.Seq != 2 {
		t.Errorf("read %d shown %d seq %d, want the cursors 1, 2 and seq 2 kept", row.Read, row.Shown, row.Seq)
	}
	if n := countRows(t, s, s.t.deliveries.table); n != 2 {
		t.Errorf("%d deliveries, want both kept for a revival", n)
	}
}

func TestDormant_isNeverWarned(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	expireToDormant(t, s, clk, "subflux-dev")
	clk.advance(72*time.Hour - time.Minute)
	mustSweep(t, s)
	if n := countRows(t, s, s.t.deliveries.table); n != 0 {
		t.Errorf("%d deliveries, want no warning before the purge", n)
	}
}

func TestDormant_itsSessionOnlyReadsItsStatus(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	expireToDormant(t, s, clk, "subflux-dev")
	_, err := s.Set(t.Context(), Keyed{}, Caller{Session: sessA}, Change{Fields: Fields{Description: "new work"}})
	wantRefusal(t, err, KindGone, "dormant", "your identity @subflux-dev expired 0 h ago with 0 unread; `grelot revive --ttl=<d>` restores it")
	wantRefusal(t, s.EndSession(t.Context(), sessA), KindGone, "dormant")
	got, err := s.Seen(t.Context(), Caller{Session: sessA})
	if err != nil {
		t.Fatalf("Seen of a dormant session: %v", err)
	}
	if got.State != stateDormant || got.Name != "subflux-dev" {
		t.Errorf("state %q name %q, want the dormant identity", got.State, got.Name)
	}
}

func TestDormant_readsCommitNothing(t *testing.T) {
	// last_seen lives in memory and only a live row is ever written, so a
	// read from a dormant or unregistered session is no bbolt commit.
	s, clk, _ := openIdentityStore(t)
	expireToDormant(t, s, clk, "subflux-dev")
	clk.advance(10 * time.Minute)
	ctx := t.Context()
	start := commitCount(t, s)

	_, err := s.Inbox(ctx, Caller{Session: sessA}, 0)
	wantRefusal(t, err, KindGone, "dormant", "`grelot revive --ttl=<d>` restores it")
	_, err = s.History(ctx, Caller{Session: sessA}, &HistoryQuery{})
	wantRefusal(t, err, KindGone, "dormant")
	if _, err := s.Who(ctx, Caller{Session: sessA}, &WhoQuery{}); err != nil {
		t.Fatalf("who from the dormant session: %v", err)
	}
	if _, err := s.Who(ctx, Caller{Session: "kiro-cli:nobody"}, &WhoQuery{}); err != nil {
		t.Fatalf("who from a session with no identity: %v", err)
	}
	if n := commitCount(t, s) - start; n != 0 {
		t.Errorf("reads from sessions with no live identity committed %d times, want none", n)
	}
}

func TestDormant_readsNeverWaitForTheWriter(t *testing.T) {
	// a who or inbox from a session with no live identity has nothing to
	// write, so it answers while a sweep or a post holds the write lock.
	s, clk, _ := openIdentityStore(t)
	expireToDormant(t, s, clk, "subflux-dev")
	ctx := t.Context()
	held, release, writer := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		writer <- s.update(ctx, func(*wtx) error { close(held); <-release; return nil })
	}()
	<-held

	var errs []error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := s.Inbox(ctx, Caller{Session: sessA}, 0)
		errs = append(errs, err)
		_, err = s.History(ctx, Caller{Session: sessA}, &HistoryQuery{})
		errs = append(errs, err)
		_, err = s.Who(ctx, Caller{Session: sessA}, &WhoQuery{})
		errs = append(errs, err)
		_, err = s.Who(ctx, Caller{Session: "kiro-cli:nobody"}, &WhoQuery{})
		errs = append(errs, err)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("reads from sessions with no live identity waited for the open write transaction")
	}
	close(release)
	<-done
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	wantRefusal(t, errs[0], KindGone, "dormant")
	wantRefusal(t, errs[1], KindGone, "dormant")
	if errs[2] != nil || errs[3] != nil {
		t.Errorf("who errors %v and %v, want none", errs[2], errs[3])
	}
}

func TestPurge_afterDormantTTLFreesTheName(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	old := expireToDormant(t, s, clk, "subflux-dev")
	purge := testNow.Add(15*time.Minute + 72*time.Hour)
	putRows(t, s, func(w *wtx) error {
		return s.t.deliveries.table.Put(w.tx, pairKey(old.ID, 1), &deliveryRow{
			Agent: old.ID, Seq: 1, Kind: kindIdentityExpiring, Lease: lease{Expires: purge.Add(time.Hour)},
		})
	})

	clk.set(purge.Add(-time.Nanosecond))
	mustSweep(t, s)
	if row := getIdentity(t, s, old.ID); row.State != stateDormant {
		t.Fatalf("state %q a nanosecond before the purge, want dormant", row.State)
	}
	clk.set(purge)
	mustSweep(t, s)
	if n := countRows(t, s, s.t.identities.table); n != 0 {
		t.Errorf("%d identity rows after the purge, want none", n)
	}
	if n := countRows(t, s, s.t.deliveries.table); n != 1 {
		t.Errorf("%d deliveries, want the row kept until its own expiry", n)
	}
	other := Where{Machine: dmi("2"), Environment: hex32("b")}
	if got := mustRegister(t, s, registration(sessB, "subflux-dev", other)); got.ID == old.ID {
		t.Errorf("the purged id %d came back", old.ID)
	}
}

func TestDormancy_survivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	clk := &clock{t: testNow}
	s, err := Open(path, &Options{Logger: slog.New(newLogSink()), Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	old := expireToDormant(t, s, clk, "subflux-dev")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path, &Options{Logger: slog.New(newLogSink()), Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_, _, err = s.Register(t.Context(), Keyed{}, new(registration(sessB, "subflux-dev", envA())))
	wantRefusal(t, err, KindConflict, "", "subflux-dev is dormant until")
	got, _, err := s.Revive(t.Context(), Keyed{}, new(revival(sessA, "", envA())))
	if err != nil {
		t.Fatalf("Revive after a restart: %v", err)
	}
	if got.Identity.ID != old.ID || got.Identity.State != stateLive {
		t.Errorf("id %d state %q, want %d live", got.Identity.ID, got.Identity.State, old.ID)
	}
}
