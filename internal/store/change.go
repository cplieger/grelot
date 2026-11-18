//go:build !windows

package store

import "slices"

// WebChange is what one commit wrote that the web UI shows.
type WebChange struct {
	// Conversations are the threads that gained a post, had a delivery
	// read, or had a post or themselves expire, in the order the commit
	// wrote them.
	Conversations []uint64
	// Agents is set when an identity or a reservation was written.
	Agents bool
}

// changedThread has Options.Changed name thread once this commits.
func (w *wtx) changedThread(thread uint64) {
	if thread != 0 && !slices.Contains(w.changed.Conversations, thread) {
		w.changed.Conversations = append(w.changed.Conversations, thread)
	}
}

// changedPathThread names path thread id, whose path is p, for a write
// that may delete its row before nameParents reads it.
func (w *wtx) changedPathThread(id uint64, p string) {
	w.changedThread(id)
	if w.changedPaths == nil {
		w.changedPaths = map[uint64]string{}
	}
	w.changedPaths[id] = p
}

// nameParents names, beside each changed path thread, the live path
// threads above its path, since their conversations show its posts.
func (w *wtx) nameParents() error {
	for _, id := range slices.Clone(w.changed.Conversations) {
		p, found, err := w.changedPath(id)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		for _, a := range ancestors(p) {
			if err := w.nameLivePathThread(a); err != nil {
				return err
			}
		}
	}
	return nil
}

// changedPath is changed thread id's path, found when it is a path thread.
func (w *wtx) changedPath(id uint64) (p string, found bool, err error) {
	if known, ok := w.changedPaths[id]; ok {
		return known, true, nil
	}
	th, found, err := w.s.t.threads.table.Get(w.r, id)
	if err != nil || !found || th.Kind != kindPath {
		return "", false, err
	}
	return th.Path, true, nil
}

// nameLivePathThread names the live path thread at p, if there is one.
func (w *wtx) nameLivePathThread(p string) error {
	id, _, found, err := w.s.t.threadKey.Lookup(w.r, []byte(p))
	if err != nil || !found {
		return err
	}
	th, live, err := w.s.t.threads.table.Get(w.r, id)
	if err == nil && live && th.Kind == kindPath && !th.Lease.due(w.now) {
		w.changedThread(id)
	}
	return err
}

func (w *wtx) putIdentity(id uint64, row *identityRow) error {
	w.changed.Agents = true
	return w.s.t.identities.table.Put(w.tx, id, row)
}

func (w *wtx) deleteIdentity(id uint64) error {
	w.changed.Agents = true
	_, err := w.s.t.identities.table.Delete(w.tx, id)
	return err
}

func (w *wtx) putReservation(id uint64, row *reservationRow) error {
	w.changed.Agents = true
	return w.s.t.resv.table.Put(w.tx, id, row)
}
