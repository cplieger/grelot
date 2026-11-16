package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/cplieger/go/httpclient"
	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/grelot/internal/version"
	"github.com/cplieger/grelot/internal/wire"
)

// defaultTimeout bounds a call, its retries included, when its Request sets
// none.
const defaultTimeout = 15 * time.Second

// maxResponse bounds a response body: an inbox page of 20 bodies of 64 KiB
// with their headers fits several times over.
const maxResponse = 8 << 20

// clientTimeout is the http.Client's ceiling, above every call's context
// deadline: the longest is a 55 s wait with its margin.
const clientTimeout = 2 * time.Minute

// API sends one command's requests to the server as one caller. Session is
// <provider>:<host session id>, empty for a command that acts as no one;
// As and Self are the delegate headers, Grelot-As and Grelot-Self.
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

// NewAPI builds the API for cfg. clientVersion goes out as Grelot-Version; the
// path root an answer names is cached under configDir. A GET, and a write
// carrying its Idempotency-Key (every one but a Request.Once), is retried
// with backoff on a network error or a retryable status other than 429,
// which answers at once naming its wait. No redirect is followed, since
// one would carry the bearer and the key elsewhere.
func NewAPI(cfg *Config, clientVersion, configDir string) (*API, error) {
	return newAPI(cfg, clientVersion, configDir, httpclient.TransportConfig{BaseDelay: retryDelay, CheckRetry: checkRetry})
}

// checkRetry is the library's verdict less 429: a rate-limited post's wait
// is up to a minute, past a call's budget.
func checkRetry(_ context.Context, resp *http.Response, err error) (bool, error) {
	if err != nil {
		return httpclient.IsTransient(err), nil
	}
	return resp.StatusCode != http.StatusTooManyRequests && httpclient.IsRetryableStatus(resp.StatusCode), nil
}

// NewOnceAPI is NewAPI sending every request once: a hook makes at most
// one request within its budget, and a version check reports a server
// that does not answer at once.
func NewOnceAPI(cfg *Config, clientVersion, configDir string) (*API, error) {
	return newAPI(cfg, clientVersion, configDir, httpclient.TransportConfig{MaxAttempts: -1})
}

func newAPI(cfg *Config, clientVersion, configDir string, retry httpclient.TransportConfig) (*API, error) {
	base, err := httpclient.CloneDefaultTransport()
	if err != nil {
		return nil, err
	}
	c := httpclient.NewRetry(base, httpclient.RefuseAllRedirects, retry)
	c.Timeout = clientTimeout
	return &API{http: c, server: cfg.Server, token: cfg.Token, version: clientVersion, configDir: configDir}, nil
}

// retryDelay is the first backoff of a retried call: a command waits on
// it, and a hook's budget bounds the whole call.
const retryDelay = 250 * time.Millisecond

// Request is one API call. Path is the route under the server URL, from
// wire.Path; Header adds to the identity headers; Body, when not nil, goes out
// as JSON. Timeout bounds the call with its retries, defaultTimeout when
// zero. A write other than a GET carries a new Idempotency-Key, which every
// retry of the call re-sends, unless Once is set: the route takes no key
// and the call is sent once.
type Request struct {
	Query   url.Values
	Header  http.Header
	Body    any
	Method  string
	Path    string
	Timeout time.Duration
	Once    bool
}

// Response is an answer: its status line and the server version it names,
// each sanitized to one line, and its raw body. Do returns it beside an
// *APIError too.
type Response struct {
	Status  string
	Version string
	Body    []byte
	Code    int
}

