// Package protopath holds the rules for grelot's canonical paths, the paths
// clients and the server exchange. They are slash paths on every OS, never
// host paths, so nothing here goes through path/filepath.
package protopath

import (
	"path"
	"strings"
)

// DefaultRoot is the path root a server uses unless configured otherwise,
// and the one a client assumes before a server has named its own.
const DefaultRoot = "/workspace"

// IsCanonical reports whether p is absolute and already in path.Clean form,
// which also rules out any ".." component.
func IsCanonical(p string) bool { return path.IsAbs(p) && path.Clean(p) == p }

// Under reports whether p is root or lies below it, comparing the two
// lexically after path.Clean. A relative path is under no absolute root.
func Under(p, root string) bool {
	if root == "" {
		return false
	}
	p, root = path.Clean(p), path.Clean(root)
	if path.IsAbs(p) != path.IsAbs(root) {
		return false
	}
	return p == root || root == "/" || strings.HasPrefix(p, root+"/")
}
