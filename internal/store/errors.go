package store

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
	// KindGone is a session whose identity has gone or is dormant (410).
	KindGone
	// KindFull is a post refused because the store is nearly full (507).
	KindFull
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
