//go:build !windows

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/cplieger/bolttable"
)

// Receipt states.
const (
	ReceiptRead    = "read"
	ReceiptUnread  = "unread"
	ReceiptDormant = "dormant"
	ReceiptRetired = "retired"
)

// Receipt is one recipient of a post, as the post fanned out. A retired
// recipient has no identity row left, so its Host is empty and its
// Relation unknown.
type Receipt struct {
	// ReadAt is when the ack that passed the delivery ran.
	ReadAt   time.Time
	Name     string
	Host     string
	Relation string
	State    string
}

// Receipts answers who has read a post.
type Receipts struct {
	PostedAt   time.Time
	Subject    string
	Kind       string
	Recipients []Receipt
	Message    uint64
	// RegisteredSince counts, for an announcement, the identities
	// registered after the post, which never received it.
	RegisteredSince int
}

// Receipts lists the recipients of message msg, in fan-out order, for
// its sender or one of its recipients, a parent holding a copy included;
// a delegate recipient is listed only to the sender and its parent.
// Refusals: KindGone for a session with no live identity;
// KindNotFound for a message that does not exist or has expired;
// KindForbidden for any other caller.
func (s *Store) Receipts(ctx context.Context, c Caller, msg uint64) (Receipts, error) {
	if _, err := s.Seen(ctx, c); err != nil {
		return Receipts{}, err
	}
	var out Receipts
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		me, err := s.liveFor(r, c, now)
		if err != nil {
			return err
		}
		m, found, err := s.messageByID(r, msg, now)
		if err != nil {
			return err
		}
		if !found {
			return refuse(KindNotFound, fmt.Sprintf("no message %d: it never existed or has expired", msg))
		}
		rows, err := s.postDeliveries(r, msg, now)
		if err != nil {
			return err
		}
		if !receivedBy(&m, rows, me.ID) {
			return refuse(KindForbidden, fmt.Sprintf("only the sender and the recipients of message %d see its receipts", msg))
		}
		out, err = s.receiptsAs(r, &me, &m, rows, now)
		return err
	})
	return out, err
}

// receiptsAs lists rows' recipients as me sees them. A copy is not a
// recipient, and a delegate recipient shows only to the sender and its
// parent.
func (s *Store) receiptsAs(r *bolttable.ReadTx, me *identityRow, m *messageRow, rows []deliveryRow, now time.Time) (Receipts, error) {
	out := Receipts{PostedAt: m.PostedAt, Subject: m.Subject, Kind: m.Kind, Message: m.ID}
	for i := range rows {
		d := &rows[i]
		if d.Via != "" || (d.Parent != 0 && me.ID != m.Sender && me.ID != d.Parent) {
			continue
		}
		rc, err := s.receipt(r, me, m, d, now)
		if err != nil {
			return out, err
		}
		out.Recipients = append(out.Recipients, rc)
	}
	var err error
	if m.Kind == kindAnnounce {
		out.RegisteredSince, err = s.registeredSince(r, m.PostedAt, now)
	}
	return out, err
}

// messageByID reads message id, unless it has expired.
func (s *Store) messageByID(r *bolttable.ReadTx, id uint64, now time.Time) (messageRow, bool, error) {
	key, _, found, err := s.t.messageID.Lookup(r, be64(id))
	if err != nil || !found {
		return messageRow{}, false, err
	}
	m, found, err := s.t.messages.table.Get(r, key)
	if err != nil || !found || m.Lease.due(now) {
		return messageRow{}, false, err
	}
	return m, true, nil
}

// postDeliveries lists message msg's unexpired deliveries, copies
// included, in recipient id order.
func (s *Store) postDeliveries(r *bolttable.ReadTx, msg uint64, now time.Time) ([]deliveryRow, error) {
	var out []deliveryRow
	err := s.t.byMessage.Scan(r, bolttable.Range{Prefix: be64(msg)}, func(_, key, _ []byte) (bool, error) {
		d, found, err := s.t.deliveries.table.Get(r, key)
		if err == nil && found && !d.Lease.due(now) {
			out = append(out, d)
		}
		return err == nil, err
	})
	return out, err
}

func receivedBy(m *messageRow, rows []deliveryRow, id uint64) bool {
	if m.Sender == id {
		return true
	}
	for i := range rows {
		if rows[i].Agent == id {
			return true
		}
	}
	return false
}

// receipt is d's recipient as me sees it. Read is the recipient's read
// cursor at or past d, or, once its row is gone, the read_at d keeps.
func (s *Store) receipt(r *bolttable.ReadTx, me *identityRow, m *messageRow, d *deliveryRow, now time.Time) (Receipt, error) {
	out := Receipt{Name: d.Name, ReadAt: d.ReadAt}
	v, found, err := s.recipientRow(r, d.Agent, now)
	if err != nil {
		return out, err
	}
	if !found {
		out.Relation, out.State = relationUnknown, ReceiptRetired
		if !d.ReadAt.IsZero() {
			out.State = ReceiptRead
		}
		return out, nil
	}
	where, err := s.whereOf(r, &v)
	if err != nil {
		return out, err
	}
	out.Host = v.Host
	out.Relation = party{where: me.Where, id: me.ID}.relationTo(party{where: where, id: v.ID}, m.Path)
	switch {
	case v.Read >= d.Seq:
		out.State = ReceiptRead
	case v.State == stateDormant:
		out.State = ReceiptDormant
	default:
		out.State = ReceiptUnread
	}
	return out, nil
}

// recipientRow is identity id as a read sees it before the sweep; a
// delegate whose parent has expired or gone reads as gone, since the
// parent's expiry ends it.
func (s *Store) recipientRow(r *bolttable.ReadTx, id uint64, now time.Time) (identityRow, bool, error) {
	row, found, err := s.t.identities.table.Get(r, id)
	if err != nil || !found {
		return identityRow{}, false, err
	}
	v := settled(&row, now, s.dormantTTL)
	if v == nil {
		return identityRow{}, false, nil
	}
	if v.Parent != 0 {
		p, pfound, perr := s.recipientRow(r, v.Parent, now)
		if perr != nil || !pfound || p.State != stateLive {
			return identityRow{}, false, perr
		}
	}
	return *v, true, nil
}

// registeredSince counts the live and dormant identities other than
// delegates registered after t; a revival keeps registered_at, so a
// revived recipient is not among them.
func (s *Store) registeredSince(r *bolttable.ReadTx, t, now time.Time) (int, error) {
	n := 0
	err := s.t.identities.table.Scan(r, bolttable.Range{}, func(_ uint64, v *identityRow) (bool, error) {
		if v.Parent == 0 && v.RegisteredAt.After(t) && settled(v, now, s.dormantTTL) != nil {
			n++
		}
		return true, nil
	})
	return n, err
}
