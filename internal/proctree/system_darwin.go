package proctree

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// System is the running system's process table.
func System() Table { return darwinTable{} }

type darwinTable struct{}

func (darwinTable) Lookup(pid int) (Process, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return Process{}, fmt.Errorf("process %d: %w", pid, err)
	}
	start := kp.Proc.P_starttime
	if start.Sec < 0 || start.Usec < 0 {
		return Process{}, fmt.Errorf("process %d: start time before 1970", pid)
	}
	p := Process{
		PID:   pid,
		PPID:  int(kp.Eproc.Ppid),
		Name:  unix.ByteSliceToString(kp.Proc.P_comm[:]),
		Start: uint64(start.Sec)*1_000_000 + uint64(start.Usec),
	}
	if raw, err := unix.SysctlRaw("kern.procargs2", pid); err == nil {
		p.Args = parseProcargs2(raw)
	}
	return p, nil
}
