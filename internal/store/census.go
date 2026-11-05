package store

import (
	"context"
	"time"

	"github.com/cplieger/bolttable"
)

// Census is what the /metrics gauges read. Unread and Oldest cover
// the wake deliveries past read of live identities only: a dormant
// identity's mail waits for a revival, and counting it would hold Oldest
// at days.
type Census struct {
	// Oldest is the write time of the oldest unread wake delivery, zero
	// with none.
	Oldest  time.Time
	Space   Space
	Threads ThreadCounts
	// MaxBytes is the ceiling on Space.InUseBytes that posts are refused
	// near.
	MaxBytes int64
	Live     int
	Dormant  int
	Online   int
	// Held counts the active grants, Queued the queue entries.
	Held   int
	Queued int
	Unread int
}

// ThreadCounts counts the live threads by kind.
type ThreadCounts struct {
	Announce, Path, Direct, Cards int
}

// Census counts the store's rows as a read sees them, expired rows the
// sweeper has not reached left out. online, which may be nil, decides
// which live identities count as online. Census writes nothing.
func (s *Store) Census(ctx context.Context, online func(*Identity) bool) (Census, error) {
	var out Census
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		if err := s.censusIdentities(r, &out, online, now); err != nil {
			return err
		}
		if err := s.t.threads.table.Scan(r, bolttable.Range{}, func(_ uint64, v *threadRow) (bool, error) {
			if !v.Lease.due(now) {
				out.Threads.add(v.Kind)
			}
			return true, nil
		}); err != nil {
			return err
		}
		return s.t.resv.table.Scan(r, bolttable.Range{}, func(_ uint64, v *reservationRow) (bool, error) {
			switch {
			case v.State == reservationQueued:
				out.Queued++
			case !v.Lease.due(now):
				out.Held++
			}
			return true, nil
		})
	})
	if err != nil {
		return Census{}, err
	}
	out.Space, err = s.Measure()
	out.MaxBytes = s.maxBytes
	return out, err
}

func (s *Store) censusIdentities(r *bolttable.ReadTx, out *Census, online func(*Identity) bool, now time.Time) error {
	var rows []identityRow
	err := s.t.identities.table.Scan(r, bolttable.Range{}, func(_ uint64, v *identityRow) (bool, error) {
		switch v = settled(v, now, s.dormantTTL); {
		case v == nil:
		case v.State == stateDormant:
			out.Dormant++
		default:
			rows = append(rows, *v)
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	for i := range rows {
		v := &rows[i]
		out.Live++
		if online != nil && s.online(online, v) {
			out.Online++
		}
		b, err := s.wakeBatch(r, v, now)
		if err != nil {
			return err
		}
		out.Unread += b.count
		if b.count > 0 && (out.Oldest.IsZero() || b.oldest.Before(out.Oldest)) {
			out.Oldest = b.oldest
		}
	}
	return nil
}

func (c *ThreadCounts) add(kind string) {
	switch kind {
	case kindAnnounce:
		c.Announce++
	case kindPath:
		c.Path++
	case kindDirect:
		c.Direct++
	case kindCards:
		c.Cards++
	}
}
