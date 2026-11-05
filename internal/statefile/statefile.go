// Package statefile keeps the client's small state files, each replaced
// whole so a reader never sees half of one.
package statefile

import (
	"errors"
	"io"
	"io/fs"
	"os"
)

// Read returns file's content, at most limit bytes of it, and nil with no
// error when the file does not exist.
func Read(file string, limit int64) ([]byte, error) {
	f, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}
