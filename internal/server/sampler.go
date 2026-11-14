//go:build !windows

package server

import (
	"context"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/store"
)

// The sampler keeps one sample a minute for 72 hours, the longest
// Statistics range; a restart empties it.
const (
	sampleEvery   = time.Minute
	sampleSlots   = 72 * 60
	latencySample = 4096
	statsFor      = 30 * time.Second
)

// sample is one minute of the server: the requests answered in it (5xx
// counted apart, P95 over every route but the held waits) and the page's
// gauges read at its end.
type sample struct {
	At time.Time
	obs.Snapshot
	P95      time.Duration
	Requests int
	Errors   int
}

// sampler is the Statistics tab's in-memory record: the minute samples,
// the requests of the minute filling, and each identity's held turns.
type sampler struct {
	ring  []sample
	held  map[uint64]heldTurns
	stats map[time.Duration]cachedStats
	// minute is the start of the minute the request tally fills.
	minute   time.Time
	latency  []time.Duration
	next     int
	requests int
	errors   int
	timed    int
	mu       sync.Mutex
}

// heldTurns is one identity's turns the stop hook held since start, under
// its newest name.
type heldTurns struct {
	name string
	n    int
}

type cachedStats struct {
	at    time.Time
	stats store.Stats
}

func newSampler() *sampler {
	return &sampler{ring: make([]sample, 0, sampleSlots), held: map[uint64]heldTurns{}, stats: map[time.Duration]cachedStats{}}
}

// heldWait reports whether route holds by design, so its latency is the
// wait and not the server's.
func heldWait(route string) bool {
	return route == "/v1/events/wait" || route == "/v1/inbox" || route == "/api/events"
}

// request tallies one answered request into the minute filling.
func (sp *sampler) request(rm httpserver.RequestMetric) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.requests++
	if rm.Status >= 500 {
		sp.errors++
	}
	if heldWait(rm.Path) {
		return
	}
	sp.timed++
	if len(sp.latency) < latencySample {
		sp.latency = append(sp.latency, rm.Latency)
	} else if i := rand.IntN(sp.timed); i < latencySample { //nolint:gosec // a reservoir sample, not a secret
		sp.latency[i] = rm.Latency
	}
}

// tick closes the minute filling once now is past it, as a sample with
// snap, and starts the one now falls in; the first tick only starts it.
func (sp *sampler) tick(now time.Time, snap *obs.Snapshot) {
	minute := now.Truncate(sampleEvery)
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if !minute.After(sp.minute) {
		return
	}
	if !sp.minute.IsZero() {
		sp.add(&sample{At: sp.minute, Snapshot: *snap, Requests: sp.requests, Errors: sp.errors, P95: p95(sp.latency)})
	}
	sp.minute, sp.requests, sp.errors, sp.timed, sp.latency = minute, 0, 0, 0, sp.latency[:0]
}

func (sp *sampler) add(s *sample) {
	if len(sp.ring) < sampleSlots {
		sp.ring = append(sp.ring, *s)
	} else {
		sp.ring[sp.next] = *s
	}
	sp.next = (sp.next + 1) % sampleSlots
}

// series lists the samples at or after from and before to, oldest first.
func (sp *sampler) series(from, to time.Time) []sample {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	out := []sample{}
	start := 0
	if len(sp.ring) == sampleSlots {
		start = sp.next
	}
	for i := range len(sp.ring) {
		s := sp.ring[(start+i)%len(sp.ring)]
		if !s.At.Before(from) && s.At.Before(to) {
			out = append(out, s)
		}
	}
	return out
}

// p95 is the nearest-rank 95th percentile of ds, 0 with none; it sorts ds.
func p95(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	slices.Sort(ds)
	return ds[(95*len(ds)+99)/100-1]
}

// heldTurn counts one turn the stop hook held for identity id, now called
// name.
func (sp *sampler) heldTurn(id uint64, name string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	h := sp.held[id]
	sp.held[id] = heldTurns{name: name, n: h.n + 1}
}

// heldTurns maps each identity's newest name to its held turns since
// start; two identities that held one name add up.
func (sp *sampler) heldTurns() map[string]int {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	out := make(map[string]int, len(sp.held))
	for _, h := range sp.held {
		out[h.name] += h.n
	}
	return out
}

// sample is the sweeper beat's part: it reads the page's gauges once the
// minute has turned.
func (s *Server) sample(ctx context.Context) {
	now := s.now()
	s.sampler.mu.Lock()
	due := now.Truncate(sampleEvery).After(s.sampler.minute)
	s.sampler.mu.Unlock()
	if !due {
		return
	}
	snap, err := s.metrics.Snapshot(ctx)
	if err != nil {
		s.log.Warn(msgSampleFailed, "error", err.Error())
	}
	s.sampler.tick(now, &snap)
}

const msgSampleFailed = "statistics sample failed"

// statsReport is the Statistics tab's data over span: the store's part,
// the window's minute samples, its delivery problems with their counts,
// and the turns held per agent since start.
type statsReport struct {
	Events      []obs.Event
	EventCounts map[obs.EventKind]int
	HeldTurns   map[string]int
	Samples     []sample
	store.Stats
}

func (s *Server) statsReport(ctx context.Context, span time.Duration) (statsReport, error) {
	st, err := s.stats(ctx, span)
	if err != nil {
		return statsReport{}, err
	}
	w := st.Window
	return statsReport{
		Stats: st, Samples: s.sampler.series(w.Start, w.End), HeldTurns: s.sampler.heldTurns(),
		Events: s.metrics.Events.Recent(w.Start, w.End), EventCounts: s.metrics.Events.Counts(w.Start, w.End),
	}, nil
}

// stats is the store's Stats over span, read at most once per statsFor.
func (s *Server) stats(ctx context.Context, span time.Duration) (store.Stats, error) {
	now := s.now()
	s.sampler.mu.Lock()
	c, ok := s.sampler.stats[span]
	s.sampler.mu.Unlock()
	if ok && now.Sub(c.at) < statsFor {
		return c.stats, nil
	}
	st, err := s.store.Stats(ctx, span)
	if err != nil {
		return st, err
	}
	s.sampler.mu.Lock()
	s.sampler.stats[span] = cachedStats{at: now, stats: st}
	s.sampler.mu.Unlock()
	return st, nil
}
