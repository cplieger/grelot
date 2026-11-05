// Package obs is grelot's /metrics page: the counters and histograms the
// server and store record into, and the gauges read from the store at
// scrape time. No label value is ever an agent name, path, subject or
// session.
package obs

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/measure"
)

// The bounded label values the record paths take.
const (
	StartedEmptyCorrupt = "corrupt"
	StartedEmptyFormat  = "format"

	RefusedStoreFull   = "store_full"
	RefusedRateLimited = "rate_limited"
	RefusedScope       = "scope"

	SendAccepted = "accepted"
	SendRetry    = "retry"
	SendGaveUp   = "gave_up"
	SendGone     = "gone"

	AttemptFirst  = "first"
	AttemptRepeat = "repeat"
)

// Metrics holds every family on the page. Build it with New.
type Metrics struct {
	reg *measure.Registry

	// SweepLastSuccess is the Unix time of the last sweep that committed.
	SweepLastSuccess *measure.Gauge
	SweepFailures    *measure.Counter
	// StoreStartedEmpty counts opens that started with no rows, by reason.
	StoreStartedEmpty *measure.LabeledCounter
	// Posts counts stored posts by thread kind.
	Posts *measure.LabeledCounter
	// PostsRefused counts refused posts by reason.
	PostsRefused *measure.LabeledCounter
	// PushDoorUp is 1 for a door that pushes, 0 for one turned off.
	PushDoorUp *measure.LabeledGauge
	// PushSends counts push tries by door and outcome.
	PushSends *measure.LabeledCounter
	// ProvidersInvalid is the number of provider files skipped at load, 1
	// for a folder that cannot be read.
	ProvidersInvalid *measure.Gauge
	// Nudges counts recorded nudges by layer and attempt.
	Nudges *measure.LabeledCounter
	// NudgeDelay is the oldest-in-batch post-to-nudge delay by layer.
	NudgeDelay   *measure.LabeledHistogram
	httpRequests *measure.LabeledCounter
	httpDuration *measure.LabeledHistogram
}

// New builds the page with every family registered and the refusal and
// started-empty reasons seeded at 0, so a first one shows in increase().
func New() *Metrics {
	m := &Metrics{
		reg:               measure.NewRegistry("grelot"),
		SweepLastSuccess:  measure.NewGauge("sweep_last_success_timestamp_seconds", "Unix time of the last store sweep that committed."),
		SweepFailures:     measure.NewCounter("sweep_failures_total", "Store sweeps that failed."),
		StoreStartedEmpty: measure.NewLabeledCounter("store_started_empty_total", "Store opens that started with no rows: a corrupt file moved aside, or a namespace of another format reset.", []string{"reason"}),
		Posts:             measure.NewLabeledCounter("posts_total", "Posts stored, by thread kind.", []string{"thread_kind"}),
		PostsRefused:      measure.NewLabeledCounter("posts_refused_total", "Posts refused, by reason.", []string{"reason"}),
		PushDoorUp:        measure.NewLabeledGauge("push_door_up", "1 while a push door sends, 0 once a refused credential turned it off.", []string{"door"}),
		PushSends:         measure.NewLabeledCounter("push_sends_total", "Push tries, by door and outcome.", []string{"door", "outcome"}),
		ProvidersInvalid:  measure.NewGauge("providers_invalid", "Provider files skipped at load; 1 for a folder that cannot be read."),
		Nudges:            measure.NewLabeledCounter("nudges_total", "Nudges recorded, by layer and attempt.", []string{"via", "attempt"}),
		NudgeDelay:        measure.NewLabeledHistogram("nudge_delay_seconds", "Delay from the oldest post in a batch to its nudge, by layer.", []string{"via"}, measure.WithBuckets(measure.APIBuckets())),
		httpRequests:      measure.NewLabeledCounter("http_requests_total", "HTTP requests, by route and status code.", []string{"route", "code"}),
		httpDuration:      measure.NewLabeledHistogram("http_request_duration_seconds", "HTTP request latency, by route.", []string{"route"}),
	}
	m.reg.MustRegister(m.SweepLastSuccess, m.SweepFailures, m.StoreStartedEmpty, m.Posts, m.PostsRefused,
		m.PushDoorUp, m.PushSends, m.ProvidersInvalid, m.Nudges, m.NudgeDelay, m.httpRequests, m.httpDuration)
	for _, r := range []string{StartedEmptyCorrupt, StartedEmptyFormat} {
		m.StoreStartedEmpty.Add(0, r)
	}
	for _, r := range []string{RefusedStoreFull, RefusedRateLimited, RefusedScope} {
		m.PostsRefused.Add(0, r)
	}
	return m
}

