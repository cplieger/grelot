package store

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/bolttable"
)

// openBubbleStore opens a store on the clock of the synctest bubble it is
// called in.
func openBubbleStore(t *testing.T) (*Store, logSink) {
	t.Helper()
	sink := newLogSink()
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{Logger: slog.New(sink)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, sink
}

// beats records each call of a Run beat, with what the beat could read.
type beats struct {
	start time.Time
	at    []time.Duration
	saw   []bool
	mu    sync.Mutex
}

func (b *beats) record(saw bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.at = append(b.at, time.Since(b.start))
	b.saw = append(b.saw, saw)
}

func (b *beats) snapshot() ([]time.Duration, []bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.at), slices.Clone(b.saw)
}

// runSweeper starts s.Run with beat and returns its stop, which cancels it
// and fails the test unless Run returns nil.
func runSweeper(t *testing.T, s *Store, beat func()) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, beat) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v once its context ended, want nil", err)
		}
	}
}

func TestRun_sweepsAtOnceAndEveryPeriodBeatingAfterEachCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := openBubbleStore(t)
		regAt(t, s, "holder")
		regAt(t, s, "waiter")
		mustReserve(t, s, reserving("holder", pathMarotte, time.Minute))
		q := mustQueue(t, s, reserving("waiter", pathMarotte, time.Hour))

		b := &beats{start: time.Now()}
		stop := runSweeper(t, s, func() {
			var row reservationRow
			err := s.db.View(context.Background(), func(r *bolttable.ReadTx) error {
				var err error
				row, _, err = s.t.resv.table.Get(r, q.Reservation.ID)
				return err
			})
			b.record(err == nil && row.State == reservationHeld)
		})
		time.Sleep(time.Minute + time.Second)
		stop()

		at, granted := b.snapshot()
		wantAt := []time.Duration{0, 15 * time.Second, 30 * time.Second, 45 * time.Second, time.Minute}
		if !slices.Equal(at, wantAt) {
			t.Errorf("beats at %v, want one at once and one every 15 s: %v", at, wantAt)
		}
		if want := []bool{false, false, false, false, true}; !slices.Equal(granted, want) {
			t.Errorf("waiter granted at each beat %v, want %v: the pass at the grant's expiry hands it over before its beat", granted, want)
		}

		time.Sleep(time.Minute)
		if after, _ := b.snapshot(); len(after) != len(at) {
			t.Errorf("%d beats after Run stopped, want none", len(after)-len(at))
		}
	})
}

func TestRun_aFailedPassLogsAnErrorAndCallsNoBeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, sink := openBubbleStore(t)
		b := &beats{start: time.Now()}
		stop := runSweeper(t, s, func() { b.record(true) })
		synctest.Wait()
		if err := s.db.Close(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(31 * time.Second)
		stop()

		if at, _ := b.snapshot(); !slices.Equal(at, []time.Duration{0}) {
			t.Errorf("beats at %v, want only the pass before the store closed", at)
		}
		lines := sink.lines(slog.LevelError, msgSweepFailed)
		if len(lines) != 2 {
			t.Fatalf("%d %q lines, want one per failed pass, 2", len(lines), msgSweepFailed)
		}
		if attr(lines[0], "error") == "" {
			t.Error("the sweep failed line names no error")
		}
	})
}

func TestRun_logsNothingWhenStoppedMidPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, sink := openBubbleStore(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := s.Run(ctx, func() {}); err != nil {
			t.Errorf("Run on an ended context returned %v, want nil", err)
		}
		if n := len(sink.lines(slog.LevelError, msgSweepFailed)); n != 0 {
			t.Errorf("%d %q lines for a stop, want none", n, msgSweepFailed)
		}
	})
}

func TestSweep_afterARestartServesReservationsDelegatesLinksAndWatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grelot.db")
	clk := &clock{t: testNow}
	sink := newLogSink()
	open := func() *Store {
		s, err := Open(path, &Options{Logger: slog.New(sink), Now: clk.now})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return s
	}
	s := open()
	holder := regAt(t, s, "holder")
	waiter := regAt(t, s, "waiter")
	warned := regAt(t, s, "warned", "/workspace/other")
	sender := regAt(t, s, "sender", "/workspace/sender")
	reader := regAt(t, s, "reader", "/workspace/reader")
	mustReserve(t, s, reserving("holder", pathMarotte, time.Hour))
	q := mustQueue(t, s, reserving("waiter", pathMarotte, time.Hour))
	long := mustReserve(t, s, reserving("warned", "/workspace/other", 2*time.Hour+4*time.Minute))
	msg := mustPost(t, s, watchedPost("sender", Target{Agent: "reader"}, NotifyRead{All: true, By: time.Hour})).Message

	parent := regAt(t, s, "parent", "/workspace/parent")
	g := grantFrom("parent", "review")
	g.TTL = time.Hour
	d := mustRegisterDelegate(t, s, sess("parent"), mustGrant(t, s, g).Link)
	spare := grantFrom("parent", "spare")
	spare.TTL = time.Hour
	mustGrant(t, s, spare)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	clk.advance(2 * time.Hour)
	s = open()
	t.Cleanup(func() { _ = s.Close() })
	if n := countRows(t, s, s.t.links.table); n != 1 {
		t.Fatalf("%d links before the sweep, want the unused one still stored", n)
	}
	mustSweep(t, s)

	t.Run("reservations", func(t *testing.T) {
		wantState(t, s, q.Reservation.ID, reservationHeld)
		if n := len(eventsOf(t, s, waiter.ID, kindReservationGranted)); n != 1 {
			t.Errorf("%d reservation_granted to the waiter, want 1", n)
		}
		if n := len(eventsOf(t, s, holder.ID, kindReservationExpired)); n != 1 {
			t.Errorf("%d reservation_expired to the holder, want 1", n)
		}
		wantState(t, s, long.Reservation.ID, reservationHeld)
		if n := len(eventsOf(t, s, warned.ID, kindReservationExpiring)); n != 1 {
			t.Errorf("%d reservation_expiring for a grant whose warning fell due while the server was down, want 1", n)
		}
	})
	t.Run("delegates", func(t *testing.T) {
		if identityExists(t, s, d.ID) {
			t.Error("the delegate outlived its lease across the restart")
		}
		if got := endedWhy(sink, d.Name); !slices.Equal(got, []string{"expired"}) {
			t.Errorf("delegate ended why %q, want [expired]", got)
		}
		if !identityExists(t, s, parent.ID) {
			t.Error("the parent went with its delegate")
		}
	})
	t.Run("links", func(t *testing.T) {
		if n := countRows(t, s, s.t.links.table); n != 0 {
			t.Errorf("%d links after the sweep, want the expired one deleted", n)
		}
	})
	t.Run("watches", func(t *testing.T) {
		if n := watchCount(t, s); n != 0 {
			t.Errorf("%d watches after the sweep, want the --by watch ended", n)
		}
		wantEvents(t, s, sender.ID, kindReceiptsDue,
			fmt.Sprintf("message %d: 0 of 1 have read %s by the deadline; still unread: @%s", msg, subject, reader.Name))
	})

	mustSweep(t, s)
	if n := len(eventsOf(t, s, warned.ID, kindReservationExpiring)); n != 1 {
		t.Errorf("%d reservation_expiring after a second sweep, want the warning once", n)
	}
}
