package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/transport"
)

// maxHold is the longest one standing or in-turn wait holds, the server's
// 55 s ceiling; a longer wait is several requests.
var maxHold = 55 * time.Second

// waitKey names the hold in a standing wait's body and an in-turn wait's
// query.
const waitKey = "wait"

// waitMargin is how long past its hold a wait request may take to answer.
const waitMargin = 15 * time.Second

// The watcher's backoff while the server cannot be reached or answers 5xx.
var (
	watchRetry    = time.Second
	watchRetryMax = 30 * time.Second
)

// watch is grelot watch: the layer 2 watcher on the standing wait. Its
// stdout belongs to the door, so it prints no status line, and its own
// lines go to stderr.
func (r *run) watch(ctx context.Context, args []string) error {
	args, argv, dashes := cutDashes(args)
	fs := flags(r.verb)
	doorName := fs.String("door", "", "")
	byHand := fs.String("transport", "", "")
	rest, err := parse(fs, args)
	switch {
	case err != nil:
		return err
	case len(rest) > 0:
		return usagef(r.verb, "unexpected %q: a command for the exec transport goes after --", rest[0])
	case r.handle != "":
		return usagef(r.verb, "a delegate is pull only: run grelot as %s wait --timeout=<d> instead", r.handle)
	case *byHand != "" && *doorName != "":
		return usagef(r.verb, "--door and --transport do not combine")
	case *byHand == "" && dashes:
		return usagef(r.verb, "a command after -- needs --transport exec")
	}
	if c, ok := r.Stdin.(io.Closer); ok {
		_ = c.Close()
	}
	// The command is fixed when the host starts it, before any grant, so it
	// always names itself and a later grant never turns its wait into a 409.
	r.self, r.statusShown = true, true
	p, err := r.provider()
	if err != nil {
		return err
	}
	door, err := watchDoor(p, *doorName, *byHand, argv)
	if err != nil {
		return err
	}
	api, err := r.client(true)
	if err != nil {
		return err
	}
	sid, err := p.SessionID(r.Lookup)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(r.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	w, err := transport.NewWatch(door, r.Stdout, log)
	if err != nil {
		return err
	}
	ws := &watcher{api: api, door: door, w: w, log: log, cannotAck: func() string { return client.CannotAck(r.ConfigDir, r.Version) }}
	return ws.run(ctx, &provider.Data{Session: sid})
}

// cutDashes splits args at the first --, answering what follows it and
// whether there was one.
func cutDashes(args []string) (before, after []string, found bool) {
	i := slices.Index(args, "--")
	if i < 0 {
		return args, nil, false
	}
	return args[:i], args[i+1:], true
}

// watchDoor is the door a watcher runs: one built by hand for --transport,
// the layer 2 door --door names, or the provider's first layer 2 door.
func watchDoor(p *provider.Provider, name, byHand string, argv []string) (*provider.Door, error) {
	if byHand != "" {
		d, err := provider.ByHand(provider.Transport(byHand), argv)
		if err != nil {
			return nil, usagef("watch", "%v", err)
		}
		return d, nil
	}
	var names []string
	for i := range p.Doors {
		d := &p.Doors[i]
		if d.Layer != 2 {
			continue
		}
		if name == "" || d.Name == name {
			return d, nil
		}
		names = append(names, d.Name)
	}
	if name == "" {
		return nil, usagef("watch", "provider %s has no watcher door: run one by hand with --transport stdout, exit or exec", p.Name)
	}
	if len(names) == 0 {
		return nil, usagef("watch", "provider %s has no watcher door %q and no other: run one by hand with --transport", p.Name, name)
	}
	return nil, usagef("watch", "provider %s has no watcher door %q: its watcher doors are %s", p.Name, name, strings.Join(names, ", "))
}

// watcher is one grelot watch process. cannotAck is the upgrade line once
// the server refuses this client's ack, which would leave each nudge's
// mail unread to wake it again.
type watcher struct {
	api       *client.API
	door      *provider.Door
	w         *transport.Watch
	log       *slog.Logger
	cannotAck func() string
}

// polled is what one poll came to: a nudge, the end reason of a 410, or
// neither when a door's hold lapsed.
type polled struct {
	nudge *client.Nudge
	end   string
}

func (ws *watcher) run(ctx context.Context, data *provider.Data) error {
	for {
		var until time.Time
		if ws.door.Hold > 0 {
			until = time.Now().Add(ws.door.Hold)
		}
		got, err := ws.poll(ctx, until)
		if err != nil {
			return err
		}
		var step transport.Step
		switch {
		case got.nudge != nil:
			data.Line, data.Seq = transport.SignalLine(got.nudge.Seq, got.nudge.Count), got.nudge.Seq
			step, err = ws.w.Nudge(ctx, data)
		case got.end != "":
			data.Line, data.Seq = transport.EndLine(got.end), 0
			step, err = ws.w.End(ctx, data)
		default:
			step, err = ws.w.Idle(ctx, data)
		}
		if err != nil {
			return err
		}
		if step.Exit {
			return &exitError{code: step.Code}
		}
	}
}

// endUpgrade is the end reason of a watcher whose client cannot ack.
const endUpgrade = "upgrade"

func (ws *watcher) stopForUpgrade() bool {
	line := ws.cannotAck()
	if line != "" {
		ws.log.Warn("this client cannot mark mail read on this server; this watcher stops", "fix", line)
	}
	return line != ""
}

// poll takes standing waits until one answers a nudge or a 410, or until
// passes (never when zero). A server that cannot be reached or answers 5xx
// is tried again with backoff, with one WARN line per outage. Once the
// server refuses this client's ack, poll answers the upgrade end before
// its next wait: the nudged mail stays unread, so every restarted watcher
// would wake for it again.
func (ws *watcher) poll(ctx context.Context, until time.Time) (polled, error) {
	backoff, failing := watchRetry, false
	for {
		if ws.stopForUpgrade() {
			return polled{end: endUpgrade}, nil
		}
		hold := holdUntil(until)
		if hold <= 0 {
			return polled{}, nil
		}
		got, done, err := ws.once(ctx, hold)
		switch {
		case done:
			return got, nil
		case err == nil:
			backoff, failing = watchRetry, false
			continue
		case ctx.Err() != nil:
			return polled{}, ctx.Err()
		case !transient(err):
			return polled{}, err
		}
		if !failing {
			failing = true
			ws.log.Warn("the server did not answer the wait; trying again", "error", err.Error())
		}
		pause := backoff
		if !until.IsZero() {
			pause = min(pause, max(time.Until(until), 0))
		}
		if err := sleep(ctx, pause); err != nil {
			return polled{}, err
		}
		backoff = min(backoff*2, watchRetryMax)
	}
}

// holdUntil is the next standing wait's hold: maxHold, cut to the time
// left before until when until is set.
func holdUntil(until time.Time) time.Duration {
	if until.IsZero() {
		return maxHold
	}
	return min(maxHold, time.Until(until).Round(time.Millisecond))
}

// once sends one standing wait of hold. done is a nudge or a 410; neither,
// with no error, is a wait that passed with none.
func (ws *watcher) once(ctx context.Context, hold time.Duration) (got polled, done bool, err error) {
	req := &client.Request{
		Method: http.MethodPost, Path: "/v1/events/wait",
		Body: map[string]string{waitKey: hold.String()}, Timeout: hold + waitMargin, Once: true,
	}
	resp, err := ws.api.Do(ctx, req, nil)
	if ae, ok := errors.AsType[*client.APIError](err); ok && ae.HTTP == http.StatusGone {
		return polled{end: ae.Code}, true, nil
	}
	if err != nil || resp.Code != http.StatusOK {
		return polled{}, false, err
	}
	var n client.Nudge
	if json.Unmarshal(resp.Body, &n) != nil {
		return polled{}, false, errors.New("grelot watch: the server's nudge is not the JSON this client reads; check that the client and server versions match with grelot me")
	}
	return polled{nudge: &n}, true, nil
}

// transient reports whether err may pass if the request is sent again: no
// answer, a 429 or a 5xx.
func transient(err error) bool {
	if _, ok := errors.AsType[*client.UnreachableError](err); ok {
		return true
	}
	ae, ok := errors.AsType[*client.APIError](err)
	return ok && (ae.HTTP == http.StatusTooManyRequests || ae.HTTP >= 500)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// wait is grelot wait --timeout: the in-turn wait, one request per hold of
// at most maxHold. The first page with mail is printed and read; at the
// timeout only the status line is.
func (r *run) wait(ctx context.Context, args []string) error {
	fs := flags(r.verb)
	timeout := fs.Duration("timeout", 0, "")
	rest, err := parse(fs, args)
	switch {
	case err != nil:
		return err
	case len(rest) > 0:
		return usagef(r.verb, "unexpected %q: grelot wait takes only --timeout", rest[0])
	case *timeout <= 0:
		return usagef(r.verb, "--timeout needs a duration above 0, such as 10m")
	}
	// A session waits on a reply right after granting, when the delegate gate
	// would refuse an unnamed request; as <handle> wait keeps the handle.
	r.self = r.handle == ""
	a, err := r.client(true)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(*timeout)
	for {
		hold := max(min(maxHold, time.Until(deadline)), 0).Round(time.Millisecond)
		req := &client.Request{
			Method: http.MethodGet, Path: "/v1/inbox",
			Query: url.Values{waitKey: {hold.String()}}, Timeout: hold + waitMargin,
		}
		var page client.Inbox
		resp, err := a.Do(ctx, r.bounded(req), &page)
		if err != nil {
			return err
		}
		if len(page.Entries) > 0 {
			r.printStatus(resp.Status)
			return r.showPage(ctx, &page)
		}
		if !time.Now().Before(deadline) {
			r.printStatus(resp.Status)
			return nil
		}
	}
}
