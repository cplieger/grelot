package store

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/cplieger/bolttable"
)

// lease is the expiry every leased row carries. A zero Expires never
// expires; a zero WarnAt has no warning due.
type lease struct {
	Expires time.Time `json:"expires,omitzero"`
	WarnAt  time.Time `json:"warn_at,omitzero"`
}

func (l lease) due(now time.Time) bool { return !l.Expires.IsZero() && !l.Expires.After(now) }

// leasePolicy is one table's lease rule. Zero bounds leave the TTL to the
// server; a nil lead warns nothing.
type leasePolicy struct {
	lead     func(ttl time.Duration) time.Duration
	name     string
	min, max time.Duration
}

// leadOf warns min(c, ttl/2) ahead, so a short lease is warned halfway.
func leadOf(c time.Duration) func(time.Duration) time.Duration {
	return func(ttl time.Duration) time.Duration { return min(c, ttl/2) }
}

var (
	identityPolicy = leasePolicy{name: "identities", min: 15 * time.Minute, max: 24 * time.Hour, lead: leadOf(30 * time.Minute)}
	threadPolicy   = leasePolicy{name: "threads"}
	messagePolicy  = leasePolicy{name: "messages"}
	deliveryPolicy = leasePolicy{name: "deliveries"}
)

// grant returns the lease of a row renewed at now for ttl, never past
// limit when limit is set. A TTL outside the policy's bounds is a
// KindInvalid refusal naming them.
func (p leasePolicy) grant(now time.Time, ttl time.Duration, limit time.Time) (lease, error) {
	if p.max > 0 && (ttl < p.min || ttl > p.max) {
		if ttl == 0 {
			return lease{}, refuse(KindInvalid, fmt.Sprintf("a ttl is required: pass --ttl <d>, from %s to %s", fmtDur(p.min), fmtDur(p.max)))
		}
		return lease{}, refuse(KindInvalid, fmt.Sprintf("ttl %s is outside the bounds: pass --ttl <d>, from %s to %s", fmtDur(ttl), fmtDur(p.min), fmtDur(p.max)))
	}
	l := lease{Expires: now.Add(ttl)}
	if !limit.IsZero() && l.Expires.After(limit) {
		l.Expires = limit
	}
	if p.lead != nil {
		l.WarnAt = l.Expires.Add(-p.lead(ttl))
		if l.WarnAt.Before(now) {
			l.WarnAt = now
		}
	}
	return l, nil
}

// fmtDur prints a duration the way --ttl accepts it, without zero units.
func fmtDur(d time.Duration) string {
	s := d.String()
	for _, z := range []string{"m0s", "h0m"} {
		if t, ok := strings.CutSuffix(s, z); ok {
			s = t + z[:1]
		}
	}
	return s
}

// timeKey is an expiry index key: big-endian nanoseconds, so byte order is
// time order.
func timeKey(t time.Time) []byte { return binary.BigEndian.AppendUint64(nil, uint64(t.UnixNano())) }

// sweeper is one table's lease seen from the sweep, whatever its row type.
type sweeper interface {
	policyName() string
	sweep(w *wtx, budget int) (int, error)
}

// leased is the one lease implementation: a table, its expires and warn_at
// indexes, its policy, and the hooks the sweep and the eager path run.
type leased[K, V any] struct {
	table    *bolttable.Table[K, V]
	expires  *bolttable.Index[K, V]
	warns    *bolttable.Index[K, V]
	lease    func(*V) *lease
	onWarn   func(w *wtx, k K, v *V) error
	onExpire func(w *wtx, k K, v *V) error
	policy   leasePolicy
}

var _ sweeper = (*leased[uint64, threadRow])(nil)

func defineLeased[K, V any](t *bolttable.Table[K, V], p leasePolicy, get func(*V) *lease) (*leased[K, V], error) {
	l := &leased[K, V]{table: t, policy: p, lease: get}
	var err error
	l.expires, err = bolttable.DefineIndex(t, p.name+".expires", bolttable.IndexOptions{Version: 1},
		func(_ K, v *V) []bolttable.Entry { return timeEntry(get(v).Expires) })
	if err != nil {
		return nil, err
	}
	if p.lead != nil {
		l.warns, err = bolttable.DefineIndex(t, p.name+".warn_at", bolttable.IndexOptions{Version: 1},
			func(_ K, v *V) []bolttable.Entry { return timeEntry(get(v).WarnAt) })
		if err != nil {
			return nil, err
		}
	}
	return l, nil
}

func timeEntry(t time.Time) []bolttable.Entry {
	if t.IsZero() {
		return nil
	}
	return []bolttable.Entry{{Key: timeKey(t)}}
}

func (l *leased[K, V]) policyName() string { return l.policy.name }

// sweep expires, then warns, at most budget due rows, and reports how many
// it handled. Expiring first means a row past both times is never warned.
func (l *leased[K, V]) sweep(w *wtx, budget int) (int, error) {
	n, err := l.expireDue(w, budget)
	if err != nil || l.warns == nil || n >= budget {
		return n, err
	}
	m, err := l.warnDue(w, budget-n)
	return n + m, err
}

func (l *leased[K, V]) expireDue(w *wtx, budget int) (int, error) {
	keys, err := dueKeys(w, l.expires, budget)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, k := range keys {
		v, found, gerr := l.table.Get(w.r, k)
		if gerr != nil {
			return n, gerr
		}
		if !found || !l.lease(&v).due(w.now) {
			continue
		}
		if err := l.onExpire(w, k, &v); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// warnDue clears each due warn_at before on_warn runs, so a warning is
// written once.
func (l *leased[K, V]) warnDue(w *wtx, budget int) (int, error) {
	keys, err := dueKeys(w, l.warns, budget)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, k := range keys {
		v, found, gerr := l.table.Get(w.r, k)
		if gerr != nil {
			return n, gerr
		}
		ls := l.lease(&v)
		if !found || ls.WarnAt.IsZero() || ls.WarnAt.After(w.now) || ls.due(w.now) {
			continue
		}
		ls.WarnAt = time.Time{}
		if err := l.table.Put(w.tx, k, &v); err != nil {
			return n, err
		}
		if err := l.onWarn(w, k, &v); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// dueKeys lists up to limit rows whose index time is at or before now.
func dueKeys[K, V any](w *wtx, ix *bolttable.Index[K, V], limit int) ([]K, error) {
	bound := timeKey(w.now)
	var ks []K
	err := ix.Scan(w.r, bolttable.Range{}, func(ikey []byte, k K, _ []byte) (bool, error) {
		if bytes.Compare(ikey, bound) > 0 {
			return false, nil
		}
		ks = append(ks, k)
		return len(ks) < limit, nil
	})
	return ks, err
}

// settle reads row k for a write, first running its due on_expire in the
// same transaction, so a write never acts on a row the sweeper has not
// reached yet. found reports whether a row is there afterwards. A row can
// be due twice over (an identity past both its expiry and its purge), so
// it loops; every on_expire must delete the row or move its expiry later,
// or this never returns.
func (l *leased[K, V]) settle(w *wtx, k K) (v V, found bool, err error) {
	for {
		v, found, err = l.table.Get(w.r, k)
		if err != nil || !found || !l.lease(&v).due(w.now) {
			return v, found, err
		}
		if err := l.onExpire(w, k, &v); err != nil {
			return v, false, err
		}
	}
}
