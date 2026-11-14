//go:build !windows

package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/cplieger/bolttable"
)

// The Statistics tab's ranges: 6 hours in 15-minute buckets, 24 and 72
// hours in hourly ones.
const (
	StatsRange6h  = 6 * time.Hour
	StatsRange24h = 24 * time.Hour
	StatsRange72h = 72 * time.Hour
)

// StatsWindow is a range aligned to its buckets: End is the end of the
// bucket now falls in, which is still filling, and Start is the range's
// length before it.
type StatsWindow struct {
	Start  time.Time
	End    time.Time
	Bucket time.Duration
}

// StatsWindowAt aligns span, one of the StatsRange values, at now.
// Refusals: KindInvalid for any other span.
func StatsWindowAt(now time.Time, span time.Duration) (StatsWindow, error) {
	var bucket time.Duration
	switch span {
	case StatsRange6h:
		bucket = 15 * time.Minute
	case StatsRange24h, StatsRange72h:
		bucket = time.Hour
	default:
		return StatsWindow{}, refuse(KindInvalid, "the range must be 6h, 24h or 72h")
	}
	end := now.Truncate(bucket).Add(bucket)
	return StatsWindow{Start: end.Add(-span), End: end, Bucket: bucket}, nil
}

// Buckets is the number of buckets in w.
func (w StatsWindow) Buckets() int { return int(w.End.Sub(w.Start) / w.Bucket) }

// Index is the bucket t falls in, false outside w.
func (w StatsWindow) Index(t time.Time) (int, bool) {
	if t.Before(w.Start) || !t.Before(w.End) {
		return 0, false
	}
	return int(t.Sub(w.Start) / w.Bucket), true
}

// PostCounts counts posts by thread kind.
type PostCounts struct {
	Announce, Path, Direct int
}

// AgentStats is one agent's activity in a window: the posts it sent, at
// PostTimes oldest first, and the deliveries it read with the longest wait
// from post to read among them.
type AgentStats struct {
	Name        string
	PostTimes   []time.Time
	LongestWait time.Duration
	Posts       int
	Read        int
}

// ThreadStats is one thread's activity in a window: its posts, the
// identities that sent or received them, and their deliveries.
type ThreadStats struct {
	ConversationThread
	Posts        int
	Participants int
	Deliveries   int
}

// DelayHistogram bins the post-to-read delays of the deliveries read in a
// window. Bin i is [Edges[i], Edges[i+1]), the last closed; Median and P95
// are nearest-rank percentiles of the delays, 0 with none. Unread counts
// the wake deliveries live identities have not read, as Census does.
type DelayHistogram struct {
	Edges  []time.Duration
	Counts []int
	Median time.Duration
	P95    time.Duration
	Read   int
	Unread int
}

// Stats is the store's part of the Statistics tab over Window. Posts has
// one entry per bucket. Agents holds every identity live during the
// window, silent ones included, and every other sender or reader in it,
// by most posts, then most reads, then name; Threads every thread with a
// post in it, by most posts, then most deliveries, then id. @user's posts
// count in Posts and Threads, but it is no agent and its reads are no
// delay.
type Stats struct {
	Window  StatsWindow
	Posts   []PostCounts
	Agents  []AgentStats
	Threads []ThreadStats
	Delays  DelayHistogram
}

// delayLadder is the language's duration ladder the delay bins follow.
var delayLadder = []time.Duration{
	0, time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second,
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour,
	24 * time.Hour, 72 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour, 30 * 24 * time.Hour, 90 * 24 * time.Hour,
}

// Stats reads the window of span, one of the StatsRange values, ending
// in the bucket now falls in. Refusals: KindInvalid for any other span.
func (s *Store) Stats(ctx context.Context, span time.Duration) (Stats, error) {
	w, err := StatsWindowAt(s.now(), span)
	if err != nil {
		return Stats{}, err
	}
	var out Stats
	err = s.read(ctx, func(r *bolttable.ReadTx) error {
		wr, rerr := s.webRead(r)
		if rerr != nil {
			return rerr
		}
		st := statsScan{wr: wr, w: w, agents: map[uint64]*AgentStats{}, threads: map[uint64]*threadTally{}, posts: make([]PostCounts, w.Buckets())}
		out, rerr = st.run()
		return rerr
	})
	return out, err
}

// threadTally is a thread's counts with the identities seen in it.
type threadTally struct {
	ids map[uint64]bool
	ThreadStats
}

type statsScan struct {
	w       StatsWindow
	wr      *webReader
	agents  map[uint64]*AgentStats
	threads map[uint64]*threadTally
	posts   []PostCounts
	delays  []time.Duration
}

func (st *statsScan) run() (Stats, error) {
	if err := st.liveAgents(); err != nil {
		return Stats{}, err
	}
	if err := st.scanPosts(); err != nil {
		return Stats{}, err
	}
	if err := st.scanDeliveries(); err != nil {
		return Stats{}, err
	}
	out := Stats{Window: st.w, Posts: st.posts, Agents: st.agentRows()}
	var err error
	if out.Threads, err = st.threadRows(); err != nil {
		return out, err
	}
	var c Census
	if err := st.wr.s.censusIdentities(st.wr.r, &c, nil, st.wr.now); err != nil {
		return out, err
	}
	out.Delays = histogram(st.delays, st.wr.s.retention())
	out.Delays.Unread = c.Unread
	return out, nil
}

