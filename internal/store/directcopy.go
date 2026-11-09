//go:build !windows

package store

import (
	"strings"
	"time"

	"github.com/cplieger/bolttable"
)

// copyWindow is how far back a direct post's subject is matched against
// its sender's direct posts in other threads.
const copyWindow = time.Hour

// copyScanCap bounds the posts each copy scan reads under the write lock:
// past it a copy may go unhinted, never a reply hinted.
const copyScanCap = 64

// copyHint is the notice on a direct post whose subject its sender sent
// another agent within copyWindow.
const copyHint = "you sent another agent a direct post with this subject in the last hour: " +
	"the same text for several agents belongs in one post, in its path thread, where each reader chooses by following, " +
	"or in announce for everyone (grelot post <path> or grelot post announce)"

// hintCopy adds copyHint to out when msg, a direct post, repeats by
// subjectKey a direct post its sender sent in another thread within
// copyWindow, unless msg is a reply: the other side of its thread posted
// that subject there.
func (w *wtx) hintCopy(msg *messageRow, out *Posted) error {
	if msg.Kind != kindDirect {
		return nil
	}
	key := subjectKey(msg.Subject)
	copied, err := w.sentElsewhere(msg, key)
	if err != nil || !copied {
		return err
	}
	reply, err := w.askedHere(msg, key)
	if err != nil || reply {
		return err
	}
	out.Notices = append(out.Notices, copyHint)
	return nil
}

func (w *wtx) sentElsewhere(msg *messageRow, key string) (bool, error) {
	since := w.now.Add(-copyWindow)
	found, n := false, 0
	err := w.s.t.bySender.Scan(w.r, bolttable.Range{Prefix: be64(msg.Sender), Reverse: true}, func(ikey, k, _ []byte) (bool, error) {
		at, err := senderPostedAt(ikey)
		if err != nil || !at.After(since) {
			return false, err
		}
		m, ok, err := w.s.t.messages.table.Get(w.r, k)
		if err != nil {
			return false, err
		}
		found = ok && m.Kind == kindDirect && m.Thread != msg.Thread && !m.Lease.due(w.now) && subjectKey(m.Subject) == key
		n++
		return !found && n < copyScanCap, nil
	})
	return found, err
}

// askedHere reports whether the other side of msg's thread posted key
// there, or the thread is too long for the scan to tell.
func (w *wtx) askedHere(msg *messageRow, key string) (bool, error) {
	found, n := false, 0
	err := w.s.t.messages.table.Scan(w.r, bolttable.Range{Prefix: be64(msg.Thread), Reverse: true}, func(_ []byte, m *messageRow) (bool, error) {
		found = m.Sender != msg.Sender && !m.Lease.due(w.now) && subjectKey(m.Subject) == key
		n++
		return !found && n < copyScanCap, nil
	})
	return found || n >= copyScanCap, err
}

// subjectKey is subject case-folded, trimmed, with one leading "Re:" cut,
// so a reply's subject matches the post it answers.
func subjectKey(subject string) string {
	s := strings.TrimSpace(subject)
	if len(s) >= 3 && strings.EqualFold(s[:3], "re:") {
		s = strings.TrimSpace(s[3:])
	}
	return strings.ToLower(s)
}
