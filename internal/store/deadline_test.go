//go:build !windows

package store

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

func countMessages(t *testing.T, s *Store) int {
	t.Helper()
	n := 0
	if err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		return s.t.messages.table.Scan(r, bolttable.Range{}, func([]byte, *messageRow) (bool, error) {
			n++
			return true, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPost_deadlinePassingWhileTheWriterIsHeldStoresNothing(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha")
	b := regAt(t, s, "bravo")
	messagesBefore, deliveriesBefore := countMessages(t, s), len(deliveriesTo(t, s, b.ID))

	held, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		holderDone <- s.update(t.Context(), func(*wtx) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	const deadline = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()
	p := posting(sess("alpha"), Target{Agent: "bravo"})
	start := time.Now()
	posted := make(chan error, 1)
	go func() {
		_, _, err := s.Post(ctx, Keyed{}, &p)
		posted <- err
	}()

	var err error
	select {
	case err = <-posted:
	case <-time.After(5 * time.Second):
		close(release)
		<-posted
		t.Fatal("Post did not return while another write held the writer")
	}
	elapsed := time.Since(start)
	close(release)
	if herr := <-holderDone; herr != nil {
		t.Fatalf("holder: %v", herr)
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Post err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > deadline+time.Second {
		t.Errorf("Post returned after %v, want about its %v deadline", elapsed, deadline)
	}
	if got := countMessages(t, s); got != messagesBefore {
		t.Errorf("%d messages after the refused post, want %d", got, messagesBefore)
	}
	if got := len(deliveriesTo(t, s, b.ID)); got != deliveriesBefore {
		t.Errorf("%d deliveries to bravo after the refused post, want %d", got, deliveriesBefore)
	}
}

func TestRetire_cancelledAfterItsChangeRanStoresAndLogsNothing(t *testing.T) {
	s, clk, sink := openIdentityStore(t)
	a := regAt(t, s, "alpha")
	k := keyFor(plain("alpha"), "key-one", "retire")
	seenAt := testNow.Add(time.Minute)
	s.noteSeen(a.ID, seenAt)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The store reads its clock once its transaction holds the writer, so
	// the context ends after the writer is held and before the commit.
	s.now = func() time.Time {
		cancel()
		return clk.now()
	}

	_, err := s.Retire(ctx, k, plain("alpha"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Retire err = %v, want context.Canceled", err)
	}
	row := getIdentity(t, s, a.ID)
	if row.State != stateLive {
		t.Errorf("alpha is %s after the cancelled retirement, want live", row.State)
	}
	if got := s.lastSeen(&row); !got.Equal(seenAt) {
		t.Errorf("alpha last seen %v after the cancelled retirement, want its noted %v", got, seenAt)
	}
	if lines := sink.lines(slog.LevelInfo, "identity retired"); len(lines) != 0 {
		t.Errorf("%d identity retired lines for a retirement that rolled back, want 0", len(lines))
	}
	s.now = clk.now
	if replayed, err := s.Retire(t.Context(), k, plain("alpha")); err != nil || replayed {
		t.Errorf("re-sent Retire = replayed %v, %v; want a fresh retirement", replayed, err)
	}
}
