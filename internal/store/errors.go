//go:build !windows

package store

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/cplieger/grelot/internal/wire"
)

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
// word naming the cause where a refusal has several, which the API answers
// as its code: the 410 reason a watcher reads, or a conflict's kind.
// Reason is a KindNotFound's resource, and Field the request field a
// KindInvalid or a too long value names.
type Error struct {
	Field  *wire.FieldError
	Msg    string
	Why    string
	Reason string
	Kind   Kind
}

func (e *Error) Error() string { return e.Msg }

func refuse(k Kind, msg string) *Error { return &Error{Kind: k, Msg: msg} }

func notFound(reason, msg string) *Error { return &Error{Kind: KindNotFound, Reason: reason, Msg: msg} }

func isKind(err error, k Kind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == k
}

// textFault is what is wrong with v as one line of text of at most max
// characters, and when required at least one, nil when nothing is.
func textFault(field, v string, maxChars int, required bool) *wire.FieldError {
	n := utf8.RuneCountInString(v)
	switch {
	case n == 0 && required:
		return &wire.FieldError{Field: field, Code: wire.FieldRequired}
	case !utf8.ValidString(v) || strings.ContainsFunc(v, isControl):
		return &wire.FieldError{Field: field, Code: wire.FieldNotText}
	case n > maxChars:
		m := int64(maxChars)
		return &wire.FieldError{Field: field, Code: wire.FieldTooLong, Max: &m}
	}
	return nil
}

// fieldRefusal is a KindInvalid with msg naming f, nil when f is.
func fieldRefusal(f *wire.FieldError, msg string) error {
	if f == nil {
		return nil
	}
	return &Error{Kind: KindInvalid, Msg: msg, Field: f}
}
