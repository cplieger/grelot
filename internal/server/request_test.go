//go:build !windows

package server

import (
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// overflowMS is an _ms value whose product with time.Millisecond is past
// int64: 2^64 ns plus 448,384 ns, so a wrapping multiply reads 448 µs.
const overflowMS = 18446744073710

func TestDurationMS(t *testing.T) {
	limit := math.MaxInt64 / int64(time.Millisecond)
	for _, tc := range []struct {
		ms   int64
		want time.Duration
	}{
		{0, 0},
		{1500, 1500 * time.Millisecond},
		{-1500, -1500 * time.Millisecond},
		{limit, time.Duration(limit) * time.Millisecond},
		{-limit, -time.Duration(limit) * time.Millisecond},
		{limit + 1, math.MaxInt64},
		{overflowMS, math.MaxInt64},
		{math.MaxInt64, math.MaxInt64},
		{-limit - 1, math.MinInt64},
		{-overflowMS, math.MinInt64},
		{math.MinInt64, math.MinInt64},
	} {
		if got := durationMS(tc.ms); got != tc.want {
			t.Errorf("durationMS(%d) = %d, want %d", tc.ms, got, tc.want)
		}
	}
}

// refusedField reports whether r is a 400 invalid whose one field error
// names field as out_of_range.
func refusedField(r *response, field string) bool {
	if r.status != http.StatusBadRequest || r.str("code") != "invalid" {
		return false
	}
	fields, _ := r.body["fields"].([]any)
	if len(fields) != 1 {
		return false
	}
	f, _ := fields[0].(map[string]any)
	return f["field"] == field && f["code"] == "out_of_range"
}

func TestDurations_refuseAnOverflowingMS(t *testing.T) {
	h := newHarness(t)
	h.mustRegister(sessA, "reader")
	hourPast := int64(overflowMS + time.Hour/time.Millisecond)
	t.Run("register ttl_ms", func(t *testing.T) {
		b := registerBody("writer", "6h")
		b["ttl_ms"] = hourPast
		if r := h.as(sessB, http.MethodPost, "/v1/register", b); !refusedField(r, "ttl_ms") {
			t.Errorf("got %d %s, want 400 invalid naming ttl_ms out_of_range", r.status, r.raw)
		}
	})
	t.Run("patchMe ttl_ms", func(t *testing.T) {
		b := map[string]any{"ttl_ms": hourPast, "where": where("a")}
		if r := h.as(sessA, http.MethodPatch, "/v1/me", b); !refusedField(r, "ttl_ms") {
			t.Errorf("got %d %s, want 400 invalid naming ttl_ms out_of_range", r.status, r.raw)
		}
	})
	t.Run("inbox wait_ms", func(t *testing.T) {
		r := h.as(sessA, http.MethodGet, "/v1/inbox?wait_ms="+strconv.FormatInt(overflowMS, 10), nil)
		if !refusedField(r, "wait_ms") {
			t.Errorf("got %d %s, want 400 invalid naming wait_ms out_of_range", r.status, r.raw)
		}
	})
	t.Run("standing wait wait_ms", func(t *testing.T) {
		r := h.self(sessA, http.MethodPost, "/v1/events/wait", map[string]any{"wait_ms": int64(overflowMS)})
		if !refusedField(r, "wait_ms") {
			t.Errorf("got %d %s, want 400 invalid naming wait_ms out_of_range", r.status, r.raw)
		}
	})
}
