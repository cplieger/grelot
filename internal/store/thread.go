package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/cplieger/bolttable"
)

// The two singleton threads' keys. A path key starts with / and a direct
// key with direct:, so no key of one kind meets another's.
const (
	announceKey = "announce"
	cardsKey    = "cards"
)

// announceTTL is how long an announcement lives; the thread never expires.
const announceTTL = 7 * 24 * time.Hour

// directKey is the direct thread of two identities, lower id first, so
// both directions land in one thread.
func directKey(a, b uint64) string {
	return fmt.Sprintf("direct:%d:%d", min(a, b), max(a, b))
}

// ensureServerThreads creates the announcement and cards threads at the
// store's first start.
func (s *Store) ensureServerThreads(ctx context.Context) error {
	return s.update(ctx, func(w *wtx) error {
		for _, st := range [...]struct{ key, kind string }{{announceKey, kindAnnounce}, {cardsKey, kindCards}} {
			th, found, err := w.thread(st.key)
			if err != nil {
				return err
			}
			if found {
				continue
			}
			th.Kind = st.kind
			if err := s.t.threads.table.Put(w.tx, th.ID, &th); err != nil {
				return err
			}
		}
		return nil
	})
}

// expireThread deletes a thread, its messages with it, and first takes a
// path thread out of its followers' follows, so a thread recreated at the
// path starts with none. A cascade deletes whole rows, so it cannot do
// that rewrite.
func expireThread(w *wtx, k uint64, v *threadRow) error {
	if v.Kind == kindPath {
		var followers []uint64
		if err := scanExact(w.r, w.s.t.byFollow, v.Path, func(id uint64) (bool, error) {
			followers = append(followers, id)
			return true, nil
		}); err != nil {
			return err
		}
		for _, id := range followers {
			row, found, err := w.s.t.identities.table.Get(w.r, id)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			row.Follows = slices.DeleteFunc(row.Follows, func(f string) bool { return f == v.Path })
			if err := w.s.t.identities.table.Put(w.tx, id, &row); err != nil {
				return err
			}
		}
	}
	return deleteRow(w, w.s.t.threads, k)
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

// PathThread is a live path thread: its path and when it expires, which is
// THREAD_TTL after its last post.
type PathThread struct {
	Expires time.Time
	Path    string
	ID      uint64
}

// Threads lists, by path, the live path threads at or under root, every
// one when root is "". Refusals: KindInvalid for a path that is not
// canonical; KindGone for a session with no live identity.
func (s *Store) Threads(ctx context.Context, c Caller, root string) ([]PathThread, error) {
	prefix := "/"
	if root != "" {
		if _, err := checkPaths([]string{root}); err != nil {
			return nil, err
		}
		prefix = root
	}
	out := []PathThread{}
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		if _, err := s.liveFor(r, c, now); err != nil {
			return err
		}
		return s.t.threadKey.Scan(r, bolttable.Range{Prefix: []byte(prefix)}, func(_ []byte, id uint64, _ []byte) (bool, error) {
			th, found, err := s.t.threads.table.Get(r, id)
			if err != nil || !found || th.Kind != kindPath || !th.Lease.Expires.After(now) {
				return err == nil, err
			}
			if root == "" || under(th.Path, root) {
				out = append(out, PathThread{Expires: th.Lease.Expires, Path: th.Path, ID: th.ID})
			}
			return true, nil
		})
	})
	return out, err
}
