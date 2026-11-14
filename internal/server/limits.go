//go:build !windows

package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cplieger/grelot/internal/obs"
	"golang.org/x/time/rate"
)

// Limits caps what one machine key, and every key together, may ask of
// /v1. Each zero field takes its default.
type Limits struct {
	// Rate is each key's sustained requests a second, and Burst how many
	// it may send at once.
	Rate  float64
	Burst int
	// KeyWaits caps the standing and in-turn waits one key holds at once,
	// and ServerWaits the waits every key holds together.
	KeyWaits    int
	ServerWaits int
}

// defaultLimits holds a key to 128 waits, a machine running about 64
// sessions that each hold a watcher's wait and an in-turn wait.
var defaultLimits = Limits{Rate: 20, Burst: 40, KeyWaits: 128, ServerWaits: 1024}

func (l Limits) orDefault() Limits {
	if l.Rate <= 0 {
		l.Rate = defaultLimits.Rate
	}
	if l.Burst <= 0 {
		l.Burst = defaultLimits.Burst
	}
	if l.KeyWaits <= 0 {
		l.KeyWaits = defaultLimits.KeyWaits
	}
	if l.ServerWaits <= 0 {
		l.ServerWaits = defaultLimits.ServerWaits
	}
	return l
}

// keyRates is a token bucket per machine key, in memory.
type keyRates struct {
	buckets map[int64]*rate.Limiter
	limit   rate.Limit
	burst   int
	mu      sync.Mutex
}

func newKeyRates(l Limits) *keyRates {
	return &keyRates{buckets: map[int64]*rate.Limiter{}, limit: rate.Limit(l.Rate), burst: l.Burst}
}

// take spends one of key's tokens at now; refused, it returns how long
// until one is back.
func (k *keyRates) take(key int64, now time.Time) (time.Duration, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	b, ok := k.buckets[key]
	if !ok {
		b = rate.NewLimiter(k.limit, k.burst)
		k.buckets[key] = b
	}
	r := b.ReserveN(now, 1)
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return d, false
	}
	return 0, true
}

// prune forgets every key whose bucket is full again, which take would
// build anew.
func (k *keyRates) prune(now time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for key, b := range k.buckets {
		if b.TokensAt(now) >= float64(k.burst) {
			delete(k.buckets, key)
		}
	}
}

// waitCaps counts the waits held, per key and in all.
type waitCaps struct {
	byKey  map[int64]int
	perKey int
	total  int
	held   int
	mu     sync.Mutex
}

func newWaitCaps(l Limits) *waitCaps {
	return &waitCaps{byKey: map[int64]int{}, perKey: l.KeyWaits, total: l.ServerWaits}
}

// hold admits one wait of key and returns its release, which must run
// once; refused, it returns the limit that refused it.
func (w *waitCaps) hold(key int64) (release func(), limit string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.byKey[key] >= w.perKey:
		return nil, obs.LimitWaitsKey
	case w.held >= w.total:
		return nil, obs.LimitWaitsServer
	}
	w.byKey[key]++
	w.held++
	return sync.OnceFunc(func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.held--
		w.byKey[key]--
		if w.byKey[key] == 0 {
			delete(w.byKey, key)
		}
	}), ""
}

// rateLimited answers a key past its request rate, recording it.
func (s *Server) rateLimited(retry time.Duration) error {
	s.metrics.RequestsLimited.Inc(obs.LimitRate)
	retry = retryAfter(retry)
	return &apiError{
		status: http.StatusTooManyRequests, code: "rate_limited", retryAfter: retry,
		msg: "over " + strconv.FormatFloat(s.limits.Rate, 'f', -1, 64) + " requests a second from this machine's key: send again in " + retry.String(),
	}
}

// holdWait admits one held wait of key, or refuses it with 429
// too_many_waits, recording the limit that refused it.
func (s *Server) holdWait(key int64) (release func(), err error) {
	release, limit := s.waits.hold(key)
	if limit == "" {
		return release, nil
	}
	s.metrics.RequestsLimited.Inc(limit)
	msg := "this machine's key holds " + strconv.Itoa(s.limits.KeyWaits) + " waits at once, its most: the client retries once one has answered"
	if limit == obs.LimitWaitsServer {
		msg = "the server holds " + strconv.Itoa(s.limits.ServerWaits) + " waits at once, its most: retry once one has answered"
	}
	return nil, &apiError{status: http.StatusTooManyRequests, code: "too_many_waits", msg: msg}
}
