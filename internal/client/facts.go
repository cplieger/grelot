package client

import (
	"encoding/json"
	"path/filepath"

	"github.com/cplieger/grelot/internal/protopath"
	"github.com/cplieger/grelot/internal/statefile"
)

// Facts is what the client keeps from the server's last answer that named
// them: the path root every canonical path starts with, and the server's
// version, which the status line compares with the client's. AckRefused is
// the client version whose inbox ack that server refused as too old, so the
// watcher and the stop hold stop waking for mail it cannot mark read.
type Facts struct {
	PathRoot   string `json:"path_root"`
	Version    string `json:"version,omitempty"`
	AckRefused string `json:"ack_refused,omitempty"`
}

// CannotAck is the upgrade line when the cached facts say this client of
// version client cannot ack on the server, "" otherwise. A newer client,
// or a server no longer newer, clears it.
func CannotAck(configDir, client string) string {
	f := LoadFacts(configDir)
	if f.AckRefused != client || !Older(client, f.Version) {
		return ""
	}
	return Upgrade(client, f.Version)
}

// NoteAck records whether the server refused this client's ack as too old.
func NoteAck(configDir, client string, refused bool) {
	f := LoadFacts(configDir)
	f.AckRefused = ""
	if refused {
		f.AckRefused = client
	}
	SaveFacts(configDir, f)
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
	f := Facts{PathRoot: protopath.DefaultRoot}
	data, err := statefile.Read(factsFile(configDir), maxFactsFile)
	if err != nil || data == nil {
		return f
	}
	var got Facts
	if json.Unmarshal(data, &got) != nil || !protopath.IsCanonical(got.PathRoot) {
		return f
	}
	return got
}

// SaveFacts records f when it differs from the cached facts. It is best
// effort: a lost write costs only a refresh at the next answer.
func SaveFacts(configDir string, f Facts) {
	if !protopath.IsCanonical(f.PathRoot) || LoadFacts(configDir) == f {
		return
	}
	data, err := json.Marshal(f)
	if err != nil {
		return
	}
	_ = statefile.Replace(factsFile(configDir), data)
}
