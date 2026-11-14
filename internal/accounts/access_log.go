//go:build !windows

package accounts

import (
	"context"
	"errors"
	"math"
	"net/netip"
	"slices"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/access/account"
	"github.com/cplieger/bolttable"
)

var (
	errLogClass     = errors.New("accounts: log entry of no account.LogClasses class")
	errLogLimit     = errors.New("accounts: log limit below 1")
	errLogCriterion = errors.New("accounts: log prune needs exactly one of a positive Keep and a non-zero Before")
)

// logTables are the security log (account.LogStore): entries keyed by Seq,
// which the table's bbolt sequence assigns inside the one writer
// transaction, so Seq order is commit order and a prune never lowers it.
type logTables struct {
	entries *bolttable.Table[uint64, logRow]
	byClass *bolttable.Index[uint64, logRow]
	counts  map[account.LogClass]*bolttable.Counter[uint64, logRow]
	// stats holds a row per class that has ever held an entry, with its
	// Keep evictions.
	stats *bolttable.Table[[]byte, logStatsRow]
}

type logRow struct {
	IP             netip.Addr
	EventAt        time.Time
	At             time.Time
	Method         access.Method
	Detail         string
	Kind           account.EventKind
	ActorKeySuffix string
	Role           access.Role
	ActorSession   string
	ActorOutOfBand string
	Class          account.LogClass
	ActorUserAgent string
	ActorLabel     string
	SessionIDs     []string
	ActorUserID    int64
	ActorKeyID     int64
	UserID         int64
	KeyID          int64
	PasskeyID      int64
	Trusted        bool
}

type logStatsRow struct {
	Evicted uint64
}

func toLogRow(e *account.LogEntry) logRow {
	ev := &e.Event
	return logRow{
		At: e.At, EventAt: ev.At, IP: e.IP, Class: e.Class, ActorLabel: e.ActorLabel, ActorUserAgent: e.ActorUserAgent,
		ActorKeySuffix: e.ActorKeySuffix, Detail: e.Detail, Kind: ev.Kind, Method: ev.Method, Role: ev.Role,
		SessionIDs: ev.SessionIDs, ActorSession: ev.Actor.SessionID, ActorOutOfBand: ev.Actor.OutOfBand,
		ActorUserID: ev.Actor.UserID, ActorKeyID: ev.Actor.KeyID, UserID: ev.UserID, KeyID: ev.KeyID,
		PasskeyID: ev.PasskeyID, Trusted: ev.Trusted,
	}
}

func (v *logRow) entry(seq uint64) account.LogEntry {
	return account.LogEntry{
		At: v.At, IP: v.IP, Class: v.Class, ActorLabel: v.ActorLabel, ActorUserAgent: v.ActorUserAgent,
		ActorKeySuffix: v.ActorKeySuffix, Detail: v.Detail,
		Event: account.Event{
			At: v.EventAt, Kind: v.Kind, Method: v.Method, Role: v.Role, SessionIDs: v.SessionIDs,
			Actor: account.EventActor{
				SessionID: v.ActorSession, OutOfBand: v.ActorOutOfBand, UserID: v.ActorUserID, KeyID: v.ActorKeyID,
			},
			UserID: v.UserID, KeyID: v.KeyID, PasskeyID: v.PasskeyID, Seq: seq, Trusted: v.Trusted,
		},
	}
}

func defineLog(ns *bolttable.Namespace, d *declarations) logTables {
	t := logTables{counts: map[account.LogClass]*bolttable.Counter[uint64, logRow]{}}
	set(d, &t.entries)(bolttable.Define(ns, prefixLog, "log", bolttable.Uint64Key{}, bolttable.JSON[logRow](1),
		bolttable.TableOptions{Silent: true}))
	set(d, &t.byClass)(bolttable.DefineIndex(t.entries, indexLogClass, bolttable.IndexOptions{Version: 1},
		func(_ uint64, v *logRow) []bolttable.Entry { return entry([]byte(v.Class)) }))
	for _, class := range account.LogClasses() {
		var c *bolttable.Counter[uint64, logRow]
		set(d, &c)(bolttable.DefineCounter(t.entries, counterLogPrefix+string(class), 1, func(_ uint64, v *logRow) int64 {
			if v.Class == class {
				return 1
			}
			return 0
		}))
		t.counts[class] = c
	}
	set(d, &t.stats)(bolttable.Define(ns, prefixLogStats, "log_stats", bolttable.BytesKey{}, bolttable.JSON[logStatsRow](1),
		bolttable.TableOptions{Silent: true}))
	return t
}

