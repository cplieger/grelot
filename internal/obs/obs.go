// Package obs is grelot's /metrics page: the counters and histograms the
// server and store record into, and the gauges read from the store at
// scrape time. No label value is ever an agent name, path, subject or
// session. Beside the page, Events keeps the recent delivery problems for
// the web UI, names included, behind sign-in.
package obs

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/cplieger/go/httpserver/v2"
	"github.com/cplieger/go/measure/v2"
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

	AuthRateLimited      = "rate_limited"
	AuthWrongKeyIdentity = "wrong_key_identity"
	authOther            = "other"

	LimitRate        = "rate"
	LimitWaitsKey    = "waits_key"
	LimitWaitsServer = "waits_server"
)

// authReasons are auth_failures_total's reasons: access's gate.failure and
// gate.forbidden reasons (access internal/failreason and gate's own), then
// grelot's. A reason access adds later counts as other, so the label stays
// bounded.
var authReasons = []string{
	"key_unknown", "key_expired", "key_retired", "account_disabled", "session_expired", "session_unknown",
	"credential_invalid", "key_required", "session_required",
	"recovery_session", "cross_origin", AuthRateLimited, AuthWrongKeyIdentity, authOther,
}

// AuthReason is reason as auth_failures_total labels it.
func AuthReason(reason string) string {
	if slices.Contains(authReasons, reason) {
		return reason
	}
	return authOther
}

// labelReason is the label of every counter that sorts by cause.
const labelReason = "reason"

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
	// WritesReplayed counts keyed writes that answered their stored first
	// result.
	WritesReplayed *measure.Counter
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
	NudgeDelay *measure.LabeledHistogram
	// AuthFailures counts credentials presented and refused, and sign-in
	// attempts the failure limiter turned away, by reason.
	AuthFailures *measure.LabeledCounter
	// RequestsLimited counts /v1 requests refused by a limit, by limit.
	RequestsLimited *measure.LabeledCounter
	// Events is recorded beside PostsRefused, PushSends and Nudges.
	Events       *Events
	httpRequests *measure.LabeledCounter
	httpDuration *measure.LabeledHistogram
}

