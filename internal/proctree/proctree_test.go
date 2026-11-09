package proctree

import (
	"errors"
	"slices"
	"testing"
)

// fakeTable is a process table held in memory; a pid it lacks fails.
type fakeTable map[int]Process

func (f fakeTable) Lookup(pid int) (Process, error) {
	p, ok := f[pid]
	if !ok {
		return Process{}, errors.New("no such process")
	}
	return p, nil
}

func table(procs ...Process) fakeTable {
	t := fakeTable{}
	for _, p := range procs {
		t[p.PID] = p
	}
	return t
}

func pids(procs []Process) []int {
	out := make([]int, len(procs))
	for i, p := range procs {
		out[i] = p.PID
	}
	return out
}

func TestAncestors(t *testing.T) {
	tests := []struct {
		name  string
		table fakeTable
		want  []int
	}{
		{"nearest first up to the root", table(
			Process{PID: 40, PPID: 30, Start: 400},
			Process{PID: 30, PPID: 20, Start: 300},
			Process{PID: 20, PPID: 1, Start: 200},
			Process{PID: 1, PPID: 0, Start: 1},
		), []int{30, 20, 1}},
		{"ends at pid 0 where the table lists it, as Windows does", table(
			Process{PID: 40, PPID: 30, Start: 400},
			Process{PID: 30, PPID: 0, Start: 300},
			Process{PID: 0, PPID: 0, Start: 0},
		), []int{30}},
		{"ends at a parent it cannot read", table(
			Process{PID: 40, PPID: 30, Start: 400},
			Process{PID: 30, PPID: 20, Start: 300},
		), []int{30}},
		{"ends at a reused pid started after its child", table(
			Process{PID: 40, PPID: 30, Start: 400},
			Process{PID: 30, PPID: 20, Start: 300},
			Process{PID: 20, PPID: 1, Start: 900},
		), []int{30}},
		{"keeps a parent started in the same tick", table(
			Process{PID: 40, PPID: 30, Start: 400},
			Process{PID: 30, PPID: 0, Start: 400},
		), []int{30}},
		{"ends at a cycle", table(
			Process{PID: 40, PPID: 30, Start: 400},
			Process{PID: 30, PPID: 20, Start: 300},
			Process{PID: 20, PPID: 30, Start: 200},
		), []int{30, 20}},
		{"ends when the parent is the process itself", table(
			Process{PID: 40, PPID: 40, Start: 400},
		), nil},
		{"nothing when the process itself cannot be read", table(
			Process{PID: 30, PPID: 0, Start: 300},
		), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pids(Ancestors(tt.table, 40)); !slices.Equal(got, tt.want) {
				t.Errorf("Ancestors = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAncestors_depthBound(t *testing.T) {
	ft := fakeTable{}
	for pid := 1; pid <= 200; pid++ {
		ft[pid] = Process{PID: pid, PPID: pid + 1, Start: uint64(1000 - pid)}
	}
	got := Ancestors(ft, 1)
	if len(got) != 64 || got[0].PID != 2 || got[63].PID != 65 {
		t.Errorf("Ancestors = %d entries from %v, want the 64 nearest (2 to 65)", len(got), pids(got[:min(len(got), 3)]))
	}
}

func TestNames(t *testing.T) {
	got := Names([]Process{{Name: "bash"}, {Name: "agentcli"}, {Name: "init"}})
	if want := []string{"bash", "agentcli", "init"}; !slices.Equal(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}
}

func TestProc(t *testing.T) {
	names := []string{"agentcli", "agentcli.exe"}
	shared := []string{"serve-many"}
	bash := Process{PID: 90, Name: "bash", Start: 9, Args: []string{"bash", "-c", "grelot"}}
	tui := Process{PID: 4242, Name: "agentcli", Start: 777, Args: []string{"agentcli", "--no-daemon"}}
	server := Process{PID: 4100, Name: "agentcli", Start: 700, Args: []string{"agentcli", "serve-many", "--listen", "unix://"}}
	unreadable := Process{PID: 4200, Name: "agentcli", Start: 750}
	tests := []struct {
		name      string
		ancestors []Process
		names     []string
		want      string
	}{
		{"the nearest named ancestor's pid and start time", []Process{bash, tui, server}, names, "4242:777"},
		{"a shared server sends none and the next one up is never tried", []Process{bash, server, tui}, names, ""},
		{"an unreadable command line counts as shared", []Process{bash, unreadable, tui}, names, ""},
		{"no ancestor named", []Process{bash}, names, ""},
		{"a provider naming no process", []Process{bash, tui}, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Proc(tt.ancestors, tt.names, shared); got != tt.want {
				t.Errorf("Proc = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHost(t *testing.T) {
	claudeNames := []string{"claude", "claude.exe"}
	codexNames := []string{"codex", "codex.exe"}
	// The ancestor chains recorded on Windows 11: a claude -p run (Claude
	// Code 2.1.319) and a codex exec run launched from an interactive Claude
	// Code session's shell (Codex 0.167.0).
	gitBash := Process{PID: 10, Name: "bash.exe", Args: []string{"bash.exe", "-c", "grelot hook"}}
	claudeInner := Process{PID: 11, Name: "claude.exe", Args: []string{`\\?\C:\x\claude.exe`, "--settings", "s.json"}}
	claudeBin := Process{PID: 12, Name: "claude.exe", Args: []string{`C:\x\bin\claude.exe`, "-p", "Reply OK", "--settings", "s.json"}}
	claudeLauncher := Process{PID: 13, Name: "claude.exe", Args: []string{"claude.exe", "-p", "Reply OK"}}
	terminal := Process{PID: 14, Name: "windowsterminal.exe"}
	pwsh := Process{PID: 20, Name: "pwsh.exe", Args: []string{"pwsh.exe", "-NoProfile", "-Command", "grelot hook"}}
	codexInner := Process{PID: 21, Name: "codex.exe", Args: []string{`\\?\C:\x\codex.exe`, "-c", "a=1", "-c", "b=2"}}
	codexBin := Process{PID: 22, Name: "codex.exe", Args: []string{`C:\x\bin\codex.exe`, "exec", "-s", "read-only", "-"}}
	codexLauncher := Process{PID: 23, Name: "codex.exe", Args: []string{"codex.exe", "exec", "-"}}
	claudeShell := Process{PID: 24, Name: "bash.exe", Args: []string{"bash.exe", "-c", "codex exec -"}}
	tests := []struct {
		name      string
		ancestors []Process
		names     []string
		want      []int
	}{
		{
			"every claude process from the hook's shells up to the launcher",
			[]Process{gitBash, gitBash, claudeInner, claudeBin, claudeLauncher, terminal},
			claudeNames,
			[]int{11, 12, 13},
		},
		{
			"codex's run ends at the shell of the host that launched it",
			[]Process{pwsh, codexInner, codexBin, codexLauncher, claudeShell, claudeInner, claudeLauncher},
			codexNames,
			[]int{21, 22, 23},
		},
		{"a run that reaches the end of the walk", []Process{gitBash, claudeInner, claudeBin}, claudeNames, []int{11, 12}},
		{"no ancestor named", []Process{gitBash, terminal}, claudeNames, []int{}},
		{"a provider naming no process", []Process{claudeInner}, nil, []int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pids(Host(tt.ancestors, tt.names)); !slices.Equal(got, tt.want) {
				t.Errorf("Host = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProc_sharedNamesAnArgument(t *testing.T) {
	// The listed word as the program itself is not an argument.
	p := Process{PID: 5, Name: "serve-many", Start: 50, Args: []string{"serve-many", "--stdio"}}
	if got := Proc([]Process{p}, []string{"serve-many"}, []string{"serve-many"}); got != "5:50" {
		t.Errorf("Proc = %q, want %q", got, "5:50")
	}
}
