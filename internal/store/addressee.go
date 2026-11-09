//go:build !windows

package store

import (
	"context"
	"strings"

	"github.com/cplieger/bolttable"
)

// Addressee is what a host push to an identity is addressed by: its
// provider and host label choose the door, Session is the host's own
// session id, without the "<provider>:" prefix grelot stores.
type Addressee struct {
	Name     string
	Provider string
	Host     string
	Session  string
	ID       uint64
}

// Addressee returns identity id's addressee. found is false unless id is
// a live identity other than a delegate, the only kind a push serves.
func (s *Store) Addressee(ctx context.Context, id uint64) (a Addressee, found bool, err error) {
	err = s.read(ctx, func(r *bolttable.ReadTx) error {
		row, ok, gerr := s.t.identities.table.Get(r, id)
		if gerr != nil || !ok {
			return gerr
		}
		a, found = s.addressee(&row)
		return nil
	})
	return a, found, err
}

// Addressees returns, in id order, every identity Addressee finds.
func (s *Store) Addressees(ctx context.Context) ([]Addressee, error) {
	out := []Addressee{}
	err := s.read(ctx, func(r *bolttable.ReadTx) error {
		return s.t.identities.table.Scan(r, bolttable.Range{}, func(_ uint64, v *identityRow) (bool, error) {
			if a, ok := s.addressee(v); ok {
				out = append(out, a)
			}
			return true, nil
		})
	})
	return out, err
}

func (s *Store) addressee(row *identityRow) (Addressee, bool) {
	v := settled(row, s.now(), s.dormantTTL)
	if v == nil || v.State != stateLive || v.Parent != 0 {
		return Addressee{}, false
	}
	_, session, _ := strings.Cut(v.Session, ":")
	return Addressee{Name: v.Name, Provider: v.Provider, Host: v.Host, Session: session, ID: v.ID}, true
}
