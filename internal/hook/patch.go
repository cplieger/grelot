package hook

import (
	"strings"
	"unicode"
)

// The file headers of a "*** Begin Patch" patch.
const (
	patchAdd    = "*** Add File: "
	patchDelete = "*** Delete File: "
	patchUpdate = "*** Update File: "
	patchMove   = "*** Move to: "
	patchEnd    = "*** End Patch"
)

// PatchPaths is every file a "*** Begin Patch" patch writes, as its headers
// spell them, in patch order with repeats kept: each added, deleted and
// updated file, and an update's move target. Text with no header has none.
func PatchPaths(text string) []string {
	var paths []string
	// inUpdate is inside an update's lines; canMove until its first change
	// or move, the only place the format reads a move.
	inUpdate, canMove := false, false
	for line := range strings.SplitSeq(text, "\n") {
		// The format trims a header at both ends, but only at its end
		// inside an update, where a leading space marks a context line.
		header := strings.TrimSpace(line)
		if inUpdate {
			header = strings.TrimRightFunc(line, unicode.IsSpace)
		}
		if header == patchEnd {
			break
		}
		if p, ok := patchHeader(header); ok {
			paths = append(paths, p)
			inUpdate = strings.HasPrefix(header, patchUpdate)
			canMove = inUpdate
			continue
		}
		if !inUpdate {
			continue
		}
		if p, ok := strings.CutPrefix(header, patchMove); ok && canMove {
			paths = append(paths, p)
		}
		canMove = false
	}
	return paths
}

func patchHeader(line string) (string, bool) {
	for _, marker := range []string{patchAdd, patchDelete, patchUpdate} {
		if p, ok := strings.CutPrefix(line, marker); ok {
			return p, true
		}
	}
	return "", false
}
