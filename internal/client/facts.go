package client

import (
	"encoding/json"
	"path"
	"path/filepath"

	"github.com/cplieger/grelot/internal/statefile"
	"github.com/cplieger/grelot/internal/store"
)

// Facts is what the client keeps from the server's last answer that named
// them: the path root every canonical path starts with, and the server's
// version, which the status line compares with the client's.
type Facts struct {
	PathRoot string `json:"path_root"`
	Version  string `json:"version,omitempty"`
}

// maxFactsFile bounds the facts file read.
const maxFactsFile = 4 << 10

// StateDir is the folder of the client's own state files.
func StateDir(configDir string) string { return filepath.Join(Dir(configDir), "state") }

func factsFile(configDir string) string { return filepath.Join(StateDir(configDir), "server.json") }

// LoadFacts reads the cached facts. A missing, unreadable or malformed file,
// or a path root that is not absolute and clean, gives the server's default
// root and no version, so a first command still names paths.
func LoadFacts(configDir string) Facts {
	f := Facts{PathRoot: store.DefaultPathRoot}
	data, err := statefile.Read(factsFile(configDir), maxFactsFile)
	if err != nil || data == nil {
		return f
	}
	var got Facts
	if json.Unmarshal(data, &got) != nil || !cleanRoot(got.PathRoot) {
		return f
	}
	return got
}

// SaveFacts records f when it differs from the cached facts. It is best
// effort: a lost write costs only a refresh at the next answer.
func SaveFacts(configDir string, f Facts) {
	if !cleanRoot(f.PathRoot) || LoadFacts(configDir) == f {
		return
	}
	data, err := json.Marshal(f)
	if err != nil {
		return
	}
	_ = statefile.Replace(factsFile(configDir), data)
}

func cleanRoot(p string) bool { return p != "" && p[0] == '/' && path.Clean(p) == p }
