//go:build !windows

package store

import (
	"context"
	"errors"
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
		_, err := s.Post(ctx, &p)
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
