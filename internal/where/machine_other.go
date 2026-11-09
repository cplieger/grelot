//go:build !linux && !darwin && !windows

package where

import "context"

// Machine is "" on a system with no machine source.
func Machine(context.Context) string { return "" }

// InContainer is false: the container check reads Linux marker files.
func InContainer() bool { return false }
