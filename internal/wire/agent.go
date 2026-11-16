package wire

import (
	"encoding/json"
	"math"
	"time"
)

// The headers of a request acting as an agent identity, and of its answer.
const (
	// HeaderVersion is the client's version on a request, the server's on
	// every answer the gate admitted.
	HeaderVersion = "Grelot-Version"
	// HeaderSession is <provider>:<host session id>, the session acted as.
	HeaderSession = "Grelot-Session"
	// HeaderAs is a delegate's handle, acting as that delegate.
	HeaderAs = "Grelot-As"
	// HeaderSelf is 1 on a request the agent's own command sent.
	HeaderSelf = "Grelot-Self"
	// HeaderStatus is the session's status line on an answer.
	HeaderStatus = "Grelot-Status"
	// HeaderEnvironment and HeaderMachine carry the session start hook's
	// environment and machine for GET /v1/me's take-over hint, never the
	// query, which access logs keep.
	HeaderEnvironment = "Grelot-Environment"
	HeaderMachine     = "Grelot-Machine"
	// HeaderIdempotencyKey is a keyed row's key, a quoted RFC 8941 String
	// of IdempotencyKeyMin to IdempotencyKeyMax characters of
	// [A-Za-z0-9_-] (draft-ietf-httpapi-idempotency-key-header).
	HeaderIdempotencyKey = "Idempotency-Key"
)

// The identity states: GET /v1/me's State, and an identity's or a peer's.
const (
	StateLive    = "live"
	StateDormant = "dormant"
	StateNone    = "none"
)

// The states of a receipt's recipient.
const (
	ReceiptRead    = "read"
	ReceiptUnread  = "unread"
	ReceiptDormant = "dormant"
	ReceiptRetired = "retired"
)

// Where is the machine, environment and checkouts a session reports.
type Where struct {
	Machine     string     `json:"machine"`
	Environment string     `json:"environment"`
	Checkouts   []Checkout `json:"checkouts"`
}

// Checkout is one working tree of the repo whose canonical root is Repo;
// Remote is a keyed hash of the clone's normalized remote.
type Checkout struct {
	Repo   string `json:"repo"`
	ID     string `json:"id"`
	Remote string `json:"remote"`
}

// Identity is an identity as its own session sees it. It never carries a
// machine, environment or checkout value, only whether each is known.
type Identity struct {
	Expires          time.Time `json:"expires_at"`
	RegisteredAt     time.Time `json:"registered_at"`
	LastSeen         time.Time `json:"last_seen,omitzero"`
	DormantSince     time.Time `json:"dormant_since,omitzero"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	State            string    `json:"state"`
	Host             string    `json:"host"`
	OS               string    `json:"os"`
	Arch             string    `json:"arch"`
	Provider         string    `json:"provider"`
	MachineKind      string    `json:"machine_kind,omitempty"`
	Notice           string    `json:"notice,omitempty"`
	Paths            []string  `json:"paths"`
	Follows          []string  `json:"follows"`
	TTLMS            int64     `json:"ttl_ms"`
	EnvironmentKnown bool      `json:"environment_known"`
	Ended            bool      `json:"ended,omitempty"`
	Threaded         bool      `json:"threaded"`
}

// RegisterRequest is POST /v1/register; Delegate is a delegate link to
// register from.
type RegisterRequest struct {
	Where       *Where   `json:"where"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Host        string   `json:"host"`
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
	Proc        string   `json:"proc"`
	Delegate    string   `json:"delegate"`
	Paths       []string `json:"paths"`
	TTLMS       int64    `json:"ttl_ms"`
	TakeOver    bool     `json:"take_over"`
}

// ReviveRequest is POST /v1/revive: this session's dormant identity, or a
// dormant one of this environment named by Name.
type ReviveRequest struct {
	Where *Where `json:"where"`
	Name  string `json:"name"`
	Host  string `json:"host"`
	OS    string `json:"os"`
	Arch  string `json:"arch"`
	Proc  string `json:"proc"`
	TTLMS int64  `json:"ttl_ms"`
}

