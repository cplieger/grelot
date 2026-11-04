package store

import (
	"encoding/binary"
	"maps"
	"slices"
	"time"

	"github.com/cplieger/bolttable"
)

// namespace is the one bolttable namespace grelot stores everything in.
const namespace = "grelot"

// Table prefixes are stored bytes: changing one changes the namespace's
// format digest, and the next start resets the store.
const (
	prefixIdentities   byte = 1
	prefixThreads      byte = 2
	prefixMessages     byte = 3
	prefixDeliveries   byte = 4
	prefixReservations byte = 5
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
)

// Reservation states: a grant, or a request waiting in the queue.
const (
	reservationHeld   = "held"
	reservationQueued = "queued"
)

// identityRow is one live or dormant identity, keyed by its id.
type identityRow struct {
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
	Session  string   `json:"session,omitempty"`
	Proc     string   `json:"proc,omitempty"`
	Paths    []string `json:"paths"`
	Follows  []string `json:"follows,omitempty"`
	LastPage seqRange `json:"last_page"`
	ID       uint64   `json:"id"`
	// Seq is the highest delivery seq written to this identity.
	Seq    uint64        `json:"seq"`
	Read   uint64        `json:"read"`
	Shown  uint64        `json:"shown"`
	Nudged uint64        `json:"nudged"`
	TTL    time.Duration `json:"ttl"`
	Nudges int           `json:"nudges"`
	// Ended is the SessionEnd report, cleared by the session's next
	// request other than the standing wait.
	Ended bool `json:"ended,omitempty"`
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
	Name    string `json:"name,omitempty"`
	Agent   uint64 `json:"agent"`
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

type tables struct {
	identities *leased[uint64, identityRow]
	byName     *bolttable.Index[uint64, identityRow]
	bySession  *bolttable.Index[uint64, identityRow]
	byPath     *bolttable.Index[uint64, identityRow]
	byFollow   *bolttable.Index[uint64, identityRow]
	byProc     *bolttable.Index[uint64, identityRow]
	threads    *leased[uint64, threadRow]
	threadKey  *bolttable.Index[uint64, threadRow]
	messages   *leased[[]byte, messageRow]
	bySender   *bolttable.Index[[]byte, messageRow]
	deliveries *leased[[]byte, deliveryRow]
	resv       *leased[uint64, reservationRow]
	resvPath   *bolttable.Index[uint64, reservationRow]
	resvHolder *bolttable.Index[uint64, reservationRow]
	resvQueue  *bolttable.Index[uint64, reservationRow]
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
	ids, err := bolttable.Define(ns, prefixIdentities, "identities",
		bolttable.Uint64Key{}, bolttable.JSON[identityRow](1), silent)
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
	if t.bySender, err = bolttable.DefineIndex(messages, "messages.sender", bolttable.IndexOptions{Version: 1}, senderEntries); err != nil {
		return nil, nil, err
	}
	if t.identities, err = defineLeased(ids, identityPolicy, func(v *identityRow) *lease { return &v.Lease }); err != nil {
		return nil, nil, err
	}
	if t.threads, err = defineLeased(threads, threadPolicy, func(v *threadRow) *lease { return &v.Lease }); err != nil {
		return nil, nil, err
	}
	if t.messages, err = defineLeased(messages, messagePolicy, func(v *messageRow) *lease { return &v.Lease }); err != nil {
		return nil, nil, err
	}
	if t.deliveries, err = defineLeased(deliveries, deliveryPolicy, func(v *deliveryRow) *lease { return &v.Lease }); err != nil {
		return nil, nil, err
	}
	if err := defineReservations(ns, &t); err != nil {
		return nil, nil, err
	}
	// Identities go first: an identity's expiry hands off its grants
	// itself, before the reservation sweep would expire them one by one.
	t.leased = []sweeper{t.identities, t.resv, t.threads, t.messages, t.deliveries}
	return s, &t, nil
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

// isPost reports whether a message or delivery kind is an agent's post.
func isPost(kind string) bool {
	return kind == kindAnnounce || kind == kindPath || kind == kindDirect
}

// defineIdentityIndexes declares the name and session lookups, which a
// dormant row keeps, the work-path and follow indexes fan-out reads, and
// the proc index the take-over hint reads.
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
	t.byProc, err = bolttable.DefineIndex(ids, "identities.proc", many,
		func(_ uint64, v *identityRow) []bolttable.Entry { return keyEntries(v.Proc) })
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
// exactly key. A Range prefix alone also matches longer keys, such as a
// subpath's; an exact key's entries sort before them, so the scan stops
// at the first longer one.
func scanExact[V any](r *bolttable.ReadTx, ix *bolttable.Index[uint64, V], key string, fn func(id uint64) (bool, error)) error {
	return ix.Scan(r, bolttable.Range{Prefix: []byte(key)}, func(ikey []byte, id uint64, _ []byte) (bool, error) {
		if string(ikey) != key {
			return false, nil
		}
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
