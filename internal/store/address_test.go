//go:build !windows

package store

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestMentions(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"space-separated names before a colon", "@alpha @bravo: text", []string{"alpha", "bravo"}},
		{"commas and and", "@alpha, @bravo and @charlie: text", []string{"alpha", "bravo", "charlie"}},
		{"one name with no colon", "@alpha text", []string{"alpha"}},
		{"a delegate handle", "@ui-design-research/gaps-v1: fits", []string{"ui-design-research/gaps-v1"}},
		{"leading space and new lines", "\n  @alpha\n@bravo\ttext", []string{"alpha", "bravo"}},
		{"a comma with no space", "@alpha,@bravo text", []string{"alpha", "bravo"}},
		{"a repeated name once", "@alpha @bravo @alpha: text", []string{"alpha", "bravo"}},
		{"the whole body", "@alpha", []string{"alpha"}},
		{"a trailing and", "@alpha and", []string{"alpha"}},
		{"and before a word ends the run", "@alpha and bravo", []string{"alpha"}},
		{"a colon ends the run", "@alpha: @bravo text", []string{"alpha"}},
		{"a word ends the run", "@alpha thanks @bravo", []string{"alpha"}},
		{"a mention later in the body", "thanks @alpha", nil},
		{"and before any mention", "and @alpha", nil},
		{"an empty body", "", nil},
		{"a bare at sign", "@ alpha", nil},
		{"an upper-case name", "@Alpha text", nil},
		{"a one-letter name", "@a text", nil},
		{"a handle with two slashes", "@alpha/b1/c2 text", nil},
		{"a colon glued to the next word", "@alpha:text", nil},
		{"an email address", "alpha@example.com: text", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mentions(tc.body); !slices.Equal(got, tc.want) {
				t.Errorf("mentions(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// addressed is a post from name to path p with body.
func addressed(name, p, body string) Posting {
	pp := posting(sess(name), Target{Path: p})
	pp.Body = body
	return pp
}

// quietOf maps each of ids to whether its one delivery of msg is quiet,
// failing the test for an identity with none.
func quietOf(t *testing.T, s *Store, msg uint64, ids map[string]uint64) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for name, id := range ids {
		i := slices.IndexFunc(deliveriesTo(t, s, id), func(d deliveryRow) bool { return d.Message == msg })
		if i < 0 {
			t.Errorf("%s has no delivery of message %d", name, msg)
			continue
		}
		out[name] = deliveriesTo(t, s, id)[i].Quiet
	}
	return out
}

func wantQuiet(t *testing.T, got, want map[string]bool) {
	t.Helper()
	for name, q := range want {
		if got[name] != q {
			t.Errorf("%s quiet %v, want %v", name, got[name], q)
		}
	}
}

func TestPost_anAddressedPathPostWakesOnlyItsNames(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha", repoSubflux)
	b := regAt(t, s, "bravo", repoSubflux)
	c := regAt(t, s, "charlie", repoSubflux)
	d := regAt(t, s, "delta", repoSubflux)
	got := mustPost(t, s, addressed("alpha", repoSubflux, "@bravo: the fix is on dev"))

	t.Run("the answer splits the recipients", func(t *testing.T) {
		if got.Delivered != 3 || !slices.Equal(got.Woken, []string{"bravo"}) || got.Quiet != 2 {
			t.Errorf("delivered %d woken %q quiet %d, want 3, [bravo], 2", got.Delivered, got.Woken, got.Quiet)
		}
	})
	t.Run("only the named recipient's delivery wakes", func(t *testing.T) {
		wantQuiet(t, quietOf(t, s, got.Message, map[string]uint64{"bravo": b.ID, "charlie": c.ID, "delta": d.ID}),
			map[string]bool{"bravo": false, "charlie": true, "delta": true})
	})
	t.Run("a quiet recipient reads it in its inbox, counted apart and not waking", func(t *testing.T) {
		p := mustInbox(t, s, sess("charlie"), 0)
		i := slices.IndexFunc(p.Entries, func(e Entry) bool { return e.Message == got.Message })
		if i < 0 {
			t.Fatalf("charlie's inbox %+v has no message %d", p.Entries, got.Message)
		}
		if e := p.Entries[i]; e.Wake || e.Kind != kindPath || e.Body != "@bravo: the fix is on dev" {
			t.Errorf("entry %+v, want the quiet path post", e)
		}
		if p.Unread.QuietPath != 1 || p.Unread.Path != 0 || p.Unread.Wake() != 0 || p.Unread.Total() != total(p.Unread) {
			t.Errorf("unread %+v, want one quiet path delivery, no wake, counted in the total", p.Unread)
		}
	})
	t.Run("the named recipient's counts wake", func(t *testing.T) {
		if u := mustInbox(t, s, sess("bravo"), 0).Unread; u.Path != 1 || u.QuietPath != 0 || u.Wake() != 1 {
			t.Errorf("bravo unread %+v, want one waking path delivery", u)
		}
	})
}

func TestPost_aQuietDeliveryIsNeitherNudgedNorHeld(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha", repoSubflux)
	b := regAt(t, s, "bravo", repoSubflux)
	c := regAt(t, s, "charlie", repoSubflux)
	mustPost(t, s, addressed("alpha", repoSubflux, "@bravo: the fix is on dev"))

	t.Run("no push or watcher nudge is due for it", func(t *testing.T) {
		wantNoNudge(t, nudgeOf(t, s, c.ID))
	})
	t.Run("the named recipient is nudged", func(t *testing.T) {
		if st := nudgeOf(t, s, b.ID); st.Due == nil || st.Due.Count != 1 {
			t.Errorf("bravo's nudge %+v, want one due over its delivery", st.Due)
		}
	})
	t.Run("a hook's hold shows no wake and moves nothing", func(t *testing.T) {
		h, err := s.Hold(t.Context(), plain("charlie"))
		if err != nil {
			t.Fatal(err)
		}
		if h.Counts.Wake() != 0 || h.Counts.QuietPath != 1 || h.Shown != 0 {
			t.Errorf("hold %+v, want the quiet delivery counted, no wake and shown left at 0", h)
		}
		if row := getIdentity(t, s, c.ID); row.Nudged != 0 || row.Shown != 0 {
			t.Errorf("nudged %d shown %d, want the hold to record no nudge", row.Nudged, row.Shown)
		}
	})
}

func TestPost_aNamedIdentityOutsideTheThreadIsWoken(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha", repoSubflux)
	b := regAt(t, s, "bravo", repoSubflux)
	e := regAt(t, s, "echo", repoGrelot)
	got := mustPost(t, s, addressed("alpha", repoSubflux, "@echo and @bravo: your call"))
	if got.Delivered != 2 || !slices.Equal(got.Woken, []string{"bravo", "echo"}) || got.Quiet != 0 {
		t.Errorf("delivered %d woken %q quiet %d, want 2, [bravo echo], 0", got.Delivered, got.Woken, got.Quiet)
	}
	ds := deliveriesTo(t, s, e.ID)
	if len(ds) != 1 || ds[0].Kind != kindPath || ds[0].Thread != got.Thread || ds[0].Quiet || ds[0].Path != "" {
		t.Errorf("echo's deliveries %+v, want the path post, waking, matched by no path", ds)
	}
	if ds := deliveriesTo(t, s, b.ID); len(ds) != 1 || ds[0].Quiet {
		t.Errorf("bravo's deliveries %+v, want one waking", ds)
	}
}

func TestPost_aPathPostNamingNoIdentityWakesEveryRecipient(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no mention", "the fix is on dev"},
		{"a mention later in the body", "the fix is on dev, thanks @bravo"},
		{"only the sender", "@alpha: note to self"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := openIdentityStore(t)
			regAt(t, s, "alpha", repoSubflux)
			b := regAt(t, s, "bravo", repoSubflux)
			c := regAt(t, s, "charlie", repoSubflux)
			got := mustPost(t, s, addressed("alpha", repoSubflux, tc.body))
			if got.Delivered != 2 || got.Woken != nil || got.Quiet != 0 || len(got.Notices) != 0 {
				t.Errorf("delivered %d woken %q quiet %d notices %q, want 2, none named, 0 and no notice",
					got.Delivered, got.Woken, got.Quiet, got.Notices)
			}
			wantQuiet(t, quietOf(t, s, got.Message, map[string]uint64{"bravo": b.ID, "charlie": c.ID}),
				map[string]bool{"bravo": false, "charlie": false})
		})
	}
}

