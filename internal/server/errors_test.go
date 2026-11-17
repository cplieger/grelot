//go:build !windows

package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/cplieger/access"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/grelot/internal/obs"
	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

// errorMembers is the members table for the agent rows' codes: the
// members each carries beyond error, code and request_id, those it must
// carry first and then those it may.
var errorMembers = map[string]struct{ required, optional []string }{
	"invalid":          {optional: []string{"fields"}},
	"cursor_invalid":   {},
	"cursor_mismatch":  {},
	"not_found":        {required: []string{"reason"}},
	"none":             {},
	"dormant":          {},
	"delegate_ended":   {},
	"link_gone":        {},
	"conflict":         {},
	"other_provider":   {},
	"forbidden":        {},
	"too_large":        {optional: []string{"fields", "reason"}},
	"key_reused":       {},
	"store_full":       {},
	"rate_limited":     {required: []string{"retry_after_ms"}},
	"too_many_waits":   {required: []string{"retry_after_ms"}},
	"timeout":          {required: []string{"retry_after_ms"}},
	"shutting_down":    {},
	"upgrade_required": {},
	"internal_error":   {},
}

func TestErrorMembers(t *testing.T) {
	s := newHarnessWith(t, store.Options{}, func(o *Options) { o.Limits.KeyWaits = 1 }).srv
	full, _ := s.waits.hold(1)
	defer full()
	cases := []struct {
		err  func() error
		code string
	}{
		{func() error {
			return fieldRefusal("limit", wire.FieldError{Field: "limit", Code: wire.FieldOutOfRange})
		}, "invalid"},
		{func() error { return &store.Error{Kind: store.KindInvalid, Msg: "no field"} }, "invalid"},
		{func() error { return errCursorInvalid }, "cursor_invalid"},
		{func() error { return errCursorMismatch }, "cursor_mismatch"},
		{func() error {
			return &store.Error{Kind: store.KindNotFound, Reason: wire.ReasonMessage, Msg: "no message"}
		}, "not_found"},
		{func() error { return &store.Error{Kind: store.KindGone, Why: "none", Msg: "none"} }, "none"},
		{func() error { return &store.Error{Kind: store.KindGone, Why: "dormant", Msg: "dormant"} }, "dormant"},
		{func() error { return &store.Error{Kind: store.KindGone, Why: "delegate_ended", Msg: "ended"} }, "delegate_ended"},
		{func() error { return &store.Error{Kind: store.KindGone, Why: "link_gone", Msg: "gone"} }, "link_gone"},
		{func() error { return &store.Error{Kind: store.KindConflict, Msg: "taken"} }, "conflict"},
		{func() error { return &store.Error{Kind: store.KindConflict, Why: "other_provider", Msg: "other"} }, "other_provider"},
		{func() error { return &store.Error{Kind: store.KindForbidden, Msg: "no"} }, "forbidden"},
		{func() error { return bodyError(&http.MaxBytesError{Limit: maxBody}) }, "too_large"},
		{func() error { return &store.Error{Kind: store.KindKeyReused, Msg: "reused"} }, "key_reused"},
		{func() error { return &store.Error{Kind: store.KindFull, Msg: "full"} }, "store_full"},
		{func() error { return s.rateLimited(1500 * time.Millisecond) }, "rate_limited"},
		{func() error {
			_, err := s.holdWait(1)
			return err
		}, "too_many_waits"},
		{func() error { return errTimeout }, "timeout"},
		{func() error { return store.ErrClosed }, "shutting_down"},
		{func() error { return refusal(wire.ErrUpgradeRequired, upgradeLine("v1.1.0", testVersion)) }, "upgrade_required"},
		{func() error { return errors.New("a bug") }, "internal_error"},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			seen[tc.code] = true
			rec := httptest.NewRecorder()
			ctx := httpserver.WithRequestID(context.Background(), "req-1")
			r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/me", http.NoBody)
			s.fail(rec, r, tc.err())
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["code"] != tc.code {
				t.Fatalf("code %v, want %s: %s", body["code"], tc.code, rec.Body)
			}
			if body["request_id"] != "req-1" {
				t.Errorf("request_id %v, want the request's req-1: %s", body["request_id"], rec.Body)
			}
			m := errorMembers[tc.code]
			for _, k := range m.required {
				if _, ok := body[k]; !ok {
					t.Errorf("no %s: %s", k, rec.Body)
				}
			}
			for k := range body {
				if k != "error" && k != "code" && k != "request_id" && !slices.Contains(m.required, k) && !slices.Contains(m.optional, k) {
					t.Errorf("carries %s, which design 1.4 does not give %s: %s", k, tc.code, rec.Body)
				}
			}
			if ms, ok := body["retry_after_ms"].(float64); ok {
				secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
				if err != nil || ms != float64(secs*1000) {
					t.Errorf("retry_after_ms %v with Retry-After %q, want whole seconds of it", ms, rec.Header().Get("Retry-After"))
				}
			}
		})
	}
	for code := range errorMembers {
		if !seen[code] {
			t.Errorf("no case answers %s", code)
		}
	}
}

func TestFail_aRateLimitRoundsItsWaitUp(t *testing.T) {
	s := newHarness(t).srv
	rec := httptest.NewRecorder()
	s.fail(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/me", http.NoBody), s.rateLimited(1500*time.Millisecond))
	var body wire.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" || body.RetryAfterMS != 2000 {
		t.Errorf("Retry-After %q and retry_after_ms %d for a 1.5 s wait, want 2 and 2000", got, body.RetryAfterMS)
	}
}

// TestAuthErrors_carriesTheEnvelope holds access's refusals to grelot's
// envelope: the code and request id always, and retry_after_ms, the
// whole seconds of the Retry-After access sets, only when a wait helps.
func TestAuthErrors_carriesTheEnvelope(t *testing.T) {
	write := AuthErrors(slog.New(slog.DiscardHandler), obs.New())
	cases := []struct {
		name    string
		e       access.Error
		retryMS float64
	}{
		{"rateLimited", access.Error{Code: access.CodeRateLimited, Status: http.StatusTooManyRequests, RetryAfter: 1500 * time.Millisecond}, 2000},
		{"unavailable", access.Error{Code: access.CodeUnavailable, Status: http.StatusServiceUnavailable, RetryAfter: 5 * time.Second}, 5000},
		{"authRequired", access.Error{Code: access.CodeAuthRequired, Status: http.StatusUnauthorized}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ctx := httpserver.WithRequestID(context.Background(), "req-1")
			write(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/me", http.NoBody), tc.e)
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["code"] != string(tc.e.Code) || body["request_id"] != "req-1" {
				t.Errorf("code %v, request_id %v, want %s and req-1: %s", body["code"], body["request_id"], tc.e.Code, rec.Body)
			}
			ms, carried := body["retry_after_ms"].(float64)
			if tc.retryMS == 0 && carried {
				t.Errorf("retry_after_ms %v on a refusal no wait fixes: %s", ms, rec.Body)
			}
			if tc.retryMS != 0 && ms != tc.retryMS {
				t.Errorf("retry_after_ms %v, want %v: %s", body["retry_after_ms"], tc.retryMS, rec.Body)
			}
		})
	}
}
