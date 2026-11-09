package proctree

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// statLine builds a /proc/<pid>/stat line: pid (comm) then fields 3 to 22 of
// proc_pid_stat(5), ppid at field 4 and starttime at field 22.
func statLine(pid, comm, ppid, start string) string {
	fields := []string{
		"S", ppid, "41", "41", "0", "-1", "4194560", "100", "0", "0", "0",
		"1", "2", "0", "0", "20", "0", "1", "0", start, "1000000", "200",
	}
	return pid + " (" + comm + ") " + strings.Join(fields, " ") + "\n"
}

func TestProcFSLookup(t *testing.T) {
	fsys := fstest.MapFS{
		"41/stat":    {Data: []byte(statLine("41", "my (odd) name", "7", "98765"))},
		"41/cmdline": {Data: []byte("agentcli\x00serve-many\x00--listen\x00unix://\x00")},
		"42/stat":    {Data: []byte(statLine("42", "kworker/0:1", "2", "5"))},
		"42/cmdline": {Data: []byte{}},
		"43/stat":    {Data: []byte(statLine("43", "bash", "1", "6"))},
		"44/stat":    {Data: []byte(statLine("44", "big", "1", "7"))},
		"44/cmdline": {Data: []byte(strings.Repeat("a", maxCmdline+1))},
		"45/stat":    {Data: []byte(statLine("45", "edge", "1", "8"))},
		"45/cmdline": {Data: []byte(strings.Repeat("a", maxCmdline-1) + "\x00")},
	}
	p := procFS{fsys}

	t.Run("name with spaces and parentheses, parent, start and arguments", func(t *testing.T) {
		got, err := p.Lookup(41)
		if err != nil {
			t.Fatal(err)
		}
		want := Process{
			PID: 41, PPID: 7, Name: "my (odd) name", Start: 98765,
			Args: []string{"agentcli", "serve-many", "--listen", "unix://"},
		}
		if got.PID != want.PID || got.PPID != want.PPID || got.Name != want.Name || got.Start != want.Start || !slices.Equal(got.Args, want.Args) {
			t.Errorf("Lookup = %+v, want %+v", got, want)
		}
	})
	for _, tt := range []struct {
		name string
		pid  int
	}{
		{"an empty command line is unreadable", 42},
		{"a missing command line is unreadable", 43},
		{"a command line over the bound is unreadable", 44},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.Lookup(tt.pid)
			if err != nil {
				t.Fatal(err)
			}
			if got.Args != nil {
				t.Errorf("Args = %d entries, want nil", len(got.Args))
			}
		})
	}
	t.Run("a command line at the bound is read", func(t *testing.T) {
		got, err := p.Lookup(45)
		if err != nil || len(got.Args) != 1 || len(got.Args[0]) != maxCmdline-1 {
			t.Errorf("Lookup = %d args, %v; want one argument of %d bytes", len(got.Args), err, maxCmdline-1)
		}
	})
	t.Run("a pid with no entry fails", func(t *testing.T) {
		if _, err := p.Lookup(99); err == nil {
			t.Error("Lookup(99) succeeded, want an error")
		}
	})
}

func TestParseStat_malformed(t *testing.T) {
	full := statLine("1", "init", "0", "3")
	for name, stat := range map[string]string{
		"no name":                "1 init S 0",
		"no opening parenthesis": strings.Replace(full, "(", "", 1),
		"fields only to 21":      full[:strings.LastIndex(full, " 3 ")],
		"short":                  "1 (init) S 0 1 1",
		"ppid not a number":      statLine("1", "init", "x", "3"),
		"start not a number":     statLine("1", "init", "0", "-3"),
	} {
		t.Run(name, func(t *testing.T) {
			if p, err := parseStat(stat); err == nil {
				t.Errorf("parseStat = %+v, want an error", p)
			}
		})
	}
}