// Registered is a register's or a revive's identity. NoEnvironment is set
// when the session sent no environment id; Unread counts a revived
// identity's deliveries past its read cursor.
type Registered struct {
	NoEnvironment string   `json:"no_environment,omitempty"`
	Identity      Identity `json:"identity"`
	Unread        int      `json:"unread,omitempty"`
	Existing      bool     `json:"existing,omitempty"`
	Moved         bool     `json:"moved,omitempty"`
}

// MeQuery is GET /v1/me's query: turn=start opens the turn unless signal
// names a signal line already read; proc asks for the take-over hint.
type MeQuery struct {
	Turn   string `json:"turn,omitempty"`
	Proc   string `json:"proc,omitempty"`
	Signal Seq    `json:"signal,omitempty"`
}

// Seq is a mailbox entry's seq; seqs start at 1.
type Seq uint64

// Range is the limits the server takes, which the document states.
func (Seq) Range() (lo, hi int64) { return 1, math.MaxInt64 }

// Me is GET /v1/me. State is live, dormant or none; a session with no
// identity gets State, the hint when one applies, and its status line.
type Me struct {
	Identity     *Identity     `json:"identity,omitempty"`
	Unread       *Counts       `json:"unread,omitempty"`
	Hint         *Hint         `json:"hint,omitempty"`
	State        string        `json:"state"`
	Wake         string        `json:"wake,omitempty"`
	Push         string        `json:"push,omitempty"`
	Version      string        `json:"version"`
	PathRoot     string        `json:"path_root"`
	Reservations []Reservation `json:"reservations,omitempty"`
	Delegates    []Delegate    `json:"delegates,omitempty"`
	Links        []Link        `json:"links,omitempty"`
	Online       bool          `json:"online"`
	SignalRead   bool          `json:"signal_read,omitempty"`
}

// Hint is the take-over hint: the identity a new session may go on as,
// and the command that does it, which only the agent runs.
type Hint struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Line  string `json:"line"`
}

// Reservation is a grant or queue entry; Relation is its holder's relation
// to the caller at Path.
type Reservation struct {
	Expires  time.Time `json:"expires_at,omitzero"`
	Since    time.Time `json:"since"`
	Path     string    `json:"path"`
	Reason   string    `json:"reason"`
	State    string    `json:"state"`
	Holder   string    `json:"holder"`
	Host     string    `json:"host"`
	Relation string    `json:"relation"`
	TTLMS    int64     `json:"ttl_ms"`
	ID       uint64    `json:"id"`
}

// Delegate is one of the caller's live delegates.
type Delegate struct {
	Expires     time.Time `json:"expires_at"`
	LastSeen    time.Time `json:"last_seen,omitzero"`
	Handle      string    `json:"handle"`
	Description string    `json:"description"`
	Bound       bool      `json:"bound"`
}

// Link is one of the caller's unused delegate links.
type Link struct {
	Expires time.Time `json:"expires_at"`
	Handle  string    `json:"handle"`
}

// Counts is deliveries by kind.
type Counts struct {
	Direct      int `json:"direct"`
	Path        int `json:"path"`
	Announce    int `json:"announce"`
	Identity    int `json:"identity"`
	Reservation int `json:"reservation"`
	Receipts    int `json:"receipts"`
	Cards       int `json:"cards"`
	Copies      int `json:"copies"`
	QuietPath   int `json:"quiet_path"`
	Wake        int `json:"wake"`
	Total       int `json:"total"`
}

