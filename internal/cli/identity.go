package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/hook"
	"github.com/cplieger/grelot/internal/proctree"
	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/internal/where"
	"github.com/cplieger/grelot/internal/wire"
)

func (r *run) register(ctx context.Context, args []string) error {
	fs := flags(r.verb)
	var paths list
	fs.Var(&paths, "paths", "")
	desc := fs.String("desc", "", "")
	ttl := fs.String("ttl", "", "")
	link := fs.String("delegate", "", "")
	takeOver := fs.Bool("take-over", false, "")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	ttlMS, err := r.ttlFlag(*ttl)
	if err != nil {
		return err
	}
	if *link != "" {
		if len(rest) > 0 || len(paths) > 0 || *ttl != "" || *takeOver {
			return usagef(r.verb, "a delegate registers with --delegate <link> --desc <your task> alone: its name is its handle, its scope replaces paths, and its lease comes from the grant")
		}
		var got wire.Registered
		body := wire.RegisterRequest{Delegate: *link, Description: *desc}
		if cerr := r.call(ctx, &client.Request{Method: http.MethodPost, Path: wire.Path("register", nil), Body: body}, &got); cerr != nil {
			return cerr
		}
		r.registered(&got, true)
		return nil
	}
	if len(rest) != 1 {
		return usagef(r.verb, "name the identity once: grelot register <name> --paths <path> --desc <what and why> --ttl=<d>")
	}
	p, err := r.provider()
	if err != nil {
		return err
	}
	r.freshRoot(ctx)
	canonical, locs, err := r.canonical(ctx, paths)
	if err != nil {
		return err
	}
	cfg, _ := r.config()
	place := r.placeOf(ctx, locs)
	body := wire.RegisterRequest{
		Where: &place, Name: rest[0], Description: *desc,
		Host: cfg.Host, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Proc:  proctree.Proc(r.tree(), p.Session.Process, p.Session.ProcessShared),
		TTLMS: ttlMS, Paths: canonical, TakeOver: *takeOver,
	}
	var got wire.Registered
	if err := r.call(ctx, &client.Request{Method: http.MethodPost, Path: wire.Path("register", nil), Body: body}, &got); err != nil {
		return err
	}
	if !got.Existing {
		r.descSent()
	}
	if note := client.ProviderNote(r.Providers, r.providerFlag, cfg, client.Shell{Lookup: r.Lookup, Ancestors: proctree.Names(r.tree())}); note != "" {
		fmt.Fprintln(r.Stdout, note)
	}
	r.registered(&got, false)
	return nil
}

// freshRoot asks the server for its path root before paths are named, when
// no answer has given it yet; a failure leaves the cached default.
func (r *run) freshRoot(ctx context.Context) {
	if client.LoadFacts(r.ConfigDir).Version == "" {
		_, _ = r.peekMe(ctx)
	}
	r.resolver = nil
}

// registered prints a register's or revive's answer: the lines the server
// owes, the identity card, then the manual for a delegate and a pointer to
// it for anyone else, with the ask to follow or start a thread outside a
// one-shot run. Every path to a top-level register or revive sends the
// agent to grelot help first; nothing does for a subagent's link.
func (r *run) registered(got *wire.Registered, delegate bool) {
	if got.NoEnvironment != "" {
		file := where.EnvironmentFile(r.ConfigDir)
		fmt.Fprintln(r.Stdout, "grelot: "+strings.Replace(got.NoEnvironment,
			"no environment id:", "no environment id ("+file+" could not be read or created):", 1))
	}
	if got.Existing {
		fmt.Fprintf(r.Stdout, "grelot: this session already has the identity @%s, returned unchanged with its lease; run grelot renew --ttl=<d> to extend it\n",
			got.Identity.Name)
	}
	if got.Unread > 0 {
		fmt.Fprintf(r.Stdout, "grelot: %d unread: run grelot inbox\n", got.Unread)
	}
	writeCard(r.Stdout, &got.Identity)
	fmt.Fprintln(r.Stdout)
	if delegate {
		fmt.Fprint(r.Stdout, manual)
		return
	}
	fmt.Fprintln(r.Stdout, "grelot: grelot help is the manual, if you have not read it")
	if followsNoThread(&got.Identity) && !hook.OneShot(r.prov, r.Lookup, r.tree) {
		fmt.Fprintln(r.Stdout, lineFollowAThread)
		if s, err := r.session(); err == nil {
			hook.NewMarks(r.ConfigDir, r.now).Set(markThreadAsked, s)
		}
	}
}

