package transport

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/go/httpclient"
	"github.com/cplieger/grelot/internal/provider"
)

// Outcome is what one delivery through a door came to.
type Outcome int

// The outcomes. Gone is a give up the door's receiver named as a session it
// no longer has.
const (
	Accepted Outcome = iota + 1
	RetryLater
	GiveUp
	Gone
)

// The Retry-After clamp: the floor keeps a receiver from making the server
// spin, and the ceiling is the dedupe window's 60 s floor, so every retry of a
// key reaches a receiver that still holds it.
const (
	retryFloor   = 5 * time.Second
	retryCeiling = 60 * time.Second
	retryDefault = 15 * time.Second
)

var httpTimeout = 10 * time.Second

// HTTPResult is one http send. Status is 0 when no response came back; Err is
// then the network error or timeout, reduced so it carries no URL.
type HTTPResult struct {
	Err        error
	Outcome    Outcome
	Status     int
	RetryAfter time.Duration
}

// HTTP sends http doors' requests. It is safe for concurrent use.
type HTTP struct{ client *http.Client }

// NewHTTP returns the sender: one try per Send with a 10 s timeout, and no
// redirect followed, since a redirect would carry the door's credential
// headers to a host the door's file does not name.
func NewHTTP() *HTTP {
	c := httpclient.New(httpTimeout)
	c.CheckRedirect = httpclient.RefuseAllRedirects
	return &HTTP{client: c}
}

// Send posts the door's request rendered with data, once. Any 2xx is
// Accepted; a status in the door's gone list is Gone; a 429 or a 5xx is
// RetryLater with the receiver's Retry-After clamped to 5 to 60 s (15 s
// without one), as are a timeout and a network error; any other status is
// GiveUp. Which give up turns off the door and which only one identity is the
// caller's to decide from Status. The error is for a request never sent: the
// door does not render, or ctx ended.
func (h *HTTP) Send(ctx context.Context, door *provider.Door, data *provider.Data) (HTTPResult, error) {
	r, err := door.RenderRequest(data)
	if err != nil {
		return HTTPResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, strings.NewReader(r.Body))
	if err != nil {
		return HTTPResult{}, errors.New("transport: the door's rendered URL is not a request URL")
	}
	for name, v := range r.Header {
		req.Header.Set(name, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return HTTPResult{}, ctx.Err()
		}
		return HTTPResult{Outcome: RetryLater, RetryAfter: retryDefault, Err: httpclient.LogSafeError(err)}, nil
	}
	_ = resp.Body.Close()
	res := HTTPResult{Status: resp.StatusCode}
	switch code := resp.StatusCode; {
	case code >= 200 && code <= 299:
		res.Outcome = Accepted
	case slices.Contains(door.Gone, code):
		res.Outcome = Gone
	case code == http.StatusTooManyRequests || (code >= 500 && code <= 599):
		res.Outcome = RetryLater
		res.RetryAfter = retryAfter(resp)
	default:
		res.Outcome = GiveUp
	}
	return res, nil
}

// retryAfter reads a Retry-After value (RFC 9110 section 10.2.3): delta
// seconds or an HTTP date, clamped; a missing or malformed one gives the
// default.
func retryAfter(resp *http.Response) time.Duration {
	h := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if _, err := http.ParseTime(h); err != nil && !isDigits(h) {
		return retryDefault
	}
	d := httpclient.ParseRetryAfterResponse(resp)
	if d == 0 && isDigits(h) && strings.Trim(h, "0") != "" {
		// httpclient reads a delta too large for an int64 as 0.
		return retryCeiling
	}
	return min(max(d, retryFloor), retryCeiling)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
