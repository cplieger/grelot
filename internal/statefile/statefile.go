// Package statefile keeps the client's small state files, each replaced
// whole so a reader never sees half of one.
package statefile

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Replace writes data to file through a temporary file beside it, which
// os.CreateTemp makes mode 0600, then renames it over file. It creates the
// directory, mode 0700, when it is missing.
func Replace(file string, data []byte) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(file)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Rename(tmp.Name(), file)
}

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
