package store

import (
	"context"

	"github.com/cplieger/bolttable"
)

// Status is what the status line of a response to c reports: c's live
// identity and its deliveries past read.
type Status struct {
	Identity Identity
	Unread   Counts
	// Delegate reports a delegate, which is pull only.
	Delegate bool
}

// Status reads c's status and writes nothing. The delegate gate does not
// apply, so a refused request still gets its status line. Refusals:
// KindGone with the register command (Why none) or the revive offer (Why
// dormant) as its Msg, which is that session's status line.
func (s *Store) Status(ctx context.Context, c Caller) (Status, error) {
	if c.As == "" && c.Agent == "" {
		c.Self = true
	}
	var out Status
	err := s.db.View(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		me, err := s.liveFor(r, c, now)
		if err != nil {
			return err
		}
		out.Identity, out.Delegate = s.view(&me), me.Parent != 0
		return s.scanUnread(r, me.ID, me.Read, now, func(d *deliveryRow) error {
			out.Unread.add(d)
			return nil
		})
	})
	return out, err
}
