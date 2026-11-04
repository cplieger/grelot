package store

import (
	"context"
	"fmt"
	"time"
)

// announceKey is the announcement thread's key. A path key starts with /
// and a direct key with direct:, so no key of one kind meets another's.
const announceKey = "announce"

// announceTTL is how long an announcement lives; the thread never expires.
const announceTTL = 7 * 24 * time.Hour

// directKey is the direct thread of two identities, lower id first, so
// both directions land in one thread.
func directKey(a, b uint64) string {
	return fmt.Sprintf("direct:%d:%d", min(a, b), max(a, b))
}

// ensureAnnounce creates the announcement thread at the store's first
// start.
func (s *Store) ensureAnnounce(ctx context.Context) error {
	return s.update(ctx, func(w *wtx) error {
		th, found, err := w.thread(announceKey)
		if err != nil || found {
			return err
		}
		th.Kind = kindAnnounce
		return s.t.threads.table.Put(w.tx, th.ID, &th)
	})
}

// thread reads the live thread under key for a write, running its due
// expiry first, or returns a new row under that key for the caller to
// fill and store, with found false.
func (w *wtx) thread(key string) (threadRow, bool, error) {
	id, _, found, err := w.s.t.threadKey.Lookup(w.r, []byte(key))
	if err != nil {
		return threadRow{}, false, err
	}
	if found {
		th, live, serr := w.s.t.threads.settle(w, id)
		if serr != nil || live {
			return th, live, serr
		}
	}
	id, err = w.s.t.threads.table.NextID(w.tx)
	return threadRow{ID: id, Key: key}, false, err
}