// AppendLog implements account.LogStore.
func (w *writer) AppendLog(_ context.Context, e *account.LogEntry) error {
	if !slices.Contains(account.LogClasses(), e.Class) {
		return errLogClass
	}
	seq, err := w.t.log.entries.NextID(w.tx)
	if err != nil {
		return err
	}
	if seq > uint64(maxID) {
		return errIDsExhausted
	}
	row := toLogRow(e)
	if err := w.t.log.entries.Put(w.tx, seq, &row); err != nil {
		return err
	}
	if _, held, err := w.t.log.stats.Get(w.r, []byte(e.Class)); err != nil {
		return err
	} else if !held {
		if err := w.t.log.stats.Put(w.tx, []byte(e.Class), &logStatsRow{}); err != nil {
			return err
		}
	}
	e.Event.Seq = seq
	return nil
}

// ListLog implements account.LogStore.
func (r reader) ListLog(_ context.Context, rg account.LogRange) ([]account.LogEntry, error) {
	if rg.Limit < 1 {
		return nil, errLogLimit
	}
	if rg.AfterSeq == math.MaxUint64 {
		return []account.LogEntry{}, nil
	}
	span := bolttable.Range{From: bolttable.Uint64Key{}.AppendKey(nil, rg.AfterSeq+1), Reverse: rg.Descending}
	if rg.BeforeSeq != 0 {
		span.To = bolttable.Uint64Key{}.AppendKey(nil, rg.BeforeSeq)
	}
	out := []account.LogEntry{}
	err := r.t.log.entries.Scan(r.r, span, func(seq uint64, v *logRow) (bool, error) {
		out = append(out, v.entry(seq))
		return len(out) < rg.Limit, nil
	})
	return out, err
}

// PruneLog implements account.LogStore.
func (w *writer) PruneLog(_ context.Context, p account.LogPrune) (int, error) {
	switch {
	case p.Limit < 1:
		return 0, errLogLimit
	case p.Keep < 0, (p.Keep > 0) == !p.Before.IsZero():
		return 0, errLogCriterion
	}
	stats, held, err := w.t.log.stats.Get(w.r, []byte(p.Class))
	if err != nil || !held {
		return 0, err
	}
	seqs, err := w.doomed(p)
	if err != nil {
		return 0, err
	}
	n, err := deleteAll(w.tx, w.t.log.entries, seqs)
	if err != nil || p.Keep == 0 || n == 0 {
		return n, err
	}
	stats.Evicted += uint64(n) //nolint:gosec // G115: n counts deleted rows, never negative
	return n, w.t.log.stats.Put(w.tx, []byte(p.Class), &stats)
}

// doomed is the Seqs p removes from its class, lowest first: those past the
// newest p.Keep, or those older than p.Before, at most p.Limit.
func (w *writer) doomed(p account.LogPrune) ([]uint64, error) {
	limit, match := p.Limit, func(*logRow) bool { return true }
	if p.Keep > 0 {
		count, err := w.t.log.counts[p.Class].Value(w.r)
		if err != nil {
			return nil, err
		}
		limit = min(p.Limit, int(count)-p.Keep)
	} else {
		match = func(v *logRow) bool { return v.At.Before(p.Before) }
	}
	if limit < 1 {
		return nil, nil
	}
	var seqs []uint64
	err := w.t.log.byClass.Scan(w.r, bolttable.Exact([]byte(p.Class)), func(_ []byte, seq uint64, _ []byte) (bool, error) {
		v, found, err := w.t.log.entries.Get(w.r, seq)
		if err != nil {
			return false, err
		}
		if found && match(&v) {
			seqs = append(seqs, seq)
		}
		return len(seqs) < limit, nil
	})
	return seqs, err
}

// LogStats implements account.LogStore.
func (r reader) LogStats(context.Context) ([]account.LogClassStats, error) {
	out := []account.LogClassStats{}
	for _, class := range account.LogClasses() {
		stats, held, err := r.t.log.stats.Get(r.r, []byte(class))
		if err != nil {
			return nil, err
		}
		if !held {
			continue
		}
		count, err := r.t.log.counts[class].Value(r.r)
		if err != nil {
			return nil, err
		}
		out = append(out, account.LogClassStats{Class: class, Count: int(count), Evicted: stats.Evicted})
	}
	return out, nil
}

// AppendLog implements account.LogStore.
func (s *Store) AppendLog(ctx context.Context, e *account.LogEntry) error {
	return s.write(ctx, func(w *writer) error { return w.AppendLog(ctx, e) })
}

// ListLog implements account.LogStore.
func (s *Store) ListLog(ctx context.Context, rg account.LogRange) ([]account.LogEntry, error) {
	return readValue(ctx, s, func(r reader) ([]account.LogEntry, error) { return r.ListLog(ctx, rg) })
}

// PruneLog implements account.LogStore.
func (s *Store) PruneLog(ctx context.Context, p account.LogPrune) (int, error) {
	return writeResult(ctx, s, func(w *writer) (int, error) { return w.PruneLog(ctx, p) })
}

// LogStats implements account.LogStore.
func (s *Store) LogStats(ctx context.Context) ([]account.LogClassStats, error) {
	return readValue(ctx, s, func(r reader) ([]account.LogClassStats, error) { return r.LogStats(ctx) })
}