// New builds the page with every family registered and the refusal,
// started-empty, auth failure and limit labels seeded at 0, so a first one
// shows in increase().
func New() *Metrics {
	m := &Metrics{
		reg:               measure.NewRegistry("grelot"),
		SweepLastSuccess:  measure.NewGauge("sweep_last_success_timestamp_seconds", "Unix time of the last store sweep that committed.", measure.UnitSeconds),
		SweepFailures:     measure.NewCounter("sweep_failures_total", "Store sweeps that failed.", measure.UnitCount),
		StoreStartedEmpty: measure.NewLabeledCounter("store_started_empty_total", "Store opens that started with no rows: a corrupt file moved aside, or a namespace of another format reset.", measure.UnitCount, []string{labelReason}, seeds(StartedEmptyCorrupt, StartedEmptyFormat)),
		Posts:             measure.NewLabeledCounter("posts_total", "Posts stored, by thread kind.", measure.UnitCount, []string{"thread_kind"}),
		PostsRefused:      measure.NewLabeledCounter("posts_refused_total", "Posts refused, by reason.", measure.UnitCount, []string{labelReason}, seeds(RefusedStoreFull, RefusedRateLimited, RefusedScope)),
		WritesReplayed:    measure.NewCounter("writes_replayed_total", "Writes sent again with their Idempotency-Key and answered their first result, changing nothing.", measure.UnitCount),
		PushDoorUp:        measure.NewLabeledGauge("push_door_up", "1 while a push door sends, 0 once a refused credential turned it off.", measure.UnitNone, []string{"door"}),
		PushSends:         measure.NewLabeledCounter("push_sends_total", "Push tries, by door and outcome.", measure.UnitCount, []string{"door", "outcome"}),
		ProvidersInvalid:  measure.NewGauge("providers_invalid", "Provider files skipped at load; 1 for a folder that cannot be read.", measure.UnitCount),
		Nudges:            measure.NewLabeledCounter("nudges_total", "Nudges recorded, by layer and attempt.", measure.UnitCount, []string{"via", "attempt"}),
		NudgeDelay:        measure.NewLabeledHistogram("nudge_delay_seconds", "Delay from the oldest post in a batch to its nudge, by layer.", measure.UnitSeconds, []string{"via"}, measure.WithBuckets(measure.APIBuckets())),
		AuthFailures:      measure.NewLabeledCounter("auth_failures_total", "Machine keys and sessions presented and refused, requests acting for another key's identity, and sign-in attempts the failure limiter refused, by reason.", measure.UnitCount, []string{labelReason}, seeds(authReasons...)),
		RequestsLimited:   measure.NewLabeledCounter("requests_limited_total", "Agent API requests refused by a key's request rate, a key's held waits or the server's held waits.", measure.UnitCount, []string{"limit"}, seeds(LimitRate, LimitWaitsKey, LimitWaitsServer)),
		httpRequests:      measure.NewLabeledCounter("http_requests_total", "HTTP requests, by route and status code.", measure.UnitCount, []string{"route", "code"}),
		httpDuration:      measure.NewLabeledHistogram("http_request_duration_seconds", "HTTP request latency, by route.", measure.UnitSeconds, []string{"route"}),
		Events:            newEvents(),
	}
	m.reg.MustRegister(m.SweepLastSuccess, m.SweepFailures, m.StoreStartedEmpty, m.Posts, m.PostsRefused, m.WritesReplayed,
		m.PushDoorUp, m.PushSends, m.ProvidersInvalid, m.Nudges, m.NudgeDelay, m.AuthFailures, m.RequestsLimited,
		m.httpRequests, m.httpDuration)
	return m
}

// seeds is WithSeed for a one-label instrument: one set per value.
func seeds(values ...string) measure.Option {
	sets := make([][]string, len(values))
	for i, v := range values {
		sets[i] = []string{v}
	}
	return measure.WithSeed(sets...)
}

// ServeBuildInfo registers grelot_build_info, 1 with the running version as
// its label. Call it once.
func (m *Metrics) ServeBuildInfo(version string) {
	info := measure.NewInfo("build_info", "Always 1, labelled with the running version.", []string{"version"})
	m.reg.MustRegister(info)
	info.Set(version)
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
	identities := measure.NewGaugeDesc("identities", "Identities, live or dormant.", measure.UnitCount, []string{"state"})
	online := measure.NewGaugeDesc("identities_online", "Live identities online now.", measure.UnitCount, nil)
	threads := measure.NewGaugeDesc("threads", "Live threads, by kind.", measure.UnitCount, []string{"kind"})
	reservations := measure.NewGaugeDesc("reservations", "Reservations held, and queue entries waiting.", measure.UnitCount, []string{"state"})
	unread := measure.NewGaugeDesc("unread_backlog", "Unread wake deliveries of live identities.", measure.UnitCount, nil)
	oldest := measure.NewGaugeDesc("unread_oldest_seconds", "Age of the oldest unread wake delivery, 0 with none.", measure.UnitSeconds, nil)
	inUse := measure.NewGaugeDesc("store_in_use_bytes", "Store bytes holding data: the pages written, less the free ones.", measure.UnitBytes, nil)
	file := measure.NewGaugeDesc("store_file_bytes", "Store file size, which stays near its high-water mark.", measure.UnitBytes, nil)
	free := measure.NewGaugeDesc("store_free_bytes", "Store bytes in free and pending pages.", measure.UnitBytes, nil)
	maxBytes := measure.NewGaugeDesc("store_max_bytes", "STORE_MAX_BYTES, the ceiling on bytes in use; new posts, identities, follows, work paths, reservations and delegate links are refused past 90 % of it.", measure.UnitBytes, nil)
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
