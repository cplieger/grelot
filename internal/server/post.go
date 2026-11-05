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
func (s *Server) post(r *http.Request, c store.Caller) (status int, body any, err error) {
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
	st, serr := s.store.Status(ctx, c)
	key, sender := st.Identity.ID, st.Identity.Name
	if st.Parent != 0 {
		key = st.Parent
	}
	if serr == nil {
		at, retry, ok := s.posts.take(key, s.now())
		if !ok {
			return 0, nil, &apiError{
				status: http.StatusTooManyRequests, code: "rate_limited", retryAfter: retry,
				msg: "over " + strconv.Itoa(postsPerMinute) + " posts a minute from this identity and its delegates: post again in " + retry.String(),
			}
		}
		defer func() {
			if err != nil {
				s.posts.give(key, at)
			}
		}()
	}
	got, err := s.store.Post(ctx, &store.Posting{
		Where: req.Where, Caller: c, Subject: req.Subject, Body: req.Body, To: to, NotifyRead: notify,
		Fields: store.Fields{Name: req.Identity.Name, Description: req.Identity.Description, Paths: req.Identity.Paths},
	})
	if err != nil {
		var se *store.Error
		if errors.As(err, &se) && se.Kind == store.KindForbidden {
			s.log.Info("post refused", "sender", sender, "rule", s.redact(se.Msg))
		}
		return 0, nil, err
	}
	s.log.Info("message posted", "thread_kind", kind, "path", s.redact(to.Path), "sender", got.Identity.Name,
		"message", got.Message, "recipients", got.Delivered, "same_environment", got.SameEnvironment,
		"other", got.Other, "subject", s.redact(req.Subject), "body", s.redact(req.Body))
	return http.StatusCreated, postResponse{
		Notices: nonNil(got.Notices), Identity: identityOut(&got.Identity), Message: got.Message, Thread: got.Thread,
		Recipients: recipientsJSON{Total: got.Delivered, SameEnvironment: got.SameEnvironment, Other: got.Other},
	}, nil
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
// bytes, with the bearer token cut out.
func (s *Server) redact(text string) string {
	out, _ := textsafe.RedactSingleLineCapped(text, logTextCap, "…", s.cfg.Token)
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
		return time.Time{}, max(time.Second, times[0].Add(time.Minute).Sub(now).Round(time.Second)), false
	}
	p.m[key] = append(times, now)
	return now, 0, true
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
