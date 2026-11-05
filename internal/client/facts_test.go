package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFacts_defaultRootWithNoFile(t *testing.T) {
	got := LoadFacts(t.TempDir())
	if got != (Facts{PathRoot: "/workspace"}) {
		t.Errorf("LoadFacts = %+v, want the server's default root /workspace and no version", got)
	}
}

func TestSaveFacts_roundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Facts{PathRoot: "/srv/work", Version: "v1.2.0"}
	SaveFacts(dir, want)
	if got := LoadFacts(dir); got != want {
		t.Errorf("LoadFacts = %+v, want %+v", got, want)
	}
	SaveFacts(dir, Facts{PathRoot: "/", Version: "v1.2.1"})
	if got := LoadFacts(dir); got != (Facts{PathRoot: "/", Version: "v1.2.1"}) {
		t.Errorf("LoadFacts after a second save = %+v, want the new facts", got)
	}
}

func TestSaveFacts_refusesARootThatIsNotClean(t *testing.T) {
	for _, root := range []string{"", "workspace", "/workspace/", "/a/../b", "/a//b"} {
		t.Run(root, func(t *testing.T) {
			dir := t.TempDir()
			SaveFacts(dir, Facts{PathRoot: "/kept"})
			SaveFacts(dir, Facts{PathRoot: root, Version: "v9.9.9"})
			if got := LoadFacts(dir); got != (Facts{PathRoot: "/kept"}) {
				t.Errorf("LoadFacts = %+v, want the earlier facts kept", got)
			}
		})
	}
}

func TestLoadFacts_badFileGivesTheDefault(t *testing.T) {
	for name, content := range map[string]string{
		"not JSON":        "{",
		"a relative root": `{"path_root":"workspace","version":"v1.0.0"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(StateDir(dir), "server.json")
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := LoadFacts(dir); got != (Facts{PathRoot: "/workspace"}) {
				t.Errorf("LoadFacts = %+v, want the default", got)
			}
		})
	}
}
