package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/wire"
)

func (r *run) reserve(ctx context.Context, args []string) error {
	fs := flags(r.verb)
	ttl := fs.String("ttl", "", "")
	reason := fs.String("reason", "", "")
	queue := fs.Bool("queue", false, "")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef(r.verb, "name one path: grelot reserve <path> --ttl=<d> --reason <why> [--queue]")
	}
	ttlMS, err := r.ttlFlag(*ttl)
	if err != nil {
		return err
	}
	canonical, _, err := r.canonical(ctx, rest)
	if err != nil {
		return err
	}
	var got wire.Reserved
	body := wire.ReserveRequest{Path: canonical[0], Reason: *reason, TTLMS: ttlMS, Queue: *queue}
	if err := r.call(ctx, &client.Request{Method: http.MethodPost, Path: wire.Path("reserve", nil), Body: body}, &got); err != nil {
		return unknownOutcome(err)
	}
	v := &got.Reservation
	if v.State == string(wire.ReservationQueued) {
		behind := ""
		if b := got.Behind; b != nil {
			behind = fmt.Sprintf(" behind @%s%s until %s", safe(b.Holder), origin(b.Host, b.Relation), stamp(b.Expires))
		}
		fmt.Fprintf(r.Stdout, "grelot: reservation %d queued for %s%s; the grant reaches you as mail; cancel with grelot release %d%s\n",
			v.ID, safe(v.Path), behind, v.ID, dataLabel)
		return nil
	}
	fmt.Fprintf(r.Stdout, "grelot: reservation %d held on %s until %s; renew with grelot renew %d --ttl=<d>, release with grelot release %d\n",
		v.ID, safe(v.Path), stamp(v.Expires), v.ID, v.ID)
	return nil
}

func (r *run) release(ctx context.Context, args []string) error {
	rest, err := parse(flags(r.verb), args)
	if err != nil {
		return err
	}
	if len(rest) != 1 || !isID(rest[0]) {
		return usagef(r.verb, "name one reservation id: grelot release <reservation id>, from grelot reservations")
	}
	var got wire.ReservationAnswer
	if err := r.call(ctx, &client.Request{Method: http.MethodDelete, Path: wire.Path("releaseReservation", map[string]string{"id": rest[0]})}, &got); err != nil {
		return err
	}
	v := &got.Reservation
	done := "released"
	if v.State == string(wire.ReservationQueued) {
		done = "cancelled"
	}
	fmt.Fprintf(r.Stdout, "grelot: reservation %d on %s %s\n", v.ID, safe(v.Path), done)
	return nil
}

func (r *run) reservations(ctx context.Context, args []string) error {
	rest, err := parse(flags(r.verb), args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usagef(r.verb, "name at most one path: grelot reservations [<path>]")
	}
	q := url.Values{}
	if len(rest) == 1 {
		canonical, _, cerr := r.canonical(ctx, rest)
		if cerr != nil {
			return cerr
		}
		q.Set(pathParam, canonical[0])
	}
	resv, err := walkList[wire.Reservation](ctx, r.call, "listReservations", q)
	if err != nil {
		return err
	}
	if len(resv) == 0 {
		fmt.Fprintln(r.Stdout, "grelot: no reservation")
		return nil
	}
	for i := range resv {
		v := &resv[i]
		until := "until " + stamp(v.Expires)
		if v.Expires.IsZero() {
			until = "for " + durationText(v.TTLMS) + " once granted"
		}
		fmt.Fprintf(r.Stdout, "reservation %d · %s · %s · @%s%s · %s · reason: %s%s\n",
			v.ID, safe(v.State), safe(v.Path), safe(v.Holder), origin(v.Host, v.Relation), until, safe(v.Reason), labelFor(v.Relation))
	}
	return nil
}