func TestPost_anUnknownNameBesideAKnownOneIsReported(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha", repoSubflux)
	b := regAt(t, s, "bravo", repoSubflux)
	c := regAt(t, s, "charlie", repoSubflux)
	got := mustPost(t, s, addressed("alpha", repoSubflux, "@nobody-here @bravo: the fix is on dev"))
	want := []string{"@nobody-here is not registered; nobody of that name gets this"}
	if !slices.Equal(got.Woken, []string{"bravo"}) || got.Quiet != 1 || !slices.Equal(got.Notices, want) {
		t.Errorf("woken %q quiet %d notices %q, want [bravo], 1 and %q", got.Woken, got.Quiet, got.Notices, want)
	}
	wantQuiet(t, quietOf(t, s, got.Message, map[string]uint64{"bravo": b.ID, "charlie": c.ID}),
		map[string]bool{"bravo": false, "charlie": true})
}

func TestPost_onlyUnknownNamesAreReportedAndWakeEveryRecipient(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha", repoSubflux)
	b := regAt(t, s, "bravo", repoSubflux)
	c := regAt(t, s, "charlie", repoSubflux)
	got := mustPost(t, s, addressed("alpha", repoSubflux, "@nobody-here and @gone-dev: the fix is on dev"))
	want := []string{
		"@nobody-here is not registered; nobody of that name gets this",
		"@gone-dev is not registered; nobody of that name gets this",
		"no name it opens with is registered, so it woke every follower",
	}
	if !slices.Equal(got.Notices, want) {
		t.Errorf("notices %q, want %q", got.Notices, want)
	}
	if got.Delivered != 2 || got.Woken != nil || got.Quiet != 0 {
		t.Errorf("delivered %d woken %q quiet %d, want 2, none named, 0", got.Delivered, got.Woken, got.Quiet)
	}
	wantQuiet(t, quietOf(t, s, got.Message, map[string]uint64{"bravo": b.ID, "charlie": c.ID}),
		map[string]bool{"bravo": false, "charlie": false})
}