// Handler serves the page, with the Go runtime and process families.
func (m *Metrics) Handler() http.Handler { return m.reg.Handler() }

// RecordHTTP records one completed request; it is the hook for
// httpserver.WithRecordRouteMetric, whose Path is a registered pattern.
func (m *Metrics) RecordHTTP(rm httpserver.RequestMetric) {
	m.httpRequests.Inc(rm.Path, strconv.Itoa(rm.Status))
	m.httpDuration.Observe(rm.Latency.Seconds(), rm.Path)
}

// State is what the scrape-time gauges read.
type State struct {
	Oldest                        time.Time
	Live, Dormant, Online         int
	Announce, Path, Direct, Cards int
	Held, Queued, Unread          int
	FileBytes, FreeBytes, InUse   int64
	MaxBytes                      int64
}

// ServeState registers the gauges read at each scrape from read, which
// reports false when it cannot count; the page then leaves them out. now
// is the clock the oldest unread age is taken against. Call it once.
func (m *Metrics) ServeState(read func(context.Context) (State, bool), now func() time.Time) {
	identities := measure.NewGaugeDesc("identities", "Identities, live or dormant.", []string{"state"})
	online := measure.NewGaugeDesc("identities_online", "Live identities online now.", nil)
	threads := measure.NewGaugeDesc("threads", "Live threads, by kind.", []string{"kind"})
	reservations := measure.NewGaugeDesc("reservations", "Reservations held, and queue entries waiting.", []string{"state"})
	unread := measure.NewGaugeDesc("unread_backlog", "Unread wake deliveries of live identities.", nil)
	oldest := measure.NewGaugeDesc("unread_oldest_seconds", "Age of the oldest unread wake delivery, 0 with none.", nil)
	inUse := measure.NewGaugeDesc("store_in_use_bytes", "Store bytes holding data: the file's bytes minus its free pages.", nil)
	file := measure.NewGaugeDesc("store_file_bytes", "Store file size, which stays near its high-water mark.", nil)
	free := measure.NewGaugeDesc("store_free_bytes", "Store bytes in free and pending pages.", nil)
	maxBytes := measure.NewGaugeDesc("store_max_bytes", "STORE_MAX_BYTES, the ceiling on bytes in use; posts are refused past 90 % of it.", nil)
	m.reg.MustRegister(measure.NewCollector(func(ctx context.Context, e *measure.Emitter) {
		st, ok := read(ctx)
		if !ok {
			return
		}
		e.Emit(identities, float64(st.Live), "live")
		e.Emit(identities, float64(st.Dormant), "dormant")
		e.Emit(online, float64(st.Online))
		e.Emit(threads, float64(st.Announce), "announce")
		e.Emit(threads, float64(st.Path), "path")
		e.Emit(threads, float64(st.Direct), "direct")
		e.Emit(threads, float64(st.Cards), "cards")
		e.Emit(reservations, float64(st.Held), "held")
		e.Emit(reservations, float64(st.Queued), "queued")
		e.Emit(unread, float64(st.Unread))
		var age float64
		if !st.Oldest.IsZero() {
			age = now().Sub(st.Oldest).Seconds()
		}
		e.Emit(oldest, age)
		e.Emit(inUse, float64(st.InUse))
		e.Emit(file, float64(st.FileBytes))
		e.Emit(free, float64(st.FreeBytes))
		e.Emit(maxBytes, float64(st.MaxBytes))
	}, identities, online, threads, reservations, unread, oldest, inUse, file, free, maxBytes))
}
