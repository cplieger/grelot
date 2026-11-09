//go:build !windows

package store

import (
	"maps"
	"slices"
)

// copies maps each parent a post owes a copy to the handle of the first
// delegate, the sender before the recipients, that the copy is for.
type copies map[uint64]string

// owe records that delegate d's parent sees the post, unless the parent is
// its sender. A parent owed by several of its delegates is owed once.
func (c copies) owe(d *identityRow, sender uint64) {
	if d.Parent == 0 || d.Parent == sender {
		return
	}
	if _, owed := c[d.Parent]; !owed {
		c[d.Parent] = d.Name
	}
}

// writeCopies writes each parent in c that is not a recipient of msg a
// quiet copy of it; a recipient parent's own delivery is the record.
func (w *wtx) writeCopies(c copies, rt *route, msg *messageRow) error {
	for _, id := range slices.Sorted(maps.Keys(c)) {
		if _, received := rt.recipients[id]; received {
			continue
		}
		p, found, err := w.s.t.identities.settle(w, id)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if err := w.deliver(&p, &deliveryRow{
			Kind: rt.thread.Kind, Via: c[id], Name: p.Name,
			Message: msg.ID, Thread: rt.thread.ID, Sender: msg.Sender, Lease: lease{Expires: msg.Lease.Expires},
		}); err != nil {
			return err
		}
	}
	return nil
}
