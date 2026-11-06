//go:build !windows

package store

import (
	"slices"
	"testing"
	"time"
)

func addresseeIDs(as []Addressee) []uint64 {
	out := make([]uint64, 0, len(as))
	for i := range as {
		out = append(out, as[i].ID)
	}
	return out
}

func TestAddressee_aLiveIdentityNamesItsHostSession(t *testing.T) {
	s, _ := openClockStore(t)
	r := registration("testhost:4f2a-b7", "subflux-dev", envA())
	r.Host = "borg/app"
	id := mustRegister(t, s, r)
	got, found, err := s.Addressee(t.Context(), id.ID)
	if err != nil || !found {
		t.Fatalf("Addressee: found %v err %v, want the live identity", found, err)
	}
	want := Addressee{ID: id.ID, Name: "subflux-dev", Provider: "testhost", Host: "borg/app", Session: "4f2a-b7"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestAddressee_aSessionIDKeepsItsOwnColons(t *testing.T) {
	s, _ := openClockStore(t)
	id := mustRegister(t, s, registration("testhost:a:b", "subflux-dev", envA()))
	got, _, err := s.Addressee(t.Context(), id.ID)
	if err != nil || got.Session != "a:b" {
		t.Errorf("session %q err %v, want a:b, the id after the provider's colon", got.Session, err)
	}
}

func TestAddressee_afterATakeOverIsTheNewSession(t *testing.T) {
	s, _ := openClockStore(t)
	orig := mustRegister(t, s, registration("testhost:old", "subflux-dev", envA()))
	r := registration("testhost:new", "subflux-dev", envA())
	r.TakeOver = true
	if _, _, err := s.Register(t.Context(), Keyed{}, &r); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.Addressee(t.Context(), orig.ID)
	if err != nil || !found || got.Session != "new" {
		t.Errorf("got %+v found %v err %v, want session new", got, found, err)
	}
}

func TestAddressee_leavesOutWhatIsNeverPushed(t *testing.T) {
	s, clk := openClockStore(t)
	parent, delegate := delegateOf(t, s, "alpha", "review")
	retired := regAt(t, s, "bravo")
	if _, err := s.Retire(t.Context(), Keyed{}, plain("bravo")); err != nil {
		t.Fatal(err)
	}
	short := registration(sess("charlie"), "charlie", envA())
	short.TTL = 15 * time.Minute
	dormant := mustRegister(t, s, short)
	clk.advance(15 * time.Minute)
	for _, tc := range []struct {
		name string
		id   uint64
	}{
		{"a delegate", delegate.ID},
		{"a retired identity", retired.ID},
		{"an identity past its lease the sweep has not reached", dormant.ID},
		{"an unknown id", 999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, found, err := s.Addressee(t.Context(), tc.id); err != nil || found {
				t.Errorf("got %+v found %v err %v, want not found", got, found, err)
			}
		})
	}
	mustSweep(t, s)
	t.Run("a dormant identity", func(t *testing.T) {
		if _, found, err := s.Addressee(t.Context(), dormant.ID); err != nil || found {
			t.Errorf("found %v err %v, want not found", found, err)
		}
	})
	t.Run("the list holds the live parent only", func(t *testing.T) {
		all, err := s.Addressees(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if got := addresseeIDs(all); !slices.Equal(got, []uint64{parent.ID}) {
			t.Errorf("ids %v, want [%d]", got, parent.ID)
		}
	})
}

func TestAddressees_listsEveryLiveIdentityInIDOrder(t *testing.T) {
	s, _ := openClockStore(t)
	a := regAt(t, s, "alpha")
	b := mustRegister(t, s, registration("otherhost:x1", "bravo", envA()))
	all, err := s.Addressees(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []Addressee{
		{ID: a.ID, Name: "alpha", Provider: "kiro-cli", Host: "host-a/app", Session: "session-alpha"},
		{ID: b.ID, Name: "bravo", Provider: "otherhost", Host: "host-a/app", Session: "x1"},
	}
	if !slices.Equal(all, want) {
		t.Errorf("got %+v, want %+v", all, want)
	}
}
