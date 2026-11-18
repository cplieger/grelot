//go:build !windows

package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// The machine keys of two machines.
const (
	keyOne int64 = 1
	keyTwo int64 = 2
)

const otherKeyMsg = "another machine's key"

func keyedRegistration(session, name string, key int64) Registration {
	r := registration(session, name, envA())
	r.Key = key
	return r
}

func TestCaller_anIdentityActsOnlyForTheKeyThatRegisteredIt(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	mustRegister(t, s, keyedRegistration(sessA, "subflux-dev", keyOne))

	if _, err := s.Status(t.Context(), Caller{Session: sessA, Key: keyOne}); err != nil {
		t.Errorf("Status from the registering key: %v", err)
	}
	_, err := s.Status(t.Context(), Caller{Session: sessA, Key: keyTwo})
	wantRefusal(t, err, KindForbidden, "", otherKeyMsg)
	p := posting(sessA, Target{Announce: true})
	p.Caller.Key = keyTwo
	_, _, err = s.Post(t.Context(), Keyed{}, &p)
	wantRefusal(t, err, KindForbidden, "", otherKeyMsg)
	wantRefusal(t, s.EndSession(t.Context(), sessA, keyTwo), KindForbidden, "", otherKeyMsg)
}

func TestCaller_anotherKeyCannotTakeTheSessionOrTheName(t *testing.T) {
	t.Run("a register from the session", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		mustRegister(t, s, keyedRegistration(sessA, "subflux-dev", keyOne))
		r := keyedRegistration(sessA, "grelot-dev", keyTwo)
		_, _, err := s.Register(t.Context(), Keyed{}, &r)
		wantRefusal(t, err, KindForbidden, "", otherKeyMsg)
	})
	t.Run("a take-over of the name", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		mustRegister(t, s, keyedRegistration(sessA, "subflux-dev", keyOne))
		r := keyedRegistration(sessB, "subflux-dev", keyTwo)
		r.TakeOver = true
		_, _, err := s.Register(t.Context(), Keyed{}, &r)
		wantRefusal(t, err, KindForbidden, "", otherKeyMsg)
	})
	t.Run("a take-over from the same key moves it", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		mustRegister(t, s, keyedRegistration(sessA, "subflux-dev", keyOne))
		r := keyedRegistration(sessB, "subflux-dev", keyOne)
		r.TakeOver = true
		got, _, err := s.Register(t.Context(), Keyed{}, &r)
		if err != nil || !got.Moved {
			t.Errorf("take-over from the registering key: %+v %v, want it moved", got, err)
		}
	})
}

func TestCaller_anotherKeyCannotReviveTheIdentity(t *testing.T) {
	t.Run("by name", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		expireToDormant(t, s, clk, "subflux-dev")
		r := revival(sessB, "subflux-dev", envA())
		r.Key = keyTwo
		_, _, err := s.Revive(t.Context(), Keyed{}, &r)
		wantRefusal(t, err, KindForbidden, "", otherKeyMsg)
	})
	t.Run("from its session", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		expireToDormant(t, s, clk, "subflux-dev")
		r := revival(sessA, "", envA())
		r.Key = keyTwo
		_, _, err := s.Revive(t.Context(), Keyed{}, &r)
		wantRefusal(t, err, KindForbidden, "", otherKeyMsg)
	})
	t.Run("the registering key revives it", func(t *testing.T) {
		s, clk, _ := openIdentityStore(t)
		expireToDormant(t, s, clk, "subflux-dev")
		mustRevive(t, s, revival(sessB, "subflux-dev", envA()))
	})
}

