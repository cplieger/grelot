package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/cplieger/go/httpclient"
	"github.com/cplieger/go/textsafe"
)

// The headers the API reads and writes.
const (
	headerVersion = "Grelot-Version"
	headerSession = "Grelot-Session"
	headerAs      = "Grelot-As"
	headerSelf    = "Grelot-Self"
	headerStatus  = "Grelot-Status"
	// HeaderEnvironment and HeaderMachine carry the caller's where values to
	// GET /v1/me for the take-over hint; the server never reads them from a
	// URL, which access logs keep.
	HeaderEnvironment = "Grelot-Environment"
	HeaderMachine     = "Grelot-Machine"
)

// DefaultTimeout bounds a call, its retries included, when its Request sets
// none.
const DefaultTimeout = 15 * time.Second

// maxResponse bounds a response body: an inbox page of 20 bodies of 64 KiB
// with their headers fits several times over.
const maxResponse = 8 << 20

// clientTimeout is the http.Client's ceiling, above every call's context
// deadline: the longest is a 55 s wait with its margin.
const clientTimeout = 2 * time.Minute

// API sends one command's requests to the server as one caller. Session is
// <provider>:<host session id>, empty for a command that acts as no one;
// As and Self are the delegate headers of design section 3.
type API struct {
	http      *http.Client
	server    string
	token     string
	version   string
	configDir string
	Session   string
	As        string
	Self      bool
}

// NewAPI builds the API for cfg. version goes out as Grelot-Version; the
// path root an answer names is cached under configDir. GET requests are
// retried with backoff, every other method is sent once, and no redirect
// is followed, since one would carry the bearer elsewhere.
func NewAPI(cfg *Config, version, configDir string) (*API, error) {
	return newAPI(cfg, version, configDir, httpclient.TransportConfig{BaseDelay: retryDelay})
}

// NewHookAPI is NewAPI sending every request once, since a hook makes at
// most one request within its budget.
func NewHookAPI(cfg *Config, version, configDir string) (*API, error) {
	return newAPI(cfg, version, configDir, httpclient.TransportConfig{MaxAttempts: -1})
}

func newAPI(cfg *Config, version, configDir string, retry httpclient.TransportConfig) (*API, error) {
	base, err := httpclient.CloneDefaultTransport()
	if err != nil {
		return nil, err
	}
	c := httpclient.NewRetry(base, httpclient.RefuseAllRedirects, retry)
	c.Timeout = clientTimeout
	return &API{http: c, server: cfg.Server, token: cfg.Token, version: version, configDir: configDir}, nil
}

// retryDelay is the first backoff of a retried GET: a command waits on it,
// and a hook's budget bounds the whole call.
const retryDelay = 250 * time.Millisecond

// Request is one API call. Path is the route under the server URL, such as
// /v1/me; Header adds to the identity headers; Body, when not nil, goes out
// as JSON. Timeout bounds the call with its retries, DefaultTimeout when
// zero.
type Request struct {
	Query   url.Values
	Header  http.Header
	Body    any
	Method  string
	Path    string
	Timeout time.Duration
}

// Response is a 2xx answer: its status line, sanitized to one line, and its
// raw body.
type Response struct {
	Status string
	Body   []byte
	Code   int
}

// APIError is a non-2xx answer. Msg is the server's reason, one line naming
// the fix; Code is its error code, such as conflict or the store's why.
type APIError struct {
	Status string
	Msg    string
	Code   string
	HTTP   int
}

func (e *APIError) Error() string { return "grelot: " + e.Msg }

// UnreachableError is a call the server never answered: no connection, a
// network failure or the call's timeout. Timeout tells the last apart, so a
// post can say its outcome is unknown instead of reading it as a refusal.
type UnreachableError struct {
	Err     error
	Server  string
	Timeout bool
}

func (e *UnreachableError) Error() string {
	if e.Timeout {
		return fmt.Sprintf("grelot: the server %s did not answer in time", e.Server)
	}
	return fmt.Sprintf("grelot: cannot reach the server %s: %v", e.Server, e.Err)
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// Do sends r and decodes a 2xx JSON body into out when out is not nil. A
// non-2xx answer is an *APIError, a call never answered an
// *UnreachableError, and ctx's own end is ctx's error.
func (a *API) Do(ctx context.Context, r *Request, out any) (Response, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := a.request(callCtx, r)
	if err != nil {
		return Response{}, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		timedOut := httpclient.CauseOf(err) == httpclient.CauseTimeout
		return Response{}, &UnreachableError{Server: a.server, Timeout: timedOut, Err: httpclient.LogSafeError(err)}
	}
	defer resp.Body.Close()
	body, err := httpclient.ReadLimitedBody(resp.Body, maxResponse)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, &UnreachableError{Server: a.server, Timeout: callCtx.Err() != nil, Err: httpclient.LogSafeError(err)}
	}
	got := Response{Status: textsafe.SanitizeSingleLine(resp.Header.Get(headerStatus)), Body: body, Code: resp.StatusCode}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := a.refusal(&got)
		return got, err
	}
	if out != nil && json.Unmarshal(body, out) != nil {
		return got, fmt.Errorf("grelot: the server's answer to %s %s is not the JSON this client reads; check that the client and server versions match with grelot me", r.Method, r.Path)
	}
	return got, nil
}

func (a *API) request(ctx context.Context, r *Request) (*http.Request, error) {
	u := a.server + r.Path
	if len(r.Query) > 0 {
		u += "?" + r.Query.Encode()
	}
	body := io.Reader(http.NoBody)
	if r.Body != nil {
		data, err := json.Marshal(r.Body)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, u, body)
	if err != nil {
		return nil, err
	}
	for name, values := range r.Header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	if r.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set(headerVersion, a.version)
	if a.Session != "" {
		req.Header.Set(headerSession, a.Session)
	}
	if a.As != "" {
		req.Header.Set(headerAs, a.As)
	}
	if a.Self {
		req.Header.Set(headerSelf, "1")
	}
	return req, nil
}

// errorBody is the server's error answer.
type errorBody struct {
	Error    string `json:"error"`
	Code     string `json:"code"`
	PathRoot string `json:"path_root"`
}

// refusal reads a non-2xx answer, caching the path root it names.
func (a *API) refusal(r *Response) error {
	var eb errorBody
	if json.Unmarshal(r.Body, &eb) != nil || eb.Error == "" {
		eb = errorBody{Error: fmt.Sprintf("the server answered %d %s", r.Code, http.StatusText(r.Code))}
	}
	if eb.PathRoot != "" {
		f := LoadFacts(a.configDir)
		f.PathRoot = eb.PathRoot
		SaveFacts(a.configDir, f)
	}
	return &APIError{
		Status: r.Status, HTTP: r.Code,
		Msg:  textsafe.SanitizeSingleLine(eb.Error),
		Code: textsafe.SanitizeSingleLine(eb.Code),
	}
}

// Me sends GET /v1/me with q and header and caches the path root and
// server version it answers.
func (a *API) Me(ctx context.Context, q url.Values, header http.Header) (*Me, Response, error) {
	var me Me
	resp, err := a.Do(ctx, &Request{Method: http.MethodGet, Path: "/v1/me", Query: q, Header: header}, &me)
	if err != nil {
		return nil, resp, err
	}
	SaveFacts(a.configDir, Facts{PathRoot: me.PathRoot, Version: me.Version})
	return &me, resp, nil
}
