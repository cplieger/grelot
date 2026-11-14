//go:build !windows

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/go/sse/v2"
	"github.com/cplieger/go/sse/v2/ssetest"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/webui/wire"
)

const (
	eventHello     = "sse:hello"
	eventKeepalive = "sse:keepalive"
	eventReset     = "sse:reset"
	// streamClosed is the Event next reports once the stream ends.
	streamClosed = "closed"
)

// stream is one open /api/events answer, read frame by frame.
type stream struct {
	frames chan ssetest.Frame
	resp   *http.Response
}

// openStream opens /api/events at url with h's browser, presenting lastID
// as Last-Event-ID unless it is empty.
func openStream(t *testing.T, h *harness, url, lastID string) *stream {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/api/events", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	h.browser.add(req)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	s := &stream{frames: make(chan ssetest.Frame, 16), resp: resp}
	go func() {
		defer close(s.frames)
		fr := ssetest.NewFrameReader(resp.Body)
		for {
			got, rerr := fr.Read(1)
			if rerr != nil {
				return
			}
			select {
			case s.frames <- got[0]:
			case <-t.Context().Done():
				return
			}
		}
	}()
	return s
}

// next is the stream's next frame whose event is not in skip, or one whose
// Event is streamClosed once the stream ends. A retry field is always
// skipped.
func (s *stream) next(t *testing.T, skip ...string) ssetest.Frame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-s.frames:
			switch {
			case !ok:
				return ssetest.Frame{Event: streamClosed}
			case f.Event == ssetest.RetryEvent || slices.Contains(skip, f.Event):
				continue
			}
			return f
		case <-deadline:
			t.Fatal("no frame within 5s")
			return ssetest.Frame{}
		}
	}
}

// hello reads the stream's hello, failing the test without one.
func (s *stream) hello(t *testing.T) sse.Hello {
	t.Helper()
	f := s.next(t, eventKeepalive)
	var h sse.Hello
	if f.Event != eventHello || json.Unmarshal([]byte(f.Data), &h) != nil {
		t.Fatalf("first frame %+v, want the hello", f)
	}
	return h
}

// changed reads the stream's next changed event.
func (s *stream) changed(t *testing.T) (wire.Changed, ssetest.Frame) {
	t.Helper()
	f := s.next(t, eventKeepalive)
	var c wire.Changed
	if f.Event != wire.EventChanged || json.Unmarshal([]byte(f.Data), &c) != nil {
		t.Fatalf("frame %+v, want a changed event", f)
	}
	return c, f
}

// streamServer serves h with a ReadTimeout below the tests' waits, which
// a stream must outlive.
func streamServer(t *testing.T, h *harness) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h.srv.Handler())
	srv.Config.ReadTimeout = 100 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func TestEvents_aReconnectResumesWithWhatCommittedWhileAway(t *testing.T) {
	h := newSignedIn(t)
	h.mustRegister(sessA, "alpha")
	srv := streamServer(t, h)

	s := openStream(t, h, srv.URL, "")
	if ct := s.resp.Header.Get("Content-Type"); s.resp.StatusCode != http.StatusOK || ct != "text/event-stream" {
		t.Fatalf("stream answer %d %s, want 200 text/event-stream", s.resp.StatusCode, ct)
	}
	if hl := s.hello(t); hl.Resumed || hl.Verdict != sse.VerdictFresh {
		t.Errorf("first hello %+v, want a fresh stream", hl)
	}
	time.Sleep(300 * time.Millisecond)
	first := userPost(h, wire.PostTarget{Agent: "alpha"}, "streamed")
	c, frame := s.changed(t)
	if !slices.Contains(c.Conversations, first.Thread) || frame.ID == "" {
		t.Errorf("change %+v id %q, want thread %d under a cursor", c, frame.ID, first.Thread)
	}
	_ = s.resp.Body.Close()

	away := userPost(h, wire.PostTarget{Announce: true}, "while away")
	back := openStream(t, h, srv.URL, frame.ID)
	if hl := back.hello(t); !hl.Resumed {
		t.Errorf("hello on reconnect %+v, want resumed", hl)
	}
	if c, _ := back.changed(t); !slices.Equal(c.Conversations, []uint64{away.Thread}) {
		t.Errorf("replayed change %+v, want the announcement thread %d alone", c, away.Thread)
	}
}

func TestEvents_theKeepaliveIsAnEventThePageSees(t *testing.T) {
	h := newSignedIn(t)
	s := openStream(t, h, streamServer(t, h).URL, "")
	hl := s.hello(t)
	if f := s.next(t); f.Event != eventKeepalive || hl.KeepaliveEvent != eventKeepalive {
		t.Errorf("frame %+v with the hello naming %q, want a dispatched %s event", f, hl.KeepaliveEvent, eventKeepalive)
	}
}

func TestEvents_aReleaseEndsEveryStreamAndRefusesNewOnes(t *testing.T) {
	h := newSignedIn(t)
	srv := streamServer(t, h)
	s := openStream(t, h, srv.URL, "")
	s.hello(t)

	h.srv.Release(t.Context())
	if f := s.next(t, eventKeepalive); f.Event != eventReset || f.Data != `{"reason":"shutdown"}` {
		t.Errorf("frame after Release %+v, want the shutdown reset", f)
	}
	if f := s.next(t, eventKeepalive); f.Event != streamClosed {
		t.Errorf("frame after the reset %+v, want the stream closed", f)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/events", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	h.browser.add(req)
	late, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = late.Body.Close() }()
	if late.StatusCode != wire.ErrSSEUnavailable.Status {
		t.Errorf("a stream opened after Release answers %d, want %d", late.StatusCode, wire.ErrSSEUnavailable.Status)
	}
	var body wire.Error
	if derr := json.NewDecoder(late.Body).Decode(&body); derr != nil || body.Code != wire.ErrSSEUnavailable.Code {
		t.Errorf("a stream opened after Release answers code %q (%v), want %s", body.Code, derr, wire.ErrSSEUnavailable.Code)
	}
}

func TestEvents_anEmptyChangePublishesNothing(t *testing.T) {
	e := newEvents(nil)
	e.Changed(store.WebChange{})
	e.Changed(store.WebChange{Conversations: []uint64{}})
	if head := e.hub.Position().Head; head != 0 {
		t.Errorf("head %d after two empty changes, want nothing published", head)
	}
	e.Changed(store.WebChange{Agents: true})
	if head := e.hub.Position().Head; head != 1 {
		t.Errorf("head %d after a change naming the agents, want one frame", head)
	}
}
