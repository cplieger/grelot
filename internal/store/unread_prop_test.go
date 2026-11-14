//go:build !windows

package store

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/bolttable"
)

// TestUnreadSummary_matchesAScanAfterEveryStep applies seeded random
// posts, some opening with an @name, read-notified posts, delegates, acks,
// clock moves, sweeps, reservations, registrations and retirements, and
// after every step compares each identity's stored summary, and what its
// readers see, with a scan of its delivery rows. The seeds together must
// write every delivery kind, a delegate's copy and a quiet path post,
// checked only when -run selects them all.
func TestUnreadSummary_matchesAScanAfterEveryStep(t *testing.T) {
	const seeds = 40
	seen := map[string]bool{}
	ran := 0
	for seed := range uint64(seeds) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			ran++
			runSummarySequence(t, seed, 160, seen)
		})
	}
	if ran < seeds {
		return
	}
	for _, kind := range []string{
		kindDirect, kindPath, kindAnnounce, kindJoin, kindLeft, kindIdentityExpiring,
		kindReservationGranted, kindReservationExpiring, kindReservationExpired,
		kindReceiptsComplete, kindReceiptsDue, "copy", "quiet",
	} {
		if !seen[kind] {
			t.Errorf("no sequence wrote a %s delivery", kind)
		}
	}
}

var summaryNames = [...]string{"ochre", "umber", "sienna", "teal", "slate", "amber"}

var summaryRepos = [...]string{repoSubflux, repoGrelot, repoDocs, repoToolbelt}

