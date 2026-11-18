package obs

import (
	"sync"
	"time"
)

// EventKind is a delivery problem the Statistics tab lists.
type EventKind uint8

// The delivery problems: a post refused, a push to retry or given up, a
// nudge repeated because the first went unread.
const (
	EventPostRefused EventKind = iota
	EventPushRetry
	EventPushGaveUp
	EventNudgeRepeat
	eventKinds
)

// String is k's name on the web API.
func (k EventKind) String() string {
	switch k {
	case EventPostRefused:
		return "post_refused"
	case EventPushRetry:
		return "push_retry"
	case EventPushGaveUp:
		return "push_gave_up"
	case EventNudgeRepeat:
		return "nudge_repeat"
	}
	return "unknown"
}

// Event is one delivery problem. Detail is a refusal's reason, a push's
// door or a nudge's layer; Agent is the sender or recipient's name. An
// Event never reaches a metric label.
type Event struct {
	At     time.Time
	Detail string
	Agent  string
	Kind   EventKind
}

// The ring keeps the newest eventCap events, and counts by minute as deep
// as the sampler's series, so a range's counts outlive the events shown.
const (
	eventCap     = 200
	eventMinutes = 72 * 60
)

// Events is the in-memory ring of recent delivery problems, emptied by a
// restart. Its zero value is not usable: New builds it.
type Events struct {
	ring    []Event
	minutes []minuteCount
	next    int
	mu      sync.Mutex
}

type minuteCount struct {
	minute int64
	n      [eventKinds]int
}

func newEvents() *Events {
	return &Events{ring: make([]Event, 0, eventCap), minutes: make([]minuteCount, eventMinutes)}
}

// Add records e, dropping the oldest event past eventCap; an e of an
// unknown kind is dropped.
func (e *Events) Add(ev Event) {
	if ev.Kind >= eventKinds {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.ring) < eventCap {
		e.ring = append(e.ring, ev)
	} else {
		e.ring[e.next] = ev
	}
	e.next = (e.next + 1) % eventCap
	m := minuteOf(ev.At)
	slot := &e.minutes[m%eventMinutes]
	if slot.minute != m {
		*slot = minuteCount{minute: m}
	}
	slot.n[ev.Kind]++
}

// Recent lists the kept events at or after from and before to, newest
// first.
func (e *Events) Recent(from, to time.Time) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []Event{}
	for i := range len(e.ring) {
		ev := e.ring[(e.next-1-i+eventCap)%eventCap]
		if !ev.At.Before(from) && ev.At.Before(to) {
			out = append(out, ev)
		}
	}
	return out
}

// Counts counts the events of each kind in the minutes from from's to
// to's, to's excluded, including those Recent no longer keeps; minutes
// over 72 hours old are gone.
func (e *Events) Counts(from, to time.Time) map[EventKind]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[EventKind]int, eventKinds)
	for k := range eventKinds {
		out[k] = 0
	}
	lo, hi := minuteOf(from), minuteOf(to)
	for i := range e.minutes {
		if m := e.minutes[i].minute; m >= lo && m < hi {
			for k, n := range e.minutes[i].n {
				out[EventKind(k)] += n
			}
		}
	}
	return out
}

func minuteOf(t time.Time) int64 { return t.Unix() / 60 }
