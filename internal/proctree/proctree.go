// Package proctree reads the process's ancestors, which name the host CLI a
// hook runs under for provider detection and the take-over hint's proc.
package proctree

import (
	"slices"
	"strconv"
)

// maxDepth bounds the walk; a host CLI sits a few levels above its hooks.
const maxDepth = 64

// Process is one entry of the process table.
type Process struct {
	// Name is the executable's name as the system lists it: comm on Linux,
	// p_comm on macOS, the image file name lowercased on Windows, whose file
	// names fold case.
	Name string
	// Args is the command line, argv[0] first, nil when it cannot be read.
	Args      []string
	PID, PPID int
	// Start is the start time in the system's own unit. It orders processes
	// of one table and, with PID, names a process across pid reuse.
	Start uint64
}

// Table looks processes up by pid.
type Table interface {
	Lookup(pid int) (Process, error)
}

// Ancestors answers pid's ancestors, nearest first. The walk ends at the
// first parent it cannot read, a pid seen before, or a parent that started
// after its child, which is a reused pid and not the real parent.
func Ancestors(t Table, pid int) []Process {
	child, err := t.Lookup(pid)
	if err != nil {
		return nil
	}
	seen := map[int]bool{pid: true}
	var out []Process
	for len(out) < maxDepth && child.PPID > 0 && !seen[child.PPID] {
		p, err := t.Lookup(child.PPID)
		if err != nil || p.Start > child.Start {
			break
		}
		seen[p.PID] = true
		out = append(out, p)
		child = p
	}
	return out
}

// Names answers the ancestors' names, nearest first, as provider detection
// takes them.
func Names(ancestors []Process) []string {
	names := make([]string, len(ancestors))
	for i, p := range ancestors {
		names[i] = p.Name
	}
	return names
}

// Proc answers the take-over hint's proc, "<pid>:<start>" of the nearest
// ancestor whose name is in names (a provider's session.process), or "".
// It answers "" without looking further up when that ancestor's arguments
// hold one of shared (session.process_shared), or cannot be read, since a
// process serving several sessions must never offer one session's identity
// to another.
func Proc(ancestors []Process, names, shared []string) string {
	i := slices.IndexFunc(ancestors, func(p Process) bool { return slices.Contains(names, p.Name) })
	if i < 0 {
		return ""
	}
	p := ancestors[i]
	if len(p.Args) == 0 || slices.ContainsFunc(p.Args[1:], func(a string) bool { return slices.Contains(shared, a) }) {
		return ""
	}
	return strconv.Itoa(p.PID) + ":" + strconv.FormatUint(p.Start, 10)
}