func runSummarySequence(t *testing.T, seed uint64, steps int, seen map[string]bool) {
	clk := &clock{t: testNow}
	s, err := Open(filepath.Join(t.TempDir(), "grelot.db"), &Options{
		Logger: slog.New(newLogSink()), Now: clk.now,
		Retention: retainFor(25 * time.Minute), DormantTTL: 40 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := rand.New(rand.NewPCG(seed, 0x5eed))
	for _, n := range summaryNames {
		summaryRegister(t, s, r, n)
	}
	for step := range steps {
		op := summaryStep(t, s, clk, r)
		if t.Failed() {
			return
		}
		recordKinds(t, s, seen)
		checkSummaries(t, s, clk.now(), fmt.Sprintf("seed %d step %d (%s)", seed, step, op))
		if t.Failed() {
			return
		}
	}
}

func summaryStep(t *testing.T, s *Store, clk *clock, r *rand.Rand) string {
	t.Helper()
	name := summaryNames[r.IntN(len(summaryNames))]
	c := Caller{Session: sess(name)}
	var err error
	var op string
	switch k := r.IntN(17); {
	case k < 4:
		op = "direct"
		_, _, err = s.Post(t.Context(), Keyed{}, new(posting(c.Session, Target{Agent: summaryNames[r.IntN(len(summaryNames))]})))
	case k < 6:
		op = "path post"
		p := posting(c.Session, Target{Path: summaryRepos[r.IntN(len(summaryRepos))]})
		if r.IntN(2) == 0 {
			p.Body = "@" + summaryNames[r.IntN(len(summaryNames))] + ": body"
		}
		_, _, err = s.Post(t.Context(), Keyed{}, &p)
	case k == 6:
		op = "announce"
		_, _, err = s.Post(t.Context(), Keyed{}, new(posting(c.Session, Target{Announce: true})))
	case k < 9:
		op = "ack"
		_, _, err = s.Ack(t.Context(), Keyed{}, c, r.Uint64N(60))
	case k == 9:
		op = "clock"
		clk.advance(time.Duration(1+r.IntN(9)) * time.Minute)
	case k == 10:
		op = "sweep"
		_, err = s.sweep(t.Context())
	case k == 11:
		op = "reserve"
		_, _, err = s.Reserve(t.Context(), Keyed{}, &Reserving{
			Caller: c, Path: summaryRepos[r.IntN(len(summaryRepos))], Reason: "editing",
			TTL: time.Duration(1+r.IntN(12)) * time.Minute, Queue: r.IntN(2) == 0,
		})
	case k == 12:
		op = "retire"
		_, _, err = s.Retire(t.Context(), Keyed{}, c)
	case k == 13:
		op = "register"
		summaryRegister(t, s, r, name)
	case k == 14:
		op = "delegate"
		err = summaryDelegate(t, s, r, name)
	default:
		op = "watched post"
		_, _, err = s.Post(t.Context(), Keyed{}, new(watchedPost(name, Target{Agent: summaryNames[r.IntN(len(summaryNames))]},
			NotifyRead{All: true, By: time.Duration(1+r.IntN(20)) * time.Minute})))
	}
	if se := (*Error)(nil); err != nil && !errors.As(err, &se) {
		t.Fatalf("%s as %s: %v", op, name, err)
	}
	return op
}

// summaryRegister registers name when it has no identity, on one or two
// repos with a short lease, so identities expire, go dormant and purge.
func summaryRegister(t *testing.T, s *Store, r *rand.Rand, name string) {
	t.Helper()
	paths := []string{summaryRepos[r.IntN(len(summaryRepos))]}
	if p := summaryRepos[r.IntN(len(summaryRepos))]; p != paths[0] {
		paths = append(paths, p)
	}
	reg := registration(sess(name), name, envA(), paths...)
	reg.TTL = time.Duration(15+r.IntN(30)) * time.Minute
	_, _, err := s.Register(t.Context(), Keyed{}, &reg)
	if se := (*Error)(nil); err != nil && !errors.As(err, &se) {
		t.Fatalf("Register(%s): %v", name, err)
	}
}

// summaryDelegate grants and registers a delegate of name, which posts to
// a path thread, so name receives a copy unless it is on that path.
func summaryDelegate(t *testing.T, s *Store, r *rand.Rand, name string) error {
	t.Helper()
	g, _, err := s.Delegate(t.Context(), Keyed{}, &Grant{
		Caller: Caller{Session: sess(name), Self: true}, Label: fmt.Sprintf("d%d", r.IntN(3)),
		AllowThreads: summaryRepos[:], TTL: 30 * time.Minute,
	})
	if err != nil {
		return err
	}
	if _, _, err := s.Register(t.Context(), Keyed{}, new(delegateReg(sess(name), g.Link))); err != nil {
		return err
	}
	p := posting(sess(name), Target{Path: summaryRepos[r.IntN(len(summaryRepos))]})
	p.Caller = as(sess(name), g.Handle)
	_, _, err = s.Post(t.Context(), Keyed{}, &p)
	return err
}

// recordKinds adds the kind of every delivery row in s to seen, and "copy"
// for a delegate's copy.
func recordKinds(t *testing.T, s *Store, seen map[string]bool) {
	t.Helper()
	err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		return s.t.deliveries.table.Scan(r, bolttable.Range{}, func(_ []byte, d *deliveryRow) (bool, error) {
			seen[d.Kind] = true
			if d.Via != "" {
				seen["copy"] = true
			}
			if d.Quiet {
				seen["quiet"] = true
			}
			return true, nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// scanned is an identity's delivery rows past read, folded by the test.
// renudges is whether one of them is mail other than an identity warning
// that wakes: not a copy, a card, a quiet path post or identity_expiring.
type scanned struct {
	oldest, minExpiry time.Time
	counts            Counts
	newest            uint64
	rows              int
	renudges          bool
}

func scanPast(t *testing.T, s *Store, r *bolttable.ReadTx, row *identityRow, keep func(*deliveryRow) bool) scanned {
	t.Helper()
	var out scanned
	err := s.t.deliveries.table.Scan(r, bolttable.Range{Prefix: be64(row.ID), From: pairKey(row.ID, row.Read+1)},
		func(_ []byte, d *deliveryRow) (bool, error) {
			if !keep(d) {
				return true, nil
			}
			out.rows++
			out.counts.add(d)
			wakes := d.Via == "" && !d.Quiet && d.Kind != kindJoin && d.Kind != kindLeft
			if wakes {
				if out.newest == 0 {
					out.oldest = d.Written
				}
				out.newest = d.Seq
			}
			if wakes && d.Kind != kindIdentityExpiring {
				out.renudges = true
			}
			if e := d.Lease.Expires; !e.IsZero() && (out.minExpiry.IsZero() || e.Before(out.minExpiry)) {
				out.minExpiry = e
			}
			return true, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func checkSummaries(t *testing.T, s *Store, now time.Time, at string) {
	t.Helper()
	err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		return s.t.identities.table.Scan(r, bolttable.Range{}, func(id uint64, row *identityRow) (bool, error) {
			all := scanPast(t, s, r, row, func(*deliveryRow) bool { return true })
			u := row.Unread
			if u.Counts != all.counts || u.Newest != all.newest || !u.Oldest.Equal(all.oldest) {
				t.Errorf("%s: identity %d stored %+v newest %d oldest %v, scan %+v newest %d oldest %v",
					at, id, u.Counts, u.Newest, u.Oldest, all.counts, all.newest, all.oldest)
			}
			if !all.minExpiry.IsZero() && (u.Due.IsZero() || u.Due.After(all.minExpiry)) {
				t.Errorf("%s: identity %d due %v, want at or before the earliest expiry %v", at, id, u.Due, all.minExpiry)
			}
			live := scanPast(t, s, r, row, func(d *deliveryRow) bool { return !d.Lease.due(now) })
			got, err := s.pastRead(r, row, now)
			if err != nil {
				return false, err
			}
			b := got.wake()
			if got.Counts != live.counts || b.newest != live.newest || !b.oldest.Equal(live.oldest) ||
				b.count != live.counts.Wake() || b.renudges != live.renudges {
				t.Errorf("%s: identity %d reads %+v batch %+v, unexpired scan %+v newest %d oldest %v",
					at, id, got.Counts, b, live.counts, live.newest, live.oldest)
			}
			return !t.Failed(), nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}
