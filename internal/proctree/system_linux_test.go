package proctree

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSystem_linux(t *testing.T) {
	self, err := System().Lookup(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	// comm holds at most 15 bytes of the executable's name (proc_pid_comm(5)).
	if want := filepath.Base(os.Args[0]); self.Name != want[:min(len(want), 15)] {
		t.Errorf("Name = %q, want %q", self.Name, want)
	}
	if !slices.Equal(self.Args, os.Args) || self.PPID != os.Getppid() || self.Start == 0 {
		t.Errorf("Lookup(self) = %+v, want args %q and parent %d", self, os.Args, os.Getppid())
	}
	up := Ancestors(System(), os.Getpid())
	if len(up) == 0 || up[0].PID != os.Getppid() || up[0].Start > self.Start {
		t.Errorf("Ancestors starts at %v, want the parent %d started before this process", pids(up), os.Getppid())
	}
}
