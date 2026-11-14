//go:build !windows

package server

import (
	"cmp"
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/access/gate"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/accounts"
	"github.com/cplieger/grelot/internal/auth"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/webui/wire"
)

// webFunc serves one JSON route of the web API as @user; k is the
// request's Idempotency-Key on a keyed route, zero otherwise.
type webFunc func(r *http.Request, k store.Keyed) (any, error)

// webUser is who every web API write acts as.
var webUser = store.Caller{User: true}

// webRoutes adds every wire.Routes route to t, each but the public ones
// as a session's row.
func (s *Server) webRoutes(t *table) {
	handlers := map[string]webFunc{
		"listConversations":    s.listConversations,
		"getConversation":      s.getConversation,
		"markConversationRead": s.markConversationRead,
		"getReceipts":          s.getReceipts,
		"createPost":           s.createPost,
		"listAgents":           s.listAgents,
		"getAgent":             s.getAgent,
		"getAgentHistory":      s.getAgentHistory,
		"getStats":             s.getStats,
		"getSettings":          s.getSettings,
		"putSettings":          s.putSettings,
		"getSignIn":            s.getSignIn,
	}
	bound := 0
	for i := range wire.Routes {
		r := &wire.Routes[i]
		class := gate.Session
		if r.Public {
			class = gate.Public
		}
		if r.Kind == wire.KindEventStream {
			t.add(r.Pattern(), class, untilSessionEnds(http.HandlerFunc(s.events.serve)))
			continue
		}
		fn, ok := handlers[r.Name]
		if !ok {
			panic("server: no handler for web route " + r.Name)
		}
		bound++
		t.add(r.Pattern(), class, httpserver.RouteDeadline(s.webJSON(fn, r), routeDeadline))
	}
	if bound != len(handlers) {
		panic("server: a web handler names no route in wire.Routes")
	}
}

// principal is the session the gate admitted r with.
func principal(r *http.Request) *access.Principal {
	p, _ := gate.Principal(r.Context())
	return &p
}

// untilSessionEnds ends a request when its session lapses, at the earlier
// of its idle and absolute deadlines, so a stream outlives no session.
func untilSessionEnds(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principal(r)
		end := p.AbsoluteDeadline
		if !p.IdleDeadline.IsZero() && (end.IsZero() || p.IdleDeadline.Before(end)) {
			end = p.IdleDeadline
		}
		if end.IsZero() {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithDeadline(r.Context(), end)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) webJSON(fn webFunc, route *wire.Route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpserver.LimitBody(w, r, maxBody)
		var k store.Keyed
		var err error
		_, sent := r.Header[headerKey]
		switch {
		case route.Keyed:
			k, err = keyOf(r, webUser)
		case sent:
			err = badRequest("this route takes no Idempotency-Key")
		}
		var body any
		if err == nil {
			body, err = fn(r, k)
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		httpserver.WriteJSONStatus(w, cmp.Or(route.Status, http.StatusOK), body)
	})
}

func uintParam(v, name string) (uint64, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, badRequest(name + " must be a whole number")
	}
	return n, nil
}

func idParam(r *http.Request) (uint64, error) {
	id, err := uintParam(r.PathValue("id"), "id")
	if err == nil && id == 0 {
		err = badRequest("id must be a whole number above 0")
	}
	return id, err
}

func (s *Server) listConversations(r *http.Request, _ store.Keyed) (body any, err error) {
	q := r.URL.Query()
	before, err := uintParam(q.Get("before"), "before")
	if err != nil {
		return nil, err
	}
	beforeThread, err := uintParam(q.Get("before_thread"), "before_thread")
	if err != nil {
		return nil, err
	}
	page, err := s.store.Conversations(r.Context(), store.ConversationsQuery{
		Show: q.Get("kind"), Before: before, BeforeThread: beforeThread,
	})
	if err != nil {
		return nil, err
	}
	out := wire.ConversationsPage{
		Rows: make([]wire.ConversationRow, 0, len(page.Rows)), Next: page.Next, NextThread: page.NextThread,
		Counts: wire.ConversationCounts{Threads: page.Counts.Threads, Direct: page.Counts.Direct},
	}
	for i := range page.Rows {
		row := &page.Rows[i]
		out.Rows = append(out.Rows, wire.ConversationRow{
			Thread: wireThread(&row.ConversationThread),
			Last: wire.LastPost{
				From: row.Last.From, Subject: row.Last.Subject, Preview: row.Last.Preview,
				ID: row.Last.ID, AtMS: ms(row.Last.At),
			},
			Participants: row.Participants, Unread: row.Unread,
		})
	}
	return out, nil
}

