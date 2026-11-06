//go:build !windows

package store

import (
	"slices"
	"testing"
	"time"
)

const (
	repoToolbelt = "/workspace/toolbelt"
	repoWTK      = "/workspace/web-terminal-kiro"
	repoDocs     = "/workspace/docs"
	engineGo     = repoToolbelt + "/engine.go"
	guideMD      = repoDocs + "/guide.md"
)

// whoFixture registers four agents: one on toolbelt holding
// engine.go, two on web-terminal-kiro with different descriptions, and
// docs-writer on docs, which holds guide.md, queues for engine.go and
// follows the web-terminal-kiro thread.
func whoFixture(t *testing.T) *Store {
	t.Helper()
	s, _, _ := openIdentityStore(t)
	reg := func(name, desc, repo string) {
		r := registration(sess(name), name, Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{co(repo, "1")}}, repo)
		r.Fields.Description = desc
		mustRegister(t, s, r)
	}
	reg("toolbelt-dev", "adding the asset chooser", repoToolbelt)
	reg("wtk-ui", "styling the terminal panel", repoWTK)
	reg("wtk-server", "fixing the pty resize", repoWTK)
	reg("docs-writer", "writing the install guide", repoDocs)
	mustReserve(t, s, reserving("toolbelt-dev", engineGo, time.Hour))
	mustReserve(t, s, reserving("docs-writer", guideMD, time.Hour))
	mustQueue(t, s, reserving("docs-writer", engineGo, time.Hour))
	mustPost(t, s, posting(sess("docs-writer"), Target{Path: repoWTK}))
	return s
}

func whoNames(t *testing.T, s *Store, q WhoQuery) []string {
	t.Helper()
	ps, err := s.Who(t.Context(), Caller{Session: sess("docs-writer")}, &q)
	if err != nil {
		t.Fatalf("Who(%+v): %v", q, err)
	}
	out := make([]string, len(ps))
	for i := range ps {
		out[i] = ps[i].Name
	}
	return out
}

func TestWho_carriesEachAgentsDeclaredWork(t *testing.T) {
	s := whoFixture(t)
	for _, sess := range []string{sess("docs-writer"), "kiro-cli:nobody"} {
		ps, err := s.Who(t.Context(), Caller{Session: sess}, &WhoQuery{})
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]Peer{}
		for _, p := range ps {
			byName[p.Name] = p
		}
		if len(byName) != 4 {
			t.Fatalf("from %s: %d identities, want 4", sess, len(byName))
		}
		docs, tool := byName["docs-writer"], byName["toolbelt-dev"]
		if want := []PeerReservation{{Expires: testNow.Add(time.Hour), Path: guideMD}}; !slices.Equal(docs.Reservations, want) {
			t.Errorf("from %s: docs-writer holds %+v, want %+v", sess, docs.Reservations, want)
		}
		if want := []PeerQueued{{Path: engineGo, Place: 1}}; !slices.Equal(docs.Queued, want) {
			t.Errorf("from %s: docs-writer queues %+v, want %+v", sess, docs.Queued, want)
		}
		if !slices.Equal(docs.Follows, []string{repoWTK}) {
			t.Errorf("from %s: docs-writer follows %v, want [%s]", sess, docs.Follows, repoWTK)
		}
		if want := []PeerReservation{{Expires: testNow.Add(time.Hour), Path: engineGo, Queue: 1}}; !slices.Equal(tool.Reservations, want) {
			t.Errorf("from %s: toolbelt-dev holds %+v, want %+v", sess, tool.Reservations, want)
		}
		if tool.Description != "adding the asset chooser" || !slices.Equal(tool.Paths, []string{repoToolbelt}) || tool.Host != "host-a/app" {
			t.Errorf("from %s: toolbelt-dev %+v, want its description, paths and host", sess, tool.Identity)
		}
	}
}