func TestPost_anUnknownDelegateHandleIsReported(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	delegateOf(t, s, "bravo", "rev")
	regAt(t, s, "alpha", repoSubflux)
	c := regAt(t, s, "charlie", repoSubflux)
	got := mustPost(t, s, addressed("alpha", repoSubflux, "@bravo/gone @charlie: check the diff"))
	want := []string{"@bravo/gone is not registered; nobody of that name gets this"}
	if !slices.Equal(got.Notices, want) || !slices.Equal(got.Woken, []string{"charlie"}) {
		t.Errorf("notices %q woken %q, want %q and [charlie]", got.Notices, got.Woken, want)
	}
	wantQuiet(t, quietOf(t, s, got.Message, map[string]uint64{"charlie": c.ID}), map[string]bool{"charlie": false})
}

func TestPost_announceAndDirectPostsIgnoreMentions(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	regAt(t, s, "alpha", repoSubflux)
	b := regAt(t, s, "bravo", repoSubflux)
	c := regAt(t, s, "charlie", repoSubflux)
	for name, to := range map[string]Target{"announce": {Announce: true}, "direct": {Agent: "charlie"}} {
		t.Run(name, func(t *testing.T) {
			p := posting(sess("alpha"), to)
			p.Body = "@bravo: read this"
			got := mustPost(t, s, p)
			if got.Woken != nil || got.Quiet != 0 {
				t.Errorf("woken %q quiet %d, want none named and 0 quiet", got.Woken, got.Quiet)
			}
			ids := map[string]uint64{"charlie": c.ID}
			if to.Announce {
				ids["bravo"] = b.ID
			}
			wantQuiet(t, quietOf(t, s, got.Message, ids), map[string]bool{"bravo": false, "charlie": false})
			if to.Agent != "" && slices.ContainsFunc(deliveriesTo(t, s, b.ID), func(d deliveryRow) bool { return d.Message == got.Message }) {
				t.Errorf("bravo, named in a direct post to charlie, received it")
			}
		})
	}
}