func (s *Server) getConversation(r *http.Request, _ store.Keyed) (body any, err error) {
	id, err := idParam(r)
	if err != nil {
		return nil, err
	}
	before, err := uintParam(r.URL.Query().Get("before"), "before")
	if err != nil {
		return nil, err
	}
	c, err := s.store.Conversation(r.Context(), id, before, 0)
	if err != nil {
		return nil, err
	}
	out := wire.Conversation{
		Thread: wireThread(&c.Thread), Next: c.Next,
		Posts:        make([]wire.Post, 0, len(c.Posts)),
		Reservations: wireReservations(c.Reservations),
		Followers:    orEmpty(c.Followers),
		Cards:        make([]wire.Agent, 0, len(c.Cards)),
	}
	for i := range c.Posts {
		out.Posts = append(out.Posts, wirePost(&c.Posts[i]))
	}
	online := s.onlineAt(s.now())
	for i := range c.Cards {
		out.Cards = append(out.Cards, wireAgent(&c.Cards[i], online))
	}
	return out, nil
}

func (s *Server) markConversationRead(r *http.Request, _ store.Keyed) (body any, err error) {
	id, err := idParam(r)
	if err != nil {
		return nil, err
	}
	var req wire.MarkReadRequest
	err = decode(r, &req)
	if err != nil {
		return nil, err
	}
	if req.UpTo == 0 {
		return nil, badRequest("up_to must name the newest post shown")
	}
	n, err := s.store.ReadThread(r.Context(), webUser, id, req.UpTo)
	if err != nil {
		return nil, err
	}
	return wire.MarkedRead{Read: n}, nil
}

func (s *Server) getReceipts(r *http.Request, _ store.Keyed) (body any, err error) {
	id, err := idParam(r)
	if err != nil {
		return nil, err
	}
	rc, err := s.store.MessageReceipts(r.Context(), id)
	if err != nil {
		return nil, err
	}
	out := wire.Receipts{
		Subject: rc.Subject, Kind: rc.Kind, Message: rc.Message, PostedAtMS: ms(rc.PostedAt),
		RegisteredSince: rc.RegisteredSince, Recipients: make([]wire.Receipt, 0, len(rc.Recipients)),
	}
	for i := range rc.Recipients {
		p := &rc.Recipients[i]
		out.Recipients = append(out.Recipients, wire.Receipt{Name: p.Name, Host: p.Host, State: p.State, ReadAtMS: ms(p.ReadAt)})
	}
	return out, nil
}

func (s *Server) createPost(r *http.Request, k store.Keyed) (body any, err error) {
	var req wire.PostRequest
	err = decode(r, &req)
	if err != nil {
		return nil, err
	}
	p, _, err := s.store.Post(r.Context(), k, &store.Posting{
		Caller: webUser, Subject: req.Subject, Body: req.Body,
		To: store.Target{Announce: req.To.Announce, Path: req.To.Path, Agent: req.To.Agent},
	})
	if err != nil {
		return nil, err
	}
	return wire.Posted{Message: p.Message, Thread: p.Thread, Delivered: p.Delivered}, nil
}

func (s *Server) listAgents(r *http.Request, _ store.Keyed) (body any, err error) {
	ids, err := s.store.Agents(r.Context())
	if err != nil {
		return nil, err
	}
	out := wire.AgentList{Agents: make([]wire.Agent, 0, len(ids))}
	online := s.onlineAt(s.now())
	for i := range ids {
		out.Agents = append(out.Agents, wireAgent(&ids[i], online))
	}
	return out, nil
}

func (s *Server) getAgent(r *http.Request, _ store.Keyed) (body any, err error) {
	card, err := s.store.AgentCard(r.Context(), r.PathValue("name"))
	if err != nil {
		return nil, err
	}
	return wire.AgentPage{
		Agent: wireAgent(&card.Identity, s.onlineAt(s.now())), Reservations: wireReservations(card.Reservations), Delegates: orEmpty(card.Delegates),
	}, nil
}

func (s *Server) getAgentHistory(r *http.Request, _ store.Keyed) (body any, err error) {
	before, err := uintParam(r.URL.Query().Get("before"), "before")
	if err != nil {
		return nil, err
	}
	h, err := s.store.AgentHistory(r.Context(), r.PathValue("name"), before, 0)
	if err != nil {
		return nil, err
	}
	out := wire.HistoryPage{Next: h.Next, Posts: make([]wire.HistoryPost, 0, len(h.Posts))}
	for i := range h.Posts {
		p := &h.Posts[i]
		out.Posts = append(out.Posts, wire.HistoryPost{Conversation: wireThread(&p.Conversation), Post: wirePost(&p.ConversationPost)})
	}
	return out, nil
}

