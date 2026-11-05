package client

import "time"

// The identity states GET /v1/me answers.
const (
	StateLive    = "live"
	StateDormant = "dormant"
	StateNone    = "none"
)

// Me is GET /v1/me's answer. A session with no identity gets only State,
// the hint when one applies, Version and PathRoot.
type Me struct {
	Identity     *Identity     `json:"identity"`
	Unread       *Counts       `json:"unread"`
	Hint         *Hint         `json:"hint"`
	State        string        `json:"state"`
	Wake         string        `json:"wake"`
	Push         string        `json:"push"`
	Version      string        `json:"version"`
	PathRoot     string        `json:"path_root"`
	Reservations []Reservation `json:"reservations"`
	Delegates    []Delegate    `json:"delegates"`
	Links        []Link        `json:"links"`
	Online       bool          `json:"online"`
}

// Identity is an identity as its own session sees it: whether its machine
// and environment are known, never their values.
type Identity struct {
	Expires          time.Time `json:"expires_at"`
	RegisteredAt     time.Time `json:"registered_at"`
	LastSeen         time.Time `json:"last_seen"`
	DormantSince     time.Time `json:"dormant_since"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	State            string    `json:"state"`
	Host             string    `json:"host"`
	OS               string    `json:"os"`
	Arch             string    `json:"arch"`
	Provider         string    `json:"provider"`
	MachineKind      string    `json:"machine_kind"`
	TTL              string    `json:"ttl"`
	Notice           string    `json:"notice"`
	Paths            []string  `json:"paths"`
	Follows          []string  `json:"follows"`
	EnvironmentKnown bool      `json:"environment_known"`
	Ended            bool      `json:"ended"`
}

// Counts is unread deliveries by kind.
type Counts struct {
	Direct      int `json:"direct"`
	Path        int `json:"path"`
	Announce    int `json:"announce"`
	Identity    int `json:"identity"`
	Reservation int `json:"reservation"`
	Receipts    int `json:"receipts"`
	Cards       int `json:"cards"`
	Copies      int `json:"copies"`
	Wake        int `json:"wake"`
	Total       int `json:"total"`
}

// Hint is the take-over or revive hint: the identity this session may go
// on as, and Line, the command that does it.
type Hint struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Line  string `json:"line"`
}

// Reservation is a held grant or a queue entry; Relation is its holder's
// relation to the caller at Path.
type Reservation struct {
	Expires  time.Time `json:"expires_at"`
	Since    time.Time `json:"since"`
	Path     string    `json:"path"`
	Reason   string    `json:"reason"`
	State    string    `json:"state"`
	Holder   string    `json:"holder"`
	Host     string    `json:"host"`
	Relation string    `json:"relation"`
	TTL      string    `json:"ttl"`
	ID       uint64    `json:"id"`
}

// Delegate is one of the identity's delegates.
type Delegate struct {
	Expires     time.Time `json:"expires_at"`
	LastSeen    time.Time `json:"last_seen"`
	Handle      string    `json:"handle"`
	Description string    `json:"description"`
	Bound       bool      `json:"bound"`
}

// Link is a delegate grant not used yet.
type Link struct {
	Expires time.Time `json:"expires_at"`
	Handle  string    `json:"handle"`
}

// Where is where a session runs, as keyed hashes only: no raw machine
// value, key or remote is ever put in one.
type Where struct {
	Machine     string     `json:"machine"`
	Environment string     `json:"environment"`
	Checkouts   []Checkout `json:"checkouts"`
}

// Checkout is the working tree of the repo whose canonical root is Repo:
// ID its checkout id, Remote the hash of its normalized remote.
type Checkout struct {
	Repo   string `json:"repo"`
	ID     string `json:"id"`
	Remote string `json:"remote"`
}

// Registered is POST /v1/register's and POST /v1/revive's answer.
// NoEnvironment is the line owed to a session that sent no environment id;
// Existing marks a register that found the session's live identity, and
// Unread is a revive's.
type Registered struct {
	NoEnvironment string   `json:"no_environment"`
	Identity      Identity `json:"identity"`
	Unread        int      `json:"unread"`
	Existing      bool     `json:"existing"`
	Moved         bool     `json:"moved"`
}

// IdentityAnswer is PATCH /v1/me's answer.
type IdentityAnswer struct {
	Identity Identity `json:"identity"`
}

// ReservationAnswer is a reservation route's answer.
type ReservationAnswer struct {
	Reservation Reservation `json:"reservation"`
}

// Peer is one identity in GET /v1/identities. A fields= query leaves the
// fields it did not name at their zero value.
type Peer struct {
	Expires      time.Time         `json:"expires_at"`
	LastSeen     time.Time         `json:"last_seen"`
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

// PeerReservation is a grant a peer holds and how many wait behind it.
type PeerReservation struct {
	Expires time.Time `json:"expires_at"`
	Path    string    `json:"path"`
	Queue   int       `json:"queue"`
}

// PeerQueued is a path a peer waits on, Place 1 next.
type PeerQueued struct {
	Path  string `json:"path"`
	Place int    `json:"place"`
}

// PeerDelegate is a delegate listed under its parent.
type PeerDelegate struct {
	LastSeen    time.Time `json:"last_seen"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Online      bool      `json:"online"`
}

