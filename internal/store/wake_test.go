//go:build !windows

package store

import (
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// wokeLog keeps every id set Options.Woke was called with.
type wokeLog struct {
	calls [][]uint64
	mu    sync.Mutex
}

func (l *wokeLog) woke(ids []uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, slices.Clone(ids))
}

// take returns the ids reported since the last take, sorted, each once.
func (l *wokeLog) take() []uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []uint64
	for _, c := range l.calls {
		out = append(out, c...)
	}
	l.calls = nil
	slices.Sort(out)
	return slices.Compact(out)
}

// openWokeStore opens a store on a test clock whose commit notice
// writes to the returned log.
func openWokeStore(t *testing.T) (*Store, *clock, *wokeLog) {
	t.Helper()
	clk := &clock{t: testNow}
	wl := &wokeLog{}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{
		Logger: slog.New(newLogSink()), Now: clk.now, Woke: wl.woke,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk, wl
}

func TestWoke_reportsEachIdentityACommitDeliveredTo(t *testing.T) {
	s, _, wl := openWokeStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	regAt(t, s, "charlie", "/workspace/other")
	wl.take()
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	if got := wl.take(); !slices.Equal(got, []uint64{bravo.ID}) {
		t.Errorf("direct post to bravo woke %v, want [%d]", got, bravo.ID)
	}
}

func TestWoke_reportsTheIdentitiesAJoinCardReaches(t *testing.T) {
	s, _, wl := openWokeStore(t)
	alpha := regAt(t, s, "alpha")
	wl.take()
	regAt(t, s, "bravo")
	if got := wl.take(); !slices.Equal(got, []uint64{alpha.ID}) {
		t.Errorf("bravo's registration woke %v, want alpha [%d], whose path it joins", got, alpha.ID)
	}
}

func TestWoke_reportsARecordedNudge(t *testing.T) {
	s, _, wl := openWokeStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	wl.take()
	mustSend(t, s, bravo.ID)
	if got := wl.take(); !slices.Equal(got, []uint64{bravo.ID}) {
		t.Errorf("RecordNudge woke %v, want [%d]", got, bravo.ID)
	}
}

func TestWoke_reportsAHoldThatRecordsTheNudge(t *testing.T) {
	s, _, wl := openWokeStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	wl.take()
	if _, err := s.Hold(t.Context(), Caller{Session: sess("bravo")}); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if got := wl.take(); !slices.Equal(got, []uint64{bravo.ID}) {
		t.Errorf("a hold showing mail woke %v, want [%d]", got, bravo.ID)
	}
	t.Run("a second hold with nothing new reports nothing", func(t *testing.T) {
		if _, err := s.Hold(t.Context(), Caller{Session: sess("bravo")}); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		if got := wl.take(); len(got) != 0 {
			t.Errorf("woke %v, want nothing", got)
		}
	})
}

func TestWoke_reportsAnAckThatMovesRead(t *testing.T) {
	s, _, wl := openWokeStore(t)
	regAt(t, s, "alpha")
	bravo := regAt(t, s, "bravo")
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	wl.take()
	a := mustAck(t, s, sess("bravo"), 1<<62)
	if got := wl.take(); !slices.Equal(got, []uint64{bravo.ID}) {
		t.Errorf("an ack moving read to %d woke %v, want [%d]", a.Read, got, bravo.ID)
	}
	t.Run("an ack that moves nothing reports nothing", func(t *testing.T) {
		mustAck(t, s, sess("bravo"), 1<<62)
		if got := wl.take(); len(got) != 0 {
			t.Errorf("woke %v, want nothing", got)
		}
	})
}

func TestWoke_reportsNothingForReadsOrARefusedWrite(t *testing.T) {
	s, _, wl := openWokeStore(t)
	regAt(t, s, "alpha")
	regAt(t, s, "bravo")
	mustPost(t, s, posting(sess("alpha"), Target{Agent: "bravo"}))
	wl.take()
	bravo := Caller{Session: sess("bravo")}
	t.Run("reads", func(t *testing.T) {
		mustInbox(t, s, sess("bravo"), 10)
		if _, err := s.Status(t.Context(), bravo); err != nil {
			t.Fatalf("Status: %v", err)
		}
		if _, err := s.History(t.Context(), bravo, &HistoryQuery{}); err != nil {
			t.Fatalf("History: %v", err)
		}
		if got := wl.take(); len(got) != 0 {
			t.Errorf("woke %v, want nothing", got)
		}
	})
	t.Run("a refused post", func(t *testing.T) {
		if _, _, err := s.Post(t.Context(), Keyed{}, new(posting(sess("alpha"), Target{Agent: "nobody"}))); err == nil {
			t.Fatal("post to an unknown name passed")
		}
		if got := wl.take(); len(got) != 0 {
			t.Errorf("woke %v, want nothing", got)
		}
	})
}

func TestWoke_reportsARequestThatClearsEnded(t *testing.T) {
	bravoAsks := map[string]func(t *testing.T, s *Store) error{
		"Seen": func(t *testing.T, s *Store) error {
			_, err := s.Seen(t.Context(), Caller{Session: sess("bravo")})
			return err
		},
		"Set": func(t *testing.T, s *Store) error {
			_, _, err := s.Set(t.Context(), Keyed{}, Caller{Session: sess("bravo")}, Change{Fields: Fields{Name: NoChange, Description: "resumed", Paths: []string{NoChange}}})
			return err
		},
		"Post": func(t *testing.T, s *Store) error {
			_, _, err := s.Post(t.Context(), Keyed{}, new(posting(sess("bravo"), Target{Agent: "alpha"})))
			return err
		},
		"Hold": func(t *testing.T, s *Store) error {
			_, err := s.Hold(t.Context(), Caller{Session: sess("bravo")})
			return err
		},
		"Ack": func(t *testing.T, s *Store) error {
			_, _, err := s.Ack(t.Context(), Keyed{}, Caller{Session: sess("bravo")}, 0)
			return err
		},
		"Register again": func(t *testing.T, s *Store) error {
			_, _, err := s.Register(t.Context(), Keyed{}, new(registration(sess("bravo"), "bravo", envA())))
			return err
		},
	}
	for name, ask := range bravoAsks {
		t.Run(name, func(t *testing.T) {
			s, _, wl := openWokeStore(t)
			regAt(t, s, "alpha")
			bravo := regAt(t, s, "bravo")
			if err := s.EndSession(t.Context(), sess("bravo")); err != nil {
				t.Fatalf("EndSession: %v", err)
			}
			wl.take()
			if err := ask(t, s); err != nil {
				t.Fatalf("%s from the ended session: %v", name, err)
			}
			if got := wl.take(); !slices.Contains(got, bravo.ID) {
				t.Errorf("%s clearing ended woke %v, want bravo [%d]: a nudge its closed session held back may now go out", name, got, bravo.ID)
			}
		})
	}
}

func TestLive_keepsOnlyTheLiveIdentities(t *testing.T) {
	s, clk, _ := openWokeStore(t)
	dormant := regAt(t, s, "alpha")
	clk.advance(7 * time.Hour)
	mustSweep(t, s)
	live := regAt(t, s, "bravo")
	retired := regAt(t, s, "charlie")
	if _, _, err := s.Retire(t.Context(), Keyed{}, Caller{Session: sess("charlie")}); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	got, err := s.Live(t.Context(), []uint64{dormant.ID, live.ID, retired.ID, 999})
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if !slices.Equal(got, []uint64{live.ID}) {
		t.Errorf("Live = %v, want [%d]: alpha dormant, charlie retired, 999 never registered", got, live.ID)
	}
}