var statsRanges = map[string]time.Duration{
	string(wire.Range6h): 6 * time.Hour, string(wire.Range24h): 24 * time.Hour, string(wire.Range72h): 72 * time.Hour,
}

func (s *Server) getStats(r *http.Request, _ store.Keyed) (body any, err error) {
	span, ok := statsRanges[r.URL.Query().Get("range")]
	if !ok {
		return nil, badRequest("range must be 6h, 24h or 72h")
	}
	rep, err := s.statsReport(r.Context(), span)
	if err != nil {
		return nil, err
	}
	return wireStats(&rep, s.started), nil
}

func (s *Server) getSettings(*http.Request, store.Keyed) (body any, err error) {
	return wireSettings(s.auth.Settings()), nil
}

func (s *Server) putSettings(r *http.Request, _ store.Keyed) (body any, err error) {
	var req wire.SettingsRequest
	if derr := decode(r, &req); derr != nil {
		return nil, derr
	}
	// Clamped, so no retention_ms overflows into a duration Put accepts.
	ms := min(max(req.RetentionMS, 0), math.MaxInt64/int64(time.Millisecond))
	m := req.Methods
	err = s.auth.PutSettings(r.Context(), principal(r), accounts.Settings{
		Retention: time.Duration(ms) * time.Millisecond,
		Methods:   accounts.Methods{Password: m.Password, Passkeys: m.Passkeys, OIDC: m.OIDC},
	})
	switch {
	case errors.Is(err, accounts.ErrInvalid):
		return nil, badRequest(err.Error())
	case errors.Is(err, auth.ErrRecentAuth):
		return nil, refusal(wire.ErrRecentAuthRequired, "confirm it is you (sign in again) to change the sign-in methods")
	case errors.Is(err, auth.ErrWouldLockOut):
		return nil, refusal(wire.ErrWouldLockOut, "you would have no way to sign in: turn on another method you use first")
	case err != nil:
		return nil, err
	}
	return wireSettings(s.auth.Settings()), nil
}

func wireSettings(st auth.Settings) wire.Settings {
	m := st.Methods
	return wire.Settings{
		Methods:     wire.Methods{Password: m.Password, Passkeys: m.Passkeys, OIDC: m.OIDC},
		RetentionMS: st.Retention.Milliseconds(), OIDCConfigured: st.OIDCConfigured,
		RetentionMinMS: accounts.MinRetention.Milliseconds(), RetentionMaxMS: accounts.MaxRetention.Milliseconds(),
	}
}

func (s *Server) getSignIn(*http.Request, store.Keyed) (body any, err error) {
	return wire.SignIn{OIDCLabel: s.auth.OIDCLabel()}, nil
}

