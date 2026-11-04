package store

import (
	"encoding/binary"

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

// identityRow is one live or dormant identity, keyed by its id.
type identityRow struct {
	Name string `json:"name"`
	ID   uint64 `json:"id"`
}

// threadRow is one announcement, path or direct thread, keyed by its id.
type threadRow struct {
	Key string `json:"key"`
	ID  uint64 `json:"id"`
}

// messageRow is one post, keyed pairKey(thread, msg).
type messageRow struct {
	Body   string `json:"body"`
	Thread uint64 `json:"thread"`
	ID     uint64 `json:"id"`
}

// deliveryRow is one recipient's copy of a message or event, keyed
// pairKey(agent, seq).
type deliveryRow struct {
	Agent uint64 `json:"agent"`
	Seq   uint64 `json:"seq"`
}

type tables struct {
	identities *bolttable.Table[uint64, identityRow]
	threads    *bolttable.Table[uint64, threadRow]
	messages   *bolttable.Table[[]byte, messageRow]
	deliveries *bolttable.Table[[]byte, deliveryRow]
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
	if t.identities, err = bolttable.Define(ns, prefixIdentities, "identities",
		bolttable.Uint64Key{}, bolttable.JSON[identityRow](1), silent); err != nil {
		return nil, nil, err
	}
	if t.threads, err = bolttable.Define(ns, prefixThreads, "threads",
		bolttable.Uint64Key{}, bolttable.JSON[threadRow](1), silent); err != nil {
		return nil, nil, err
	}
	if t.messages, err = bolttable.Define(ns, prefixMessages, "messages",
		bolttable.BytesKey{}, bolttable.JSON[messageRow](1), silent); err != nil {
		return nil, nil, err
	}
	if t.deliveries, err = bolttable.Define(ns, prefixDeliveries, "deliveries",
		bolttable.BytesKey{}, bolttable.JSON[deliveryRow](1), silent); err != nil {
		return nil, nil, err
	}
	return s, &t, nil
}

// pairKey is be64(a) followed by be64(b). The fixed width keeps a prefix
// scan on be64(a) from reaching another a's rows, and keeps b in numeric
// order under it.
func pairKey(a, b uint64) []byte {
	k := make([]byte, 0, 16)
	k = binary.BigEndian.AppendUint64(k, a)
	return binary.BigEndian.AppendUint64(k, b)
}