// PatchMeRequest is PATCH /v1/me: new fields, with Where when the paths
// change, or TTLMS and Where to renew the lease.
type PatchMeRequest struct {
	Where       *Where   `json:"where"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Paths       []string `json:"paths"`
	TTLMS       int64    `json:"ttl_ms"`
}

// IdentityAnswer is the caller's identity after a change, with the
// change's notices.
type IdentityAnswer struct {
	Notices  []string `json:"notices"`
	Identity Identity `json:"identity"`
}

// Retired is DELETE /v1/me's answer.
type Retired struct {
	Retired bool `json:"retired"`
}

// IdentitiesQuery is GET /v1/identities' query.
type IdentitiesQuery struct {
	Cursor string    `json:"cursor,omitempty"`
	Path   string    `json:"path,omitempty"`
	Repo   string    `json:"repo,omitempty"`
	Holds  string    `json:"holds,omitempty"`
	Queued string    `json:"queued,omitempty"`
	Q      string    `json:"q,omitempty"`
	Online Flag      `json:"online,omitempty"`
	Here   Flag      `json:"here,omitempty"`
	All    Flag      `json:"all,omitempty"`
	Limit  PageLimit `json:"limit,omitempty"`
}

// Peers is GET /v1/identities: the identities by name.
type Peers struct {
	List[Peer]
}

// Peer is one identity as another sees it. It never carries a where
// value, a session id or a delegate's scope.
type Peer struct {
	Expires      time.Time         `json:"expires_at"`
	LastSeen     time.Time         `json:"last_seen,omitzero"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	State        string            `json:"state"`
	Host         string            `json:"host"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	Relation     string            `json:"relation"`
	Paths        []string          `json:"paths"`
	Follows      []string          `json:"follows"`
	Reservations []PeerReservation `json:"reservations"`
	Queued       []PeerQueued      `json:"queued"`
	Delegates    []PeerDelegate    `json:"delegates"`
	Online       bool              `json:"online"`
}

// PeerReservation is a path a peer holds, with its queue's length.
type PeerReservation struct {
	Expires time.Time `json:"expires_at"`
	Path    string    `json:"path"`
	Queue   int       `json:"queue"`
}

// PeerQueued is a path a peer waits for, at its place in the queue.
type PeerQueued struct {
	Path  string `json:"path"`
	Place int    `json:"place"`
}

// PeerDelegate is one of a peer's live delegates.
type PeerDelegate struct {
	LastSeen    time.Time `json:"last_seen,omitzero"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Online      bool      `json:"online"`
}

// Version is GET /v1/version: the server's version and path root.
type Version struct {
	Version  string `json:"version"`
	PathRoot string `json:"path_root"`
}

// PostRequest is POST /v1/posts. To is "announce", {"path": p} or
// {"agent": name}; NotifyRead is absent, "all" or a list of recipient
// names; ByMS is how long a read receipt is asked within.
type PostRequest struct {
	Where      *Where          `json:"where"`
	Subject    string          `json:"subject"`
	Body       string          `json:"body"`
	To         json.RawMessage `json:"to"`
	NotifyRead json.RawMessage `json:"notify_read"`
	Identity   IdentityFields  `json:"identity"`
	ByMS       int64           `json:"by_ms"`
}

// IdentityFields are the identity block every post fills, each a new
// value or "no change".
type IdentityFields struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Paths       []string `json:"paths"`
}

// Posted is a post the server stored.
type Posted struct {
	Notices    []string   `json:"notices"`
	Identity   Identity   `json:"identity"`
	Recipients Recipients `json:"recipients"`
	Message    uint64     `json:"message"`
	Thread     uint64     `json:"thread"`
}

// Recipients counts a post's recipients; Woken and Quiet are set only for
// a path post whose opening @names matched an identity.
type Recipients struct {
	Woken           []string `json:"woken,omitempty"`
	Total           int      `json:"total"`
	SameEnvironment int      `json:"same_environment"`
	Other           int      `json:"other"`
	Quiet           int      `json:"quiet,omitempty"`
}

// PathQuery is a query naming one path.
type PathQuery struct {
	Path string `json:"path,omitempty"`
}

// ThreadsQuery is GET /v1/threads' query.
type ThreadsQuery struct {
	Path   string    `json:"path,omitempty"`
	Cursor string    `json:"cursor,omitempty"`
	Limit  PageLimit `json:"limit,omitempty"`
}

// Threads is GET /v1/threads: the live path threads under the path, by
// path.
type Threads struct {
	List[Thread]
}

// Thread is one live path thread.
type Thread struct {
	Expires time.Time `json:"expires_at"`
	Path    string    `json:"path"`
	ID      uint64    `json:"id"`
}

