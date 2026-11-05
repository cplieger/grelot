package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

// entryJSON is one inbox or history entry. Subject, body, text and the
// paths are other agents' text, which the client labels as data.
type entryJSON struct {
	PostedAt   time.Time `json:"posted_at,omitzero"`
	Written    time.Time `json:"written"`
	ReadAt     time.Time `json:"read_at,omitzero"`
	Kind       string    `json:"kind"`
	Subject    string    `json:"subject,omitempty"`
	Body       string    `json:"body,omitempty"`
	Text       string    `json:"text,omitempty"`
	ThreadPath string    `json:"thread_path,omitempty"`
	Matched    string    `json:"matched,omitempty"`
	From       string    `json:"from,omitempty"`
	Host       string    `json:"host,omitempty"`
	OS         string    `json:"os,omitempty"`
	Arch       string    `json:"arch,omitempty"`
	Relation   string    `json:"relation,omitempty"`
	Via        string    `json:"via,omitempty"`
	Seq        uint64    `json:"seq,omitempty"`
	Message    uint64    `json:"message,omitempty"`
	Thread     uint64    `json:"thread,omitempty"`
	Wake       bool      `json:"wake"`
	Truncated  bool      `json:"truncated,omitempty"`
}

func entriesOut(es []store.Entry) []entryJSON {
	out := make([]entryJSON, 0, len(es))
	for i := range es {
		e := &es[i]
		out = append(out, entryJSON{
			PostedAt: e.PostedAt.UTC(), Written: e.Written.UTC(), ReadAt: e.ReadAt.UTC(),
			Kind: e.Kind, Subject: e.Subject, Body: e.Body, Text: e.Text,
			ThreadPath: e.ThreadPath, Matched: e.Matched, From: e.From, Host: e.Host, OS: e.OS, Arch: e.Arch,
			Relation: e.Relation, Via: e.Via, Seq: e.Seq, Message: e.Message, Thread: e.Thread,
			Wake: e.Wake, Truncated: e.Truncated,
		})
	}
	return out
}

type inboxResponse struct {
	Entries []entryJSON `json:"entries"`
	Unread  countsJSON  `json:"unread"`
}

// inbox is GET /v1/inbox: the oldest unread entries, which it marks
// nothing; the client acks what it printed. With wait it is the in-turn
// wait.
func (s *Server) inbox(r *http.Request, c store.Caller) (status int, body any, err error) {
	limit, err := intParam(r, "limit")
	if err != nil {
		return 0, nil, err
	}
	wait, err := waitParam(r)
	if err != nil {
		return 0, nil, err
	}
	page, err := s.inTurnWait(r.Context(), c, limit, wait)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, inboxResponse{Entries: entriesOut(page.Entries), Unread: countsOut(page.Unread)}, nil
}

type ackRequest struct {
	Upto uint64 `json:"upto"`
}

type ackResponse struct {
	Left countsJSON `json:"left"`
	Read uint64     `json:"read"`
}

// ack is POST /v1/inbox/ack, which marks read every entry up to upto.
func (s *Server) ack(r *http.Request, c store.Caller) (status int, body any, err error) {
	var req ackRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	got, err := s.store.Ack(r.Context(), c, req.Upto)
	if err != nil {
		return 0, nil, err
	}
	if got.Count > 0 {
		s.log.Info("inbox read", "name", got.Identity.Name, "count", got.Count,
			"oldest_delay_ms", s.now().Sub(got.Oldest).Milliseconds())
	}
	return http.StatusOK, ackResponse{Left: countsOut(got.Left), Read: got.Read}, nil
}

