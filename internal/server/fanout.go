//go:build !windows

package server

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/grelot/internal/store"
)

// fanOutWindow is how far back a direct post's subject is matched against
// the sender's own direct posts to other agents.
const fanOutWindow = time.Hour

// replyWindow is how far back the other side's post with a subject makes a
// direct post a reply: a direct thread's default life.
const replyWindow = store.DefaultThreadTTL

// fanOutCap bounds the posts held, the oldest dropped first: a hint may
// miss under a flood, and memory stays fixed.
const fanOutCap = 4096

// fanOutHint is the notice on a direct post whose subject the sender sent
// another agent within the window.
const fanOutHint = "you sent another agent a direct post with this subject in the last hour: " +
	"the same text for several agents belongs in one post, in its path thread, where each reader chooses by following, " +
	"or in announce for everyone (grelot post <path> or grelot post announce)"

type directSent struct {
	at      time.Time
	subject string
	sender  uint64
	thread  uint64
}

// fanOut remembers recent direct posts in memory, oldest first.
type fanOut struct {
	sent []directSent
	mu   sync.Mutex
}

// record notes sender's direct post in thread at now and answers whether
// sender sent the same subject, by subjectKey, in another direct thread
// within fanOutWindow. A reply, the same subject the other side of thread
// sent within replyWindow, answers false.
func (f *fanOut) record(sender, thread uint64, subject string, now time.Time) bool {
	key := subjectKey(subject)
	hour := now.Add(-fanOutWindow)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropBefore(now.Add(-replyWindow))
	elsewhere, reply := false, false
	for _, d := range f.sent {
		switch {
		case d.subject != key:
		case d.thread == thread && d.sender != sender:
			reply = true
		case d.thread != thread && d.sender == sender && d.at.After(hour):
			elsewhere = true
		}
	}
	if len(f.sent) >= fanOutCap {
		f.sent = slices.Delete(f.sent, 0, len(f.sent)-fanOutCap+1)
	}
	f.sent = append(f.sent, directSent{at: now, subject: key, sender: sender, thread: thread})
	return elsewhere && !reply
}

// prune drops the posts older than replyWindow.
func (f *fanOut) prune(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropBefore(now.Add(-replyWindow))
}

func (f *fanOut) dropBefore(t time.Time) {
	i := 0
	for i < len(f.sent) && !f.sent[i].at.After(t) {
		i++
	}
	f.sent = slices.Delete(f.sent, 0, i)
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
