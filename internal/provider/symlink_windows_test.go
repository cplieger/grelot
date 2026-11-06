package provider

import (
	"errors"
	"syscall"
)

// symlinkUnavailable is a Symlink error that means this user may not create
// links: Windows grants it only to administrators and in Developer Mode.
func symlinkUnavailable(err error) bool { return errors.Is(err, syscall.ERROR_PRIVILEGE_NOT_HELD) }