func TestPost_aNamedDormantIdentityGetsItAndTheSenderIsTold(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	regAt(t, s, "alpha", repoSubflux)
	c := regAt(t, s, "charlie", repoSubflux)
	rb := registration(sessB, "bravo", envA(), repoGrelot)
	rb.TTL = 15 * time.Minute
	b := mustRegister(t, s, rb)
	clk.advance(20 * time.Minute)
	mustSweep(t, s)
	got := mustPost(t, s, addressed("alpha", repoSubflux, "@bravo: when you are back"))
	want := "@bravo is dormant since 2026-11-04 05:54 UTC; it gets this if it revives before 2026-11-07 05:54 UTC"
	if !slices.Equal(got.Notices, []string{want}) || !slices.Equal(got.Woken, []string{"bravo"}) || got.Quiet != 1 {
		t.Errorf("notices %q woken %q quiet %d, want %q, [bravo], 1", got.Notices, got.Woken, got.Quiet, want)
	}
	wantQuiet(t, quietOf(t, s, got.Message, map[string]uint64{"bravo": b.ID, "charlie": c.ID}),
		map[string]bool{"bravo": false, "charlie": true})
}

func TestPost_aDelegateHandleIsNamedAsADirectPostReachesIt(t *testing.T) {
	t.Run("its parent wakes it", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		p, d := delegateOf(t, s, "bravo", "rev")
		c := regAt(t, s, "charlie", repoSubflux)
		pp := addressed("bravo", repoSubflux, "@bravo/rev: check the diff")
		pp.Caller = self("bravo")
		got := mustPost(t, s, pp)
		if !slices.Equal(got.Woken, []string{"bravo/rev"}) || got.Quiet != 1 {
			t.Errorf("woken %q quiet %d, want [bravo/rev], 1", got.Woken, got.Quiet)
		}
		wantQuiet(t, quietOf(t, s, got.Message, map[string]uint64{"bravo/rev": d.ID, "charlie": c.ID}),
			map[string]bool{"bravo/rev": false, "charlie": true})
		if ds := deliveriesTo(t, s, p.ID); len(ds) != 0 {
			t.Errorf("the sending parent got %+v, want no copy of its own post", ds)
		}
	})
	t.Run("an identity its scope leaves out reaches it not and wakes everyone", func(t *testing.T) {
		s, _, _ := openIdentityStore(t)
		_, d := delegateOf(t, s, "bravo", "rev")
		regAt(t, s, "alpha", repoSubflux)
		c := regAt(t, s, "charlie", repoSubflux)
		got := mustPost(t, s, addressed("alpha", repoSubflux, "@bravo/rev: check the diff"))
		if got.Woken != nil || got.Quiet != 0 || len(got.Notices) != 0 {
			t.Errorf("woken %q quiet %d notices %q, want none named, 0 quiet and no notice", got.Woken, got.Quiet, got.Notices)
		}
		if ds := deliveriesTo(t, s, d.ID); len(ds) != 0 {
			t.Errorf("the delegate got %+v outside its scope", ds)
		}
		wantQuiet(t, quietOf(t, s, got.Message, map[string]uint64{"charlie": c.ID}), map[string]bool{"charlie": false})
	})
}