// lineFollowAThread ends a top-level register or revive while
// followsNoThread holds.
const lineFollowAThread = "grelot: you follow no thread yet: run `grelot follow <path>` for one your work depends on, such as a library you use; if none fits, start your work path's thread with a post saying what you work on there"

// followsNoThread reports an identity that has never followed a path nor
// posted in a path thread. Follows decide alone against a server that
// sends no Threaded.
func followsNoThread(id *wire.Identity) bool {
	return !id.Threaded && len(id.Follows) == 0
}

func (r *run) revive(ctx context.Context, args []string) error {
	fs := flags(r.verb)
	ttl := fs.String("ttl", "", "")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usagef(r.verb, "name at most one identity: grelot revive [<name>] --ttl=<d>")
	}
	ttlMS, err := r.ttlFlag(*ttl)
	if err != nil {
		return err
	}
	p, err := r.provider()
	if err != nil {
		return err
	}
	cfg, _ := r.config()
	body := wire.ReviveRequest{
		Host: cfg.Host, OS: runtime.GOOS, Arch: runtime.GOARCH, TTLMS: ttlMS,
		Proc: proctree.Proc(r.tree(), p.Session.Process, p.Session.ProcessShared),
	}
	var paths []string
	if len(rest) == 1 {
		body.Name = rest[0]
		paths = r.dormantPaths(ctx, rest[0])
	} else if me, err := r.peekMe(ctx); err == nil && me.Identity != nil {
		paths = me.Identity.Paths
	}
	place := r.placeHere(ctx, paths)
	body.Where = &place
	var got wire.Registered
	if err := r.call(ctx, &client.Request{Method: http.MethodPost, Path: wire.Path("revive", nil), Body: body}, &got); err != nil {
		return err
	}
	r.registered(&got, false)
	return nil
}

// dormantPaths are the work paths of the dormant identity name, nil when
// none is listed; the server answers for a name it cannot revive.
func (r *run) dormantPaths(ctx context.Context, name string) []string {
	peers, err := walkList[wire.Peer](ctx, r.peek, "listIdentities", url.Values{"all": {"1"}, "q": {name}})
	if err != nil {
		return nil
	}
	for i := range peers {
		if p := &peers[i]; p.Name == name && p.State == wire.StateDormant {
			return p.Paths
		}
	}
	return nil
}

func (r *run) me(ctx context.Context, args []string) error {
	if _, err := parse(flags(r.verb), args); err != nil {
		return err
	}
	a, err := r.client(true)
	if err != nil {
		_, noHost := errors.AsType[*provider.DetectError](err)
		ns, noSession := errors.AsType[*provider.NoSessionError](err)
		if noHost || noSession && ns.Env != "" {
			return r.meWithoutSession(ctx, err)
		}
		return err
	}
	me, resp, err := a.Me(ctx, url.Values{}, http.Header{})
	if err != nil {
		return err
	}
	r.printStatus(resp.Status)
	switch {
	case me.State == wire.StateNone:
		fmt.Fprintln(r.Stdout, "grelot: grelot help says how to register, with the command the line above names")
		return nil
	case me.Identity == nil:
		return nil
	}
	writeCard(r.Stdout, me.Identity)
	writeMe(r.Stdout, me, where.EnvironmentFile(r.ConfigDir))
	return nil
}

// meWithoutSession is grelot me in a shell that holds no host session,
// such as a terminal no agent runs in: it checks the server and the token
// with a read any caller may make, then prints why there is no identity
// to show.
func (r *run) meWithoutSession(ctx context.Context, why error) error {
	a, err := r.client(false)
	if err != nil {
		return err
	}
	resp, err := a.Version(ctx, r.bounded(&client.Request{}))
	if err != nil {
		return err
	}
	r.printStatus(resp.Status)
	fmt.Fprintf(r.Stdout, "grelot: the server at %s answers and accepts this key\n", safe(r.cfg.Server))
	fmt.Fprintln(r.Stdout, noSessionLine(why))
	fmt.Fprintln(r.Stdout, "grelot: run grelot me in your agent's shell for its status line and the register command")
	return nil
}

// noSessionLine says why me has no session to show. A shell where no host
// matched and no provider file failed is a plain terminal, where the
// --provider advice of the detection error is wrong, so it gets its own line.
func noSessionLine(why error) string {
	if d, ok := errors.AsType[*provider.DetectError](why); ok && len(d.Matched) == 0 && len(d.Invalid) == 0 {
		return "grelot: this shell is not an agent's session: no host's session variable is set here"
	}
	line := safe(why.Error())
	if !strings.HasPrefix(line, "grelot") {
		line = "grelot: " + line
	}
	return line
}

