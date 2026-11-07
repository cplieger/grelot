package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/cplieger/grelot/internal/client"
)

const (
	// pathParam is the query parameter that names a path in grelot form.
	pathParam = "path"
	// fieldsParam is the query parameter that picks the answer's fields.
	fieldsParam = "fields"
	// identitiesPath lists the server's identities.
	identitiesPath = "/v1/identities"
)

func (r *run) who(ctx context.Context, args []string) error {
	fs := flags(r.verb)
	asJSON := fs.Bool("json", false, "")
	text := map[string]*string{"repo": fs.String("repo", "", ""), fieldsParam: fs.String(fieldsParam, "", "")}
	local := map[string]*string{pathParam: fs.String("path", "", ""), "holds": fs.String("holds", "", ""), "queued": fs.String("queued", "", "")}
	set := map[string]*bool{"online": fs.Bool("online", false, ""), "here": fs.Bool("here", false, ""), "all": fs.Bool("all", false, "")}
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	q := url.Values{}
	if len(rest) > 0 {
		q.Set("q", strings.Join(rest, " "))
	}
	for name, v := range text {
		if *v != "" {
			q.Set(name, *v)
		}
	}
	for name, v := range set {
		if *v {
			q.Set(name, "1")
		}
	}
	if qerr := r.canonicalQuery(ctx, q, local); qerr != nil {
		return qerr
	}
	a, err := r.client(false)
	if err != nil {
		return err
	}
	resp, err := a.Do(ctx, r.bounded(&client.Request{Method: http.MethodGet, Path: identitiesPath, Query: q}), nil)
	if err != nil {
		return err
	}
	if *asJSON {
		_, err := r.Stdout.Write(resp.Body)
		return err
	}
	r.printStatus(resp.Status)
	return r.writePeers(resp.Body)
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

func (r *run) writePeers(body []byte) error {
	var got client.Peers
	if err := json.Unmarshal(body, &got); err != nil {
		return errors.New("the server's answer to GET /v1/identities is not the JSON this client reads; check that the client and server versions match with grelot me")
	}
	if len(got.Identities) == 0 {
		fmt.Fprintln(r.Stdout, "grelot: no identity matches")
		return nil
	}
	for i := range got.Identities {
		writePeer(r.Stdout, &got.Identities[i])
	}
	return nil
}

// writePeer prints one identity's block, with the data label on its
// header line.
func writePeer(w io.Writer, p *client.Peer) {
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
	case p.State == client.StateDormant:
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
