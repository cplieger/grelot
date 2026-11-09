package main

import (
	"fmt"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// toolNames are matched anywhere in an identifier, literal or comment. Every
// tool-specific fact belongs in a provider file, never in Go code.
var toolNames = []string{"marotte", "claude", "codex", "kiro", "copilot", "antigravity"}

func TestNoToolNamesInCode(t *testing.T) {
	hits, err := toolNameHits(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		t.Errorf("%s: a tool name in Go code; move the fact to a provider file", h)
	}
}

// toolNameHits tokenizes every non-test Go file under root and reports each
// tool name as "path:line: name". It skips directories named testdata and
// paths starting with a dot, which hold checkouts and editor state.
func toolNameHits(root string) ([]string, error) {
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") {
			return nil
		}
		found, err := fileToolNames(path)
		hits = append(hits, found...)
		return err
	})
	return hits, err
}

func fileToolNames(path string) ([]string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var errs scanner.ErrorList
	var s scanner.Scanner
	s.Init(fset.AddFile(path, -1, len(src)), src, errs.Add, scanner.ScanComments)
	var hits []string
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.IDENT && tok != token.STRING && tok != token.COMMENT {
			continue
		}
		for _, n := range namesIn(lit) {
			hits = append(hits, fmt.Sprintf("%s: %s", fset.Position(pos), n))
		}
	}
	if errs.Len() > 0 {
		return hits, errs.Err()
	}
	return hits, nil
}

// namesIn reports the tool names one token holds. kas is a common run of
// letters (taskAssign), so it counts only as a whole word or identifier
// segment; cursor is an ordinary word (the read cursor, bbolt's Cursor), so
// it counts only as a string literal equal to the provider's name, the one
// token strconv.Unquote accepts.
func namesIn(lit string) []string {
	var found []string
	lower := strings.ToLower(lit)
	for _, n := range toolNames {
		if strings.Contains(lower, n) {
			found = append(found, n)
		}
	}
	if hasSegment(lit, "kas") {
		found = append(found, "kas")
	}
	if s, err := strconv.Unquote(lit); err == nil && strings.EqualFold(s, "cursor") {
		found = append(found, `"cursor"`)
	}
	return found
}

// hasSegment reports whether text holds want, case-insensitively, as a whole
// run of letters or a camel-case segment of one: kasHost and KASHost split
// into kas and Host, taskAssign into task and Assign.
func hasSegment(text, want string) bool {
	for word := range strings.FieldsFuncSeq(text, func(r rune) bool { return !unicode.IsLetter(r) }) {
		r := []rune(word)
		start := 0
		for i := 1; i <= len(r); i++ {
			if i < len(r) && !camelBoundary(r, i) {
				continue
			}
			if strings.EqualFold(string(r[start:i]), want) {
				return true
			}
			start = i
		}
	}
	return false
}

// camelBoundary reports whether a segment starts at r[i]: a lower-case letter
// followed by an upper-case one, or the last capital of an acronym followed by
// a lower-case letter (the H of KASHost).
func camelBoundary(r []rune, i int) bool {
	prev, cur := r[i-1], r[i]
	if unicode.IsLower(prev) && unicode.IsUpper(cur) {
		return true
	}
	return i+1 < len(r) && unicode.IsUpper(prev) && unicode.IsUpper(cur) && unicode.IsLower(r[i+1])
}

func TestToolNameHitsPlantedTree(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name:  "host segment in an identifier",
			files: map[string]string{"a.go": "package a\n\nvar kasHost = 1\n"},
			want:  []string{"a.go:3:5: kas"},
		},
		{
			name:  "host in capitals in a comment",
			files: map[string]string{"a.go": "// KAS\npackage a\n"},
			want:  []string{"a.go:1:1: kas"},
		},
		{
			name:  "acronym segment before a word",
			files: map[string]string{"a.go": "package a\n\nvar KASHost, hostKas, kas_port = 1, 2, 3\n"},
			want:  []string{"a.go:3:5: kas", "a.go:3:14: kas", "a.go:3:23: kas"},
		},
		{
			name:  "host letters inside an ordinary word",
			files: map[string]string{"a.go": "package a\n\n// tasks and Kasbah.\nvar taskAssign, masks = 1, 2\n"},
		},
		{
			name: "each name in an identifier, a literal and a comment",
			files: map[string]string{"a.go": "package a\n\n" +
				"var useMarotte = \"Claude\"\n" +
				"// CODEX\n" +
				"var x = `KiroHost`\n" +
				"var githubCopilot, AntiGravity = 1, 2\n"},
			want: []string{
				"a.go:3:5: marotte", "a.go:3:18: claude", "a.go:4:1: codex",
				"a.go:5:9: kiro", "a.go:6:5: copilot", "a.go:6:20: antigravity",
			},
		},
		{
			name:  "cursor provider name as a literal",
			files: map[string]string{"a.go": "package a\n\nvar a, b = \"cursor\", `Cursor`\n"},
			want:  []string{"a.go:3:12: \"cursor\"", "a.go:3:22: \"cursor\""},
		},
		{
			name: "cursor as an ordinary word",
			files: map[string]string{"a.go": "package a\n\n// the read cursor\n" +
				"var cursor, readCursor = \"cursor position\", 1\n"},
		},
		{
			name: "scratch file in a package",
			files: map[string]string{
				"internal/x/x.go":       "package x\n",
				"internal/x/scratch.go": "package x\n\n/* try the claude hook */\n",
			},
			want: []string{"internal/x/scratch.go:3:1: claude"},
		},
		{
			name: "embed declaration in the providers folder",
			files: map[string]string{
				"providers/embed.go":     "// Package providers holds the marotte file.\npackage providers\n",
				"providers/marotte.yaml": "name: marotte\n",
			},
			want: []string{"providers/embed.go:1:1: marotte"},
		},
		{
			name: "files the rule exempts",
			files: map[string]string{
				"a_test.go":         "package a\n\nvar marotte, kasHost = \"cursor\", 1\n",
				"providers/kiro.md": "kiro and KAS\n",
				"testdata/a.go":     "package a // codex\n",
				".worktrees/x/a.go": "package a // codex\n",
				"internal/.a.go":    "package internal // codex\n",
			},
		},
		{
			name: "files whose path starts with an underscore",
			files: map[string]string{
				"_old/a.go":          "package a // codex\n",
				"internal/_draft.go": "package internal // codex\n",
			},
			want: []string{"_old/a.go:1:11: codex", "internal/_draft.go:1:18: codex"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := plantTree(t, tc.files)
			got, err := toolNameHits(root)
			if err != nil {
				t.Fatal(err)
			}
			want := make([]string, len(tc.want))
			for i, w := range tc.want {
				want[i] = filepath.Join(root, w)
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("hits:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
	t.Run("root given as the working directory", func(t *testing.T) {
		t.Chdir(plantTree(t, map[string]string{"a.go": "package a // codex\n"}))
		got, err := toolNameHits(".")
		if err != nil {
			t.Fatal(err)
		}
		if want := "a.go:1:11: codex"; strings.Join(got, "\n") != want {
			t.Errorf("hits %q, want %q", got, want)
		}
	})
	t.Run("file that does not tokenize", func(t *testing.T) {
		root := plantTree(t, map[string]string{"a.go": "package a\n\nvar s = \"open\n"})
		if _, err := toolNameHits(root); err == nil {
			t.Error("toolNameHits answered no error for an unterminated literal")
		}
	})
}

func plantTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