// history is GET /v1/history, which reads the mailbox and moves nothing.
func (s *Server) history(r *http.Request, c store.Caller) (status int, body any, err error) {
	q := r.URL.Query()
	hq := store.HistoryQuery{Path: q.Get("path"), With: strings.TrimPrefix(q.Get("with"), "@"), Grep: q.Get("grep")}
	if from := q.Get("from"); from == "@me" {
		hq.FromMe = true
	} else {
		hq.From = strings.TrimPrefix(from, "@")
	}
	if v := q.Get("thread"); v != "" {
		if hq.Thread, err = strconv.ParseUint(v, 10, 64); err != nil || hq.Thread == 0 {
			return 0, nil, badRequest("thread must be a thread id")
		}
	}
	if hq.Limit, err = intParam(r, "limit"); err != nil {
		return 0, nil, err
	}
	now := s.now()
	if hq.Since, err = timeParam(q.Get("since"), "since", now); err != nil {
		return 0, nil, err
	}
	if hq.Until, err = timeParam(q.Get("until"), "until", now); err != nil {
		return 0, nil, err
	}
	for _, f := range []struct {
		dst  *bool
		name string
	}{{&hq.Announce, "announce"}, {&hq.Full, "full"}, {&hq.LastRead, "last_read"}} {
		if *f.dst, err = flag(q.Get(f.name), f.name); err != nil {
			return 0, nil, err
		}
	}
	got, err := s.store.History(r.Context(), c, &hq)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"entries": entriesOut(got.Entries)}, nil
}

// timeParam reads a history bound: a duration back from now, such as
// 2h, or an RFC 3339 time.
func timeParam(v, name string, now time.Time) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Time{}, badRequest(name + " must be a duration such as 2h or an RFC 3339 time")
}

// intParam reads a non-negative integer query parameter; 0 when absent.
func intParam(r *http.Request, name string) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, badRequest(name + " must be a whole number")
	}
	return n, nil
}

type threadJSON struct {
	Expires time.Time `json:"expires_at"`
	Path    string    `json:"path"`
	ID      uint64    `json:"id"`
}

type threadsResponse struct {
	Threads []threadJSON `json:"threads"`
}

// threads is GET /v1/threads: the live path threads under path.
func (s *Server) threads(r *http.Request, c store.Caller) (status int, body any, err error) {
	got, err := s.store.Threads(r.Context(), c, r.URL.Query().Get("path"))
	if err != nil {
		return 0, nil, err
	}
	out := make([]threadJSON, 0, len(got))
	for _, t := range got {
		out = append(out, threadJSON{Expires: t.Expires.UTC(), Path: t.Path, ID: t.ID})
	}
	return http.StatusOK, threadsResponse{Threads: out}, nil
}

type receiptJSON struct {
	ReadAt   time.Time `json:"read_at,omitzero"`
	Name     string    `json:"name"`
	Host     string    `json:"host"`
	Relation string    `json:"relation"`
	State    string    `json:"state"`
}

type receiptsResponse struct {
	PostedAt        time.Time     `json:"posted_at"`
	Subject         string        `json:"subject"`
	Kind            string        `json:"kind"`
	Recipients      []receiptJSON `json:"recipients"`
	Message         uint64        `json:"message"`
	RegisteredSince int           `json:"registered_since,omitempty"`
}

// receipts is GET /v1/messages/{id}/receipts.
func (s *Server) receipts(r *http.Request, c store.Caller) (status int, body any, err error) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		return 0, nil, badRequest("the message id must be a whole number")
	}
	got, err := s.store.Receipts(r.Context(), c, id)
	if err != nil {
		return 0, nil, err
	}
	out := receiptsResponse{
		PostedAt: got.PostedAt.UTC(), Subject: got.Subject, Kind: got.Kind, Message: got.Message,
		RegisteredSince: got.RegisteredSince, Recipients: make([]receiptJSON, 0, len(got.Recipients)),
	}
	for _, v := range got.Recipients {
		out.Recipients = append(out.Recipients, receiptJSON{
			ReadAt: v.ReadAt.UTC(), Name: v.Name, Host: v.Host, Relation: v.Relation, State: v.State,
		})
	}
	return http.StatusOK, out, nil
}

// follow is PUT /v1/follows?path=.
func (s *Server) follow(r *http.Request, c store.Caller) (status int, body any, err error) {
	got, err := s.store.Follow(r.Context(), c, r.URL.Query().Get("path"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, identityResponse{Identity: identityOut(&got)}, nil
}

// unfollow is DELETE /v1/follows?path=.
func (s *Server) unfollow(r *http.Request, c store.Caller) (status int, body any, err error) {
	got, err := s.store.Unfollow(r.Context(), c, r.URL.Query().Get("path"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, identityResponse{Identity: identityOut(&got)}, nil
}
