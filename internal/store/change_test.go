//go:build !windows

package store

import (
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// openChangeStore is a store whose Options.Changed records every change.
func openChangeStore(t *testing.T) (*Store, *clock, *[]WebChange) {
	t.Helper()
	clk := &clock{t: testNow}
	var got []WebChange
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{
		Logger: slog.New(newLogSink()), Now: clk.now,
		Changed: func(c WebChange) { got = append(got, c) },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk, &got
}

func TestChanged_anIdentityDeleteNamesTheAgents(t *testing.T) {
	t.Run("retire", func(t *testing.T) {
		s, _, got := openChangeStore(t)
		regAt(t, s, "alpha")
		*got = nil
		if _, _, err := s.Retire(t.Context(), Keyed{}, Caller{Session: sess("alpha")}); err != nil {
			t.Fatal(err)
		}
		if len(*got) != 1 || !(*got)[0].Agents {
			t.Errorf("changes after a retirement %+v, want one naming the agents", *got)
		}
	})
	t.Run("dormant purge", func(t *testing.T) {
		s, clk, got := openChangeStore(t)
		expireToDormant(t, s, clk, "subflux-dev")
		*got = nil
		clk.set(testNow.Add(15*time.Minute + 72*time.Hour))
		mustSweep(t, s)
		if len(*got) == 0 || !(*got)[len(*got)-1].Agents {
			t.Errorf("changes after the purge %+v, want the last naming the agents", *got)
		}
	})
}

func TestChanged_anExpirySweepNamesTheThread(t *testing.T) {
	const retention = 30 * time.Minute
	clk := &clock{t: testNow}
	var got []WebChange
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{
		Logger: slog.New(newLogSink()), Now: clk.now, Retention: retainFor(retention),
		Changed: func(c WebChange) { got = append(got, c) },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	regAt(t, s, "alpha", repoSubflux)
	first := say(t, s, "alpha", Target{Path: repoSubflux}, "first")
	clk.advance(retention / 2)
	say(t, s, "alpha", Target{Path: repoSubflux}, "second")
	names := func() bool {
		return slices.ContainsFunc(got, func(c WebChange) bool { return slices.Contains(c.Conversations, first.Thread) })
	}

	got = nil
	clk.advance(retention / 2)
	mustSweep(t, s)
	if !names() {
		t.Errorf("changes after the first post expired %+v, want one naming thread %d", got, first.Thread)
	}
	got = nil
	clk.advance(retention / 2)
	mustSweep(t, s)
	if !names() {
		t.Errorf("changes after the thread expired %+v, want one naming thread %d", got, first.Thread)
	}
}

func TestChanged_aSubpathsChangeNamesTheConversationsAboveIt(t *testing.T) {
	const retention = time.Hour
	clk := &clock{t: testNow}
	var got []WebChange
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{
		Logger: slog.New(newLogSink()), Now: clk.now, Retention: retainFor(retention),
		Changed: func(c WebChange) { got = append(got, c) },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	regAt(t, s, "alpha", pathWeb)
	regAt(t, s, "dave", "/workspace")
	child := say(t, s, "alpha", Target{Path: pathWeb}, "first child")
	clk.advance(30 * time.Minute)
	parent := say(t, s, "dave", Target{Path: pathMarotte}, "parent")
	sibling := say(t, s, "dave", Target{Path: pathMarotte + "-old"}, "sibling")
	clk.advance(10 * time.Minute)
	names := func(thread uint64) bool {
		return slices.ContainsFunc(got, func(c WebChange) bool { return slices.Contains(c.Conversations, thread) })
	}
	check := func(after string) {
		t.Helper()
		if !names(child.Thread) || !names(parent.Thread) {
			t.Errorf("changes after %s %+v, want threads %d and %d", after, got, child.Thread, parent.Thread)
		}
		if names(sibling.Thread) {
			t.Errorf("changes after %s %+v name the name-prefix sibling %d", after, got, sibling.Thread)
		}
	}

	got = nil
	say(t, s, "alpha", Target{Path: pathWeb}, "second child")
	check("a post at " + pathWeb)

	got = nil
	mustAck(t, s, sess("dave"), 1<<62)
	check("dave read the child posts")

	// Only the first child post is due: the parent's post and both
	// threads outlive it.
	got = nil
	clk.advance(retention - 40*time.Minute + time.Minute)
	mustSweep(t, s)
	check("the first child post expired")

	// The child thread ends with its second post, while a later post keeps
	// the parent thread live.
	say(t, s, "dave", Target{Path: pathMarotte}, "parent later")
	clk.advance(30 * time.Minute)
	mustSweep(t, s)
	got = nil
	clk.advance(10 * time.Minute)
	mustSweep(t, s)
	check("the child thread expired")
}
