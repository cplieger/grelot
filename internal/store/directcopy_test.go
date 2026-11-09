//go:build !windows

package store

import (
	"fmt"
	"slices"
	"testing"
)

// copyHintedAfter posts one subject to bravo, fillers posts of other
// subjects to charlie, then the subject to delta, and answers whether the
// last post carries copyHint.
func copyHintedAfter(t *testing.T, fillers int) bool {
	t.Helper()
	s, _, _ := openIdentityStore(t)
	for _, n := range []string{"alpha", "bravo", "charlie", "delta"} {
		regAt(t, s, n, "/workspace/"+n)
	}
	post := func(to, subject string) Posted {
		p := posting(sess("alpha"), Target{Agent: to})
		p.Subject = subject
		return mustPost(t, s, p)
	}
	post("bravo", "the freeze")
	for i := range fillers {
		post("charlie", fmt.Sprintf("filler %d", i))
	}
	return slices.Contains(post("delta", "the freeze").Notices, copyHint)
}

func TestPost_theCopyScanReadsAtMostItsCap(t *testing.T) {
	// The scan reads the new post, then the fillers, then the first copy.
	t.Run("copy at the cap", func(t *testing.T) {
		if !copyHintedAfter(t, copyScanCap-2) {
			t.Errorf("copy %d posts back: no hint, want one", copyScanCap)
		}
	})
	t.Run("copy past the cap", func(t *testing.T) {
		if copyHintedAfter(t, copyScanCap-1) {
			t.Errorf("copy %d posts back: hinted, want none past the cap", copyScanCap+1)
		}
	})
}