// Peers is GET /v1/identities's answer.
type Peers struct {
	Identities []Peer `json:"identities"`
}

// Entry is one inbox or history entry: a post or card with its message and
// sender, or an event with only Text. Subject, Body, Text and the paths are
// other agents' text.
type Entry struct {
	PostedAt   time.Time `json:"posted_at"`
	Written    time.Time `json:"written"`
	Kind       string    `json:"kind"`
	Subject    string    `json:"subject"`
	Body       string    `json:"body"`
	Text       string    `json:"text"`
	ThreadPath string    `json:"thread_path"`
	From       string    `json:"from"`
	Host       string    `json:"host"`
	Relation   string    `json:"relation"`
	Via        string    `json:"via"`
	Seq        uint64    `json:"seq"`
	Message    uint64    `json:"message"`
	Thread     uint64    `json:"thread"`
	Truncated  bool      `json:"truncated"`
}

// Inbox is GET /v1/inbox's answer: the oldest deliveries past read.
type Inbox struct {
	Entries []Entry `json:"entries"`
}

// Acked is POST /v1/inbox/ack's answer: what is still past read.
type Acked struct {
	Left Counts `json:"left"`
}

// History is GET /v1/history's answer.
type History struct {
	Entries []Entry `json:"entries"`
}

// Posted is POST /v1/posts's answer.
type Posted struct {
	Notices    []string   `json:"notices"`
	Recipients Recipients `json:"recipients"`
	Message    uint64     `json:"message"`
	Thread     uint64     `json:"thread"`
}

// Recipients counts a post's recipients: SameEnvironment share the
// sender's environment, Other do not.
type Recipients struct {
	Total           int `json:"total"`
	SameEnvironment int `json:"same_environment"`
	Other           int `json:"other"`
}

// The receipt states of a recipient.
const (
	ReceiptRead    = "read"
	ReceiptUnread  = "unread"
	ReceiptDormant = "dormant"
	ReceiptRetired = "retired"
)

// Receipts is GET /v1/messages/{id}/receipts's answer. RegisteredSince
// counts, for an announcement, the identities registered after it, which
// never received it.
type Receipts struct {
	PostedAt        time.Time `json:"posted_at"`
	Subject         string    `json:"subject"`
	Kind            string    `json:"kind"`
	Recipients      []Receipt `json:"recipients"`
	Message         uint64    `json:"message"`
	RegisteredSince int       `json:"registered_since"`
}

// Receipt is one recipient of a post and whether it has read it.
type Receipt struct {
	ReadAt   time.Time `json:"read_at"`
	Name     string    `json:"name"`
	Host     string    `json:"host"`
	Relation string    `json:"relation"`
	State    string    `json:"state"`
}

// Threads is GET /v1/threads's answer.
type Threads struct {
	Threads []Thread `json:"threads"`
}

// Thread is a live path thread.
type Thread struct {
	Expires time.Time `json:"expires_at"`
	Path    string    `json:"path"`
	ID      uint64    `json:"id"`
}

// ReservationQueued is the state of a queue entry, as against a grant.
const ReservationQueued = "queued"

// Reserved is POST /v1/reservations's answer; Behind is the grant a queue
// entry waits on.
type Reserved struct {
	Behind      *Reservation `json:"behind"`
	Reservation Reservation  `json:"reservation"`
}

// Reservations is GET /v1/reservations's answer.
type Reservations struct {
	Reservations []Reservation `json:"reservations"`
}

// Granted is POST /v1/delegates's answer: the single-use link and the
// handle its delegate will have.
type Granted struct {
	Expires time.Time `json:"expires_at"`
	Link    string    `json:"link"`
	Handle  string    `json:"handle"`
}

// Revoked is DELETE /v1/delegates/{label}'s answer: the delegates ended
// and whether an unused link went.
type Revoked struct {
	Ended []string `json:"ended"`
	Link  bool     `json:"link"`
}
