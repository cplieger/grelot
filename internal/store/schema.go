package store

import (
	"encoding/binary"
	"time"

	"github.com/cplieger/bolttable"
)

// namespace is the one bolttable namespace grelot stores everything in.
const namespace = "grelot"

// Table prefixes are stored bytes: changing one changes the namespace's
// format digest, and the next start resets the store.
const (
	prefixIdentities byte = 1
	prefixThreads    byte = 2
	prefixMessages   byte = 3
	prefixDeliveries byte = 4
)

// Identity states.
const (
	stateLive    = "live"
	stateDormant = "dormant"
)

// Delivery kinds that carry their text in the row, having no message.
const (
	kindIdentityExpiring = "identity_expiring"
)

// identityRow is one live or dormant identity, keyed by its id.
type identityRow struct {
	Lease lease `json:"lease"`
	// DormantSince is set while State is dormant.
	DormantSince time.Time     `json:"dormant_since,omitzero"`
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	State        string        `json:"state"`
	ID           uint64        `json:"id"`
	TTL          time.Duration `json:"ttl"`
	// Seq is the highest delivery seq written to this identity.
	Seq uint64 `json:"seq"`
}

// threadRow is one announcement, path or direct thread, keyed by its id.
type threadRow struct {
	Lease lease  `json:"lease"`
	Key   string `json:"key"`
	ID    uint64 `json:"id"`
}

// messageRow is one post, keyed pairKey(thread, msg).
type messageRow struct {
	Lease  lease  `json:"lease"`
	Body   string `json:"body"`
	Thread uint64 `json:"thread"`
	ID     uint64 `json:"id"`
}

// deliveryRow is one recipient's copy of a message or event, keyed
// pairKey(agent, seq).
type deliveryRow struct {
	Written time.Time `json:"written"`
	Lease   lease     `json:"lease"`
	Kind    string    `json:"kind"`
	Text    string    `json:"text,omitempty"`
	Agent   uint64    `json:"agent"`
	Seq     uint64    `json:"seq"`
}

type tables struct {
	identities *leased[uint64, identityRow]
	threads    *leased[uint64, threadRow]
	messages   *leased[[]byte, messageRow]
	deliveries *leased[[]byte, deliveryRow]
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
	t.leased = []sweeper{t.identities, t.threads, t.messages, t.deliveries}
	return s, &t, nil
}

func be64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

// pairKey is be64(a) followed by be64(b). The fixed width keeps a prefix
// scan on be64(a) from reaching another a's rows, and keeps b in numeric
// order under it.
func pairKey(a, b uint64) []byte { return binary.BigEndian.AppendUint64(be64(a), b) }
