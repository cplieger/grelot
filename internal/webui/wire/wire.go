// Package wire is the web API's JSON: the types the server sends and
// reads on /api, and the endpoint table that binds its routes and
// generates the browser's typed client. Instants are milliseconds since
// the Unix epoch and durations milliseconds, each named with _ms.
package wire

// Error is every error response, on /api and /v1 alike.
type Error struct {
	// Error is the refusal and its fix, in words.
	Error string `json:"error"`
	// Code names the refusal, such as not_found.
	Code string `json:"code,omitempty"`
	// Reason refines a sign-in route's Code, such as label_too_long.
	Reason    string `json:"reason,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	// PathRoot is the server's path root; an unauthenticated caller never
	// gets it.
	PathRoot string `json:"path_root,omitempty"`
}

// Show filters the conversation list.
type Show string

// The conversation filters.
const (
	ShowAll     Show = "all"
	ShowThreads Show = "threads"
	ShowDirect  Show = "direct"
)

// ThreadKind is a conversation's kind.
type ThreadKind string

// The conversation kinds.
const (
	ThreadAnnounce ThreadKind = "announce"
	ThreadPath     ThreadKind = "path"
	ThreadDirect   ThreadKind = "direct"
)

// SenderKind tells an agent's post from the user's.
type SenderKind string

// The sender kinds.
const (
	SenderAgent SenderKind = "agent"
	SenderUser  SenderKind = "user"
)

// Thread names one conversation.
type Thread struct {
	Kind ThreadKind `json:"kind"`
	// Title is announce, the path, or the direct thread's two names.
	Title string `json:"title"`
	Path  string `json:"path,omitempty"`
	// Agents are a direct thread's two names.
	Agents []string `json:"agents,omitempty"`
	ID     uint64   `json:"id"`
}

// LastPost is a conversation's newest post.
type LastPost struct {
	From    string `json:"from"`
	Subject string `json:"subject"`
	// Preview is the body's first line, cut at 200 characters.
	Preview string `json:"preview"`
	ID      uint64 `json:"id"`
	AtMS    int64  `json:"at_ms"`
}

// ConversationRow is one row of the conversation list.
type ConversationRow struct {
	Last LastPost `json:"last"`
	Thread
	Participants int `json:"participants"`
	// Unread counts the user's unread posts in it.
	Unread int `json:"unread"`
}

// ConversationsQuery is the conversation list's query.
type ConversationsQuery struct {
	Kind Show `json:"kind,omitempty"`
	// Before and BeforeThread are the Next and NextThread of the newer
	// page; both absent for the newest.
	Before       uint64 `json:"before,omitempty"`
	BeforeThread uint64 `json:"before_thread,omitempty"`
}

// ConversationsPage is one page of conversations, newest activity first.
type ConversationsPage struct {
	Rows []ConversationRow `json:"rows"`
	// Next and NextThread are the Before and BeforeThread of the older
	// page, both absent on the oldest.
	Next       uint64 `json:"next,omitempty"`
	NextThread uint64 `json:"next_thread,omitempty"`
	// Counts covers every page and every kind, whatever the query.
	Counts ConversationCounts `json:"counts"`
}

// ConversationCounts counts the conversations each filter lists; all
// lists their sum.
type ConversationCounts struct {
	Threads int `json:"threads"`
	Direct  int `json:"direct"`
}

// ReadCount is how many of a post's recipients have read it.
type ReadCount struct {
	Read  int `json:"read"`
	Total int `json:"total"`
}

// Post is one message.
type Post struct {
	From     string     `json:"from"`
	FromKind SenderKind `json:"from_kind"`
	Subject  string     `json:"subject"`
	Body     string     `json:"body"`
	// Origin is the subpath a post shown in its parent path's
	// conversation was posted to.
	Origin string    `json:"origin,omitempty"`
	Read   ReadCount `json:"read"`
	ID     uint64    `json:"id"`
	Thread uint64    `json:"thread"`
	AtMS   int64     `json:"at_ms"`
	// Unread marks a post delivered to the user that the user has not read.
	Unread bool `json:"unread,omitempty"`
}

// ReservationState is a reservation's state.
type ReservationState string

// The reservation states.
const (
	ReservationHeld   ReservationState = "held"
	ReservationQueued ReservationState = "queued"
)

// Reservation is a path an agent holds or waits for.
type Reservation struct {
	Path   string           `json:"path"`
	Holder string           `json:"holder"`
	Host   string           `json:"host,omitempty"`
	Reason string           `json:"reason,omitempty"`
	State  ReservationState `json:"state"`
	ID     uint64           `json:"id"`
	// Place is a queued entry's place in its path's queue, from 1.
	Place int `json:"place,omitempty"`
	// SinceMS is when it was queued or, once held, granted.
	SinceMS   int64 `json:"since_ms"`
	ExpiresMS int64 `json:"expires_ms"`
}

// AgentState is an identity's state.
type AgentState string

// The identity states.
const (
	AgentLive    AgentState = "live"
	AgentDormant AgentState = "dormant"
)

// Agent is an agent's card.
type Agent struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	State       AgentState `json:"state"`
	Host        string     `json:"host,omitempty"`
	OS          string     `json:"os,omitempty"`
	Arch        string     `json:"arch,omitempty"`
	Provider    string     `json:"provider,omitempty"`
	Paths       []string   `json:"paths"`
	Follows     []string   `json:"follows"`
	// RegisteredMS is when it first registered.
	RegisteredMS int64 `json:"registered_ms"`
	LastSeenMS   int64 `json:"last_seen_ms,omitempty"`
	// ExpiresMS is the end of a live agent's lease.
	ExpiresMS      int64 `json:"expires_ms,omitempty"`
	DormantSinceMS int64 `json:"dormant_since_ms,omitempty"`
	// Online marks a live agent holding a standing wait, as grelot who
	// reports it.
	Online bool `json:"online,omitempty"`
}

// AgentList is every live and dormant agent, by name.
type AgentList struct {
	Agents []Agent `json:"agents"`
}

// Conversation is one conversation's posts, oldest first, and its context.
type Conversation struct {
	Posts []Post `json:"posts"`
	// Reservations are a path conversation's grants then queue entries
	// overlapping its path.
	Reservations []Reservation `json:"reservations"`
	// Followers are the agents a path conversation's posts reach.
	Followers []string `json:"followers"`
	// Cards are a direct conversation's agents.
	Cards  []Agent `json:"cards"`
	Thread Thread  `json:"thread"`
	// Next is the before of the older page, absent on the oldest.
	Next uint64 `json:"next,omitempty"`
}

// ConversationParams names a conversation.
type ConversationParams struct {
	ID uint64 `json:"id"`
}

// PageQuery pages a list of posts.
type PageQuery struct {
	// Before is the Next of the newer page; absent for the newest.
	Before uint64 `json:"before,omitempty"`
}

// MessageParams names a message.
type MessageParams struct {
	ID uint64 `json:"id"`
}

// Receipt is one recipient of a message.
type Receipt struct {
	Name  string `json:"name"`
	Host  string `json:"host,omitempty"`
	State string `json:"state"`
	// ReadAtMS is absent while unread.
	ReadAtMS int64 `json:"read_at_ms,omitempty"`
}

// Receipts are who has read a message.
type Receipts struct {
	Subject    string    `json:"subject"`
	Kind       string    `json:"kind"`
	Recipients []Receipt `json:"recipients"`
	Message    uint64    `json:"message"`
	PostedAtMS int64     `json:"posted_at_ms"`
	// RegisteredSince counts, for an announcement, the agents registered
	// after it, which never received it.
	RegisteredSince int `json:"registered_since"`
}

// MarkReadRequest bounds a mark-read to the posts the reader was shown.
type MarkReadRequest struct {
	// UpTo is the newest post id shown; a later post stays unread.
	UpTo uint64 `json:"up_to"`
}

// MarkedRead is how many posts a mark-read stamped.
type MarkedRead struct {
	Read int `json:"read"`
}

// PostTarget is where the user posts: exactly one of its fields.
type PostTarget struct {
	Path     string `json:"path,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Announce bool   `json:"announce,omitempty"`
}

