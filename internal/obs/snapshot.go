package obs

import (
	"context"
	"time"

	"github.com/cplieger/go/measure"
)

// Snapshot is the page's state and process gauges at one gather. State is
// false when the store could not count, which leaves the store's fields 0.
type Snapshot struct {
	OldestUnread time.Duration
	InUse        int64
	MaxBytes     int64
	Heap         int64
	Resident     int64
	Live         int
	Dormant      int
	Online       int
	Unread       int
	Held         int
	Queued       int
	Goroutines   int
	State        bool
}

// Snapshot gathers the page and reads its gauges, so the Statistics tab
// shows the numbers a scrape would. A gauge the platform does not report
// reads 0.
func (m *Metrics) Snapshot(ctx context.Context) (Snapshot, error) {
	fs, err := m.reg.Gather(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	var out Snapshot
	for i := range fs {
		f := &fs[i]
		switch f.Name {
		case "grelot_identities":
			out.State = len(f.Samples) > 0
			out.Live, out.Dormant = int(labelled(f, "live")), int(labelled(f, "dormant"))
		case "grelot_identities_online":
			out.Online = int(only(f))
		case "grelot_unread_backlog":
			out.Unread = int(only(f))
		case "grelot_unread_oldest_seconds":
			out.OldestUnread = time.Duration(only(f) * float64(time.Second))
		case "grelot_reservations":
			out.Held, out.Queued = int(labelled(f, "held")), int(labelled(f, "queued"))
		case "grelot_store_in_use_bytes":
			out.InUse = int64(only(f))
		case "grelot_store_max_bytes":
			out.MaxBytes = int64(only(f))
		case "go_memstats_heap_alloc_bytes":
			out.Heap = int64(only(f))
		case "process_resident_memory_bytes":
			out.Resident = int64(only(f))
		case "go_goroutines":
			out.Goroutines = int(only(f))
		}
	}
	return out, nil
}

func only(f *measure.Family) float64 {
	if len(f.Samples) == 0 {
		return 0
	}
	return f.Samples[0].Value
}

// labelled is the value of f's series whose one label is v.
func labelled(f *measure.Family, v string) float64 {
	for _, s := range f.Samples {
		if len(s.Labels) == 1 && s.Labels[0].Value == v {
			return s.Value
		}
	}
	return 0
}
