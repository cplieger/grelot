//go:build !windows

package statefile

import (
	"context"

	"github.com/cplieger/go/filesafe"
)

// Replace writes data to file atomically, mode 0600, creating missing
// directories mode 0700. It is not cancellable: callers write it best
// effort inside an answer they have already received.
func Replace(file string, data []byte) error {
	_, err := filesafe.WriteFile(context.Background(), file, data,
		filesafe.WithMode(0o600), filesafe.WithMkdirMode(0o700))
	return err
}
