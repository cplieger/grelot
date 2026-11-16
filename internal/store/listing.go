//go:build !windows

package store

import "cmp"

// Listing is one page of a list read: Items in the read's order past its
// position, at most its limit; Total the items matching over every page,
// counted in the same read; More set when an item past the page matches.
type Listing[T any] struct {
	Items []T
	Total int
	More  bool
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
