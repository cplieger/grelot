//go:build !windows

package store

import (
	"encoding/binary"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/cplieger/bolttable"
)

// The bolttable namespaces: the keyed writes' first results, and
// everything else.
const (
	namespace         = "grelot"
	requestsNamespace = "requests"
)

// Table prefixes are stored bytes: changing one changes the namespace's
// format digest, and the next start resets the store.
const (
	prefixIdentities   byte = 1
	prefixThreads      byte = 2
	prefixMessages     byte = 3
	prefixDeliveries   byte = 4
	prefixReservations byte = 5
	prefixLinks        byte = 6
	prefixWatches      byte = 7
	prefixRequests     byte = 8
)

// Identity states.
const (
	stateLive    = "live"
	stateDormant = "dormant"
)

// Thread kinds, which are also the kinds of the deliveries their posts
// write.
const (
	kindAnnounce = "announce"
	kindPath     = "path"
	kindDirect   = "direct"
	kindCards    = "cards"
)

// Card kinds: the cards thread's messages and their deliveries.
const (
	kindJoin = "join"
	kindLeft = "left"
)

// Delivery kinds that carry their text in the row, having no message.
const (
	kindIdentityExpiring    = "identity_expiring"
	kindReservationGranted  = "reservation_granted"
	kindReservationExpiring = "reservation_expiring"
	kindReservationExpired  = "reservation_expired"
	kindReceiptsComplete    = "receipts_complete"
	kindReceiptsDue         = "receipts_due"
)

// Reservation states: a grant, or a request waiting in the queue.
const (
	reservationHeld   = "held"
	reservationQueued = "queued"
)

