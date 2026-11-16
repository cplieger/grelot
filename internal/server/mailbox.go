//go:build !windows

package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

func entriesOut(es []store.Entry) []wire.Entry {
	out := make([]wire.Entry, 0, len(es))
	for i := range es {
		e := &es[i]
		out = append(out, wire.Entry{
			PostedAt: e.PostedAt.UTC(), Written: e.Written.UTC(), ReadAt: e.ReadAt.UTC(),
			Kind: e.Kind, Subject: e.Subject, Body: e.Body, Text: e.Text,
			ThreadPath: e.ThreadPath, Matched: e.Matched, From: e.From, Host: e.Host, OS: e.OS, Arch: e.Arch,
			Relation: e.Relation, Via: e.Via, Seq: e.Seq, Message: e.Message, Thread: e.Thread,
			Wake: e.Wake, Truncated: e.Truncated,
		})
	}
	return out
}

// inbox is GET /v1/inbox: the oldest unread entries, which it marks
// nothing; the client acks what it printed. With wait it is the in-turn
// wait.
func (s *Server) inbox(r *http.Request, c store.Caller, _ store.Keyed) (status int, body any, err error) {
	p := pagingOf(r)
	q := store.InboxQuery{Limit: p.limit}
	if p.after != nil {
		if q.After, err = afterSeq(p.after); err != nil {
			return 0, nil, err
		}
	}
	wait, err := waitParam(r)
	if err != nil {
		return 0, nil, err
	}
	page, err := s.inTurnWait(r.Context(), c, q, wait)
	if err != nil {
		return 0, nil, err
	}
	out := wire.Inbox{List: wire.List[wire.Entry]{Items: entriesOut(page.Items), Total: page.Total}, Unread: countsOut(page.Unread)}
	if n := len(page.Items); n > 0 {
		out.NextCursor, err = s.next(r, page.More, seqPosition(page.Items[n-1].Seq))
	}
	return http.StatusOK, out, err
}

// ack is POST /v1/inbox/ack, which marks read every entry up to upto.
func (s *Server) ack(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	var req wire.AckRequest
	if derr := decode(r, &req); derr != nil {
		return 0, nil, derr
	}
	got, replayed, err := s.store.Ack(r.Context(), k, c, req.Upto)
	if err != nil {
		return 0, nil, err
	}
	if got.Count > 0 && !replayed {
		s.log.Info("inbox read", "name", got.Identity.Name, "count", got.Count,
			"oldest_delay_ms", s.now().Sub(got.Oldest).Milliseconds())
	}
	return http.StatusOK, wire.Acked{Left: countsOut(got.Left), Read: got.Read}, nil
}

// history is GET /v1/history, which reads the mailbox and moves nothing.
func (s *Server) history(r *http.Request, c store.Caller, _ store.Keyed) (status int, body any, err error) {
	hq, err := historyQuery(r)
	if err != nil {
		return 0, nil, err
	}
	p := pagingOf(r)
	hq.Limit = p.limit
	if p.after != nil {
		if hq.Before, hq.Since, err = afterHistory(p.after); err != nil {
			return 0, nil, err
		}
	}
	got, err := s.store.History(r.Context(), c, &hq)
	if err != nil {
		return 0, nil, err
	}
	out := wire.History{List: wire.List[wire.Entry]{Items: entriesOut(got.Items), Total: got.Total}}
	if len(got.Items) > 0 {
		out.NextCursor, err = s.next(r, got.More, historyPosition(got.Items[0].Key(), got.Since))
	}
	return http.StatusOK, out, err
}

// historyQuery reads GET /v1/history's filters.
func historyQuery(r *http.Request) (hq store.HistoryQuery, err error) {
	q := r.URL.Query()
	hq = store.HistoryQuery{Path: q.Get("path"), With: strings.TrimPrefix(q.Get("with"), "@"), Grep: q.Get("grep")}
	if from := q.Get("from"); from == "@me" {
		hq.FromMe = true
	} else {
		hq.From = strings.TrimPrefix(from, "@")
	}
	if v := q.Get("thread"); v != "" {
		if hq.Thread, err = strconv.ParseUint(v, 10, 64); err != nil || hq.Thread == 0 {
			return hq, badRequest("thread must be a thread id")
		}
	}
	if hq.Since, err = instantParam(q.Get("since"), "since"); err != nil {
		return hq, err
	}
	if hq.Until, err = instantParam(q.Get("until"), "until"); err != nil {
		return hq, err
	}
	for _, f := range []struct {
		dst  *bool
		name string
	}{{&hq.Announce, "announce"}, {&hq.Full, "full"}, {&hq.LastRead, "last_read"}} {
		if *f.dst, err = flag(q.Get(f.name), f.name); err != nil {
			return hq, err
		}
	}
	return hq, nil
}

// instantParam reads a history bound, an RFC 3339 instant.
func instantParam(v, name string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, badRequest(name + " must be an RFC 3339 instant")
	}
	return t, nil
}

// threads is GET /v1/threads: the live path threads under path.
func (s *Server) threads(r *http.Request, c store.Caller, _ store.Keyed) (status int, body any, err error) {
	p := pagingOf(r)
	got, err := s.store.Threads(r.Context(), c, store.ThreadQuery{Root: r.URL.Query().Get("path"), After: string(p.after), Limit: p.limit})
	if err != nil {
		return 0, nil, err
	}
	out := wire.Threads{List: wire.List[wire.Thread]{Items: make([]wire.Thread, 0, len(got.Items)), Total: got.Total}}
	for _, t := range got.Items {
		out.Items = append(out.Items, wire.Thread{Expires: t.Expires.UTC(), Path: t.Path, ID: t.ID})
	}
	if n := len(got.Items); n > 0 {
		out.NextCursor, err = s.next(r, got.More, []byte(got.Items[n-1].Path))
	}
	return http.StatusOK, out, err
}

// receipts is GET /v1/messages/{id}/receipts.
func (s *Server) receipts(r *http.Request, c store.Caller, _ store.Keyed) (status int, body any, err error) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		return 0, nil, badRequest("the message id must be a whole number")
	}
	got, err := s.store.Receipts(r.Context(), c, id)
	if err != nil {
		return 0, nil, err
	}
	out := wire.Receipts{
		PostedAt: got.PostedAt.UTC(), Subject: got.Subject, Kind: got.Kind, Message: got.Message,
		RegisteredSince: got.RegisteredSince, Recipients: make([]wire.Receipt, 0, len(got.Recipients)),
	}
	for _, v := range got.Recipients {
		out.Recipients = append(out.Recipients, wire.Receipt{
			ReadAt: v.ReadAt.UTC(), Name: v.Name, Host: v.Host, Relation: v.Relation, State: v.State,
		})
	}
	return http.StatusOK, out, nil
}

// follow is PUT /v1/follows?path=.
func (s *Server) follow(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	got, err := s.store.Follow(r.Context(), k, c, r.URL.Query().Get("path"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, wire.IdentityAnswer{Notices: nonNil(got.Notices), Identity: identityOut(&got.Identity)}, nil
}

// unfollow is DELETE /v1/follows?path=.
func (s *Server) unfollow(r *http.Request, c store.Caller, k store.Keyed) (status int, body any, err error) {
	got, err := s.store.Unfollow(r.Context(), k, c, r.URL.Query().Get("path"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, wire.IdentityAnswer{Notices: nonNil(got.Notices), Identity: identityOut(&got.Identity)}, nil
}
