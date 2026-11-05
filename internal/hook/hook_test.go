package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/provider"
)

func TestInput_String(t *testing.T) {
	in, err := ParseInput([]byte(`{"session_id":"s1","n":42,"ok":true,"off":false,"nul":null,"tool":{"input":{"command":"ls"}},"list":["a"]}`))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"session_id":         "s1",
		"n":                  "42",
		"ok":                 "true",
		"off":                "false",
		"nul":                "",
		"tool.input.command": "ls",
		"tool.input":         "",
		"list":               "",
		"missing":            "",
		"session_id.deeper":  "",
		"":                   "",
	} {
		t.Run(strings.ReplaceAll(path, ".", "_")+"_", func(t *testing.T) {
			if got := in.String(path); got != want {
				t.Errorf("String(%q) = %q, want %q", path, got, want)
			}
		})
	}
}

func TestReadInput_emptyIsAnEmptyObject(t *testing.T) {
	in, err := ReadInput(strings.NewReader("  \n"))
	if err != nil || in.String("session_id") != "" {
		t.Errorf("ReadInput(blank) = %v, %v; want an empty object", in, err)
	}
}

func TestReadInput_refusals(t *testing.T) {
	for name, data := range map[string]string{
		"not json": "session_id=s1",
		"an array": `["s1"]`,
		"over cap": `{"x":"` + strings.Repeat("a", maxInput) + `"}`,
	} {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			if _, err := ReadInput(strings.NewReader(data)); err == nil {
				t.Error("ReadInput answered no error")
			}
		})
	}
}

func TestInput_Matches(t *testing.T) {
	in, _ := ParseInput([]byte(`{"reason":"delete","kind":{"main":"yes"}}`))
	cases := []struct {
		when map[string][]string
		name string
		want bool
	}{
		{name: "no filter", when: nil, want: true},
		{name: "listed value", when: map[string][]string{"reason": {"clear", "delete"}}, want: true},
		{name: "other value", when: map[string][]string{"reason": {"clear"}}, want: false},
		{name: "nested path", when: map[string][]string{"kind.main": {"yes"}}, want: true},
		{name: "every path must match", when: map[string][]string{"reason": {"delete"}, "kind.main": {"no"}}, want: false},
		{name: "missing path", when: map[string][]string{"source": {"startup"}}, want: false},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			if got := in.Matches(c.when); got != c.want {
				t.Errorf("Matches(%v) = %v, want %v", c.when, got, c.want)
			}
		})
	}
}

// recordFile is a provider whose hooks read each session's record from
// sessions/<id>.json under REC_DIR, or ~/.rec.
const recordFile = `name: rec
detect: {env: [REC_ID]}
session:
  env: REC_ID
  stdin: session_id
  record:
    dir_env: REC_DIR
    dir_default: ~/.rec
    glob: "sessions/{{.HookSession}}.json"
    parent: meta.parentSessionId
    reason: meta.createdReason
    same_agent: [rewind]
    same_agent_note: "this chat is a rewind"
`

func loadProvider(t *testing.T, name, data string) *provider.Provider {
	t.Helper()
	p, err := provider.Load(name, []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type fixture struct {
	t     *testing.T
	p     *provider.Provider
	marks *Marks
	sh    Shell
	dir   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, p: loadProvider(t, "rec", recordFile), dir: dir, marks: NewMarks(t.TempDir(), time.Now)}
	f.sh = Shell{
		Lookup:  func(name string) (string, bool) { return dir, name == "REC_DIR" },
		Home:    func() (string, error) { return "", os.ErrNotExist },
		Session: "root",
	}
	return f
}

func (f *fixture) record(session, body string) {
	f.t.Helper()
	file := filepath.Join(f.dir, "sessions", session+".json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) classify(stdin string) Class {
	f.t.Helper()
	in, err := ParseInput([]byte(stdin))
	if err != nil {
		f.t.Fatal(err)
	}
	return Classify(f.p, in, "agent_id", f.sh, f.marks)
}

func TestClassify(t *testing.T) {
	f := newFixture(t)
	f.record("step", `{"meta":{}}`)
	f.record("tangent", `{"meta":{"parentSessionId":"root","createdReason":"tangent"}}`)
	f.record("rewound", `{"meta":{"parentSessionId":"root","createdReason":"rewind"}}`)
	f.record("broken", `{"meta":`)
	cases := []struct {
		name  string
		stdin string
		want  Class
	}{
		{name: "same session", stdin: `{"session_id":"root"}`, want: Class{Stdin: "root", Kind: Parent}},
		{name: "no stdin session", stdin: `{}`, want: Class{Kind: Parent}},
		{name: "agent_id", stdin: `{"session_id":"root","agent_id":"a1"}`, want: Class{Stdin: "root", Kind: Child}},
		{name: "a step's record has no parent", stdin: `{"session_id":"step"}`, want: Class{Stdin: "step", Kind: Child}},
		{name: "a fork beside its parent", stdin: `{"session_id":"tangent"}`, want: Class{Stdin: "tangent", Kind: Child, Fork: true}},
		{name: "a same-agent fork", stdin: `{"session_id":"rewound"}`, want: Class{Stdin: "rewound", Kind: SameAgent}},
		{name: "no record", stdin: `{"session_id":"gone"}`, want: Class{Stdin: "gone", Kind: Child}},
		{name: "unreadable record", stdin: `{"session_id":"broken"}`, want: Class{Stdin: "broken", Kind: Child}},
		{name: "a session id that is no path component", stdin: `{"session_id":"../tangent"}`, want: Class{Stdin: "../tangent", Kind: Child}},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			if got := f.classify(c.stdin); got != c.want {
				t.Errorf("Classify(%s) = %+v, want %+v", c.stdin, got, c.want)
			}
		})
	}
}

