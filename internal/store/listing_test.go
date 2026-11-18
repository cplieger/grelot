//go:build !windows

package store

import (
	"errors"
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

// Each paged read reports the identity it read as, and refuses a later
// page whose position was read as another identity than the caller's now,
// 0 standing for a caller with none.
func TestPagedReads_refuseAPositionReadAsAnotherIdentity(t *testing.T) {
	s, _ := openClockStore(t)
	beta := regAt(t, s, "beta", "/workspace/b")
	regAt(t, s, "alpha", "/workspace/a")
	say(t, s, "alpha", Target{Agent: "beta"}, "hello")
	say(t, s, "alpha", Target{Path: "/workspace/a/x"}, "on x")
	mustReserve(t, s, Reserving{Caller: Caller{Session: sess("alpha")}, Path: "/workspace/a/x", Reason: "a change", TTL: time.Hour})
	none := uint64(0)
	other := beta.ID + 100
	read := map[string]func(c Caller, readAs *uint64) (uint64, error){
		"Who": func(c Caller, readAs *uint64) (uint64, error) {
			l, err := s.Who(t.Context(), c, &WhoQuery{ReadAs: readAs})
			return l.ReadAs, err
		},
		"Threads": func(c Caller, readAs *uint64) (uint64, error) {
			l, err := s.Threads(t.Context(), c, ThreadQuery{ReadAs: readAs})
			return l.ReadAs, err
		},
		"Reservations": func(c Caller, readAs *uint64) (uint64, error) {
			l, err := s.Reservations(t.Context(), c, ReservationQuery{ReadAs: readAs})
			return l.ReadAs, err
		},
		"Inbox": func(c Caller, readAs *uint64) (uint64, error) {
			p, err := s.Inbox(t.Context(), c, InboxQuery{ReadAs: readAs})
			return p.ReadAs, err
		},
		"History": func(c Caller, readAs *uint64) (uint64, error) {
			p, err := s.History(t.Context(), c, &HistoryQuery{ReadAs: readAs})
			return p.ReadAs, err
		},
	}
	me := Caller{Session: sess("beta")}
	for name, read := range read {
		t.Run(name, func(t *testing.T) {
			if got, err := read(me, nil); err != nil || got != beta.ID {
				t.Errorf("a first page read as %d, %v; want %d", got, err, beta.ID)
			}
			if _, err := read(me, &beta.ID); err != nil {
				t.Errorf("a later page read as the caller: %v, want it read", err)
			}
			for _, as := range []*uint64{&other, &none} {
				if _, err := read(me, as); !errors.Is(err, ErrCursorMismatch) {
					t.Errorf("a later page read as %d: %v, want ErrCursorMismatch", *as, err)
				}
			}
		})
	}
	nobody := Caller{Session: sess("nobody")}
	for _, name := range []string{"Who", "Reservations"} {
		t.Run(name+" with no identity", func(t *testing.T) {
			if got, err := read[name](nobody, nil); err != nil || got != 0 {
				t.Errorf("a first page read as %d, %v; want 0", got, err)
			}
			if _, err := read[name](nobody, &none); err != nil {
				t.Errorf("a later page read as no identity: %v, want it read", err)
			}
			if _, err := read[name](nobody, &beta.ID); !errors.Is(err, ErrCursorMismatch) {
				t.Errorf("a later page read as %d by a caller with none: %v, want ErrCursorMismatch", beta.ID, err)
			}
		})
	}
}

// A retire and a registration on the same session make a new identity
// whose seqs start again, so the old identity's inbox position is refused
// rather than applied to the new mailbox.
func TestInbox_aRetireAndRegisterOnTheSessionRefusesTheOldPosition(t *testing.T) {
	s, _ := openClockStore(t)
	beta := regAt(t, s, "beta", "/workspace/b")
	regAt(t, s, "alpha", "/workspace/a")
	c := Caller{Session: sess("beta")}
	if _, _, err := s.Retire(t.Context(), Keyed{}, c); err != nil {
		t.Fatal(err)
	}
	gamma := mustRegister(t, s, registration(sess("beta"), "gamma", envA(), "/workspace/b"))
	say(t, s, "alpha", Target{Agent: "gamma"}, "hello")
	if gamma.ID == beta.ID {
		t.Fatalf("gamma took beta's id %d", beta.ID)
	}
	if _, err := s.Inbox(t.Context(), c, InboxQuery{ReadAs: &beta.ID, After: 1}); !errors.Is(err, ErrCursorMismatch) {
		t.Errorf("beta's position read on gamma's session: %v, want ErrCursorMismatch", err)
	}
}
