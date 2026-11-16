//go:build !windows

package accounts

import (
	"context"
	"errors"

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

// WriteSecrets creates an accounts file at path in the current format
// whose secrets row is s, whatever its key lengths.
func WriteSecrets(path string, s Secrets) error {
	schema, t, _, err := newSchema()
	if err != nil {
		return err
	}
	db, err := bolttable.Open(path, schema, bolttable.Options{})
	if err != nil {
		return err
	}
	row := secretsRow(s)
	err = db.Update(context.Background(), func(tx *bolttable.Tx) error { return t.secrets.Put(tx, rowKey, &row) })
	return errors.Join(err, db.Close())
}