// Receipts is one message's read receipts.
type Receipts struct {
	PostedAt        time.Time `json:"posted_at"`
	Subject         string    `json:"subject"`
	Kind            string    `json:"kind"`
	Recipients      []Receipt `json:"recipients"`
	Message         uint64    `json:"message"`
	RegisteredSince int       `json:"registered_since,omitempty"`
}

// Receipt is one recipient's receipt.
type Receipt struct {
	ReadAt   time.Time `json:"read_at,omitzero"`
	Name     string    `json:"name"`
	Host     string    `json:"host"`
	Relation string    `json:"relation"`
	State    string    `json:"state"`
}

// HistoryQuery is GET /v1/history's query; from is @me or a name.
type HistoryQuery struct {
	// Since is an RFC 3339 instant, the oldest entry's earliest time; when
	// absent, the 24 hours before the first page, or every entry with path
	// or last_read=1.
	Since time.Time `json:"since,omitzero"`
	// Until is an RFC 3339 instant, the newest entry's latest time.
	Until    time.Time `json:"until,omitzero"`
	Cursor   string    `json:"cursor,omitempty"`
	Path     string    `json:"path,omitempty"`
	With     string    `json:"with,omitempty"`
	From     string    `json:"from,omitempty"`
	Grep     string    `json:"grep,omitempty"`
	Announce Flag      `json:"announce,omitempty"`
	Full     Flag      `json:"full,omitempty"`
	LastRead Flag      `json:"last_read,omitempty"`
	Thread   ThreadID  `json:"thread,omitempty"`
	Limit    PageLimit `json:"limit,omitempty"`
}

// ThreadID names a thread; ids start at 1.
type ThreadID uint64

// Range is the limits the server takes, which the document states.
func (ThreadID) Range() (lo, hi int64) { return 1, math.MaxInt64 }

// History is GET /v1/history: the caller's mailbox, which it moves
// nothing in; pages go back in time, each oldest first.
type History struct {
	List[Entry]
}

// Entry is one inbox or history entry. Subject, body, text and the paths
// are other agents' text, which the client labels as data.
type Entry struct {
	PostedAt   time.Time `json:"posted_at,omitzero"`
	Written    time.Time `json:"written"`
	ReadAt     time.Time `json:"read_at,omitzero"`
	Kind       string    `json:"kind"`
	Subject    string    `json:"subject,omitempty"`
	Body       string    `json:"body,omitempty"`
	Text       string    `json:"text,omitempty"`
	ThreadPath string    `json:"thread_path,omitempty"`
	Matched    string    `json:"matched,omitempty"`
	From       string    `json:"from,omitempty"`
	Host       string    `json:"host,omitempty"`
	OS         string    `json:"os,omitempty"`
	Arch       string    `json:"arch,omitempty"`
	Relation   string    `json:"relation,omitempty"`
	Via        string    `json:"via,omitempty"`
	Seq        uint64    `json:"seq,omitempty"`
	Message    uint64    `json:"message,omitempty"`
	Thread     uint64    `json:"thread,omitempty"`
	Wake       bool      `json:"wake"`
	Truncated  bool      `json:"truncated,omitempty"`
}

// InboxQuery is GET /v1/inbox's query; WaitMS, up to MaxWaitMS, holds the
// read until wake mail arrives.
type InboxQuery struct {
	Cursor string      `json:"cursor,omitempty"`
	Limit  PageLimit   `json:"limit,omitempty"`
	WaitMS InboxWaitMS `json:"wait_ms,omitempty"`
}

// Inbox is GET /v1/inbox: the unread entries oldest first, which it marks
// nothing read; Total counts the unread entries.
type Inbox struct {
	List[Entry]
	Unread Counts `json:"unread"`
}

// AckRequest marks read every entry up to Upto.
type AckRequest struct {
	Upto uint64 `json:"upto"`
}

// Acked is what an ack left unread.
type Acked struct {
	Left Counts `json:"left"`
	Read uint64 `json:"read"`
}