// ms is t in milliseconds since the Unix epoch, 0 for the zero time.
func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// orEmpty answers a nil slice as an empty one, which JSON sends as [].
func orEmpty[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func wireThread(t *store.ConversationThread) wire.Thread {
	return wire.Thread{Kind: wire.ThreadKind(t.Kind), Title: t.Title, Path: t.Path, Agents: t.Agents, ID: t.ID}
}

func wirePost(p *store.ConversationPost) wire.Post {
	from := wire.SenderAgent
	if p.FromUser {
		from = wire.SenderUser
	}
	return wire.Post{
		From: p.From, FromKind: from, Subject: p.Subject, Body: p.Body, Origin: p.Origin,
		Read: wire.ReadCount{Read: p.Read, Total: p.Recipients}, ID: p.ID, Thread: p.Thread, AtMS: ms(p.At),
		Unread: p.Unread,
	}
}

func wireReservations(rs []store.ReservationEntry) []wire.Reservation {
	out := make([]wire.Reservation, 0, len(rs))
	for i := range rs {
		e := &rs[i]
		out = append(out, wire.Reservation{
			Path: e.Path, Holder: e.Holder, Host: e.Host, Reason: e.Reason, State: wire.ReservationState(e.State),
			ID: e.ID, Place: e.Place, SinceMS: ms(e.Since), ExpiresMS: ms(e.Expires),
		})
	}
	return out
}

func wireAgent(v *store.Identity, online func(*store.Identity) bool) wire.Agent {
	return wire.Agent{
		Name: v.Name, Description: v.Description, State: wire.AgentState(v.State),
		Host: v.Host, OS: v.OS, Arch: v.Arch, Provider: v.Provider,
		Paths: orEmpty(v.Paths), Follows: orEmpty(v.Follows),
		RegisteredMS: ms(v.RegisteredAt), LastSeenMS: ms(v.LastSeen),
		ExpiresMS: ms(v.Expires), DormantSinceMS: ms(v.DormantSince),
		Online: wire.AgentState(v.State) == wire.AgentLive && online(v),
	}
}

func wireStats(rep *statsReport, started time.Time) wire.Stats {
	w := rep.Window
	out := wire.Stats{
		StartMS: ms(w.Start), EndMS: ms(w.End), BucketMS: w.Bucket.Milliseconds(), StartedMS: ms(started),
		Posts:   make([]wire.PostBucket, 0, len(rep.Posts)),
		Agents:  make([]wire.AgentStats, 0, len(rep.Agents)),
		Threads: make([]wire.ThreadStats, 0, len(rep.Threads)),
		Samples: make([]wire.Sample, 0, len(rep.Samples)),
		Events:  make([]wire.Event, 0, len(rep.Events)),
		Delays: wire.DelayHistogram{
			EdgesMS: make([]int64, 0, len(rep.Delays.Edges)), Counts: orEmpty(rep.Delays.Counts),
			MedianMS: rep.Delays.Median.Milliseconds(), P95MS: rep.Delays.P95.Milliseconds(),
			Read: rep.Delays.Read, Unread: rep.Delays.Unread,
		},
		EventCounts: wire.EventCounts{
			PostRefused: rep.EventCounts[obs.EventPostRefused], PushRetry: rep.EventCounts[obs.EventPushRetry],
			PushGaveUp: rep.EventCounts[obs.EventPushGaveUp], NudgeRepeat: rep.EventCounts[obs.EventNudgeRepeat],
		},
		HeldTurns: wireHeldTurns(rep.HeldTurns),
	}
	for i, c := range rep.Posts {
		out.Posts = append(out.Posts, wire.PostBucket{
			StartMS: ms(w.Start.Add(time.Duration(i) * w.Bucket)), Announce: c.Announce, Path: c.Path, Direct: c.Direct,
			Open: i == len(rep.Posts)-1,
		})
	}
	for i := range rep.Agents {
		a := &rep.Agents[i]
		times := make([]int64, 0, len(a.PostTimes))
		for _, t := range a.PostTimes {
			times = append(times, ms(t))
		}
		out.Agents = append(out.Agents, wire.AgentStats{
			Name: a.Name, PostTimesMS: times, Posts: a.Posts, Read: a.Read, LongestWaitMS: a.LongestWait.Milliseconds(),
		})
	}
	for i := range rep.Threads {
		t := &rep.Threads[i]
		out.Threads = append(out.Threads, wire.ThreadStats{
			Thread: wireThread(&t.ConversationThread), Posts: t.Posts, Participants: t.Participants, Deliveries: t.Deliveries,
		})
	}
	for _, e := range rep.Delays.Edges {
		out.Delays.EdgesMS = append(out.Delays.EdgesMS, e.Milliseconds())
	}
	for i := range rep.Samples {
		out.Samples = append(out.Samples, wireSample(&rep.Samples[i]))
	}
	for i := range rep.Events {
		e := &rep.Events[i]
		out.Events = append(out.Events, wire.Event{Kind: wire.EventKind(e.Kind.String()), Agent: e.Agent, Detail: e.Detail, AtMS: ms(e.At)})
	}
	return out
}

func wireSample(sm *sample) wire.Sample {
	return wire.Sample{
		AtMS: ms(sm.At), Requests: sm.Requests, Errors: sm.Errors, P95MS: sm.P95.Milliseconds(),
		Live: sm.Live, Dormant: sm.Dormant, Online: sm.Online, Unread: sm.Unread,
		OldestUnreadMS: sm.OldestUnread.Milliseconds(), Held: sm.Held, Queued: sm.Queued,
		InUseBytes: sm.InUse, MaxBytes: sm.MaxBytes, HeapBytes: sm.Heap, ResidentBytes: sm.Resident,
		Goroutines: sm.Goroutines, Counted: sm.State,
	}
}

// wireHeldTurns lists held turns most first, then by name.
func wireHeldTurns(m map[string]int) []wire.HeldTurns {
	out := make([]wire.HeldTurns, 0, len(m))
	for name, n := range m {
		out = append(out, wire.HeldTurns{Name: name, Turns: n})
	}
	slices.SortFunc(out, func(a, b wire.HeldTurns) int {
		return cmp.Or(cmp.Compare(b.Turns, a.Turns), cmp.Compare(a.Name, b.Name))
	})
	return out
}
