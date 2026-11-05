package store

import (
	"testing"
	"time"
)

func TestStatus_countsUnreadAndWritesNothing(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	mustRegister(t, s, registration(sessA, "subflux-dev", envA()))
	mustRegister(t, s, registration(sessB, "store-dev", envA()))
	if _, err := s.Post(t.Context(), &Posting{
		Caller: Caller{Session: sessB}, Subject: "hi", Body: "b", To: Target{Agent: "subflux-dev"},
		Fields: Fields{Name: NoChange, Description: NoChange, Paths: []string{NoChange}},
	}); err != nil {
		t.Fatal(err)
	}
	before := commitCount(t, s)
	st, err := s.Status(t.Context(), Caller{Session: sessA})
	if err != nil {
		t.Fatal(err)
	}
	if st.Identity.Name != "subflux-dev" || st.Unread.Direct != 1 || st.Unread.Cards != 1 || st.Unread.Total() != 2 || st.Parent != 0 {
		t.Errorf("status %+v, want subflux-dev with one direct message and store-dev's join card unread, not a delegate", st)
	}
	if commitCount(t, s) != before {
		t.Error("Status committed")
	}
}

func TestStatus_noIdentityAndDormantAreTheirLines(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	_, err := s.Status(t.Context(), Caller{Session: sessA})
	wantRefusal(t, err, KindGone, "none", "grelot register")
	mustRegister(t, s, registration(sessA, "subflux-dev", envA()))
	clk.advance(7 * time.Hour)
	_, err = s.Status(t.Context(), Caller{Session: sessA})
	wantRefusal(t, err, KindGone, "dormant", "grelot revive --ttl")
}

func TestStatus_passesTheDelegateGateAndMarksADelegate(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	mustRegister(t, s, registration(sessA, "subflux-dev", envA()))
	g, err := s.Delegate(t.Context(), &Grant{Caller: Caller{Session: sessA}, Label: "tests", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Inbox(t.Context(), Caller{Session: sessA}, 0); err == nil {
		t.Fatal("the gate let an unnamed inbox through")
	}
	if st, err := s.Status(t.Context(), Caller{Session: sessA}); err != nil || st.Identity.Name != "subflux-dev" {
		t.Errorf("status %+v, %v; want the parent past the gate", st, err)
	}
	mustRegisterDelegate(t, s, sessA, g.Link)
	st, err := s.Status(t.Context(), Caller{Session: sessA, As: g.Handle})
	if err != nil || st.Parent == 0 || st.Identity.Name != g.Handle {
		t.Errorf("status %+v, %v; want the delegate %s", st, err, g.Handle)
	}
}
