package cli

import (
	"errors"
	"testing"
	"time"
)

func TestDurationMS(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{{"", 0}, {"2h", 7_200_000}, {"90m", 5_400_000}, {"1500ms", 1500}} {
		if got, err := durationMS("renew", "ttl", tc.in); err != nil || got != tc.want {
			t.Errorf("durationMS(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"soon", "2d", "-1h"} {
		var ue *usageError
		if _, err := durationMS("renew", "ttl", bad); !errors.As(err, &ue) {
			t.Errorf("durationMS(%q) = %v, want a usage error before any request", bad, err)
		}
	}
}

func TestInstant(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ in, want string }{
		{"2h", "2026-11-05T07:00:00Z"},
		{"2d", "2026-11-03T09:00:00Z"},
		{"0s", "2026-11-05T09:00:00Z"},
		{"2026-11-01T12:30:00+01:00", "2026-11-01T12:30:00+01:00"},
	} {
		if got, err := instant("history", "since", tc.in, now); err != nil || got != tc.want {
			t.Errorf("instant(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"soon", "-2h", "2026-11-01", "99999d"} {
		var ue *usageError
		if _, err := instant("history", "since", bad, now); !errors.As(err, &ue) {
			t.Errorf("instant(%q) = %v, want a usage error before any request", bad, err)
		}
	}
}
