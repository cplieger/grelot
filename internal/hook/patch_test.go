package hook

import (
	"slices"
	"strings"
	"testing"
)

// The expected paths follow openai/codex's patch parser at rust-v0.169.0
// (codex-rs/apply-patch/src/streaming_parser.rs, grammar in
// codex-rs/core/assets/tools/apply_patch.lark): a header is a line trimmed
// at both ends, or only at its end inside an update hunk, that starts with
// "*** Add File: ", "*** Delete File: " or "*** Update File: "; an update's
// "*** Move to: " line comes before its first change.
func TestPatchPaths(t *testing.T) {
	for _, c := range []struct {
		name, patch string
		want        []string
	}{
		{
			name: "every header in patch order",
			patch: "*** Begin Patch\n" +
				"*** Add File: docs/new.md\n+# New\n" +
				"*** Update File: internal/engine.go\n@@ func run\n-\told()\n+\tnew()\n" +
				"*** Delete File: old.go\n" +
				"*** Update File: a.go\n*** Move to: b/a.go\n@@\n context\n" +
				"*** End Patch\n",
			want: []string{"docs/new.md", "internal/engine.go", "old.go", "a.go", "b/a.go"},
		},
		{
			name:  "an environment line and absolute paths",
			patch: "*** Begin Patch\n*** Environment ID: remote\n*** Update File: /home/user/src/x.go\n@@\n-a\n+b\n*** End Patch",
			want:  []string{"/home/user/src/x.go"},
		},
		{
			name:  "crlf lines",
			patch: "*** Begin Patch\r\n*** Add File: a.txt\r\n+a\r\n*** Update File: C:\\work\\b.txt\r\n@@\r\n-a\r\n+b\r\n*** End Patch\r\n",
			want:  []string{"a.txt", `C:\work\b.txt`},
		},
		{
			name:  "an added line that reads like a header",
			patch: "*** Begin Patch\n*** Add File: notes.md\n+*** Update File: not-a-file.go\n*** End Patch\n",
			want:  []string{"notes.md"},
		},
		{
			name:  "an indented header after an added file",
			patch: "*** Begin Patch\n*** Add File: notes.md\n+x\n  *** Delete File: gone.txt\n*** End Patch\n",
			want:  []string{"notes.md", "gone.txt"},
		},
		{
			name:  "a context line that reads like a header",
			patch: "*** Begin Patch\n*** Update File: a.go\n@@\n *** Add File: context.go\n-a\n+b\n*** End Patch\n",
			want:  []string{"a.go"},
		},
		{
			name:  "a move line after the first change",
			patch: "*** Begin Patch\n*** Update File: a.go\n@@\n-a\n+b\n*** Move to: late.go\n*** End Patch\n",
			want:  []string{"a.go"},
		},
		{
			name:  "a second move line",
			patch: "*** Begin Patch\n*** Update File: a.go\n*** Move to: b.go\n*** Move to: c.go\n@@\n-a\n+b\n*** End Patch\n",
			want:  []string{"a.go", "b.go"},
		},
		{
			name:  "a header after the end",
			patch: "*** Begin Patch\n*** Add File: a.txt\n+a\n*** End Patch\n*** Add File: after.txt\n",
			want:  []string{"a.txt"},
		},
		{
			name:  "a header with no path",
			patch: "*** Begin Patch\n*** Add File: \n*** Delete File:\n*** Update File: kept.go\n*** End Patch\n",
			want:  []string{"kept.go"},
		},
		{name: "no headers", patch: "rewrite engine.go to use the new API", want: nil},
		{name: "empty", patch: "", want: nil},
	} {
		t.Run(strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			if got := PatchPaths(c.patch); !slices.Equal(got, c.want) {
				t.Errorf("PatchPaths = %q, want %q", got, c.want)
			}
		})
	}
}