func (r *run) set(ctx context.Context, args []string) error {
	fs := flags(r.verb)
	var paths list
	fs.Var(&paths, "paths", "")
	name := fs.String("name", "", "")
	desc := fs.String("desc", "", "")
	ttl := fs.String("ttl", "", "")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usagef(r.verb, "unexpected %q: grelot set takes only --name, --paths, --desc and --ttl", rest[0])
	}
	ttlMS, err := r.ttlFlag(*ttl)
	if err != nil {
		return err
	}
	body := wire.PatchMeRequest{Name: *name, Description: *desc, TTLMS: ttlMS}
	if len(paths) > 0 {
		r.freshRoot(ctx)
		canonical, locs, err := r.canonical(ctx, paths)
		if err != nil {
			return err
		}
		body.Paths = canonical
		if len(locs) > 0 {
			w := r.placeOf(ctx, locs)
			body.Where = &w
		}
	}
	if body.Where == nil && *ttl != "" {
		w := r.placeHere(ctx, r.currentPaths(ctx))
		body.Where = &w
	}
	return r.patchMe(ctx, &body, func(id *wire.Identity) {
		if *desc != "" {
			r.descSent()
		}
		writeCard(r.Stdout, id)
	})
}

func (r *run) patchMe(ctx context.Context, body *wire.PatchMeRequest, show func(*wire.Identity)) error {
	var got wire.IdentityAnswer
	if err := r.call(ctx, &client.Request{Method: http.MethodPatch, Path: wire.Path("patchMe", nil), Body: body}, &got); err != nil {
		return err
	}
	if got.Identity.Notice != "" {
		fmt.Fprintln(r.Stdout, "grelot: "+got.Identity.Notice)
	}
	show(&got.Identity)
	return nil
}

// currentPaths are the identity's work paths, nil when GET /v1/me names
// none.
func (r *run) currentPaths(ctx context.Context) []string {
	me, err := r.peekMe(ctx)
	if err != nil || me.Identity == nil {
		return nil
	}
	return me.Identity.Paths
}

func (r *run) renew(ctx context.Context, args []string) error {
	fs := flags(r.verb)
	ttl := fs.String("ttl", "", "")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usagef(r.verb, "name at most one reservation: grelot renew [<reservation id>] --ttl=<d>")
	}
	ttlMS, err := r.ttlFlag(*ttl)
	if err != nil {
		return err
	}
	if len(rest) == 1 {
		return r.renewReservation(ctx, rest[0], ttlMS)
	}
	w := r.placeHere(ctx, r.currentPaths(ctx))
	return r.patchMe(ctx, &wire.PatchMeRequest{TTLMS: ttlMS, Where: &w}, func(id *wire.Identity) {
		fmt.Fprintf(r.Stdout, "grelot: @%s renewed for %s, until %s\n", id.Name, durationText(id.TTLMS), stamp(id.Expires))
	})
}

func (r *run) renewReservation(ctx context.Context, id string, ttlMS int64) error {
	if !isID(id) {
		return usagef(r.verb, "%q is not a reservation id: grelot reservations lists them", id)
	}
	var got wire.ReservationAnswer
	req := &client.Request{Method: http.MethodPatch, Path: wire.Path("renewReservation", map[string]string{"id": id}), Body: wire.RenewRequest{TTLMS: ttlMS}}
	if err := r.call(ctx, req, &got); err != nil {
		return err
	}
	v := &got.Reservation
	fmt.Fprintf(r.Stdout, "grelot: reservation %d on %s renewed for %s, until %s\n", v.ID, safe(v.Path), durationText(v.TTLMS), stamp(v.Expires))
	return nil
}

func isID(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (r *run) bye(ctx context.Context, args []string) error {
	rest, err := parse(flags(r.verb), args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usagef(r.verb, "grelot bye takes no arguments")
	}
	// The answer's status line is read after the retirement, so it could
	// only ask the agent that just left to register: this line replaces it.
	if err := r.peek(ctx, &client.Request{Method: http.MethodDelete, Path: wire.Path("deleteMe", nil)}, nil); err != nil {
		return err
	}
	fmt.Fprintln(r.Stdout, "grelot: identity retired: its reservations went to their queues and its delegates ended")
	return nil
}