// identityRow is one live or dormant identity, keyed by its id.
type identityRow struct {
	// Scope is a delegate's, set with Parent.
	Scope        *scope    `json:"scope,omitempty"`
	Lease        lease     `json:"lease"`
	RegisteredAt time.Time `json:"registered_at"`
	// DormantSince is set while State is dormant.
	DormantSince time.Time `json:"dormant_since,omitzero"`
	LastSeen     time.Time `json:"last_seen,omitzero"`
	NudgedAt     time.Time `json:"nudged_at,omitzero"`
	Where        Where     `json:"where"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	State        string    `json:"state"`
	Host         string    `json:"host"`
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	Provider     string    `json:"provider"`
	// Session is empty on a dormant row a fresh registration detached.
	Session  string        `json:"session,omitempty"`
	Proc     string        `json:"proc,omitempty"`
	Paths    []string      `json:"paths"`
	Follows  []string      `json:"follows,omitempty"`
	Unread   unreadSummary `json:"unread"`
	LastPage seqRange      `json:"last_page"`
	ID       uint64        `json:"id"`
	// Seq is the highest delivery seq written to this identity.
	Seq    uint64        `json:"seq"`
	Read   uint64        `json:"read"`
	Shown  uint64        `json:"shown"`
	Nudged uint64        `json:"nudged"`
	TTL    time.Duration `json:"ttl"`
	Nudges int           `json:"nudges"`
	// Parent is a delegate's parent identity, 0 on any other row.
	Parent uint64 `json:"parent,omitempty"`
	// Key is the machine key that registered the identity, or its
	// parent's; 0 on @user.
	Key int64 `json:"key,omitempty"`
	// Threaded is set for good by the identity's first follow or path
	// post, whatever it unfollows later.
	Threaded bool `json:"threaded,omitempty"`
	// Ended is the SessionEnd report, cleared by the session's next
	// request other than the standing wait.
	Ended bool `json:"ended,omitempty"`
}

// scope is what a delegate may post in and receive, fixed at the grant.
// Agents are identity ids, resolved at the grant; a deny wins over an
// allow.
type scope struct {
	AllowThreads []string `json:"allow_threads,omitempty"`
	DenyThreads  []string `json:"deny_threads,omitempty"`
	AllowAgents  []uint64 `json:"allow_agents,omitempty"`
	DenyAgents   []uint64 `json:"deny_agents,omitempty"`
	Announce     bool     `json:"announce,omitempty"`
	DenyAnnounce bool     `json:"deny_announce,omitempty"`
}

// linkRow is one unused delegate link, keyed pairKey(parent, id). Only the
// link's SHA-256 is stored; using it deletes the row.
type linkRow struct {
	Lease  lease         `json:"lease"`
	Label  string        `json:"label"`
	Hash   []byte        `json:"hash"`
	Scope  scope         `json:"scope"`
	Parent uint64        `json:"parent"`
	ID     uint64        `json:"id"`
	TTL    time.Duration `json:"ttl"`
}

// seqRange is an inclusive range of delivery seqs.
type seqRange struct {
	From uint64 `json:"from"`
	To   uint64 `json:"to"`
}

// threadRow is one announcement, path or direct thread, keyed by its id.
type threadRow struct {
	Lease lease  `json:"lease"`
	Kind  string `json:"kind"`
	// Key is unique: announceKey, the path, or directKey of the pair.
	Key string `json:"key"`
	// Path is a path thread's path.
	Path string `json:"path,omitempty"`
	ID   uint64 `json:"id"`
}

// messageRow is one post, keyed pairKey(thread, msg). It keeps the
// sender's labels and where as they stood at the post, so a reader's
// relation to the sender outlives the sender's row.
type messageRow struct {
	PostedAt   time.Time `json:"posted_at"`
	Lease      lease     `json:"lease"`
	Kind       string    `json:"kind"`
	Path       string    `json:"path,omitempty"`
	Subject    string    `json:"subject"`
	Body       string    `json:"body"`
	SenderName string    `json:"sender_name"`
	Host       string    `json:"host"`
	OS         string    `json:"os"`
	Arch       string    `json:"arch"`
	Where      Where     `json:"where"`
	Thread     uint64    `json:"thread"`
	ID         uint64    `json:"id"`
	Sender     uint64    `json:"sender"`
}

// deliveryRow is one recipient's copy of a message or event, keyed
// pairKey(agent, seq).
type deliveryRow struct {
	Written time.Time `json:"written"`
	ReadAt  time.Time `json:"read_at,omitzero"`
	Lease   lease     `json:"lease"`
	Kind    string    `json:"kind"`
	Text    string    `json:"text,omitempty"`
	// Path is the work path or follow that matched a path-thread post,
	// the most specific one.
	Path string `json:"path,omitempty"`
	// Name is the recipient's name at fan-out.
	Name string `json:"name,omitempty"`
	// Via is the handle of the delegate a parent's copy is for; a copy is
	// quiet and is not a recipient of its message.
	Via   string `json:"via,omitempty"`
	Agent uint64 `json:"agent"`
	// Quiet is a path post's delivery to a recipient it did not address.
	Quiet bool `json:"quiet,omitempty"`
	// Watch is the receipt watch waiting for this recipient's read, if any.
	Watch uint64 `json:"watch,omitempty"`
	// Parent is a delegate recipient's parent at fan-out, which receipts
	// still need once the delegate's row is gone.
	Parent  uint64 `json:"parent,omitempty"`
	Seq     uint64 `json:"seq"`
	Message uint64 `json:"message,omitempty"`
	Thread  uint64 `json:"thread,omitempty"`
	Sender  uint64 `json:"sender,omitempty"`
}

// reservationRow is one grant or queue entry, keyed by its id. A queued
// row has no lease: it keeps its TTL as a duration, counted from its
// grant, and ends with its holder.
type reservationRow struct {
	Lease lease `json:"lease"`
	// Since is when the row was queued or, once held, granted.
	Since  time.Time     `json:"since"`
	Path   string        `json:"path"`
	Reason string        `json:"reason"`
	State  string        `json:"state"`
	Holder uint64        `json:"holder"`
	ID     uint64        `json:"id"`
	TTL    time.Duration `json:"ttl"`
}

// watchRow is one receipt watch, keyed by its id: the recipients of a
// post still to read it, and how many have read or retired since.
type watchRow struct {
	Lease   lease          `json:"lease"`
	Subject string         `json:"subject"`
	Pending []watchPending `json:"pending"`
	Message uint64         `json:"message"`
	Sender  uint64         `json:"sender"`
	ID      uint64         `json:"id"`
	Total   int            `json:"total"`
	Read    int            `json:"read"`
	Retired int            `json:"retired"`
	// By is a --by watch, whose expiry writes receipts_due.
	By bool `json:"by,omitempty"`
}

// watchPending is a recipient a watch still waits for, named as at the
// fan-out.
type watchPending struct {
	Name string `json:"name"`
	ID   uint64 `json:"id"`
}

type tables struct {
	identities *leased[uint64, identityRow]
	byName     *bolttable.Index[uint64, identityRow]
	bySession  *bolttable.Index[uint64, identityRow]
	byPath     *bolttable.Index[uint64, identityRow]
	byFollow   *bolttable.Index[uint64, identityRow]
	byProc     *bolttable.Index[uint64, identityRow]
	byParent   *bolttable.Index[uint64, identityRow]
	// byAllow indexes a delegate's allowed path threads.
	byAllow    *bolttable.Index[uint64, identityRow]
	threads    *leased[uint64, threadRow]
	threadKey  *bolttable.Index[uint64, threadRow]
	messages   *leased[[]byte, messageRow]
	bySender   *bolttable.Index[[]byte, messageRow]
	messageID  *bolttable.Index[[]byte, messageRow]
	deliveries *leased[[]byte, deliveryRow]
	// byMessage lists a post's deliveries, its parents' copies included.
	byMessage  *bolttable.Index[[]byte, deliveryRow]
	resv       *leased[uint64, reservationRow]
	resvPath   *bolttable.Index[uint64, reservationRow]
	resvHolder *bolttable.Index[uint64, reservationRow]
	resvQueue  *bolttable.Index[uint64, reservationRow]
	links      *leased[[]byte, linkRow]
	linkHash   *bolttable.Index[[]byte, linkRow]
	watches    *leased[uint64, watchRow]
	watchBy    *bolttable.Index[uint64, watchRow]
	requests   *leased[[]byte, requestRow]
	// leased lists every table above, in the order the sweep walks them.
	leased []sweeper
}

// newSchema declares the store's schema and its tables.
func newSchema() (*bolttable.Schema, *tables, error) {
	s := bolttable.NewSchema("grelot")
	ns, err := s.Namespace(namespace)
	if err != nil {
		return nil, nil, err
	}
	silent := bolttable.TableOptions{Silent: true}
	var t tables
	// Version 4 resets an older store, whose identities name no Key.
	ids, err := bolttable.Define(ns, prefixIdentities, "identities",
		bolttable.Uint64Key{}, bolttable.JSON[identityRow](4), silent)
	if err != nil {
		return nil, nil, err
	}
	threads, err := bolttable.Define(ns, prefixThreads, "threads",
		bolttable.Uint64Key{}, bolttable.JSON[threadRow](1), silent)
	if err != nil {
		return nil, nil, err
	}
	messages, err := bolttable.Define(ns, prefixMessages, "messages",
		bolttable.BytesKey{}, bolttable.JSON[messageRow](1), silent)
	if err != nil {
		return nil, nil, err
	}
	deliveries, err := bolttable.Define(ns, prefixDeliveries, "deliveries",
		bolttable.BytesKey{}, bolttable.JSON[deliveryRow](1), silent)
	if err != nil {
		return nil, nil, err
	}
	err = bolttable.CascadePrefix(threads, messages, 1, be64)
	if err != nil {
		return nil, nil, err
	}
	if ierr := defineIdentityIndexes(ids, &t); ierr != nil {
		return nil, nil, ierr
	}
	if t.threadKey, err = bolttable.DefineIndex(threads, "threads.key", bolttable.IndexOptions{Unique: true, Version: 1},
		func(_ uint64, v *threadRow) []bolttable.Entry { return keyEntries(v.Key) }); err != nil {
		return nil, nil, err
	}
	if err := defineMessageIndexes(&t, messages, deliveries); err != nil {
		return nil, nil, err
	}
	if err := defineCoreLeases(&t, ids, threads, messages, deliveries); err != nil {
		return nil, nil, err
	}
	if err := defineReservations(ns, &t); err != nil {
		return nil, nil, err
	}
	if err := defineLinks(ns, ids, &t); err != nil {
		return nil, nil, err
	}
	if err := defineWatches(ns, &t); err != nil {
		return nil, nil, err
	}
	if err := defineRequests(s, &t); err != nil {
		return nil, nil, err
	}
	// Identities go first: an identity's expiry hands off its grants and
	// deletes its links itself, before their own sweeps would reach them
	// one by one.
	t.leased = []sweeper{t.identities, t.links, t.resv, t.watches, t.threads, t.messages, t.deliveries, t.requests}
	return s, &t, nil
}

// defineCoreLeases declares the leases of identities, threads, messages
// and deliveries.
func defineCoreLeases(t *tables, ids *bolttable.Table[uint64, identityRow], threads *bolttable.Table[uint64, threadRow],
	messages *bolttable.Table[[]byte, messageRow], deliveries *bolttable.Table[[]byte, deliveryRow],
) error {
	var err error
	if t.identities, err = defineLeased(ids, identityPolicy, func(v *identityRow) *lease { return &v.Lease }); err != nil {
		return err
	}
	if t.threads, err = defineLeased(threads, threadPolicy, func(v *threadRow) *lease { return &v.Lease }); err != nil {
		return err
	}
	if t.messages, err = defineLeased(messages, messagePolicy, func(v *messageRow) *lease { return &v.Lease }); err != nil {
		return err
	}
	t.deliveries, err = defineLeased(deliveries, deliveryPolicy, func(v *deliveryRow) *lease { return &v.Lease })
	return err
}

// defineMessageIndexes declares the sender index history walks, the
// lookup by message id, and the deliveries by message receipts walk.
func defineMessageIndexes(t *tables, messages *bolttable.Table[[]byte, messageRow], deliveries *bolttable.Table[[]byte, deliveryRow]) error {
	var err error
	if t.bySender, err = bolttable.DefineIndex(messages, "messages.sender", bolttable.IndexOptions{Version: 1}, senderEntries); err != nil {
		return err
	}
	if t.messageID, err = bolttable.DefineIndex(messages, "messages.id", bolttable.IndexOptions{Unique: true, Version: 1},
		func(_ []byte, v *messageRow) []bolttable.Entry { return []bolttable.Entry{{Key: be64(v.ID)}} }); err != nil {
		return err
	}
	t.byMessage, err = bolttable.DefineIndex(deliveries, "deliveries.message", bolttable.IndexOptions{Version: 1},
		func(_ []byte, v *deliveryRow) []bolttable.Entry {
			if !isPost(v.Kind) {
				return nil
			}
			return []bolttable.Entry{{Key: be64(v.Message)}}
		})
	return err
}

// senderEntries keys a post by pairKey(sender, posted_at in Unix ns), so
// a reverse scan under be64(sender) walks the sender's posts newest
// first. Cards are left out: their sender is the identity a card is about.
func senderEntries(_ []byte, v *messageRow) []bolttable.Entry {
	if !isPost(v.Kind) {
		return nil
	}
	return []bolttable.Entry{{Key: pairKey(v.Sender, uint64(v.PostedAt.UnixNano()))}}
}

// senderPostedAt is the post time a senderEntries key carries.
func senderPostedAt(ikey []byte) (time.Time, error) {
	if len(ikey) != 16 {
		return time.Time{}, fmt.Errorf("store: sender index key %x is not 16 bytes", ikey)
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(ikey[8:]))), nil //nolint:gosec // the key holds a UnixNano written from an int64
}

// isPost reports whether a message or delivery kind is an agent's post.
func isPost(kind string) bool {
	return kind == kindAnnounce || kind == kindPath || kind == kindDirect
}

// defineIdentityIndexes declares the name and session lookups, which a
// dormant row keeps, the work-path, follow and allowed-thread indexes
// fan-out reads, the proc index the take-over hint reads, and the parent
// and bound-agent indexes.
func defineIdentityIndexes(ids *bolttable.Table[uint64, identityRow], t *tables) error {
	unique := bolttable.IndexOptions{Unique: true, Version: 1}
	many := bolttable.IndexOptions{Version: 1}
	var err error
	if t.byName, err = bolttable.DefineIndex(ids, "identities.name", unique,
		func(_ uint64, v *identityRow) []bolttable.Entry { return keyEntries(v.Name) }); err != nil {
		return err
	}
	if t.bySession, err = bolttable.DefineIndex(ids, "identities.session", unique,
		func(_ uint64, v *identityRow) []bolttable.Entry { return keyEntries(v.Session) }); err != nil {
		return err
	}
	if t.byPath, err = bolttable.DefineIndex(ids, "identities.paths", many,
		func(_ uint64, v *identityRow) []bolttable.Entry { return keyEntries(v.Paths...) }); err != nil {
		return err
	}
	if t.byFollow, err = bolttable.DefineIndex(ids, "identities.follows", many,
		func(_ uint64, v *identityRow) []bolttable.Entry { return keyEntries(v.Follows...) }); err != nil {
		return err
	}
	if t.byProc, err = bolttable.DefineIndex(ids, "identities.proc", many,
		func(_ uint64, v *identityRow) []bolttable.Entry { return keyEntries(v.Proc) }); err != nil {
		return err
	}
	if t.byAllow, err = bolttable.DefineIndex(ids, "identities.allow", many,
		func(_ uint64, v *identityRow) []bolttable.Entry {
			if v.Scope == nil {
				return nil
			}
			return keyEntries(v.Scope.AllowThreads...)
		}); err != nil {
		return err
	}
	t.byParent, err = bolttable.DefineIndex(ids, "identities.parent", many,
		func(_ uint64, v *identityRow) []bolttable.Entry {
			if v.Parent == 0 {
				return nil
			}
			return []bolttable.Entry{{Key: be64(v.Parent)}}
		})
	return err
}

// defineLinks declares the unused delegate links, their lease and the
// lookup by hash a registration reads. A deleted identity's links go by
// cascade; an expiry keeps the row, so it deletes them itself.
func defineLinks(ns *bolttable.Namespace, ids *bolttable.Table[uint64, identityRow], t *tables) error {
	links, err := bolttable.Define(ns, prefixLinks, "links",
		bolttable.BytesKey{}, bolttable.JSON[linkRow](1), bolttable.TableOptions{Silent: true})
	if err != nil {
		return err
	}
	if cerr := bolttable.CascadePrefix(ids, links, 1, be64); cerr != nil {
		return cerr
	}
	if t.links, err = defineLeased(links, linkPolicy, func(v *linkRow) *lease { return &v.Lease }); err != nil {
		return err
	}
	t.linkHash, err = bolttable.DefineIndex(links, "links.hash", bolttable.IndexOptions{Unique: true, Version: 1},
		func(_ []byte, v *linkRow) []bolttable.Entry { return []bolttable.Entry{{Key: v.Hash}} })
	return err
}

// defineWatches declares the receipt watches, their lease and the lookup
// by sender its retirement or purge reads.
func defineWatches(ns *bolttable.Namespace, t *tables) error {
	watches, err := bolttable.Define(ns, prefixWatches, "receipt_watches",
		bolttable.Uint64Key{}, bolttable.JSON[watchRow](1), bolttable.TableOptions{Silent: true})
	if err != nil {
		return err
	}
	if t.watches, err = defineLeased(watches, watchPolicy, func(v *watchRow) *lease { return &v.Lease }); err != nil {
		return err
	}
	t.watchBy, err = bolttable.DefineIndex(watches, "receipt_watches.sender", bolttable.IndexOptions{Version: 1},
		func(_ uint64, v *watchRow) []bolttable.Entry { return []bolttable.Entry{{Key: be64(v.Sender)}} })
	return err
}

// defineRequests declares the keyed writes' first results and their lease,
// in a namespace of their own, so neither one's format change resets the
// other's rows.
func defineRequests(s *bolttable.Schema, t *tables) error {
	ns, err := s.Namespace(requestsNamespace)
	if err != nil {
		return err
	}
	requests, err := bolttable.Define(ns, prefixRequests, "requests",
		bolttable.BytesKey{}, bolttable.JSON[requestRow](1), bolttable.TableOptions{Silent: true})
	if err != nil {
		return err
	}
	t.requests, err = defineLeased(requests, requestPolicy, func(v *requestRow) *lease { return &v.Lease })
	return err
}

// defineReservations declares the reservations table, its lease and its
// lookups by path, which overlap checks and fan-out read, by holder, and
// the queue in enqueue order. Ids grow, so a queued row's id is its place
// in the queue.
func defineReservations(ns *bolttable.Namespace, t *tables) error {
	resv, err := bolttable.Define(ns, prefixReservations, "reservations",
		bolttable.Uint64Key{}, bolttable.JSON[reservationRow](1), bolttable.TableOptions{Silent: true})
	if err != nil {
		return err
	}
	if t.resv, err = defineLeased(resv, reservationPolicy, func(v *reservationRow) *lease { return &v.Lease }); err != nil {
		return err
	}
	many := bolttable.IndexOptions{Version: 1}
	if t.resvPath, err = bolttable.DefineIndex(resv, "reservations.path", many,
		func(_ uint64, v *reservationRow) []bolttable.Entry { return keyEntries(v.Path) }); err != nil {
		return err
	}
	if t.resvHolder, err = bolttable.DefineIndex(resv, "reservations.holder", many,
		func(_ uint64, v *reservationRow) []bolttable.Entry { return []bolttable.Entry{{Key: be64(v.Holder)}} }); err != nil {
		return err
	}
	t.resvQueue, err = bolttable.DefineIndex(resv, "reservations.queue", many,
		func(id uint64, v *reservationRow) []bolttable.Entry {
			if v.State != reservationQueued {
				return nil
			}
			return []bolttable.Entry{{Key: be64(id)}}
		})
	return err
}

// scanExact calls fn for each row whose entry in a non-unique index is
// exactly key, never a longer one such as a subpath's.
func scanExact[V any](r *bolttable.ReadTx, ix *bolttable.Index[uint64, V], key string, fn func(id uint64) (bool, error)) error {
	return ix.Scan(r, bolttable.Exact([]byte(key)), func(_ []byte, id uint64, _ []byte) (bool, error) {
		return fn(id)
	})
}

// overlapIDs lists, in id order, the rows of a path index with a path at,
// above or under p.
func overlapIDs[V any](r *bolttable.ReadTx, ix *bolttable.Index[uint64, V], p string) ([]uint64, error) {
	set := map[uint64]bool{}
	add := func(id uint64) (bool, error) {
		set[id] = true
		return true, nil
	}
	above := ancestors(p)
	for _, a := range above[:len(above)-1] {
		if err := scanExact(r, ix, a, add); err != nil {
			return nil, err
		}
	}
	// The prefix also matches a name-prefix sibling such as p-old, which
	// under drops.
	err := ix.Scan(r, bolttable.Range{Prefix: []byte(p)}, func(ikey []byte, id uint64, _ []byte) (bool, error) {
		if under(string(ikey), p) {
			set[id] = true
		}
		return true, nil
	})
	return slices.Sorted(maps.Keys(set)), err
}

// keyEntries is one index entry per non-empty key.
func keyEntries(keys ...string) []bolttable.Entry {
	var out []bolttable.Entry
	for _, k := range keys {
		if k != "" {
			out = append(out, bolttable.Entry{Key: []byte(k)})
		}
	}
	return out
}

func be64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

// pairKey is be64(a) followed by be64(b). The fixed width keeps a prefix
// scan on be64(a) from reaching another a's rows, and keeps b in numeric
// order under it.
func pairKey(a, b uint64) []byte { return binary.BigEndian.AppendUint64(be64(a), b) }
