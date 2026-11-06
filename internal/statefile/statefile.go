// Package statefile keeps the client's small state files, each replaced
// whole so a reader never sees half of one.
package statefile

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/cplieger/go/filesafe"
)

// Replace writes data to file atomically, creating missing directories. Off
// Windows the file is mode 0600 and new directories 0700; on Windows they
// inherit the directory's ACL. It is not cancellable: callers write it best
// effort inside an answer they have already received.
func Replace(file string, data []byte) error {
	var opts []filesafe.Option
	if runtime.GOOS == "windows" {
		// filesafe's mode options return ErrModeUnsupported on Windows,
		// where Go's chmod only sets the read-only attribute.
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			return err
		}
	} else {
		opts = []filesafe.Option{filesafe.WithMode(0o600), filesafe.WithMkdirMode(0o700)}
	}
	_, err := filesafe.WriteFile(context.Background(), file, data, opts...)
	return err
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
