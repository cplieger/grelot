//go:build !windows

package server

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/store"
	"github.com/cplieger/grelot/internal/wire"
)

// The cursor layout: a 12-byte nonce, a 16-byte tag, a
// format byte, an 8-byte issue time and a 32-byte binding around the
// position, unpadded base64url, so a thread's 1024-byte path seals to 1458
// characters.
const (
	wantMaxCursor   = 1458
	wantMaxPosition = 1024
)

// longestPosition is each Paged row's longest position:
// a name or delegate handle of 97 bytes, a thread path of 1024, a
// reservation id or a delivery seq of 8, and a history entry's key of 28
// (its written instant of 12, message and seq; an own post carries no
// seq) with the walk's 12-byte lower bound.
var longestPosition = map[string][]byte{
	"listIdentities":   bytes.Repeat([]byte("n"), 97),
	"listThreads":      bytes.Repeat([]byte("p"), wantMaxPosition),
	"listReservations": bytes.Repeat([]byte{0xff}, 8),
	"readInbox":        bytes.Repeat([]byte{0xff}, 8),
	"getHistory":       bytes.Repeat([]byte{0xff}, 40),
}

func cursorCode(err error) string {
	if ae, ok := errors.AsType[*apiError](err); ok {
		return string(ae.code)
	}
	return fmt.Sprintf("not a refusal: %v", err)
}

func TestCursor_everyPagedRowsLongestPositionRoundTrips(t *testing.T) {
	c := newCursors(testCursorKey, time.Now)
	for i := range wire.Routes {
		r := &wire.Routes[i]
		if !r.Paged {
			continue
		}
		t.Run(r.Name, func(t *testing.T) {
			pos, ok := longestPosition[r.Name]
			if !ok {
				t.Fatalf("no longest position for the Paged row %s", r.Name)
			}
			scope := cursorScope{row: r.Name, query: "path=%2Fworkspace", credential: "key:1"}
			sealed, err := c.seal(scope, pos)
			if err != nil {
				t.Fatal(err)
			}
			if len(sealed) > wantMaxCursor {
				t.Errorf("the cursor is %d characters, over %d", len(sealed), wantMaxCursor)
			}
			if r.Name == "listThreads" && len(sealed) != wantMaxCursor {
				t.Errorf("a 1024-byte path seals to %d characters, want exactly %d", len(sealed), wantMaxCursor)
			}
			got, err := c.open(scope, sealed)
			if err != nil || !bytes.Equal(got, pos) {
				t.Errorf("open = %x, %v; want the sealed position back", got, err)
			}
		})
	}
	for name := range longestPosition {
		if i := wireRow(name); i < 0 || !wire.Routes[i].Paged {
			t.Errorf("%s has a longest position but is no Paged row", name)
		}
	}
}

func wireRow(name string) int {
	for i := range wire.Routes {
		if wire.Routes[i].Name == name {
			return i
		}
	}
	return -1
}

func TestCursor_aPositionPastTheBoundIsNotSealed(t *testing.T) {
	c := newCursors(testCursorKey, time.Now)
	_, err := c.seal(cursorScope{row: "listThreads"}, bytes.Repeat([]byte("p"), wantMaxPosition+1))
	if !errors.Is(err, errPositionTooLong) {
		t.Errorf("seal of %d bytes = %v, want errPositionTooLong", wantMaxPosition+1, err)
	}
}

func TestCursor_refusals(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	clock := now
	c := newCursors(testCursorKey, func() time.Time { return clock })
	scope := cursorScope{row: "listThreads", query: "path=%2Fworkspace", credential: "key:1"}
	sealed, err := c.seal(scope, []byte("/workspace/a"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := []byte(sealed)
	tampered[len(tampered)/2] ^= 'A' ^ 'B'
	other := newCursors(bytes.Repeat([]byte{9}, 32), time.Now)
	foreign, err := other.seal(scope, []byte("/workspace/a"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		scope  cursorScope
		cursor string
		after  time.Duration
		want   string
	}{
		{"one character past the bound", scope, strings.Repeat("A", wantMaxCursor+1), 0, "cursor_invalid"},
		{"not base64url", scope, "a+b/", 0, "cursor_invalid"},
		{"too short to hold a tag", scope, "AAAA", 0, "cursor_invalid"},
		{"one byte changed", scope, string(tampered), 0, "cursor_invalid"},
		{"sealed under another key", scope, foreign, 0, "cursor_invalid"},
		{"older than 24 hours", scope, sealed, 24*time.Hour + time.Second, "cursor_invalid"},
		{"another row", cursorScope{row: "getHistory", query: scope.query, credential: scope.credential}, sealed, 0, "cursor_mismatch"},
		{"another query", cursorScope{row: scope.row, query: "path=%2Fother", credential: scope.credential}, sealed, 0, "cursor_mismatch"},
		{"another credential", cursorScope{row: scope.row, query: scope.query, credential: "key:2"}, sealed, 0, "cursor_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock = now.Add(tc.after)
			_, err := c.open(tc.scope, tc.cursor)
			if got := cursorCode(err); got != tc.want {
				t.Errorf("open = %s, want %s", got, tc.want)
			}
		})
	}
	clock = now.Add(24 * time.Hour)
	if _, err := c.open(scope, sealed); err != nil {
		t.Errorf("a cursor 24 hours old: %v, want it opened", err)
	}
}

func TestHistoryPosition_roundTripsEveryRFC3339InstantAndNoBound(t *testing.T) {
	plusOne, err := time.Parse(time.RFC3339, "0001-01-01T00:00:00+01:00")
	if err != nil {
		t.Fatal(err)
	}
	instants := map[string]time.Time{
		"the Unix epoch":        time.Unix(0, 0),
		"before 1678":           time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC),
		"year 1":                time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		"year 1 at +01:00":      plusOne,
		"year 9999's last ns":   time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
		"a written-now instant": time.Date(2026, 11, 16, 9, 0, 0, 123456789, time.UTC),
	}
	for name, at := range instants {
		t.Run(name, func(t *testing.T) {
			k := store.EntryKey{Written: at, Message: 7, Seq: 9}
			for _, bound := range []*time.Time{nil, &at} {
				gotKey, gotBound, err := afterHistory(historyPosition(k, bound))
				if err != nil || !gotKey.Written.Equal(at) || gotKey.Message != 7 || gotKey.Seq != 9 {
					t.Errorf("key = %+v, %v; want %+v back", gotKey, err, k)
				}
				switch {
				case bound == nil && gotBound != nil:
					t.Errorf("no bound came back as %v", *gotBound)
				case bound != nil && (gotBound == nil || !gotBound.Equal(at)):
					t.Errorf("bound %v came back as %v", at, gotBound)
				}
			}
		})
	}
}

func TestNewCursors_panicsOnAKeyNot32BytesLong(t *testing.T) {
	for _, n := range []int{0, 16, 24, 33} {
		t.Run(fmt.Sprintf("%d bytes", n), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("newCursors with a %d-byte key did not panic", n)
				}
			}()
			newCursors(make([]byte, n), time.Now)
		})
	}
}
