//go:build !linux && !darwin && !windows

package proctree

import "errors"

// System is the running system's process table, which this system does not
// expose: every lookup fails, so no ancestor is ever named.
func System() Table { return noTable{} }

type noTable struct{}

func (noTable) Lookup(int) (Process, error) {
	return Process{}, errors.New("no process table on this system")
}
