//go:build !windows

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/store"
)

// postsPerMinute is the post limit of one identity, a delegate's posts
// counted as its parent's.
const postsPerMinute = 30

// announce is the target word of an announcement and its thread kind.
const announce = "announce"

// logTextCap caps each client text a log line carries.
const logTextCap = 4 << 10

type postRequest struct {
	Where    *store.Where `json:"where"`
	Identity struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Paths       []string `json:"paths"`
	} `json:"identity"`
	Subject    string          `json:"subject"`
	Body       string          `json:"body"`
	To         json.RawMessage `json:"to"`
	NotifyRead json.RawMessage `json:"notify_read"`
	By         duration        `json:"by"`
}

type recipientsJSON struct {
	Total           int `json:"total"`
	SameEnvironment int `json:"same_environment"`
	Other           int `json:"other"`
}

type postResponse struct {
	Notices    []string       `json:"notices"`
	Identity   identityJSON   `json:"identity"`
	Recipients recipientsJSON `json:"recipients"`
	Message    uint64         `json:"message"`
	Thread     uint64         `json:"thread"`
}

// post is POST /v1/posts.
func (s *Server) post(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	var req postRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	to, kind, err := postTarget(req.To)
	if err != nil {
		return 0, nil, err
	}
	notify, err := notifyRead(req.NotifyRead)
	if err != nil {
		return 0, nil, err
	}
	notify.By = time.Duration(req.By)
	ctx := r.Context()
	// A re-send answers its first result before the rate limit counts it,
	// so an empty bucket never turns a committed post into a 429.
	switch first, found, rerr := s.store.ReplayPost(ctx, k); {
	case rerr != nil:
		return 0, nil, rerr
	case found:
		return http.StatusCreated, postOut(&first), nil
	}
	st, serr := s.store.Status(ctx, c)
	key, sender := st.Identity.ID, st.Identity.Name
	if st.Parent != 0 {
		key = st.Parent
	}
	replayed := false
	if serr == nil {
		at, retry, ok := s.posts.take(key, s.now())
		if !ok {
			s.metrics.PostsRefused.Inc(obs.RefusedRateLimited)
			return 0, nil, &apiError{
				status: http.StatusTooManyRequests, code: "rate_limited", retryAfter: retry,
				msg: "over " + strconv.Itoa(postsPerMinute) + " posts a minute from this identity and its delegates: post again in " + retry.String(),
			}
		}
		defer func() {
			if err != nil || replayed {
				s.posts.give(key, at)
			}
		}()
	}
	got, replayed, err := s.store.Post(ctx, k, &store.Posting{
		Where: req.Where, Caller: c, Subject: req.Subject, Body: req.Body, To: to, NotifyRead: notify,
		Fields: store.Fields{Name: req.Identity.Name, Description: req.Identity.Description, Paths: req.Identity.Paths},
	})
	switch {
	case err != nil:
		s.postRefused(err, sender)
		return 0, nil, err
	case !replayed:
		s.log.Info("message posted", "thread_kind", kind, "path", s.redact(to.Path), "to", to.Agent, "sender", got.Identity.Name,
			"message", got.Message, "recipients", got.Delivered, "same_environment", got.SameEnvironment,
			"other", got.Other, "subject", s.redact(req.Subject), "body", s.redact(req.Body))
		s.hintFanOut(&got, to.Agent != "", req.Subject)
	}
	return http.StatusCreated, postOut(&got), nil
}

// hintFanOut adds fanOutHint to a direct post's answer when fanOut.record
// finds its subject sent to another agent.
func (s *Server) hintFanOut(got *store.Posted, direct bool, subject string) {
	if direct && s.fanOut.record(got.Identity.ID, got.Thread, subject, s.now()) {
		got.Notices = append(got.Notices, fanOutHint)
	}
}

func postOut(got *store.Posted) postResponse {
	return postResponse{
		Notices: nonNil(got.Notices), Identity: identityOut(&got.Identity), Message: got.Message, Thread: got.Thread,
		Recipients: recipientsJSON{Total: got.Delivered, SameEnvironment: got.SameEnvironment, Other: got.Other},
	}
}

