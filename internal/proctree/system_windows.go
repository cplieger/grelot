package proctree

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// System is the running system's process table. Its first lookup takes one
// snapshot, which every later lookup reads.
func System() Table { return &windowsTable{} }

type entry struct {
	name string
	ppid int
}

type windowsTable struct {
	entries map[int]entry
	err     error
	once    sync.Once
}

func (t *windowsTable) Lookup(pid int) (Process, error) {
	t.once.Do(func() { t.entries, t.err = snapshot() })
	if t.err != nil {
		return Process{}, t.err
	}
	e, ok := t.entries[pid]
	if !ok || pid <= 0 || pid > math.MaxUint32 {
		return Process{}, fmt.Errorf("process %d not found", pid)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return Process{}, fmt.Errorf("process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return Process{}, fmt.Errorf("process %d: %w", pid, err)
	}
	return Process{
		PID: pid, PPID: e.ppid, Name: e.name,
		Start: uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime),
		Args:  commandLine(h),
	}, nil
}

func snapshot() (map[int]entry, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	entries := map[int]entry{}
	pe := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		entries[int(pe.ProcessID)] = entry{
			ppid: int(pe.ParentProcessID),
			name: strings.ToLower(windows.UTF16ToString(pe.ExeFile[:])),
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return entries, nil
}

// commandLine reads a process's command line through
// ProcessCommandLineInformation (Windows 8.1 and later), which needs only
// PROCESS_QUERY_LIMITED_INFORMATION, and splits it by CommandLineToArgvW's
// rules (https://learn.microsoft.com/windows/win32/api/winternl/nf-winternl-ntqueryinformationprocess).
func commandLine(h windows.Handle) []string {
	var size uint32
	for range 4 {
		// uint64 words keep the UNICODE_STRING header pointer-aligned.
		size = (max(size, 1024) + 7) &^ 7
		buf := make([]uint64, size/8)
		err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation,
			unsafe.Pointer(&buf[0]), size, &size)
		if errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			continue
		}
		if err != nil {
			return nil
		}
		line := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0])).String()
		args, err := windows.DecomposeCommandLine(line)
		if err != nil || len(args) == 0 {
			return nil
		}
		return args
	}
	return nil
}