// APIError is a non-2xx answer: the envelope's members, Msg its error
// sanitized to one line. Upgrade is set when a server newer than this
// client refused a request it may have refused for that reason: the line
// naming the client to install.
type APIError struct {
	Status  string
	Msg     string
	Code    string
	Reason  string
	Upgrade string
	Fields  []wire.FieldError
	// RetryAfter is how long the server asked the caller to wait, zero
	// when no wait helps.
	RetryAfter time.Duration
	HTTP       int
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
		timeout = defaultTimeout
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
	got := Response{
		Status:  textsafe.SanitizeSingleLine(resp.Header.Get(wire.HeaderStatus)),
		Version: textsafe.SanitizeSingleLine(resp.Header.Get(wire.HeaderVersion)),
		Body:    body, Code: resp.StatusCode,
	}
	server := a.noteVersion(resp.Header.Get(wire.HeaderVersion))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := a.refusal(&got, server)
		return got, err
	}
	if out != nil && json.Unmarshal(body, out) != nil {
		return got, fmt.Errorf("grelot: the server's answer to %s %s is not the JSON this client reads; check that the client and server versions match with grelot version", r.Method, r.Path)
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
	req.Header.Set(wire.HeaderVersion, a.version)
	if a.Session != "" {
		req.Header.Set(wire.HeaderSession, a.Session)
	}
	if a.As != "" {
		req.Header.Set(wire.HeaderAs, a.As)
	}
	if a.Self {
		req.Header.Set(wire.HeaderSelf, "1")
	}
	if r.Method != http.MethodGet && !r.Once {
		req.Header.Set(wire.HeaderIdempotencyKey, newKey())
	}
	return req, nil
}

// newKey is a fresh Idempotency-Key: 16 random bytes as base64url, quoted
// as the RFC 8941 String the header carries.
func newKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error.
	return `"` + base64.RawURLEncoding.EncodeToString(b) + `"`
}

// noteVersion caches the server version an answer names and answers it,
// or the cached one when the answer names none.
func (a *API) noteVersion(v string) string {
	f := LoadFacts(a.configDir)
	if _, ok := version.Parse(v); !ok || v == f.Version {
		return f.Version
	}
	f.Version = v
	SaveFacts(a.configDir, f)
	return v
}

// skewStatus are the refusals a newer server may answer a request it no
// longer reads: a malformed request, a route or method it dropped, a body
// it no longer takes.
var skewStatus = map[int]bool{
	http.StatusBadRequest: true, http.StatusNotFound: true, http.StatusMethodNotAllowed: true,
	http.StatusUnsupportedMediaType: true, http.StatusUnprocessableEntity: true,
}

// refusal reads a non-2xx answer from a server of version server.
func (a *API) refusal(r *Response, server string) error {
	var eb wire.Error
	if json.Unmarshal(r.Body, &eb) != nil || eb.Error == "" {
		eb = wire.Error{Error: fmt.Sprintf("the server answered %d %s", r.Code, http.StatusText(r.Code))}
	}
	ae := &APIError{
		Status: r.Status, HTTP: r.Code,
		Msg:    textsafe.SanitizeSingleLine(eb.Error),
		Code:   textsafe.SanitizeSingleLine(eb.Code),
		Reason: textsafe.SanitizeSingleLine(eb.Reason),
		Fields: eb.Fields, RetryAfter: time.Duration(eb.RetryAfterMS) * time.Millisecond,
	}
	if skewStatus[r.Code] && older(a.version, server) {
		ae.Upgrade = upgradeLine(a.version, server)
	}
	return ae
}

// Me sends GET /v1/me with q and header and caches the path root and
// server version it answers.
func (a *API) Me(ctx context.Context, q url.Values, header http.Header) (*wire.Me, Response, error) {
	var me wire.Me
	resp, err := a.Do(ctx, &Request{Method: http.MethodGet, Path: wire.Path("getMe", nil), Query: q, Header: header}, &me)
	if err != nil {
		return nil, resp, err
	}
	a.noteFacts(me.PathRoot, me.Version)
	return &me, resp, nil
}

// Version sends GET /v1/version, as r asks, and caches the path root and
// server version it answers.
func (a *API) Version(ctx context.Context, r *Request) (*wire.Version, Response, error) {
	var v wire.Version
	r.Method, r.Path = http.MethodGet, wire.Path("getVersion", nil)
	resp, err := a.Do(ctx, r, &v)
	if err != nil {
		return nil, resp, err
	}
	a.noteFacts(v.PathRoot, v.Version)
	return &v, resp, nil
}

func (a *API) noteFacts(pathRoot, server string) {
	f := LoadFacts(a.configDir)
	f.PathRoot, f.Version = pathRoot, server
	SaveFacts(a.configDir, f)
}
