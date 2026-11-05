package cli

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/cplieger/grelot/internal/canon"
	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/where"
)

// noChange is the field value that keeps the identity's current one.
const noChange = "no change"

// canonical is each local path in grelot's form, with where each lies;
// "no change" passes as it is.
func (r *run) canonical(ctx context.Context, locals []string) ([]string, []canon.Location, error) {
	res, err := r.paths()
	if err != nil {
		return nil, nil, err
	}
	out := make([]string, 0, len(locals))
	var locs []canon.Location
	for _, p := range locals {
		if p == noChange {
			out = append(out, p)
			continue
		}
		loc, err := res.Path(ctx, p)
		if err != nil {
			return nil, nil, fmt.Errorf("grelot cannot name the path %s: %w", p, err)
		}
		out = append(out, loc.Canonical)
		locs = append(locs, loc)
	}
	return out, locs, nil
}

// placeOf is this session's where with one checkout per repo the
// locations lie in, the first location's working tree winning.
func (r *run) placeOf(ctx context.Context, locs []canon.Location) client.Where {
	w := r.machineAndEnvironment(ctx)
	w.Checkouts = []client.Checkout{}
	for _, loc := range locs {
		c := r.checkoutOf(ctx, loc)
		if c == nil || slices.ContainsFunc(w.Checkouts, func(o client.Checkout) bool { return o.Repo == c.Repo }) {
			continue
		}
		w.Checkouts = append(w.Checkouts, *c)
	}
	return w
}

// checkoutOf is loc's checkout entry, nil outside a clone with a remote or
// where the checkout key can neither be read nor created.
func (r *run) checkoutOf(ctx context.Context, loc canon.Location) *client.Checkout {
	if loc.Clone == nil || loc.Clone.Repo == "" {
		return nil
	}
	id := where.Checkout(ctx, loc.Clone.GitDir, loc.Clone.Toplevel)
	if id == "" {
		return nil
	}
	return &client.Checkout{
		Repo: path.Join(r.root, loc.Clone.Repo), ID: id, Remote: where.Remote(loc.Clone.Remote),
	}
}

// placeHere is this session's where for a command that names no paths: the
// working directory's checkout, when one of paths lies in its repo, since
// the server keeps the entries of every other repo.
func (r *run) placeHere(ctx context.Context, paths []string) client.Where {
	w := r.machineAndEnvironment(ctx)
	w.Checkouts = []client.Checkout{}
	res, err := r.paths()
	if err != nil {
		return w
	}
	wd, err := r.Getwd()
	if err != nil {
		return w
	}
	loc, err := res.Path(ctx, wd)
	if err != nil {
		return w
	}
	c := r.checkoutOf(ctx, loc)
	if c != nil && slices.ContainsFunc(paths, func(p string) bool { return p == c.Repo || strings.HasPrefix(p, c.Repo+"/") }) {
		w.Checkouts = append(w.Checkouts, *c)
	}
	return w
}
