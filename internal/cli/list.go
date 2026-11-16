package cli

import (
	"cmp"
	"context"
	"flag"
	"maps"
	"net/http"
	"net/url"
	"strconv"

	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/wire"
)

// printPage is the query of one page a command prints: limit entries,
// printLimit at 0, from cursor when it is set.
func printPage(verb string, limit int, cursor string) (url.Values, error) {
	if limit < 0 || limit > wire.PageLimitMax {
		return nil, usagef(verb, "--limit must be 1 to %d", wire.PageLimitMax)
	}
	q := url.Values{wire.QueryLimit: {strconv.Itoa(cmp.Or(limit, printLimit))}}
	if cursor != "" {
		q.Set(wire.QueryCursor, cursor)
	}
	return q, nil
}

// setFlags sets q's parameter of each flag of set that is on, as 1.
func setFlags(q url.Values, set map[string]*bool) {
	for name, v := range set {
		if *v {
			q.Set(name, "1")
		}
	}
}

// boolFlags defines one bool flag per name on fs.
func boolFlags(fs *flag.FlagSet, names ...string) map[string]*bool {
	set := make(map[string]*bool, len(names))
	for _, name := range names {
		set[name] = fs.Bool(name, false, "")
	}
	return set
}

// sender sends one request and decodes its answer into out: run.call,
// run.peek or a command's own.
type sender func(ctx context.Context, req *client.Request, out any) error

// pageLimit is the page size walkList asks for.
var pageLimit = wire.PageLimitMax

// walkList reads every page of the list row named row with query q,
// pageLimit at a time, through send, and returns their items in the row's
// order.
func walkList[T any](ctx context.Context, send sender, row string, q url.Values) ([]T, error) {
	q = maps.Clone(q)
	if q == nil {
		q = url.Values{}
	}
	q.Set(wire.QueryLimit, strconv.Itoa(pageLimit))
	var all []T
	for {
		var page wire.List[T]
		if err := send(ctx, &client.Request{Method: http.MethodGet, Path: wire.Path(row, nil), Query: q}, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextCursor == "" {
			return all, nil
		}
		q.Set(wire.QueryCursor, page.NextCursor)
	}
}

// walkPeers is every identity GET /v1/identities lists for q.
func (r *run) walkPeers(ctx context.Context, q url.Values, send sender) ([]wire.Peer, error) {
	return walkList[wire.Peer](ctx, send, "listIdentities", q)
}
