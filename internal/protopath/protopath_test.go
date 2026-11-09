package protopath

import "testing"

func TestIsCanonical(t *testing.T) {
	for p, want := range map[string]bool{
		"/workspace":             true,
		"/workspace/toolbelt/x":  true,
		"/":                      true,
		"/c/Users/u/notes.md":    true,
		`/workspace/a\b`:         true, // a backslash is a name byte, never a separator
		"":                       false,
		"workspace":              false,
		"/workspace/":            false,
		"/workspace//x":          false,
		"/workspace/./x":         false,
		"/workspace/../etc":      false,
		"/..":                    false,
		`C:\workspace`:           false,
		"C:/workspace":           false,
		"/workspace/toolbelt/..": false,
	} {
		if got := IsCanonical(p); got != want {
			t.Errorf("IsCanonical(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestUnder(t *testing.T) {
	tests := []struct {
		p, root string
		want    bool
	}{
		{"/workspace/a", "/workspace", true},
		{"/workspace", "/workspace", true},
		{"/workspace/a/b", "/workspace/a", true},
		{"/workspace/ab", "/workspace/a", false},
		{"/workspace", "/workspace/a", false},
		{"/other", "/workspace", false},
		{"/anything", "/", true},
		{"/workspace/a/../b", "/workspace/a", false},
		{"workspace/a", "/workspace", false},
		{"/workspace/a", "", false},
		{`/workspace\a`, "/workspace", false},
	}
	for _, tt := range tests {
		if got := Under(tt.p, tt.root); got != tt.want {
			t.Errorf("Under(%q, %q) = %v, want %v", tt.p, tt.root, got, tt.want)
		}
	}
}

func TestWord(t *testing.T) {
	for p, want := range map[string]string{
		"/workspace/bolttable":     "'/workspace/bolttable'",
		"/workspace/my notes & co": "'/workspace/my notes & co'",
		"/workspace/o'reilly":      "",
	} {
		if got, ok := Word(p); got != want || ok != (want != "") {
			t.Errorf("Word(%q) = %q, %v, want %q", p, got, ok, want)
		}
	}
}