// liveAgents adds every identity live at some point of the window: one
// registered before its end that was not dormant from before its start.
func (st *statsScan) liveAgents() error {
	wr := st.wr
	return wr.s.t.identities.table.Scan(wr.r, bolttable.Range{}, func(_ uint64, v *identityRow) (bool, error) {
		v = settled(v, wr.now, wr.s.dormantTTL)
		if v == nil || isUser(v) || !v.RegisteredAt.Before(st.w.End) {
			return true, nil
		}
		if v.State == stateDormant && v.DormantSince.Before(st.w.Start) {
			return true, nil
		}
		st.agents[v.ID] = &AgentStats{Name: v.Name}
		return true, nil
	})
}

// agent is identity id's row of Agents: an identity with no row there yet
// joins under name, the one its post or delivery carried.
func (st *statsScan) agent(id uint64, name string) *AgentStats {
	a, ok := st.agents[id]
	if !ok {
		a = &AgentStats{Name: name}
		st.agents[id] = a
	}
	return a
}

func (st *statsScan) thread(id uint64) *threadTally {
	t, ok := st.threads[id]
	if !ok {
		t = &threadTally{ids: map[uint64]bool{}}
		st.threads[id] = t
	}
	return t
}

func (st *statsScan) scanPosts() error {
	wr := st.wr
	return wr.s.t.messages.table.Scan(wr.r, bolttable.Range{}, func(_ []byte, m *messageRow) (bool, error) {
		i, in := st.w.Index(m.PostedAt)
		if !in || m.Lease.due(wr.now) || !isPost(m.Kind) {
			return true, nil
		}
		switch m.Kind {
		case kindAnnounce:
			st.posts[i].Announce++
		case kindPath:
			st.posts[i].Path++
		case kindDirect:
			st.posts[i].Direct++
		}
		t := st.thread(m.Thread)
		t.Posts++
		t.ids[m.Sender] = true
		if m.Sender != wr.user.ID {
			a := st.agent(m.Sender, m.SenderName)
			a.Posts++
			a.PostTimes = append(a.PostTimes, m.PostedAt)
		}
		return true, nil
	})
}

// scanDeliveries counts the window's post deliveries into their threads,
// and the reads in it into their readers and the delays; a parent's copy
// is no delivery.
func (st *statsScan) scanDeliveries() error {
	wr := st.wr
	return wr.s.t.deliveries.table.Scan(wr.r, bolttable.Range{}, func(_ []byte, d *deliveryRow) (bool, error) {
		if d.Lease.due(wr.now) || !isPost(d.Kind) || d.Via != "" {
			return true, nil
		}
		if _, in := st.w.Index(d.Written); in {
			t := st.thread(d.Thread)
			t.Deliveries++
			t.ids[d.Agent] = true
		}
		if _, in := st.w.Index(d.ReadAt); in && d.Agent != wr.user.ID {
			wait := d.ReadAt.Sub(d.Written)
			a := st.agent(d.Agent, d.Name)
			a.Read++
			a.LongestWait = max(a.LongestWait, wait)
			st.delays = append(st.delays, wait)
		}
		return true, nil
	})
}

func (st *statsScan) agentRows() []AgentStats {
	out := make([]AgentStats, 0, len(st.agents))
	for _, a := range st.agents {
		slices.SortFunc(a.PostTimes, func(x, y time.Time) int { return x.Compare(y) })
		out = append(out, *a)
	}
	slices.SortFunc(out, func(a, b AgentStats) int {
		return cmp.Or(cmp.Compare(b.Posts, a.Posts), cmp.Compare(b.Read, a.Read), cmp.Compare(a.Name, b.Name))
	})
	return out
}

func (st *statsScan) threadRows() ([]ThreadStats, error) {
	out := make([]ThreadStats, 0, len(st.threads))
	for id, t := range st.threads {
		th, found, err := st.wr.s.t.threads.table.Get(st.wr.r, id)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("store: a post names thread %d, which is gone", id)
		}
		if t.ConversationThread, err = st.wr.conversationThread(&th); err != nil {
			return nil, err
		}
		t.Participants = len(t.ids)
		out = append(out, t.ThreadStats)
	}
	slices.SortFunc(out, func(a, b ThreadStats) int {
		return cmp.Or(cmp.Compare(b.Posts, a.Posts), cmp.Compare(b.Deliveries, a.Deliveries), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// histogram bins delays on delayLadder up to its first edge at or above
// max(retention, the longest delay), the last edge if none is.
func histogram(delays []time.Duration, retention time.Duration) DelayHistogram {
	slices.Sort(delays)
	top := retention
	if n := len(delays); n > 0 {
		top = max(top, delays[n-1])
	}
	last := len(delayLadder) - 1
	for i, e := range delayLadder {
		if e >= top {
			last = i
			break
		}
	}
	out := DelayHistogram{Edges: slices.Clone(delayLadder[:last+1]), Counts: make([]int, last), Read: len(delays)}
	for _, d := range delays {
		bin, _ := slices.BinarySearch(out.Edges, d)
		if bin == len(out.Edges) || out.Edges[bin] != d {
			bin--
		}
		out.Counts[min(max(bin, 0), last-1)]++
	}
	out.Median, out.P95 = nearestRank(delays, 50), nearestRank(delays, 95)
	return out
}

// nearestRank is the p-th percentile of sorted: the smallest value with at
// least p percent of them at or below it.
func nearestRank(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100
	return sorted[max(rank, 1)-1]
}
