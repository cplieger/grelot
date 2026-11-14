package wire

// StatsRange is a Statistics range.
type StatsRange string

// The Statistics ranges.
const (
	Range6h  StatsRange = "6h"
	Range24h StatsRange = "24h"
	Range72h StatsRange = "72h"
)

// StatsQuery is the Statistics tab's query.
type StatsQuery struct {
	Range StatsRange `json:"range"`
}

// PostBucket counts one bucket's posts by conversation kind.
type PostBucket struct {
	StartMS  int64 `json:"start_ms"`
	Announce int   `json:"announce"`
	Path     int   `json:"path"`
	Direct   int   `json:"direct"`
	// Open marks the last bucket, which is still filling.
	Open bool `json:"open,omitempty"`
}

// AgentStats is one agent live during the range.
type AgentStats struct {
	Name string `json:"name"`
	// PostTimesMS are its posts' instants, oldest first.
	PostTimesMS []int64 `json:"post_times_ms"`
	Posts       int     `json:"posts"`
	// Read counts the deliveries it read.
	Read int `json:"read"`
	// LongestWaitMS is its longest wait from post to read.
	LongestWaitMS int64 `json:"longest_wait_ms"`
}

// ThreadStats is one conversation's activity in the range.
type ThreadStats struct {
	Thread       Thread `json:"thread"`
	Posts        int    `json:"posts"`
	Participants int    `json:"participants"`
	Deliveries   int    `json:"deliveries"`
}

// DelayHistogram bins the post-to-read delays of the deliveries read in
// the range: bin i is [edges_ms[i], edges_ms[i+1]), the last closed.
type DelayHistogram struct {
	EdgesMS  []int64 `json:"edges_ms"`
	Counts   []int   `json:"counts"`
	MedianMS int64   `json:"median_ms"`
	P95MS    int64   `json:"p95_ms"`
	Read     int     `json:"read"`
	// Unread counts the deliveries still unread, as the unread figure does.
	Unread int `json:"unread"`
}

// Sample is one minute of the server.
type Sample struct {
	AtMS     int64 `json:"at_ms"`
	Requests int   `json:"requests"`
	// Errors counts the 5xx answers.
	Errors int `json:"errors"`
	// P95MS is the latency's 95th percentile over every route but the
	// held waits.
	P95MS          int64 `json:"p95_ms"`
	Live           int   `json:"live"`
	Dormant        int   `json:"dormant"`
	Online         int   `json:"online"`
	Unread         int   `json:"unread"`
	OldestUnreadMS int64 `json:"oldest_unread_ms"`
	Held           int   `json:"held"`
	Queued         int   `json:"queued"`
	InUseBytes     int64 `json:"in_use_bytes"`
	MaxBytes       int64 `json:"max_bytes"`
	HeapBytes      int64 `json:"heap_bytes"`
	ResidentBytes  int64 `json:"resident_bytes"`
	Goroutines     int   `json:"goroutines"`
	// Counted is false when the store could not count that minute, which
	// leaves its store figures zero.
	Counted bool `json:"counted"`
}

// EventKind is a delivery problem's kind.
type EventKind string

// The delivery problems.
const (
	EventPostRefused EventKind = "post_refused"
	EventPushRetry   EventKind = "push_retry"
	EventPushGaveUp  EventKind = "push_gave_up"
	EventNudgeRepeat EventKind = "nudge_repeat"
)

// Event is one delivery problem.
type Event struct {
	Kind   EventKind `json:"kind"`
	Agent  string    `json:"agent"`
	Detail string    `json:"detail"`
	AtMS   int64     `json:"at_ms"`
}

// EventCounts counts the range's delivery problems by kind, beyond the
// newest 200 the server keeps.
type EventCounts struct {
	PostRefused int `json:"post_refused"`
	PushRetry   int `json:"push_retry"`
	PushGaveUp  int `json:"push_gave_up"`
	NudgeRepeat int `json:"nudge_repeat"`
}

// HeldTurns counts the turns the stop hook held for one agent.
type HeldTurns struct {
	Name  string `json:"name"`
	Turns int    `json:"turns"`
}

// Stats is the Statistics tab over a range aligned to its buckets: end_ms
// is the end of the bucket still filling, start_ms the range before it.
type Stats struct {
	Posts   []PostBucket  `json:"posts"`
	Agents  []AgentStats  `json:"agents"`
	Threads []ThreadStats `json:"threads"`
	// Samples are the range's minutes since the server started, oldest
	// first.
	Samples []Sample `json:"samples"`
	// Events are the range's newest delivery problems, newest first.
	Events []Event `json:"events"`
	// HeldTurns are held turns per agent since the server started, most
	// first.
	HeldTurns   []HeldTurns    `json:"held_turns"`
	Delays      DelayHistogram `json:"delays"`
	EventCounts EventCounts    `json:"event_counts"`
	StartMS     int64          `json:"start_ms"`
	EndMS       int64          `json:"end_ms"`
	BucketMS    int64          `json:"bucket_ms"`
	// StartedMS is when the server started, which emptied its samples,
	// events and held turns.
	StartedMS int64 `json:"started_ms"`
}
