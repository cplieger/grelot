package hook

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cplieger/grelot/internal/statefile"
)

// The kinds of mark a hook keeps.
const (
	markReplaced = "replaced"
	// MarkOnce is a role's first print for a session, for a role under once.
	MarkOnce = "once"
)

// markLife is how long a mark is kept after it was last set; a session
// idle for that long is not resumed into the same hook state.
const markLife = 30 * 24 * time.Hour

// Marks are small per-session files under the client's state directory.
// Every write is best effort: a failed one never fails a hook.
type Marks struct {
	now      func() time.Time
	dir      string
	readOnly bool
}

// NewMarks keeps marks under configDir's grelot/state/marks folder.
func NewMarks(configDir string, now func() time.Time) *Marks {
	return &Marks{dir: filepath.Join(configDir, "grelot", "state", "marks"), now: now}
}

// ReadMarks reads the marks NewMarks keeps and never writes one: the
// marks of a client with no usable config, which leaves no state behind.
func ReadMarks(configDir string, now func() time.Time) *Marks {
	m := NewMarks(configDir, now)
	m.readOnly = true
	return m
}

// file names a mark by a hash, since a session key may hold any character.
func (m *Marks) file(kind, key string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + key))
	return filepath.Join(m.dir, kind+"-"+hex.EncodeToString(sum[:16]))
}

// Has reports whether the mark is set.
func (m *Marks) Has(kind, key string) bool {
	_, ok := m.When(kind, key)
	return ok
}

// When is the time the mark was last set.
func (m *Marks) When(kind, key string) (time.Time, bool) {
	data, err := statefile.Read(m.file(kind, key), 64)
	if err != nil || data == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	return t, err == nil
}

// Set sets the mark to now and drops the marks not set within markLife.
// ReadMarks' Set writes nothing.
func (m *Marks) Set(kind, key string) {
	if m.readOnly {
		return
	}
	_ = statefile.Replace(m.file(kind, key), []byte(m.now().UTC().Format(time.RFC3339Nano)+"\n"))
	m.prune()
}

// prune reads file ages on the wall clock, which file times are kept on,
// whatever clock the marks' own times come from.
func (m *Marks) prune() {
	now := time.Now()
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > markLife {
			_ = os.Remove(filepath.Join(m.dir, e.Name()))
		}
	}
}