// ReserveRequest is POST /v1/reservations: a grant, or with Queue a queue
// entry when the path is taken.
type ReserveRequest struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	TTLMS  int64  `json:"ttl_ms"`
	Queue  bool   `json:"queue"`
}

// Reserved is a new reservation; Behind is the grant a queue entry waits
// on.
type Reserved struct {
	Behind      *Reservation `json:"behind,omitempty"`
	Reservation Reservation  `json:"reservation"`
}

// ReservationsQuery is GET /v1/reservations' query.
type ReservationsQuery struct {
	Path   string    `json:"path,omitempty"`
	Cursor string    `json:"cursor,omitempty"`
	Limit  PageLimit `json:"limit,omitempty"`
}

// Reservations is GET /v1/reservations: the grants and queue entries by
// id.
type Reservations struct {
	List[Reservation]
}

// ReservationParams names a reservation.
type ReservationParams struct {
	ID uint64 `json:"id"`
}

// RenewRequest renews a reservation.
type RenewRequest struct {
	TTLMS int64 `json:"ttl_ms"`
}

// ReservationAnswer is a reservation after a renewal or a release.
type ReservationAnswer struct {
	Reservation Reservation `json:"reservation"`
}

// GrantRequest is POST /v1/delegates: a single-use link a subagent
// registers with, scoped by the allow and deny lists.
type GrantRequest struct {
	Label        string   `json:"label"`
	AllowThreads []string `json:"allow_threads"`
	AllowAgents  []string `json:"allow_agents"`
	DenyThreads  []string `json:"deny_threads"`
	DenyAgents   []string `json:"deny_agents"`
	TTLMS        int64    `json:"ttl_ms"`
}

// Granted is a new delegate link.
type Granted struct {
	Expires time.Time `json:"expires_at"`
	Link    string    `json:"link"`
	Handle  string    `json:"handle"`
}

// DelegateParams names a delegate by its label.
type DelegateParams struct {
	Label string `json:"label"`
}

// Revoked is the delegates a revoke ended, and whether it revoked an
// unused link.
type Revoked struct {
	Ended []string `json:"ended"`
	Link  bool     `json:"link"`
}

// LifecycleRequest is a hook's report: Event is session_end, subagent_stop
// or turn_end, AgentID set when it came from inside a subagent.
type LifecycleRequest struct {
	Event   string `json:"event"`
	AgentID string `json:"agent_id"`
}

// Lifecycle is the event a lifecycle report recorded.
type Lifecycle struct {
	Event string `json:"event"`
}

// HoldRequest is the stop hook's hold; For is "stop", and Last says the
// host runs no second stop hook this turn.
type HoldRequest struct {
	For  string `json:"for"`
	Last bool   `json:"last"`
}

// Held is a stop hold's answer. Counts are the deliveries past read and
// Shown the shown cursor as it stood; Block holds the turn for the wake
// mail counted; Down is set once per down episode.
type Held struct {
	Wake   string `json:"wake"`
	Counts Counts `json:"counts"`
	Shown  uint64 `json:"shown"`
	Block  bool   `json:"block"`
	Down   bool   `json:"down"`
}

// WaitRequest is a standing wait of up to MaxWaitMS.
type WaitRequest struct {
	WaitMS StandingWaitMS `json:"wait_ms"`
}

// MaxWaitMS bounds a held wait.
const MaxWaitMS = 55_000

// InboxWaitMS is how long an inbox read holds for wake mail; 0 holds
// nothing.
type InboxWaitMS int64

// Range is the limits the server takes, which the document states.
func (InboxWaitMS) Range() (lo, hi int64) { return 0, MaxWaitMS }

// StandingWaitMS is how long a standing wait holds.
type StandingWaitMS int64

// Range is the limits the server takes, which the document states.
func (StandingWaitMS) Range() (lo, hi int64) { return 1, MaxWaitMS }

// Nudge is a nudge as a standing wait returns it: the signal line's seq,
// the wake deliveries past read, and which send it is, 1 to 4.
type Nudge struct {
	Seq     uint64 `json:"seq"`
	Count   int    `json:"count"`
	Attempt int    `json:"attempt"`
}
