package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/grelot/internal/webui"
)

// fixture writes a front-end package shaped like internal/webui/static-src,
// with one dependency, @x/ui, that ships a stylesheet naming a font, icons
// and its notices, and returns its options.
func fixture(t *testing.T) *options {
	t.Helper()
	dir := t.TempDir()
	o := &options{src: filepath.Join(dir, "src"), out: filepath.Join(dir, "static"), iconPackage: "@x/ui"}
	files := map[string]string{
		"package.json": `{"dependencies": {"@x/ui": "1.0.0"}}`,
		"index.html": `<html><head><script src="/prepaint.js"></script>` +
			`<link rel="stylesheet" href="/app.css"><script type="module" src="/app.js"></script></head></html>`,
		"src/main.ts":     `import { hello } from "@x/ui/hello"; declare const __ICON_BASE__: string; hello(__ICON_BASE__);`,
		"src/prepaint.ts": `document.documentElement.dataset["theme"] = "light";`,
		"src/app.css":     `@import "@x/ui/css/system"; :root { --cp-accent-h: 200; }`,
		"node_modules/@x/ui/package.json": `{"name": "@x/ui", "type": "module",
			"exports": {"./hello": "./hello.ts", "./css/system": "./css/system.css"}}`,
		"node_modules/@x/ui/hello.ts":                  `export function hello(base: string): void { console.log("icons at", base); }`,
		"node_modules/@x/ui/css/system.css":            `@font-face { font-family: X; src: url("../fonts/x.woff2"); } p { color: red; }`,
		"node_modules/@x/ui/fonts/x.woff2":             "font bytes",
		"node_modules/@x/ui/fonts/LICENSE-x":           "OFL",
		"node_modules/@x/ui/icons/svg/plus.svg":        "<svg>plus</svg>",
		"node_modules/@x/ui/icons/svg/x.svg":           "<svg>x</svg>",
		"node_modules/@x/ui/icons/LICENSE-lucide":      "ISC",
		"node_modules/@x/ui/LICENSE":                   "Apache-2.0",
		"node_modules/@x/ui/NOTICE":                    "notice",
		"node_modules/@x/ui/src/not-a-notice/README":   "x",
		"node_modules/@x/ui/THIRD_PARTY_NOTICES.md":    "third parties",
		"node_modules/@x/ui/icons/svg/not-an-icon.txt": "x",
	}
	for name, body := range files {
		writeFile(t, filepath.Join(o.src, filepath.FromSlash(name)), body)
	}
	writeFile(t, filepath.Join(o.out, keepName), "")
	return o
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// outputs lists every file under out, slash-separated and out-relative.
func outputs(t *testing.T, out string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(out, p)
		names = append(names, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

var (
	placeholderRef = regexp.MustCompile(`(?:src|href)="/([^"]+)"`)
	// esbuild's [hash]: 8 letters of base32.
	stamped = regexp.MustCompile(`(^|/)[^/]+-[A-Z2-7]{8}(\.[a-z0-9]+|/[^/]+)$`)
)

func TestRun_thePageNamesTheStampedBundles(t *testing.T) {
	o := fixture(t)
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	page := readFile(t, filepath.Join(o.out, indexName))
	refs := placeholderRef.FindAllStringSubmatch(page, -1)
	if len(refs) != 3 {
		t.Fatalf("index.html names %d files, want the prepaint script, the stylesheet and the app: %s", len(refs), page)
	}
	for i, want := range []string{"prepaint-", "app-", "app-"} {
		name := refs[i][1]
		if !strings.HasPrefix(name, want) || !stamped.MatchString(name) {
			t.Errorf("reference %d is %q, want a stamped %s file", i, name, want)
		}
		if _, err := os.Stat(filepath.Join(o.out, name)); err != nil {
			t.Errorf("index.html names %s, which the bundle did not write: %v", name, err)
		}
	}
	css := readFile(t, filepath.Join(o.out, refs[1][1]))
	if !strings.Contains(css, "color:red") || !regexp.MustCompile(`--cp-accent-h:\s*200`).MatchString(css) {
		t.Errorf("the stylesheet lacks the library's rule or the app's seed: %s", css)
	}
	if strings.Index(css, "color:red") > strings.Index(css, "--cp-accent-h") {
		t.Errorf("the app's seed comes before the library's rules, so they would override it: %s", css)
	}
	font := regexp.MustCompile(`url\("?\./(assets/x-[A-Z2-7]{8}\.woff2)"?\)`).FindStringSubmatch(css)
	if font == nil {
		t.Fatalf("the stylesheet names no stamped font: %s", css)
	}
	if got := readFile(t, filepath.Join(o.out, font[1])); got != "font bytes" {
		t.Errorf("%s holds %q, want the font", font[1], got)
	}
}

func TestRun_theAppReadsTheStampedIconFolder(t *testing.T) {
	o := fixture(t)
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	page := readFile(t, filepath.Join(o.out, indexName))
	app := placeholderRef.FindAllStringSubmatch(page, -1)[2][1]
	base := regexp.MustCompile(`"(/assets/icons-[A-Z2-7]{8}/)"`).FindStringSubmatch(readFile(t, filepath.Join(o.out, app)))
	if base == nil {
		t.Fatal("the app does not read a stamped icon folder as __ICON_BASE__")
	}
	dir := filepath.Join(o.out, filepath.FromSlash(strings.TrimPrefix(base[1], "/")))
	if got := readFile(t, filepath.Join(dir, "plus.svg")); got != "<svg>plus</svg>" {
		t.Errorf("plus.svg holds %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "not-an-icon.txt")); err == nil {
		t.Error("a file that is no svg was copied with the icons")
	}
}

func TestRun_anIconChangeMovesTheIconFolder(t *testing.T) {
	o := fixture(t)
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	before := iconFolder(t, o.out)
	writeFile(t, filepath.Join(o.src, "node_modules", "@x", "ui", "icons", "svg", "x.svg"), "<svg>new x</svg>")
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	if after := iconFolder(t, o.out); after == before {
		t.Errorf("the icon folder stayed %s after an icon changed, so a browser keeps the old one for a year", before)
	}
}

func iconFolder(t *testing.T, out string) string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(out, "assets", "icons-*"))
	if err != nil || len(m) != 1 {
		t.Fatalf("icon folders %v (%v), want one", m, err)
	}
	return filepath.Base(m[0])
}

// internal/webui.Handler caches every file but index.html and licenses/
// for a year, so every other output must carry a content stamp.
func TestRun_everyOtherOutputIsContentStamped(t *testing.T) {
	o := fixture(t)
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	for _, name := range outputs(t, o.out) {
		if name == indexName || name == keepName || strings.HasPrefix(name, licenseDir+"/") {
			continue
		}
		if !stamped.MatchString(name) {
			t.Errorf("%s carries no content stamp", name)
		}
	}
}

// internal/webui.Handler types a file by its extension and serves one it
// does not know as application/octet-stream, which a browser refuses as a
// script, a stylesheet or a font.
func TestRun_internalWebuiTypesEveryOutput(t *testing.T) {
	o := fixture(t)
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	h := webui.Handler(os.DirFS(o.out))
	for _, name := range outputs(t, o.out) {
		if name == keepName {
			continue
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/"+name, nil))
		if got := rec.Header().Get("Content-Type"); rec.Code != http.StatusOK || got == "application/octet-stream" {
			t.Errorf("%s: %d %q, want 200 and a type of its own", name, rec.Code, got)
		}
	}
}

func TestRun_eachDependencysNoticesAreCopied(t *testing.T) {
	o := fixture(t)
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"LICENSE": "Apache-2.0", "NOTICE": "notice", "THIRD_PARTY_NOTICES.md": "third parties",
		"fonts/LICENSE-x": "OFL", "icons/LICENSE-lucide": "ISC",
	} {
		if got := readFile(t, filepath.Join(o.out, licenseDir, "ui", filepath.FromSlash(name))); got != want {
			t.Errorf("licenses/ui/%s holds %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(o.out, licenseDir, "ui", "src")); err == nil {
		t.Error("a folder with no LICENSE was copied with the notices")
	}
}

func TestRun_aSecondRunLeavesOnlyItsOwnOutputs(t *testing.T) {
	o := fixture(t)
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	first := outputs(t, o.out)
	writeFile(t, filepath.Join(o.src, "src", "main.ts"), `console.log("changed");`)
	writeFile(t, filepath.Join(o.out, "stale-ABCDEFGH.js"), "old")
	if err := run(o); err != nil {
		t.Fatal(err)
	}
	second := outputs(t, o.out)
	if slices.Contains(second, "stale-ABCDEFGH.js") {
		t.Error("a file of an earlier run survived")
	}
	if !slices.Contains(second, keepName) {
		t.Errorf("%s was removed, so a clean clone's embed would fail", keepName)
	}
	if len(second) != len(first) {
		t.Errorf("the second run left %d files, the first %d: %v", len(second), len(first), second)
	}
}

func TestRun_aPageMissingAPlaceholderIsRefused(t *testing.T) {
	o := fixture(t)
	writeFile(t, filepath.Join(o.src, indexName), `<html><script src="/prepaint.js"></script><script src="/app.js"></script></html>`)
	err := run(o)
	if err == nil || !strings.Contains(err.Error(), appStylesheet) {
		t.Fatalf("run: %v, want a refusal naming %s", err, appStylesheet)
	}
}

func TestRun_aDependencyWithNoLicenseIsRefused(t *testing.T) {
	o := fixture(t)
	if err := os.Remove(filepath.Join(o.src, "node_modules", "@x", "ui", "LICENSE")); err != nil {
		t.Fatal(err)
	}
	err := run(o)
	if err == nil || !strings.Contains(err.Error(), "@x/ui ships no LICENSE") {
		t.Fatalf("run: %v, want a refusal naming @x/ui", err)
	}
}

func TestRun_aBuildErrorIsReported(t *testing.T) {
	o := fixture(t)
	writeFile(t, filepath.Join(o.src, "src", "main.ts"), `import "./missing.js";`)
	err := run(o)
	if err == nil || !strings.Contains(err.Error(), "missing.js") {
		t.Fatalf("run: %v, want esbuild's error naming the missing import", err)
	}
}
