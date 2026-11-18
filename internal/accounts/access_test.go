//go:build !windows

package accounts_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cplieger/access/accesstest"
	"github.com/cplieger/access/account"
	"github.com/cplieger/grelot/internal/accounts"
)

var errInjectedCommit = errors.New("injected commit failure")

// commitFault fails every run of the first write after arm, the reruns of
// its grouped commit included, as a failed bbolt commit would.
type commitFault struct {
	selected context.Context
	mu       sync.Mutex
	armed    bool
}

func (f *commitFault) arm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed = true
}

func (f *commitFault) ask(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.armed {
		f.armed, f.selected = false, ctx
	}
	if f.selected != nil && ctx == f.selected {
		return errInjectedCommit
	}
	return nil
}

func TestStore_keepsTheAccountStoreContract(t *testing.T) {
	var mu sync.Mutex
	paths := map[account.Store]string{}
	dbs := map[account.Store]*accounts.DB{}
	track := func(db *accounts.DB, path string) account.Store {
		mu.Lock()
		defer mu.Unlock()
		paths[db.Access()], dbs[db.Access()] = path, db
		return db.Access()
	}
	newStore := func(t *testing.T) account.Store {
		path := filepath.Join(t.TempDir(), "accounts.db")
		return track(open(t, path), path)
	}
	reopen := func(t *testing.T, old account.Store) account.Store {
		mu.Lock()
		db, path := dbs[old], paths[old]
		mu.Unlock()
		closeDB(t, db)
		return track(open(t, path), path)
	}
	newFaulty := func(t *testing.T) (account.Store, accesstest.DurabilityFaults) {
		f := &commitFault{}
		db, err := accounts.OpenFaulty(filepath.Join(t.TempDir(), "accounts.db"), f.ask)
		if err != nil {
			t.Fatalf("OpenFaulty: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		publishes := func() int {
			n, err := db.Commits(t.Context())
			if err != nil {
				t.Errorf("Commits: %v", err)
			}
			return int(n)
		}
		return db.Access(), accesstest.DurabilityFaults{BeforePublish: f.arm, Publishes: publishes}
	}

	accesstest.StoreContract(t, newStore, accesstest.ContractConfig{NewFaulty: newFaulty, Reopen: reopen})
}