func TestPost_aDelegateSenderWakesOnlyTheNamesItsScopeReaches(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	parent := regAt(t, s, "subflux-dev", repoGrelot)
	e := regAt(t, s, "echo", repoGrelot)
	c := regAt(t, s, "charlie", repoSubflux)
	d := scopedDelegate(t, s, "subflux-dev", "review", func(g *Grant) { g.AllowThreads = []string{repoSubflux} })
	post := func(body string) Posted {
		p := asPost("subflux-dev", d.Name, Target{Path: repoSubflux})
		p.Body = body
		return mustPost(t, s, p)
	}

	t.Run("a name outside its scope reaches it not and wakes everyone", func(t *testing.T) {
		got := post("@echo: check this")
		if got.Woken != nil || got.Quiet != 0 || len(got.Notices) != 0 {
			t.Errorf("woken %q quiet %d notices %q, want none named, 0 quiet and no notice", got.Woken, got.Quiet, got.Notices)
		}
		if ds := deliveriesTo(t, s, e.ID); len(ds) != 0 {
			t.Errorf("echo got %+v outside the delegate's scope", ds)
		}
		wantQuiet(t, quietOf(t, s, got.Message, map[string]uint64{"charlie": c.ID}), map[string]bool{"charlie": false})
	})
	t.Run("its parent, which its scope always reaches, is woken", func(t *testing.T) {
		got := post("@subflux-dev: done")
		if !slices.Equal(got.Woken, []string{"subflux-dev"}) || got.Quiet != 1 {
			t.Errorf("woken %q quiet %d, want [subflux-dev], 1", got.Woken, got.Quiet)
		}
		wantQuiet(t, quietOf(t, s, got.Message, map[string]uint64{"subflux-dev": parent.ID, "charlie": c.ID}),
			map[string]bool{"subflux-dev": false, "charlie": true})
	})
}

func TestPost_readReceiptsCountQuietRecipients(t *testing.T) {
	s, _, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha", repoSubflux)
	regAt(t, s, "bravo", repoSubflux)
	regAt(t, s, "charlie", repoSubflux)
	p := addressed("alpha", repoSubflux, "@bravo: the fix is on dev")
	p.NotifyRead = NotifyRead{All: true}
	msg := mustPost(t, s, p).Message

	wantStates(t, mustReceipts(t, s, plain("alpha"), msg), "bravo unread", "charlie unread")
	readAll(t, s, "bravo")
	t.Run("the quiet recipient still owes its read", func(t *testing.T) {
		wantEvents(t, s, sender.ID, kindReceiptsComplete)
	})
	readAll(t, s, "charlie")
	t.Run("its read is recorded and completes the receipts", func(t *testing.T) {
		wantStates(t, mustReceipts(t, s, plain("alpha"), msg), "bravo read", "charlie read")
		wantEvents(t, s, sender.ID, kindReceiptsComplete, fmt.Sprintf("message %d: all 2 have read %s", msg, subject))
	})
}

func TestPost_aDeadlineNamesAQuietRecipientStillUnread(t *testing.T) {
	s, clk, _ := openIdentityStore(t)
	sender := regAt(t, s, "alpha")
	regAt(t, s, "bravo", repoSubflux)
	regAt(t, s, "charlie", repoSubflux)
	p := watchedPost("alpha", Target{Path: repoSubflux}, NotifyRead{Names: []string{"charlie"}, By: time.Hour})
	p.Body = "@bravo: the fix is on dev"
	msg := mustPost(t, s, p).Message
	clk.advance(time.Hour)
	mustSweep(t, s)
	wantEvents(t, s, sender.ID, kindReceiptsDue,
		fmt.Sprintf("message %d: 0 of 1 have read %s by the deadline; still unread: @charlie", msg, subject))
}
