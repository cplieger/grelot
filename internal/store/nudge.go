//go:build !windows

package store

import (
	"context"
	"time"

	"github.com/cplieger/bolttable"
	"github.com/cplieger/grelot/internal/obs"
)

// NudgeVia is the layer that sent a nudge, logged as nudge sent's via.
type NudgeVia string

// The layers that send a nudge. A hook's hold records its own.
const (
	NudgeByPush    NudgeVia = "push"
	NudgeByWatcher NudgeVia = "watcher"
	nudgeByHook    NudgeVia = "hook"
)

const msgNudgeSent = "nudge sent"

// reNudgeAfter is how long after the nth send the next one goes out. Its
// length caps a nudge at len+1 sends, the bound on what a session that
// ended without bye costs.
var reNudgeAfter = [...]time.Duration{3 * time.Minute, 15 * time.Minute, time.Hour}

// Nudge is one send of the fixed signal line, whose seq is Seq, the newest
// wake seq it covers. Count is the wake deliveries past Read, and Attempt
// which send of the outstanding nudge it is, 1 to 4. Oldest is the write
// time of the oldest of those deliveries.
type Nudge struct {
	Oldest   time.Time
	Identity uint64
	Seq      uint64
	Read     uint64
	Count    int
	Attempt  int
}

// NudgeState is an identity's nudge state at one instant. Due is the send due
// now; Next is when the outstanding nudge's next re-nudge comes due. Each
// is zero when there is none.
type NudgeState struct {
	Next time.Time
	Due  *Nudge
}

// Nudging is identity id's nudge state now. found is false when id has no
// live identity. A delegate, which is pull only, a session the SessionEnd
// report closed and @user are never nudged.
func (s *Store) Nudging(ctx context.Context, id uint64) (out NudgeState, found bool, err error) {
	err = s.read(ctx, func(r *bolttable.ReadTx) error {
		now := s.now()
		row, ok, gerr := s.t.identities.table.Get(r, id)
		if gerr != nil || !ok {
			return gerr
		}
		v := settled(&row, now, s.dormantTTL)
		if v == nil || v.State != stateLive {
			return nil
		}
		found = true
		out, err = s.nudging(r, v, now)
		return err
	})
	return out, found, err
}

// RecordNudge records n, a send Nudging reported due, once it went out.
// It re-checks in its transaction that n is still the send due, same
// attempt and same read, so of two senders racing on one nudge only the
// first records; recorded false means nothing was written, and the caller
// reads Nudging again.
func (s *Store) RecordNudge(ctx context.Context, n *Nudge, via NudgeVia) (got Nudge, recorded bool, err error) {
	err = s.update(ctx, func(w *wtx) error {
		me, found, serr := w.s.t.identities.settle(w, n.Identity)
		if serr != nil || !found || me.State != stateLive {
			return serr
		}
		now, nerr := w.s.nudging(w.r, &me, w.now)
		if nerr != nil || now.Due == nil || now.Due.Attempt != n.Attempt || now.Due.Read != n.Read {
			return nerr
		}
		got = *now.Due
		got.Seq = n.Seq
		recorded = true
		w.recordNudge(&me, &got, via)
		return w.putIdentity(me.ID, &me)
	})
	return got, recorded, err
}

// recordNudge sets n as me's outstanding nudge, sent now, logs the send
// and counts it once the transaction commits; the caller stores me.
func (w *wtx) recordNudge(me *identityRow, n *Nudge, via NudgeVia) {
	me.Nudged, me.Nudges, me.NudgedAt = n.Seq, n.Attempt, w.now
	w.wake(me.ID)
	delay := w.now.Sub(n.Oldest)
	w.logInfo(msgNudgeSent, "via", string(via), "attempt", n.Attempt, "seq", n.Seq,
		"oldest_delay_ms", delay.Milliseconds())
	attempt := obs.AttemptFirst
	if n.Attempt > 1 {
		attempt = obs.AttemptRepeat
	}
	name, at := me.Name, w.now
	w.tx.AfterCommit(func() {
		w.s.m.Nudges.Inc(string(via), attempt)
		w.s.m.NudgeDelay.Observe(delay.Seconds(), string(via))
		if attempt == obs.AttemptRepeat {
			w.s.m.Events.Add(obs.Event{At: at, Kind: obs.EventNudgeRepeat, Detail: string(via), Agent: name})
		}
	})
}

// nudging is me's nudge state at now. A nudge is outstanding while read is
// below nudged: then nothing new goes out until its re-nudge time, and a
// re-nudge goes out only while a wake event other than identity_expiring
// is unread, since that warning is sent once.
func (s *Store) nudging(r *bolttable.ReadTx, me *identityRow, now time.Time) (NudgeState, error) {
	var out NudgeState
	if me.Parent != 0 || me.Ended || isUser(me) {
		return out, nil
	}
	outstanding := me.Read < me.Nudged
	if outstanding && me.Nudges > len(reNudgeAfter) {
		return out, nil
	}
	u, err := s.pastRead(r, me, now)
	if err != nil {
		return out, err
	}
	b := u.wake()
	if outstanding {
		if at := me.NudgedAt.Add(reNudgeAfter[me.Nudges-1]); now.Before(at) {
			if b.renudges {
				out.Next = at
			}
			return out, nil
		}
	}
	due := &Nudge{Oldest: b.oldest, Identity: me.ID, Seq: b.newest, Read: me.Read, Count: b.count, Attempt: 1}
	switch {
	case b.count == 0:
	case !outstanding:
		out.Due = due
	case !b.renudges:
	default:
		due.Attempt = me.Nudges + 1
		out.Due = due
	}
	return out, nil
}

// wakeBatch is the wake deliveries past an identity's read.
type wakeBatch struct {
	oldest time.Time
	newest uint64
	count  int
	// renudges is a wake delivery other than identity_expiring.
	renudges bool
}

// add counts d when it wakes. Deliveries come in seq order.
func (b *wakeBatch) add(d *deliveryRow) {
	if !d.wakes() {
		return
	}
	if b.count == 0 {
		b.oldest = d.Written
	}
	b.count++
	b.newest = d.Seq
	b.renudges = b.renudges || d.Kind != kindIdentityExpiring
}
