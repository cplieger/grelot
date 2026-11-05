package statefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplace_createsDirectoryAndReplacesWhole(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a", "b", "state")
	if err := Replace(file, []byte("first, longer content")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if err := Replace(file, []byte("second")); err != nil {
		t.Fatalf("Replace again: %v", err)
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != "second" {
		t.Fatalf("content = %q, %v; want %q", got, err, "second")
	}
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil || len(entries) != 1 {
		t.Errorf("directory holds %d entries (%v), want only the state file", len(entries), err)
	}
}

func TestReplace_unwritableDirectoryFails(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(filepath.Join(blocker, "state"), []byte("x")); err == nil {
		t.Error("Replace under a regular file succeeded, want an error")
	}
}

func TestReplace_failedRenameLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Replace(target, []byte("x")); err == nil {
		t.Fatal("Replace over a non-empty directory succeeded, want an error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("directory holds %d entries (%v), want only the blocking directory", len(entries), err)
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	t.Run("missing", func(t *testing.T) {
		got, err := Read(filepath.Join(dir, "none"), 10)
		if got != nil || err != nil {
			t.Errorf("Read(missing) = %q, %v; want nil, nil", got, err)
		}
	})
	t.Run("capped", func(t *testing.T) {
		file := filepath.Join(dir, "long")
		if err := os.WriteFile(file, []byte("0123456789"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := Read(file, 4)
		if string(got) != "0123" || err != nil {
			t.Errorf("Read(cap 4) = %q, %v; want %q", got, err, "0123")
		}
	})
	t.Run("directory", func(t *testing.T) {
		if _, err := Read(dir, 10); err == nil {
			t.Error("Read(directory) succeeded, want an error")
		}
	})
}