func TestClassify_differingIDsWithoutARecordRunForTheParent(t *testing.T) {
	p := loadProvider(t, "plain", "name: plain\nsession: {env: PLAIN_ID, stdin: session_id}\n")
	in, _ := ParseInput([]byte(`{"session_id":"other"}`))
	got := Classify(p, in, "agent_id", Shell{Session: "root"}, NewMarks(t.TempDir(), time.Now))
	if got.Kind != Parent {
		t.Errorf("Kind = %v, want Parent", got.Kind)
	}
}

// A rewind replaces its parent: the parent's hooks go quiet, the fork's
// pass, and a rewind of the fork replaces the fork in turn.
func TestClassify_rewindReplacesItsParent(t *testing.T) {
	f := newFixture(t)
	f.record("r1", `{"meta":{"parentSessionId":"root","createdReason":"rewind"}}`)
	f.record("r2", `{"meta":{"parentSessionId":"r1","createdReason":"rewind"}}`)
	if got := f.classify(`{"session_id":"root"}`).Kind; got != Parent {
		t.Fatalf("root before any rewind: Kind = %v, want Parent", got)
	}
	if got := f.classify(`{"session_id":"r1"}`).Kind; got != SameAgent {
		t.Fatalf("r1: Kind = %v, want SameAgent", got)
	}
	if got := f.classify(`{"session_id":"root"}`).Kind; got != Replaced {
		t.Errorf("root after the rewind: Kind = %v, want Replaced", got)
	}
	if got := f.classify(`{"session_id":"r1"}`).Kind; got != SameAgent {
		t.Errorf("r1 again: Kind = %v, want SameAgent", got)
	}
	if got := f.classify(`{"session_id":"r2"}`).Kind; got != SameAgent {
		t.Fatalf("r2: Kind = %v, want SameAgent", got)
	}
	if got := f.classify(`{"session_id":"r1"}`).Kind; got != Replaced {
		t.Errorf("r1 after the second rewind: Kind = %v, want Replaced", got)
	}
}

func TestClassify_recordDirectoryDefaultUnderHome(t *testing.T) {
	f := newFixture(t)
	home := t.TempDir()
	f.dir = filepath.Join(home, ".rec")
	f.record("tangent", `{"meta":{"parentSessionId":"root","createdReason":"tangent"}}`)
	f.sh.Lookup = func(string) (string, bool) { return "", false }
	f.sh.Home = func() (string, error) { return home, nil }
	if got := f.classify(`{"session_id":"tangent"}`); !got.Fork {
		t.Errorf("Classify = %+v, want a fork read from ~/.rec", got)
	}
}

func TestClassify_relativeRecordDirectoryIsNoRecord(t *testing.T) {
	f := newFixture(t)
	f.sh.Lookup = func(string) (string, bool) { return "relative/dir", true }
	if got := f.classify(`{"session_id":"tangent"}`); got.Kind != Child || got.Fork {
		t.Errorf("Classify = %+v, want a child that is no fork", got)
	}
}

func TestMarks(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	m := NewMarks(dir, func() time.Time { return at })
	if m.Has(MarkOnce, "a") {
		t.Fatal("a fresh mark is set")
	}
	m.Set(MarkOnce, "a")
	if got, ok := m.When(MarkOnce, "a"); !ok || !got.Equal(at) {
		t.Errorf("When = %v, %v; want %v", got, ok, at)
	}
	if m.Has(MarkOnce, "b") || m.Has(markReplaced, "a") {
		t.Error("a mark is set for another key or kind")
	}
	if !NewMarks(dir, time.Now).Has(MarkOnce, "a") {
		t.Error("a second process does not see the mark")
	}
}

func TestMarks_pruneDropsOldMarks(t *testing.T) {
	m := NewMarks(t.TempDir(), time.Now)
	m.Set(MarkOnce, "old")
	old := time.Now().Add(-markLife - time.Hour)
	if err := os.Chtimes(m.file(MarkOnce, "old"), old, old); err != nil {
		t.Fatal(err)
	}
	m.Set(MarkOnce, "new")
	if m.Has(MarkOnce, "old") || !m.Has(MarkOnce, "new") {
		t.Error("prune kept the old mark or dropped the new one")
	}
}

func TestMarks_unwritableDirectoryIsBestEffort(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewMarks(file, time.Now)
	m.Set(MarkOnce, "a")
	if m.Has(MarkOnce, "a") {
		t.Error("a mark under a file reads as set")
	}
}
