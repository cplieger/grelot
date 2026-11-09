package proctree

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
)

// maxCmdline bounds a command line read; a longer one counts as unreadable,
// since the part left out could hold a session.process_shared argument.
const maxCmdline = 256 << 10

var _ Table = procFS{}

// procFS reads the Linux process table from a proc filesystem root.
type procFS struct{ fsys fs.FS }

func (p procFS) Lookup(pid int) (Process, error) {
	id := strconv.Itoa(pid)
	stat, err := fs.ReadFile(p.fsys, id+"/stat")
	if err != nil {
		return Process{}, err
	}
	proc, err := parseStat(string(stat))
	if err != nil {
		return Process{}, fmt.Errorf("process %d: %w", pid, err)
	}
	proc.PID = pid
	proc.Args = readCmdline(p.fsys, id+"/cmdline")
	return proc, nil
}

// parseStat reads the name, parent and start time from /proc/<pid>/stat.
// The name sits in parentheses and may itself hold spaces and parentheses,
// so the fields after it start at the last ')' (proc_pid_stat(5)).
func parseStat(stat string) (Process, error) {
	open, end := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open < 0 || end < open {
		return Process{}, errors.New("stat has no name")
	}
	// Fields 3 onward: state is index 0, ppid (field 4) index 1, starttime
	// (field 22) index 19.
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 20 {
		return Process{}, errors.New("stat is short")
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return Process{}, fmt.Errorf("stat ppid: %w", err)
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return Process{}, fmt.Errorf("stat starttime: %w", err)
	}
	return Process{PPID: ppid, Name: stat[open+1 : end], Start: start}, nil
}

func readCmdline(fsys fs.FS, name string) []string {
	f, err := fsys.Open(name)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCmdline+1))
	if err != nil || len(b) == 0 || len(b) > maxCmdline {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}