func TestCaller_aDelegateBelongsToItsParentsKey(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	mustRegister(t, s, keyedRegistration(sess("parent"), "parent", keyOne))
	g := grantFrom("parent", "review")
	g.Caller.Key = keyOne
	granted := mustGrant(t, s, g)

	stolen := delegateReg("codex:thread-1", granted.Link)
	stolen.Key = keyTwo
	_, _, err := s.Register(t.Context(), Keyed{}, &stolen)
	wantRefusal(t, err, KindForbidden, "", otherKeyMsg)

	own := delegateReg("codex:thread-1", granted.Link)
	own.Key = keyOne
	got, _, err := s.Register(t.Context(), Keyed{}, &own)
	if err != nil {
		t.Fatalf("delegate from the parent's key: %v", err)
	}
	if row := getIdentity(t, s, got.Identity.ID); row.Key != keyOne {
		t.Errorf("the delegate's key = %d, want its parent's %d", row.Key, keyOne)
	}
	_, err = s.Status(t.Context(), Caller{Session: sess("parent"), As: "parent/review", Key: keyTwo})
	wantRefusal(t, err, KindForbidden, "", otherKeyMsg)
	wantRefusal(t, s.SubagentStop(t.Context(), "codex:thread-1", keyTwo), KindForbidden, "", otherKeyMsg)
}

func TestTakeOverHint_namesOnlyAnIdentityOfTheSameKey(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	r := keyedRegistration(sessA, "subflux-dev", keyOne)
	r.Proc = "proc-1"
	mustRegister(t, s, r)

	if _, found, err := s.TakeOverHint(t.Context(), sessB, keyTwo, "proc-1", envA()); err != nil || found {
		t.Errorf("hint for another key: found %v, %v; want none", found, err)
	}
	if got, found, err := s.TakeOverHint(t.Context(), sessB, keyOne, "proc-1", envA()); err != nil || !found || got.Name != "subflux-dev" {
		t.Errorf("hint for the same key: %q found %v, %v; want subflux-dev", got.Name, found, err)
	}
}

