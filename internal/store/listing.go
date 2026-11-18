//go:build !windows

package store

import (
	"cmp"

	"github.com/cplieger/grelot/internal/wire"
)

// Listing is one page of a list read: Items in the read's order past its
// position, at most its limit; Total the items matching over every page,
// counted in the same read; More set when an item past the page matches;
// ReadAs the identity the read answered as, which a later page's query
// passes back (resumes).
type Listing[T any] struct {
	Items  []T
	Total  int
	ReadAs uint64
	More   bool
}

// ErrCursorMismatch refuses a later page that is not this caller's.
var ErrCursorMismatch = &Error{
	Kind: KindInvalid, Why: wire.ErrCursorMismatch.Code,
	Msg: "the cursor belongs to another list, query or caller: read the first page again",
}

// resumes refuses a later page whose position was read as readAs when
// the caller is now id, 0 for none: a retire and a registration on one
// session make a new id whose seqs start again. A nil readAs is a first
// page. Each read checks it inside the transaction that applies the
// position, so no retire lands between the check and the read.
func resumes(readAs *uint64, id uint64) error {
	if readAs != nil && *readAs != id {
		return ErrCursorMismatch
	}
	return nil
}

// pastKey is listing's past for a read ordered by key, the items whose key
// sorts after after; nil, the first page, when after is its zero value.
func pastKey[T any, K cmp.Ordered](after K, key func(*T) K) func(*T) bool {
	var zero K
	if after == zero {
		return nil
	}
	return func(v *T) bool { return key(v) > after }
}

// listing pages all, the matches in the read's order, from the first item
// past reports true for, to limit items; a nil past starts at the first
// and a zero limit takes every item.
func listing[T any](all []T, past func(*T) bool, limit int) Listing[T] {
	start := 0
	if past != nil {
		start = len(all)
		for i := range all {
			if past(&all[i]) {
				start = i
				break
			}
		}
	}
	rest := all[start:]
	if limit == 0 || limit > len(rest) {
		limit = len(rest)
	}
	return Listing[T]{Items: rest[:limit:limit], Total: len(all), More: len(rest) > limit}
}
