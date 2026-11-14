//go:build !windows

package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/cplieger/bolttable"
)

// AgentCard is one identity for the web UI's agent page: its card, its
// grants and queue entries (grants first, each part in id order), and its
// live delegates' handles, sorted.
type AgentCard struct {
	Reservations []ReservationEntry
	Delegates    []string
	Identity
}

// HistoryPost is a post an identity sent or received, with the
// conversation it belongs to.
type HistoryPost struct {
	Conversation ConversationThread
	ConversationPost
}

// AgentHistoryPage is one page of AgentHistory, newest first. Next is the
// Before of the older page, 0 on the oldest.
type AgentHistoryPage struct {
	Posts []HistoryPost
	Next  uint64
}

// Agents lists the live and dormant identities but @user and delegates,
// by name.
func (s *Store) Agents(ctx context.Context) ([]Identity, error) {
	var out []Identity
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		return s.t.identities.table.Scan(r, bolttable.Range{}, func(_ uint64, v *identityRow) (bool, error) {
			if v = settled(v, now, s.dormantTTL); v != nil && v.Parent == 0 && !isUser(v) {
				out = append(out, s.view(v))
			}
			return true, nil
		})
	})
	slices.SortFunc(out, func(a, b Identity) int { return cmp.Compare(a.Name, b.Name) })
	return out, err
}

// AgentCard reads the live or dormant identity called name. Refusals:
// KindNotFound for a name no identity holds.
func (s *Store) AgentCard(ctx context.Context, name string) (AgentCard, error) {
	var out AgentCard
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		wr, err := s.webRead(r)
		if err == nil {
			out, err = wr.card(name)
		}
		return err
	})
	return out, err
}

func (wr *webReader) card(name string) (AgentCard, error) {
	row, err := wr.agent(name)
	if err != nil {
		return AgentCard{}, err
	}
	out := AgentCard{Identity: wr.s.view(&row)}
	var ids []uint64
	err = wr.s.t.resvHolder.Scan(wr.r, bolttable.Exact(be64(row.ID)), func(_ []byte, id uint64, _ []byte) (bool, error) {
		ids = append(ids, id)
		return true, nil
	})
	if err != nil {
		return out, err
	}
	if out.Reservations, err = wr.reservations(ids); err != nil {
		return out, err
	}
	out.Delegates, err = wr.delegates(row.ID)
	return out, err
}

// AgentHistory reads the posts the identity called name sent or received,
// a parent's copies of its delegates' mail left out, older than before (0
// for the newest), up to limit, newest first. Refusals: KindInvalid for a
// limit outside 0 to maxWebLimit; KindNotFound for a name no identity holds.
func (s *Store) AgentHistory(ctx context.Context, name string, before uint64, limit int) (AgentHistoryPage, error) {
	limit, lerr := webLimit(limit, defaultPostsLimit)
	if lerr != nil {
		return AgentHistoryPage{}, lerr
	}
	var out AgentHistoryPage
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		wr, err := s.webRead(r)
		if err == nil {
			out, err = wr.history(name, before, limit)
		}
		return err
	})
	return out, err
}

func (wr *webReader) history(name string, before uint64, limit int) (AgentHistoryPage, error) {
	var out AgentHistoryPage
	row, err := wr.agent(name)
	if err != nil {
		return out, err
	}
	sent, err := wr.sentBy(row.ID, before, limit)
	if err != nil {
		return out, err
	}
	got, err := wr.receivedBy(row.ID, before, limit)
	if err != nil {
		return out, err
	}
	ms := slices.Concat(sent, got)
	slices.SortFunc(ms, func(a, b messageRow) int { return cmp.Compare(b.ID, a.ID) })
	if len(ms) > limit {
		ms = ms[:limit]
		out.Next = ms[limit-1].ID
	}
	out.Posts, err = wr.historyPosts(ms)
	return out, err
}

func (wr *webReader) agent(name string) (identityRow, error) {
	row, found, err := wr.s.lookupIdentity(wr.r, wr.s.t.byName, name)
	if err == nil && !found {
		err = refuse(KindNotFound, fmt.Sprintf("no agent called @%s: it never registered or has expired", name))
	}
	return row, err
}

// delegates lists, sorted, the handles of parent's live delegates.
func (wr *webReader) delegates(parent uint64) ([]string, error) {
	var out []string
	err := wr.s.t.byParent.Scan(wr.r, bolttable.Exact(be64(parent)), func(_ []byte, id uint64, _ []byte) (bool, error) {
		v, found, err := wr.s.recipientRow(wr.r, id, wr.now)
		if found {
			out = append(out, v.Name)
		}
		return err == nil, err
	})
	slices.Sort(out)
	return out, err
}

// sentBy reads up to limit+1 of id's unexpired posts older than before,
// newest first.
func (wr *webReader) sentBy(id, before uint64, limit int) ([]messageRow, error) {
	var out []messageRow
	err := wr.s.t.bySender.Scan(wr.r, bolttable.Range{Prefix: be64(id), Reverse: true}, func(_, k, _ []byte) (bool, error) {
		m, found, err := wr.s.t.messages.table.Get(wr.r, k)
		if err != nil {
			return false, err
		}
		if found && !m.Lease.due(wr.now) && (before == 0 || m.ID < before) {
			out = append(out, m)
		}
		return len(out) <= limit, nil
	})
	return out, err
}

// receivedBy reads the messages of up to limit+1 of id's unexpired post
// deliveries older than before, newest first, copies left out.
func (wr *webReader) receivedBy(id, before uint64, limit int) ([]messageRow, error) {
	var out []messageRow
	err := wr.s.t.deliveries.table.Scan(wr.r, bolttable.Range{Prefix: be64(id), Reverse: true}, func(_ []byte, d *deliveryRow) (bool, error) {
		if d.Lease.due(wr.now) || !isPost(d.Kind) || d.Via != "" || (before != 0 && d.Message >= before) {
			return true, nil
		}
		m, found, err := wr.s.t.messages.table.Get(wr.r, pairKey(d.Thread, d.Message))
		switch {
		case err != nil:
			return false, err
		case !found:
			// A delivery expires with its message, as entry relies on.
			return false, fmt.Errorf("store: delivery %d of identity %d names message %d, which is gone", d.Seq, id, d.Message)
		}
		out = append(out, m)
		return len(out) <= limit, nil
	})
	return out, err
}

// historyPosts reads each of ms with its read count and conversation.
func (wr *webReader) historyPosts(ms []messageRow) ([]HistoryPost, error) {
	out := make([]HistoryPost, 0, len(ms))
	for i := range ms {
		p, err := wr.post(&ms[i])
		if err != nil {
			return nil, err
		}
		th, found, err := wr.s.t.threads.table.Get(wr.r, ms[i].Thread)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("store: message %d names thread %d, which is gone", ms[i].ID, ms[i].Thread)
		}
		ct, err := wr.conversationThread(&th)
		if err != nil {
			return nil, err
		}
		out = append(out, HistoryPost{ConversationPost: p, Conversation: ct})
	}
	return out, nil
}
