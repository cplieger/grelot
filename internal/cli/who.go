package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/wire"
)

const (
	// pathParam is the query parameter that names a path in grelot form.
	pathParam = "path"
	// fieldsFlag picks the fields --json prints of each identity.
	fieldsFlag = "fields"
)

func (r *run) who(ctx context.Context, args []string) error {
	fs := flags(r.verb)
	asJSON := fs.Bool("json", false, "")
	q, picked, err := r.whoQuery(ctx, fs, args)
	if err != nil {
		return err
	}
	a, err := r.client(false)
	if err != nil {
		return err
	}
	send := func(ctx context.Context, req *client.Request, out any) error {
		resp, derr := a.Do(ctx, r.bounded(req), out)
		if derr == nil && !*asJSON {
			r.printStatus(resp.Status)
		}
		return derr
	}
	peers, err := walkList[wire.Peer](ctx, send, "listIdentities", q)
	switch {
	case err != nil:
		return err
	case *asJSON:
		return writePeersJSON(r.Stdout, peers, picked)
	case len(peers) == 0:
		fmt.Fprintln(r.Stdout, "grelot: no identity matches")
		return nil
	}
	for i := range peers {
		writePeer(r.Stdout, &peers[i])
	}
	return nil
}

// whoQuery parses who's other flags on fs into GET /v1/identities' query
// and the fields --json keeps.
func (r *run) whoQuery(ctx context.Context, fs *flag.FlagSet, args []string) (url.Values, []string, error) {
	repo := fs.String("repo", "", "")
	fields := fs.String(fieldsFlag, "", "")
	local := map[string]*string{pathParam: fs.String("path", "", ""), "holds": fs.String("holds", "", ""), "queued": fs.String("queued", "", "")}
	set := boolFlags(fs, "online", "here", "all")
	rest, err := parse(fs, args)
	if err != nil {
		return nil, nil, err
	}
	picked, err := whoFieldSet(r.verb, *fields)
	if err != nil {
		return nil, nil, err
	}
	q := url.Values{}
	if len(rest) > 0 {
		q.Set("q", strings.Join(rest, " "))
	}
	if *repo != "" {
		q.Set("repo", *repo)
	}
	setFlags(q, set)
	return q, picked, r.canonicalQuery(ctx, q, local)
}

// whoFields are the names --fields takes, in wire.Peer's JSON names.
var whoFields = []string{
	"arch", "delegates", "description", "expires_at", "follows", "host", "last_seen",
	"name", "online", "os", "paths", "queued", "relation", "reservations", "state",
}

// whoFieldSet parses --fields, which always keeps name; nil means every
// field.
func whoFieldSet(verb, v string) ([]string, error) {
	if v == "" {
		return nil, nil
	}
	out := []string{"name"}
	for f := range strings.SplitSeq(v, ",") {
		if f = strings.TrimSpace(f); !slices.Contains(whoFields, f) {
			return nil, usagef(verb, "unknown field %q in --fields: valid fields are %s", f, strings.Join(whoFields, ", "))
		}
		out = append(out, f)
	}
	return out, nil
}

// writePeersJSON prints peers as one GET /v1/identities answer holding
// every page, each identity cut to fields when they are named.
func writePeersJSON(w io.Writer, peers []wire.Peer, fields []string) error {
	items := make([]any, 0, len(peers))
	for i := range peers {
		if fields == nil {
			items = append(items, peers[i])
			continue
		}
		picked, err := pick(&peers[i], fields)
		if err != nil {
			return err
		}
		items = append(items, picked)
	}
	b, err := json.Marshal(map[string]any{"items": items, "total": len(peers)})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// pick is p with only the named fields.
func pick(p *wire.Peer, fields []string) (map[string]json.RawMessage, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(fields))
	for _, f := range fields {
		if v, ok := all[f]; ok {
			out[f] = v
		}
	}
	return out, nil
}

// canonicalQuery sets each local path filter in its grelot form, so a
// clone at any local root finds the same agents.
func (r *run) canonicalQuery(ctx context.Context, q url.Values, local map[string]*string) error {
	for name, v := range local {
		if *v == "" {
			continue
		}
		canonical, _, err := r.canonical(ctx, []string{*v})
		if err != nil {
			return err
		}
		q.Set(name, canonical[0])
	}
	return nil
}

// writePeer prints one identity's block, with the data label on its
// header line.
func writePeer(w io.Writer, p *wire.Peer) {
	head := []string{"@" + safe(p.Name)}
	if p.Host != "" {
		head = append(head, safe(p.Host))
	}
	if p.OS != "" {
		head = append(head, safe(p.OS)+"/"+safe(p.Arch))
	}
	if p.Relation != "" {
		head = append(head, safe(p.Relation))
	}
	switch {
	case p.State == wire.StateDormant:
		head = append(head, "dormant, purged at "+stamp(p.Expires))
	case p.Online:
		head = append(head, "online")
	case !p.LastSeen.IsZero():
		head = append(head, "last seen "+stamp(p.LastSeen))
	}
	fmt.Fprintln(w, strings.Join(head, " · ")+labelFor(p.Relation))
	line := func(label, text string) {
		if text != "" {
			fmt.Fprintf(w, "  %s: %s\n", label, text)
		}
	}
	line("description", safe(p.Description))
	line("paths", joined(p.Paths))
	line("follows", joined(p.Follows))
	for _, v := range p.Reservations {
		line("holds", fmt.Sprintf("%s until %s, %d queued", safe(v.Path), stamp(v.Expires), v.Queue))
	}
	for _, v := range p.Queued {
		line("queued", fmt.Sprintf("%s, place %d", safe(v.Path), v.Place))
	}
	for _, d := range p.Delegates {
		seen := "online"
		if !d.Online {
			seen = "last seen " + stamp(d.LastSeen)
		}
		fmt.Fprintf(w, "    @%s · %s · %s\n", safe(d.Name), seen, safe(d.Description))
	}
}
