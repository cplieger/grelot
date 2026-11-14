//go:build !windows

package accounts

import (
	"context"

	"github.com/cplieger/bolttable"
)

// OpenFaulty opens path as Open does, with fault asked before each commit.
func OpenFaulty(path string, fault func(context.Context) error) (*DB, error) {
	return open(path, &bolttable.Options{CommitFault: fault})
}

// Commits counts the bbolt commits the file has made since it opened.
func (d *DB) Commits(ctx context.Context) (uint64, error) {
	st, err := d.db.Stats(ctx)
	return st.Commits, err
}