func TestWho_filters(t *testing.T) {
	s := whoFixture(t)
	for _, tc := range []struct {
		name string
		q    WhoQuery
		want []string
	}{
		{"the repo exactly", WhoQuery{Repo: "web-terminal-kiro"}, []string{"wtk-server", "wtk-ui"}},
		{"a repo name prefix is no repo", WhoQuery{Repo: "web-terminal"}, nil},
		{"the holder of a file", WhoQuery{Holds: engineGo}, []string{"toolbelt-dev"}},
		{"the holder of its folder", WhoQuery{Holds: repoToolbelt}, []string{"toolbelt-dev"}},
		{"the waiter", WhoQuery{Queued: engineGo}, []string{"docs-writer"}},
		{"repo and text", WhoQuery{Repo: "web-terminal-kiro", Text: "PTY"}, []string{"wtk-server"}},
		{"a path under work paths and a follow", WhoQuery{Path: repoWTK + "/src"}, []string{"docs-writer", "wtk-server", "wtk-ui"}},
		{"online", WhoQuery{Online: func(id *Identity) bool { return id.Name == "wtk-ui" }}, []string{"wtk-ui"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := whoNames(t, s, tc.q); !slices.Equal(got, tc.want) {
				t.Errorf("who %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWho_textFindsAParentByItsDelegate(t *testing.T) {
	s := whoFixture(t)
	mustRegisterDelegate(t, s, sess("toolbelt-dev"), mustGrant(t, s, grantFrom("toolbelt-dev", "audit")).Link)
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"AUDIT", []string{"toolbelt-dev"}},
		{"review the diff", []string{"toolbelt-dev"}},
		{"chooser", []string{"toolbelt-dev"}},
		{"no such work", nil},
	} {
		ps, err := s.Who(t.Context(), Caller{Session: sess("docs-writer")}, &WhoQuery{Text: tc.text})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for i := range ps {
			got = append(got, ps[i].Name)
			if ps[i].Name == "toolbelt-dev" && (len(ps[i].Delegates) != 1 || ps[i].Delegates[0].Name != "toolbelt-dev/audit") {
				t.Errorf("q=%s: toolbelt-dev delegates %+v, want toolbelt-dev/audit nested", tc.text, ps[i].Delegates)
			}
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("q=%s: %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestWho_refusesAFilterThatIsNotCanonical(t *testing.T) {
	s := whoFixture(t)
	for _, tc := range []struct {
		name string
		q    WhoQuery
	}{
		{"holds with ..", WhoQuery{Holds: repoToolbelt + "/../docs"}},
		{"a relative queued", WhoQuery{Queued: "toolbelt/engine.go"}},
		{"a path with a trailing slash", WhoQuery{Path: repoWTK + "/"}},
		{"a repo with a slash", WhoQuery{Repo: "cplieger/docs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Who(t.Context(), Caller{Session: sess("docs-writer")}, &tc.q)
			wantRefusal(t, err, KindInvalid, "", "/workspace")
		})
	}
}

func TestRepoCollision_anotherRemoteUnderOneRootIsRefused(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	docs := func(id, remote string) Where {
		return Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{{Repo: repoDocs, ID: hex32(id), Remote: hex32(remote)}}}
	}
	mustRegister(t, s, registration(sess("docs-a"), "docs-a", docs("1", "e"), repoDocs))

	_, err := s.Register(t.Context(), new(registration(sess("docs-b"), "docs-b", docs("2", "f"), repoDocs)))
	wantRefusal(t, err, KindConflict, "other_repository", "/workspace/docs is another repository for docs-a", "repos:")

	t.Run("the same remote elsewhere", func(t *testing.T) {
		mustRegister(t, s, registration(sess("docs-c"), "docs-c", docs("3", "e"), repoDocs))
	})
	t.Run("the other remote under its alias", func(t *testing.T) {
		alias := Where{Machine: dmi("1"), Environment: hex32("a"), Checkouts: []Checkout{{Repo: "/workspace/team-docs", ID: hex32("2"), Remote: hex32("f")}}}
		mustRegister(t, s, registration(sess("docs-b"), "docs-b", alias, "/workspace/team-docs"))
	})
	t.Run("a set moving onto the root", func(t *testing.T) {
		w := docs("4", "f")
		_, err := s.Set(t.Context(), Caller{Session: sess("docs-b")}, Change{Where: &w, Fields: Fields{Paths: []string{repoDocs}}})
		wantRefusal(t, err, KindConflict, "other_repository", "another repository")
	})
	t.Run("a revive onto the root", func(t *testing.T) {
		r := registration(sess("docs-d"), "docs-d", docs("5", "e"), repoDocs)
		r.TTL = 15 * time.Minute
		mustRegister(t, s, r)
		clk.advance(15 * time.Minute)
		w := docs("6", "f")
		_, err := s.Revive(t.Context(), &Revival{Session: sess("docs-d"), Where: &w, TTL: time.Hour})
		wantRefusal(t, err, KindConflict, "other_repository", "another repository")
	})
}
