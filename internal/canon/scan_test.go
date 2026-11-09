package canon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scanHost is a host whose home, temp dir and path root are directories of
// its own, with a clone at its canonical root (toolbelt), a clone at another
// local root (subflux) and a scratch tree under the root that is no clone.
// Its fields are in slash form, as a body names paths and git prints them.
type scanHost struct {
	r                                *Resolver
	home, temp, root, subflux, notes string
}

func newScanHost(t *testing.T) scanHost {
	t.Helper()
	isolateGit(t)
	base := filepath.ToSlash(tempDir(t))
	h := scanHost{
		home:    base + "/home",
		temp:    base + "/tmp",
		root:    base + "/workspace",
		subflux: base + "/src/subflux",
	}
	for _, d := range []string{h.home, h.temp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitRepo(t, filepath.Join(h.root, "toolbelt"), "origin", "https://github.com/cplieger/toolbelt")
	writeFile(t, filepath.Join(h.root, "toolbelt", "engine.go"))
	gitRepo(t, h.subflux, "origin", "git@github.com:cplieger/subflux.git")
	writeFile(t, filepath.Join(h.subflux, "x.go"))
	writeFile(t, filepath.Join(h.subflux, "pkg", "y.go"))
	h.notes = h.root + "/_scratch/notes.md"
	writeFile(t, h.notes)
	h.r = New(h.root, nil, nil)
	h.r.home, h.r.temp = h.home, h.temp
	return h
}

func TestScanLocal(t *testing.T) {
	h := newScanHost(t)
	tests := []struct {
		name, body string
		want       Finding
	}{
		{"temp dir", "the log is at " + h.temp + "/build.log", Finding{Token: h.temp + "/build.log"}},
		{"home by absolute path", "see " + h.home + "/notes for the plan", Finding{Token: h.home + "/notes"}},
		{"trailing ellipsis on a real name", "the log at " + h.temp + "/build.log...", Finding{Token: h.temp + "/build.log"}},
		{"non-clone tree under the root", "read " + h.notes, Finding{Token: h.notes}},
		{"clone at another local root", "the bug is in " + h.subflux + "/x.go", Finding{Token: h.subflux + "/x.go", Canonical: h.root + "/subflux/x.go"}},
		{"subdirectory of that clone", "and " + h.subflux + "/pkg/y.go", Finding{Token: h.subflux + "/pkg/y.go", Canonical: h.root + "/subflux/pkg/y.go"}},
		{"punctuation around it", "(see " + h.temp + "/a.md).", Finding{Token: h.temp + "/a.md"}},
		{"first of several", "/v1/me then " + h.root + "/toolbelt/engine.go, " + h.temp + "/a and ~/b", Finding{Token: h.temp + "/a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := h.r.ScanLocal(t.Context(), tt.body)
			if !ok || got != tt.want {
				t.Errorf("got %+v, %v; want %+v", got, ok, tt.want)
			}
		})
	}
}

func TestScanLocal_none(t *testing.T) {
	h := newScanHost(t)
	for name, body := range map[string]string{
		"clone at its canonical root":        "write " + h.root + "/toolbelt/engine.go",
		"canonical path missing locally":     "write " + h.root + "/subflux/x.go",
		"API route":                          "call /v1/me first",
		"no component":                       "a / b // c ~/ d",
		"no path":                            "nothing to see",
		"the path root itself":               "everything under " + h.root + " is shared",
		"the path root before a placeholder": "write `" + h.root + "/...` paths",
		"a placeholder inside a path":        "see " + h.home + "/.../notes",
		"an ellipsis character":              "see " + h.temp + "/…",
		"a named placeholder":                "read " + h.temp + "/<name>.log",
		"the recipient's own home":           "add the rule to ~/.codex/rules/grelot.rules",
		"a device in a shell command":        "run `go test ./... > /dev/null 2>&1` then `cat /dev/stdin`",
		"the standard streams":               "it writes to /dev/stdout, never /dev/stderr",
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := h.r.ScanLocal(t.Context(), body); ok {
				t.Errorf("got %+v, want none", got)
			}
		})
	}
}

func TestScanLocal_checksAtMost32Candidates(t *testing.T) {
	h := newScanHost(t)
	local := h.temp + "/a.log"
	if got, ok := h.r.ScanLocal(t.Context(), strings.Repeat("/v1/me ", 32)+local); ok {
		t.Errorf("33rd candidate: got %+v, want none", got)
	}
	if got, ok := h.r.ScanLocal(t.Context(), strings.Repeat("/v1/me ", 31)+local); !ok || got.Token != local {
		t.Errorf("32nd candidate: got %+v, %v; want %q", got, ok, local)
	}
}

func TestScanLocal_windows(t *testing.T) {
	g := fakeGit{"C:/src/subflux": {Toplevel: "C:/src/subflux", Remote: "git@github.com:cplieger/subflux.git"}}
	r := windowsResolver("/workspace", g, []string{"C:/", "C:/src", "C:/src/subflux"}, []string{"C:/src/subflux/x.go"})
	r.home, r.temp = `C:\Users\U`, `C:\Users\U\AppData\Local\Temp`
	tests := []struct {
		name, body string
		want       Finding
	}{
		{"home in another case", `see c:\users\u\notes.md`, Finding{Token: `c:\users\u\notes.md`}},
		{"temp with forward slashes", "C:/Users/u/AppData/Local/Temp/x.log", Finding{Token: "C:/Users/u/AppData/Local/Temp/x.log"}},
		{"clone at another local root", `fix C:\src\subflux\x.go`, Finding{Token: `C:\src\subflux\x.go`, Canonical: "/workspace/subflux/x.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := r.ScanLocal(t.Context(), tt.body)
			if !ok || got != tt.want {
				t.Errorf("got %+v, %v; want %+v", got, ok, tt.want)
			}
		})
	}
	if got, ok := r.ScanLocal(t.Context(), `see C:\Users\U\...\notes.md`); ok {
		t.Errorf("a placeholder in a drive path: got %+v, want none", got)
	}
	if got, ok := r.ScanLocal(t.Context(), `D:\elsewhere\f.txt`); ok {
		t.Errorf("a missing path outside home and temp: got %+v, want none", got)
	}
	nul := windowsResolver("/workspace", g, []string{"C:/", "C:/src", "C:/src/subflux"}, []string{"C:/nul", "C:/src/subflux/NUL"})
	for _, body := range []string{`dir > C:\nul`, `copy x C:/src/subflux/NUL`} {
		if got, ok := nul.ScanLocal(t.Context(), body); ok {
			t.Errorf("%s: got %+v, want none: NUL is the null device in every directory", body, got)
		}
	}
}