func (r *run) delegate(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "revoke" {
		return r.revoke(ctx, args[1:])
	}
	fs := flags(r.verb)
	var allowThreads, allowAgents, denyThreads, denyAgents list
	fs.Var(&allowThreads, "allow-thread", "")
	fs.Var(&allowAgents, "allow-agent", "")
	fs.Var(&denyThreads, "deny-thread", "")
	fs.Var(&denyAgents, "deny-agent", "")
	ttl := fs.String("ttl", "", "")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef(r.verb, "name one label: grelot delegate <label> --ttl=<d> [--allow-thread <path>|announce] [--allow-agent <name>]")
	}
	ttlMS, err := r.ttlFlag(*ttl)
	if err != nil {
		return err
	}
	body := wire.GrantRequest{Label: rest[0], TTLMS: ttlMS, AllowAgents: names(allowAgents), DenyAgents: names(denyAgents)}
	if body.AllowThreads, err = r.threadScope(ctx, allowThreads); err != nil {
		return err
	}
	if body.DenyThreads, err = r.threadScope(ctx, denyThreads); err != nil {
		return err
	}
	var got wire.Granted
	if err := r.call(ctx, &client.Request{Method: http.MethodPost, Path: wire.Path("grantDelegate", nil), Body: body}, &got); err != nil {
		return err
	}
	h := safe(got.Handle)
	fmt.Fprintf(r.Stdout, "grelot: link for @%s, unused until %s:\n  %s\n", h, stamp(got.Expires), safe(got.Link))
	fmt.Fprintln(r.Stdout, "Put this line in the subagent's prompt:")
	fmt.Fprintf(r.Stdout, "  Run `grelot help`, then register with `grelot register --delegate %s --desc <your task>`, prefix every grelot command with `grelot as %s`, never `--self`, and finish with `grelot as %s bye`.\n",
		safe(got.Link), h, h)
	fmt.Fprintln(r.Stdout, "grelot: until the link is used and its delegate has ended, add --self to your own commands, such as grelot --self inbox")
	return nil
}

// threadScope is each scope thread in grelot form, announce kept.
func (r *run) threadScope(ctx context.Context, threads list) ([]string, error) {
	out := make([]string, 0, len(threads))
	for _, t := range threads {
		if t == announce {
			out = append(out, t)
			continue
		}
		canonical, _, err := r.canonical(ctx, []string{t})
		if err != nil {
			return nil, err
		}
		out = append(out, canonical[0])
	}
	return out, nil
}

// names is each name without its @.
func names(l list) []string {
	out := make([]string, len(l))
	for i, n := range l {
		out[i] = strings.TrimPrefix(n, "@")
	}
	return out
}

func (r *run) revoke(ctx context.Context, args []string) error {
	rest, err := parse(flags(r.verb), args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef(r.verb, "name one label: grelot delegate revoke <label>")
	}
	var got wire.Revoked
	if err := r.call(ctx, &client.Request{Method: http.MethodDelete, Path: wire.Path("revokeDelegate", map[string]string{"label": rest[0]})}, &got); err != nil {
		return err
	}
	if got.Link {
		fmt.Fprintf(r.Stdout, "grelot: the unused link %s is revoked\n", safe(rest[0]))
	}
	for _, h := range got.Ended {
		fmt.Fprintf(r.Stdout, "grelot: @%s ended\n", safe(h))
	}
	return nil
}

// as runs a verb as one of this session's delegates.
func (r *run) as(ctx context.Context, args []string) error {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		return errHelp
	}
	if len(args) < 2 {
		return usagef(r.verb, "name the handle and the command: grelot as <handle> <command> ...")
	}
	if r.self {
		return usagef(r.verb, "--self acts as this session's own identity and as acts as a delegate: use one")
	}
	verb := args[1]
	fn, ok := verbs[verb]
	if !ok || verb == "as" {
		return usagef(r.verb, "unknown command %q after the handle: grelot help lists the commands", verb)
	}
	r.handle, r.verb = args[0], verb
	return fn(r, ctx, args[2:])
}
