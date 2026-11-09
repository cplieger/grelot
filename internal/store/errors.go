//go:build !windows

package store

import "errors"

// Kind sorts a refusal by what the caller must do about it; the HTTP layer
// maps each to its status.
type Kind int

// The kinds of refusal, each named for the HTTP status it answers.
const (
	// KindInvalid is a request to fix and resend (400).
	KindInvalid Kind = iota + 1
	// KindNotFound names something that is not there (404).
	KindNotFound
	// KindConflict collides with another identity's state (409).
	KindConflict
	// KindGone is an identity, delegate or link that has gone, or a
	// dormant identity (410).
	KindGone
	// KindFull is a write that adds rows, refused because the store is
	// nearly full (507).
	KindFull
	// KindForbidden is a request outside what the caller may do (403).
	KindForbidden
	// KindKeyReused is an Idempotency-Key sent with a different request
	// than its first (422).
	KindKeyReused
)

// Error is a refusal the caller sees. Msg names the fix; Why is a fixed
// word naming the cause where a refusal has several: the move refused log
// line's why, or the 410 reason a watcher reads.
type Error struct {
	Msg  string
	Why  string
	Kind Kind
}

func (e *Error) Error() string { return e.Msg }

func refuse(k Kind, msg string) *Error { return &Error{Kind: k, Msg: msg} }

func isKind(err error, k Kind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == k
}
