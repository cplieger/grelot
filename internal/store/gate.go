package store

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/bolttable"
)

// gateRefusal is the delegate gate on a request from me's session that
// names neither a handle nor Self. While that session holds an unused link
// or a live delegate registered from it, a subagent or step shares the
// session's id, so such a request may be one that forgot `as`: it is
// refused before it reads or moves anything of me's. A delegate bound to
// its own session holds nothing here, since its shell carries its own id.
func (s *Store) gateRefusal(r *bolttable.ReadTx, me *identityRow, now time.Time) error {
	links, err := s.linksOf(r, me.ID)
	if err != nil {
		return err
	}
	var handles []string
	for i := range links {
		if !links[i].Lease.due(now) {
			handles = append(handles, "@"+me.Name+"/"+links[i].Label)
		}
	}
	ids, err := s.delegatesOf(r, me.ID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		d, found, err := s.t.identities.table.Get(r, id)
		if err != nil {
			return err
		}
		if found && d.Session == "" && settled(&d, now, s.dormantTTL) != nil {
			handles = append(handles, "@"+d.Name)
		}
	}
	if len(handles) == 0 {
		return nil
	}
	slices.Sort(handles)
	return &Error{
		Kind: KindConflict, Why: "delegate_gate",
		Msg: fmt.Sprintf("this session has delegates (%s): a subagent or workflow step runs `grelot as <its handle> …`; only the agent that granted them adds --self",
			strings.Join(handles, ", ")),
	}
}
