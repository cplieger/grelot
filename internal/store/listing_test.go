//go:build !windows

package store

import (
	"slices"
	"testing"
	"time"
)

// walkPages reads one list read page by page with limit 2, from the
// first page and then past each page's last item, and returns every
// item's key in the order read, checking each page's Total and More.
func walkPages[T any](t *testing.T, want int, read func(after T) (Listing[T], error), key func(*T) string) []string {
	t.Helper()
	var keys []string
	var after T
	for page := 1; page <= want+1; page++ {
		got, err := read(after)
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != want {
			t.Errorf("page %d: Total %d, want %d", page, got.Total, want)
		}
		if len(got.Items) > 2 {
			t.Errorf("page %d holds %d items, over the limit of 2", page, len(got.Items))
		}
		for i := range got.Items {
			keys = append(keys, key(&got.Items[i]))
		}
		if more := len(keys) < want; got.More != more {
			t.Errorf("page %d: More %v, want %v", page, got.More, more)
		}
		if !got.More || len(got.Items) == 0 {
			return keys
		}
		after = got.Items[len(got.Items)-1]
	}
	t.Errorf("the walk did not end within %d pages", want+1)
	return keys
}

func TestWho_pagesByName(t *testing.T) {
	s, _ := openClockStore(t)
	for _, name := range []string{"echo", "alpha", "delta", "charlie", "bravo"} {
		regAt(t, s, name, "/workspace/"+name)
	}
	got := walkPages(t, 5, func(after Peer) (Listing[Peer], error) {
		return s.Who(t.Context(), Caller{Session: sess("alpha")}, &WhoQuery{After: after.Name, Limit: 2})
	}, func(p *Peer) string { return p.Name })
	if want := []string{"alpha", "bravo", "charlie", "delta", "echo"}; !slices.Equal(got, want) {
		t.Errorf("the walk read %v, want %v", got, want)
	}
}

func TestThreads_pagesByPath(t *testing.T) {
	s, _ := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/p")
	for _, p := range []string{"e", "a", "d", "c", "b"} {
		say(t, s, "alpha", Target{Path: "/workspace/p/" + p}, "on "+p)
	}
	got := walkPages(t, 5, func(after PathThread) (Listing[PathThread], error) {
		return s.Threads(t.Context(), Caller{Session: sess("alpha")}, ThreadQuery{Root: "/workspace/p", After: after.Path, Limit: 2})
	}, func(th *PathThread) string { return th.Path })
	want := []string{"/workspace/p/a", "/workspace/p/b", "/workspace/p/c", "/workspace/p/d", "/workspace/p/e"}
	if !slices.Equal(got, want) {
		t.Errorf("the walk read %v, want %v", got, want)
	}
}

func TestReservations_pagesById(t *testing.T) {
	s, _ := openClockStore(t)
	regAt(t, s, "alpha", "/workspace/r")
	var want []string
	for _, p := range []string{"e", "a", "d", "c", "b"} {
		mustReserve(t, s, Reserving{Caller: Caller{Session: sess("alpha")}, Path: "/workspace/r/" + p, Reason: "a change", TTL: time.Hour})
		want = append(want, "/workspace/r/"+p)
	}
	got := walkPages(t, 5, func(after Reservation) (Listing[Reservation], error) {
		return s.Reservations(t.Context(), Caller{Session: sess("alpha")}, ReservationQuery{After: after.ID, Limit: 2})
	}, func(v *Reservation) string { return v.Path })
	if !slices.Equal(got, want) {
		t.Errorf("the walk read %v, want %v (id order)", got, want)
	}
}
