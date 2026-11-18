package obs

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func refusedAt(at time.Time, agent string) Event {
	return Event{At: at, Kind: EventPostRefused, Detail: RefusedRateLimited, Agent: agent}
}

func TestEvents_keepsTheNewest200NewestFirst(t *testing.T) {
	e := New().Events
	for i := range 250 {
		e.Add(refusedAt(testNow.Add(time.Duration(i)*time.Second), fmt.Sprintf("agent-%d", i)))
	}

	got := e.Recent(testNow, testNow.Add(time.Hour))

	if len(got) != 200 {
		t.Fatalf("%d events kept, want 200", len(got))
	}
	if got[0].Agent != "agent-249" || got[199].Agent != "agent-50" {
		t.Errorf("kept %s to %s, want agent-249 down to agent-50", got[0].Agent, got[199].Agent)
	}
}

func TestEvents_recentKeepsOnlyTheRange(t *testing.T) {
	e := New().Events
	e.Add(refusedAt(testNow.Add(-time.Second), "before"))
	e.Add(refusedAt(testNow, "first"))
	e.Add(refusedAt(testNow.Add(time.Minute-time.Nanosecond), "last"))
	e.Add(refusedAt(testNow.Add(time.Minute), "after"))

	got := e.Recent(testNow, testNow.Add(time.Minute))

	if len(got) != 2 || got[0].Agent != "last" || got[1].Agent != "first" {
		t.Errorf("recent %+v, want last then first", got)
	}
}

func TestEvents_countsOutliveTheRingByKindAndMinute(t *testing.T) {
	e := New().Events
	for i := range 300 {
		e.Add(Event{At: testNow.Add(time.Duration(i) * time.Second), Kind: EventPushRetry, Detail: "testhost/push"})
	}
	e.Add(Event{At: testNow.Add(2 * time.Minute), Kind: EventNudgeRepeat, Detail: "push"})
	e.Add(Event{At: testNow.Add(10 * time.Minute), Kind: EventPushGaveUp, Detail: "testhost/push"})

	got := e.Counts(testNow, testNow.Add(10*time.Minute))

	want := map[EventKind]int{EventPostRefused: 0, EventPushRetry: 300, EventPushGaveUp: 0, EventNudgeRepeat: 1}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s counted %d, want %d", k, got[k], n)
		}
	}
	if len(got) != len(want) {
		t.Errorf("counts %v, want every kind and only those", got)
	}
}

func TestEvents_countsForgetMinutesOver72HoursOld(t *testing.T) {
	e := New().Events
	e.Add(refusedAt(testNow, "old"))
	e.Add(refusedAt(testNow.Add(72*time.Hour), "new"))

	if n := e.Counts(testNow, testNow.Add(time.Minute))[EventPostRefused]; n != 0 {
		t.Errorf("a minute 72 hours old counts %d, want 0 once its slot is reused", n)
	}
	if n := e.Counts(testNow, testNow.Add(73*time.Hour))[EventPostRefused]; n != 1 {
		t.Errorf("the range counts %d, want only the new event", n)
	}
}

func TestEvents_anUnknownKindIsDropped(t *testing.T) {
	e := New().Events
	e.Add(Event{At: testNow, Kind: EventKind(99)})

	if got := e.Recent(testNow, testNow.Add(time.Minute)); len(got) != 0 {
		t.Errorf("recent %+v, want nothing", got)
	}
}

func TestEventKind_namesEachKind(t *testing.T) {
	for k, want := range map[EventKind]string{
		EventPostRefused: "post_refused", EventPushRetry: "push_retry",
		EventPushGaveUp: "push_gave_up", EventNudgeRepeat: "nudge_repeat", EventKind(99): "unknown",
	} {
		if got := k.String(); got != want {
			t.Errorf("EventKind(%d) = %q, want %q", k, got, want)
		}
	}
}

func TestSnapshot_readsTheGaugesAScrapeWould(t *testing.T) {
	m := New()
	m.ServeState(func(context.Context) (State, bool) {
		return State{
			Oldest: testNow.Add(-90 * time.Second), Live: 4, Dormant: 2, Online: 3,
			Held: 5, Queued: 1, Unread: 7, InUse: 4096, MaxBytes: 1 << 20,
		}, true
	}, func() time.Time { return testNow })

	got, err := m.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	want := Snapshot{
		State: true, Live: 4, Dormant: 2, Online: 3, Held: 5, Queued: 1, Unread: 7,
		OldestUnread: 90 * time.Second, InUse: 4096, MaxBytes: 1 << 20,
	}
	if got.Heap <= 0 || got.Goroutines <= 0 {
		t.Errorf("heap %d and goroutines %d, want the runtime's, above 0", got.Heap, got.Goroutines)
	}
	got.Heap, got.Resident, got.Goroutines = 0, 0, 0
	if got != want {
		t.Errorf("snapshot %+v, want %+v", got, want)
	}
}

func TestSnapshot_aFailedCountLeavesTheStoreOut(t *testing.T) {
	m := New()
	m.ServeState(func(context.Context) (State, bool) { return State{Live: 4}, false }, time.Now)

	got, err := m.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if got.State || got.Live != 0 {
		t.Errorf("snapshot %+v, want State false and no live count", got)
	}
}
