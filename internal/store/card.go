package store

import (
	"fmt"
	"slices"
	"strings"
)

// card is a join or left card owed for one path of an identity, with the
// identity as it stood at the event.
type card struct {
	kind  string
	path  string
	about identityRow
}

// queueCards owes a card of kind for each of paths about row. The cards
// are written when the transaction's request is done (flushOwed), so a
// card that an eager expiry owes mid-request never changes a row the
// request still holds a copy of.
func (w *wtx) queueCards(kind string, row *identityRow, paths []string) {
	about := *row
	about.Where = cloneWhere(row.Where)
	about.Paths = slices.Clone(row.Paths)
	for _, p := range paths {
		w.cards = append(w.cards, card{about: about, kind: kind, path: p})
	}
}

// flushOwed writes every owed event and card. Settling a card's recipient
// can owe more of either, which the loop writes too.
func (w *wtx) flushOwed() error {
	for {
		switch {
		case len(w.events) > 0:
			e := w.events[0]
			w.events = w.events[1:]
			if err := w.writeEvent(e.to, e.kind, e.text); err != nil {
				return err
			}
		case len(w.cards) > 0:
			c := w.cards[0]
			w.cards = w.cards[1:]
			if err := w.writeCard(&c); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// writeCard stores c in the cards thread and delivers it to each live
// identity whose work paths or held reservations overlap c's path either
// way, but not to the identity it is about. A card nobody receives is not stored. It never
// touches a path thread.
func (w *wtx) writeCard(c *card) error {
	rs, err := w.cardRecipients(c)
	if err != nil || len(rs) == 0 {
		return err
	}
	msg, err := w.storeCard(c)
	if err != nil {
		return err
	}
	for i := range rs {
		r := &rs[i]
		if err := w.deliver(r, &deliveryRow{
			Kind: c.kind, Path: c.path, Name: r.Name,
			Message: msg.ID, Thread: msg.Thread, Sender: c.about.ID, Lease: lease{Expires: msg.Lease.Expires},
		}); err != nil {
			return err
		}
	}
	return nil
}

// cardRecipients settles and returns, in id order, the live identities
// overlapping c's path, but not the one c is about.
func (w *wtx) cardRecipients(c *card) ([]identityRow, error) {
	ids, err := w.overlapping(c.path)
	if err != nil {
		return nil, err
	}
	var out []identityRow
	for _, id := range ids {
		if id == c.about.ID {
			continue
		}
		r, found, err := w.s.t.identities.settle(w, id)
		if err != nil {
			return nil, err
		}
		if found && r.State == stateLive {
			out = append(out, r)
		}
	}
	return out, nil
}

// overlapping lists, in id order, the identities with a work path or a
// held reservation at, above or under p, settling the reservations first.
func (w *wtx) overlapping(p string) ([]uint64, error) {
	ids, err := overlapIDs(w.r, w.s.t.byPath, p)
	if err != nil {
		return nil, err
	}
	held, err := w.heldOverlapping(p)
	if err != nil {
		return nil, err
	}
	for i := range held {
		ids = append(ids, held[i].Holder)
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// storeCard stores c as a message in the cards thread, which Open created
// and which never expires, holding the labels and where of the identity it
// is about, living THREAD_TTL.
func (w *wtx) storeCard(c *card) (*messageRow, error) {
	th, _, err := w.thread(cardsKey)
	if err != nil {
		return nil, err
	}
	ls := serverLease(w.now, w.s.threadTTL)
	id, err := w.s.t.messages.table.NextID(w.tx)
	if err != nil {
		return nil, err
	}
	a := &c.about
	subject := fmt.Sprintf("%s is now active on %s", a.Name, c.path)
	if c.kind == kindLeft {
		subject = fmt.Sprintf("%s has left %s", a.Name, c.path)
	}
	m := messageRow{
		ID: id, Thread: th.ID, Kind: c.kind, Path: c.path, PostedAt: w.now, Lease: ls,
		Sender: a.ID, SenderName: a.Name, Host: a.Host, OS: a.OS, Arch: a.Arch, Where: a.Where,
		Subject: subject,
		Body:    fmt.Sprintf("provider %s\npaths: %s\ndescription: %s", a.Provider, strings.Join(a.Paths, ", "), a.Description),
	}
	if err := w.s.t.messages.table.Put(w.tx, pairKey(th.ID, id), &m); err != nil {
		return nil, err
	}
	w.countPost(kindCards)
	return &m, nil
}

// countPost counts a stored post in a thread of kind once its transaction
// commits.
func (w *wtx) countPost(kind string) {
	w.tx.AfterCommit(func() { w.s.m.Posts.Inc(kind) })
}