// openKeyedStore is openIdentityStore whose KeyLive holds every key live
// but those dead names; a test adds to dead as it revokes a key.
func openKeyedStore(t *testing.T, dead map[int64]bool) (*Store, *clock) {
	t.Helper()
	clk := &clock{t: testNow}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{
		Now: clk.now, KeyLive: func(key int64) (bool, error) { return !dead[key], nil },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

// A machine's sessions keep their identities when its key is replaced: a
// row bound to a key that no longer authenticates binds nothing.
func TestCaller_anIdentityOfADeadKeyIsRebound(t *testing.T) {
	t.Run("its own session registers under the new key", func(t *testing.T) {
		dead := map[int64]bool{}
		s, _ := openKeyedStore(t, dead)
		id := mustRegister(t, s, keyedRegistration(sessA, "subflux-dev", keyOne))
		dead[keyOne] = true

		r := keyedRegistration(sessA, "subflux-dev", keyTwo)
		got, _, err := s.Register(t.Context(), Keyed{}, &r)

		if err != nil || !got.Existing || got.Identity.ID != id.ID {
			t.Fatalf("register under the new key: %+v %v, want the same identity back", got, err)
		}
		if row := getIdentity(t, s, id.ID); row.Key != keyTwo {
			t.Errorf("the identity's key = %d, want the new key %d", row.Key, keyTwo)
		}
		if _, err := s.Status(t.Context(), Caller{Session: sessA, Key: keyTwo}); err != nil {
			t.Errorf("Status under the new key: %v", err)
		}
	})
	t.Run("another key's take-over moves it", func(t *testing.T) {
		dead := map[int64]bool{}
		s, _ := openKeyedStore(t, dead)
		id := mustRegister(t, s, keyedRegistration(sessA, "subflux-dev", keyOne))
		dead[keyOne] = true

		r := keyedRegistration(sessB, "subflux-dev", keyTwo)
		r.TakeOver = true
		got, _, err := s.Register(t.Context(), Keyed{}, &r)

		if err != nil || !got.Moved {
			t.Fatalf("take-over of a dead key's identity: %+v %v, want it moved", got, err)
		}
		if row := getIdentity(t, s, id.ID); row.Key != keyTwo || row.Session != sessB {
			t.Errorf("the identity is key %d session %q, want key %d session %q", row.Key, row.Session, keyTwo, sessB)
		}
	})
	t.Run("a revive by name and from its session", func(t *testing.T) {
		for _, name := range []string{"subflux-dev", ""} {
			dead := map[int64]bool{}
			s, clk := openKeyedStore(t, dead)
			r := keyedRegistration(sessA, "subflux-dev", keyOne)
			r.TTL = 15 * time.Minute
			id := mustRegister(t, s, r)
			clk.advance(15 * time.Minute)
			mustSweep(t, s)
			dead[keyOne] = true

			rv := revival(sessA, name, envA())
			rv.Key = keyTwo
			mustRevive(t, s, rv)

			if row := getIdentity(t, s, id.ID); row.Key != keyTwo {
				t.Errorf("revive %q: the identity's key = %d, want %d", name, row.Key, keyTwo)
			}
		}
	})
	t.Run("a delegate follows its parent", func(t *testing.T) {
		dead := map[int64]bool{}
		s, _ := openKeyedStore(t, dead)
		mustRegister(t, s, keyedRegistration(sess("parent"), "parent", keyOne))
		g := grantFrom("parent", "review")
		g.Caller.Key = keyOne
		d := delegateReg("codex:thread-1", mustGrant(t, s, g).Link)
		d.Key = keyOne
		del, _, err := s.Register(t.Context(), Keyed{}, &d)
		if err != nil {
			t.Fatal(err)
		}
		dead[keyOne] = true

		r := keyedRegistration(sess("parent"), "parent", keyTwo)
		r.Self = true
		if _, _, err := s.Register(t.Context(), Keyed{}, &r); err != nil {
			t.Fatalf("the parent's register under the new key: %v", err)
		}

		if row := getIdentity(t, s, del.Identity.ID); row.Key != keyTwo {
			t.Errorf("the delegate's key = %d, want its parent's new key %d", row.Key, keyTwo)
		}
		dead[keyOne] = false
		if _, err := s.Status(t.Context(), Caller{Session: "codex:thread-1", Key: keyOne}); err == nil {
			t.Error("the delegate still acts for the old key")
		}
	})
	t.Run("a link of a dead key's parent rebinds the parent", func(t *testing.T) {
		dead := map[int64]bool{}
		s, _ := openKeyedStore(t, dead)
		parent := mustRegister(t, s, keyedRegistration(sess("parent"), "parent", keyOne))
		g := grantFrom("parent", "review")
		g.Caller.Key = keyOne
		link := mustGrant(t, s, g).Link
		dead[keyOne] = true

		d := delegateReg("codex:thread-1", link)
		d.Key = keyTwo
		got, _, err := s.Register(t.Context(), Keyed{}, &d)
		if err != nil {
			t.Fatalf("delegate under the new key: %v", err)
		}
		if p, c := getIdentity(t, s, parent.ID), getIdentity(t, s, got.Identity.ID); p.Key != keyTwo || c.Key != keyTwo {
			t.Errorf("parent key %d, delegate key %d, want both %d", p.Key, c.Key, keyTwo)
		}
	})
	t.Run("a read and a write under the new key act before any register", func(t *testing.T) {
		dead := map[int64]bool{}
		s, _ := openKeyedStore(t, dead)
		mustRegister(t, s, keyedRegistration(sessA, "subflux-dev", keyOne))
		dead[keyOne] = true

		if _, err := s.Status(t.Context(), Caller{Session: sessA, Key: keyTwo}); err != nil {
			t.Errorf("Status under the new key: %v", err)
		}
		p := posting(sessA, Target{Announce: true})
		p.Caller.Key = keyTwo
		if _, _, err := s.Post(t.Context(), Keyed{}, &p); err != nil {
			t.Errorf("Post under the new key: %v", err)
		}
	})
	t.Run("a subagent stop under the new key ends the delegate", func(t *testing.T) {
		dead := map[int64]bool{}
		s, _ := openKeyedStore(t, dead)
		mustRegister(t, s, keyedRegistration(sess("parent"), "parent", keyOne))
		g := grantFrom("parent", "review")
		g.Caller.Key = keyOne
		d := delegateReg("codex:thread-1", mustGrant(t, s, g).Link)
		d.Key = keyOne
		del, _, err := s.Register(t.Context(), Keyed{}, &d)
		if err != nil {
			t.Fatal(err)
		}
		dead[keyOne] = true
		if _, err := s.Status(t.Context(), Caller{Session: sess("parent"), As: del.Identity.Name, Key: keyTwo}); err != nil {
			t.Fatalf("the delegate before its stop: %v", err)
		}

		if err := s.SubagentStop(t.Context(), "codex:thread-1", keyTwo); err != nil {
			t.Fatalf("SubagentStop under the new key: %v", err)
		}
		if _, err := s.Status(t.Context(), Caller{Session: sess("parent"), As: del.Identity.Name, Key: keyTwo}); err == nil {
			t.Error("the delegate still acts after its subagent stopped")
		}
	})
	t.Run("a delegate registers from a session a dead key's dormant identity holds", func(t *testing.T) {
		dead := map[int64]bool{}
		s, clk := openKeyedStore(t, dead)
		r := keyedRegistration("codex:thread-1", "old-dev", keyOne)
		r.TTL = 15 * time.Minute
		mustRegister(t, s, r)
		clk.advance(15 * time.Minute)
		mustSweep(t, s)
		dead[keyOne] = true
		mustRegister(t, s, keyedRegistration(sess("parent"), "parent", keyTwo))
		g := grantFrom("parent", "review")
		g.Caller.Key = keyTwo
		d := delegateReg("codex:thread-1", mustGrant(t, s, g).Link)
		d.Key = keyTwo

		if _, _, err := s.Register(t.Context(), Keyed{}, &d); err != nil {
			t.Errorf("delegate from the dead key's session: %v", err)
		}
	})
	t.Run("the take-over hint names it", func(t *testing.T) {
		dead := map[int64]bool{}
		s, _ := openKeyedStore(t, dead)
		r := keyedRegistration(sessA, "subflux-dev", keyOne)
		r.Proc = "proc-1"
		mustRegister(t, s, r)
		dead[keyOne] = true

		if got, found, err := s.TakeOverHint(t.Context(), sessB, keyTwo, "proc-1", envA()); err != nil || !found || got.Name != "subflux-dev" {
			t.Errorf("hint for a new key: %q found %v, %v; want subflux-dev", got.Name, found, err)
		}
	})
}

func TestCaller_anotherLiveKeyIsStillRefused(t *testing.T) {
	dead := map[int64]bool{}
	s, _ := openKeyedStore(t, dead)
	mustRegister(t, s, keyedRegistration(sessA, "subflux-dev", keyOne))
	dead[3] = true

	r := keyedRegistration(sessA, "subflux-dev", keyTwo)
	_, _, err := s.Register(t.Context(), Keyed{}, &r)

	wantRefusal(t, err, KindForbidden, "", otherKeyMsg)
}

func TestCaller_aFailedKeyCheckFailsTheRequest(t *testing.T) {
	clk := &clock{t: testNow}
	broken := errors.New("accounts file unreadable")
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{
		Now: clk.now, KeyLive: func(int64) (bool, error) { return false, broken },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mustRegister(t, s, keyedRegistration(sessA, "subflux-dev", keyOne))

	if _, err := s.Status(t.Context(), Caller{Session: sessA, Key: keyTwo}); !errors.Is(err, broken) {
		t.Errorf("Status under another key while the check fails: %v, want the check's error", err)
	}
	if _, err := s.Status(t.Context(), Caller{Session: sessA, Key: keyOne}); err != nil {
		t.Errorf("Status under its own key, which needs no check: %v", err)
	}
}