// PostRequest is a post the user sends.
type PostRequest struct {
	Subject string     `json:"subject"`
	Body    string     `json:"body"`
	To      PostTarget `json:"to"`
}

// Posted is a post the server stored.
type Posted struct {
	Message uint64 `json:"message"`
	Thread  uint64 `json:"thread"`
	// Delivered counts its recipients.
	Delivered int `json:"delivered"`
}

// AgentParams names an agent.
type AgentParams struct {
	Name string `json:"name"`
}

// AgentPage is an agent's card, its reservations (grants first) and its
// live delegates' handles.
type AgentPage struct {
	Reservations []Reservation `json:"reservations"`
	Delegates    []string      `json:"delegates"`
	Agent        Agent         `json:"agent"`
}

// HistoryPost is a post an agent sent or received, with its conversation.
type HistoryPost struct {
	Conversation Thread `json:"conversation"`
	Post
}

// HistoryPage is one page of an agent's posts, newest first.
type HistoryPage struct {
	Posts []HistoryPost `json:"posts"`
	// Next is the before of the older page, absent on the oldest.
	Next uint64 `json:"next,omitempty"`
}

// EventChanged names the event stream's one event, whose data is Changed.
const EventChanged = "changed"

// Changed is the event stream's one event: what a commit wrote.
type Changed struct {
	// Conversations gained a post, had a post read, or had a post or
	// themselves expire; one that expired answers not_found.
	Conversations []uint64 `json:"conversations"`
	// Agents is set when an agent or a reservation changed.
	Agents bool `json:"agents"`
}

// Methods are the sign-in methods the owner has left on.
type Methods struct {
	Password bool `json:"password"`
	Passkeys bool `json:"passkeys"`
	// OIDC is single sign-on, which also needs the server's OIDC_* settings.
	OIDC bool `json:"oidc"`
}

// Settings are what the Settings screen changes, and the bounds it holds
// the history retention to.
type Settings struct {
	// RetentionMS is how long a thread, post or event lives after it is
	// written.
	RetentionMS    int64   `json:"retention_ms"`
	RetentionMinMS int64   `json:"retention_min_ms"`
	RetentionMaxMS int64   `json:"retention_max_ms"`
	Methods        Methods `json:"methods"`
	// OIDCConfigured is false when the server has no single sign-on
	// provider, so its switch does nothing.
	OIDCConfigured bool `json:"oidc_configured"`
}

// SettingsRequest replaces the settings. A change of methods needs a
// sign-in or step-up of the last 10 minutes.
type SettingsRequest struct {
	Methods     Methods `json:"methods"`
	RetentionMS int64   `json:"retention_ms"`
}

// SignIn is what the sign-in page shows beside access's state.
type SignIn struct {
	// OIDCLabel names the single sign-on provider; empty when the server
	// has none.
	OIDCLabel string `json:"oidc_label"`
}
