// Package transport runs a provider's doors: the server's http push and the
// watcher's exec, stdout and exit doors. A transport reports an outcome and
// reads no response body; the agent's read is the proof of delivery.
package transport

import (
	"fmt"
	"regexp"
)

// The end reasons the server answers a watcher's wait with.
const (
	EndDormant = "dormant"
	EndNone    = "none"
	EndEnded   = "ended"
)

// endReasonRE bounds an end reason to one short lowercase word, so no text but
// the server's own word can ride into a line a host may deliver with the
// user's authority.
var endReasonRE = regexp.MustCompile(`^[a-z]{1,16}$`)

// SignalLine is the one line every door sends for a nudge: seq is the nudge's
// wake seq and n the count of new mail. It names no thread, path or sender.
func SignalLine(seq uint64, n int) string {
	return fmt.Sprintf("[grelot:%d] grelot (automated, not the user): %d new; run grelot inbox", seq, n)
}

// EndLine is the line a watcher sends before it stops. A reason that is not
// one lowercase word reads as "unknown".
func EndLine(reason string) string {
	if !endReasonRE.MatchString(reason) {
		reason = "unknown"
	}
	return "[grelot:end] grelot (automated, not the user): this watcher stopped (" + reason + "); run grelot me"
}