// postRefused logs and counts a post the store refused for its scope, and
// counts one refused for a full store.
func (s *Server) postRefused(err error, sender string) {
	se, ok := errors.AsType[*store.Error](err)
	switch {
	case !ok:
	case se.Kind == store.KindForbidden:
		s.metrics.PostsRefused.Inc(obs.RefusedScope)
		s.log.Info("post refused", "sender", sender, "rule", s.redact(se.Msg))
	case se.Kind == store.KindFull:
		s.metrics.PostsRefused.Inc(obs.RefusedStoreFull)
	}
}

// postTarget reads to: "announce", {"path": p} or {"agent": name}.
func postTarget(raw json.RawMessage) (store.Target, string, error) {
	const want = `to must be "announce", {"path": <path>} or {"agent": <name>}`
	var word string
	if json.Unmarshal(raw, &word) == nil {
		if word != announce {
			return store.Target{}, "", badRequest(want)
		}
		return store.Target{Announce: true}, announce, nil
	}
	var obj struct {
		Path  string `json:"path"`
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || (obj.Path == "") == (obj.Agent == "") {
		return store.Target{}, "", badRequest(want)
	}
	if obj.Path != "" {
		return store.Target{Path: obj.Path}, "path", nil
	}
	return store.Target{Agent: obj.Agent}, "direct", nil
}

// notifyRead reads notify_read: absent, "all", or a list of names.
func notifyRead(raw json.RawMessage) (store.NotifyRead, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return store.NotifyRead{}, nil
	}
	var word string
	if json.Unmarshal(raw, &word) == nil && word == "all" {
		return store.NotifyRead{All: true}, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil || len(names) == 0 {
		return store.NotifyRead{}, badRequest(`notify_read must be "all" or a list of recipient names`)
	}
	return store.NotifyRead{Names: names}, nil
}

// redact makes client text fit a log line: one line, at most logTextCap
// bytes, with the bearer token and every door secret cut out.
func (s *Server) redact(text string) string {
	out, _ := textsafe.RedactSingleLineCapped(text, logTextCap, "…", s.needles...)
	return out
}

// postLimit counts each identity's posts over the last minute, in memory.
type postLimit struct {
	m  map[uint64][]time.Time
	mu sync.Mutex
}

func newPostLimit() *postLimit { return &postLimit{m: map[uint64][]time.Time{}} }

// take admits one post of key at now, returning the instant it recorded
// for give; refused, it returns how long until the oldest post counted
// leaves the minute.
func (p *postLimit) take(key uint64, now time.Time) (at time.Time, retry time.Duration, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	times := p.m[key]
	for len(times) > 0 && !times[0].After(now.Add(-time.Minute)) {
		times = times[1:]
	}
	if len(times) >= postsPerMinute {
		p.m[key] = times
		return time.Time{}, retryAfter(times[0].Add(time.Minute).Sub(now)), false
	}
	p.m[key] = append(times, now)
	return now, 0, true
}

// prune drops every key whose posts have all left the minute.
func (p *postLimit) prune(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, times := range p.m {
		if !times[len(times)-1].After(now.Add(-time.Minute)) {
			delete(p.m, key)
		}
	}
}

// give takes back the post take recorded at at, for a post the store
// refused.
func (p *postLimit) give(key uint64, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	times := p.m[key]
	for i, v := range slices.Backward(times) {
		if v.Equal(at) {
			times = append(times[:i], times[i+1:]...)
			break
		}
	}
	if len(times) == 0 {
		delete(p.m, key)
		return
	}
	p.m[key] = times
}

// retryAfter is d rounded up to whole seconds with a 1 s floor, so a client
// that waits the Retry-After it was sent is admitted (RFC 9110 section 10.2.3).
func retryAfter(d time.Duration) time.Duration {
	if r := d.Truncate(time.Second); r < d {
		d = r + time.Second
	}
	return max(time.Second, d)
}
