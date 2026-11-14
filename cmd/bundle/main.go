// Command bundle builds grelot's browser UI: esbuild (a Go library, so no
// Node at build time) bundles the TypeScript and the stylesheet, and the
// page, the icons and the libraries' notices are copied beside them. tsc is
// the type gate; esbuild does not typecheck.
//
// Usage: go run ./cmd/bundle from the repo root. It reads
// internal/webui/static-src and its node_modules, and writes
// internal/webui/static, which internal/webui embeds.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

func main() {
	if err := run(&options{
		src:         filepath.Join("internal", "webui", "static-src"),
		out:         filepath.Join("internal", "webui", "static"),
		iconPackage: "@cplieger/ui",
	}); err != nil {
		fmt.Fprintln(os.Stderr, "bundle:", err)
		os.Exit(1)
	}
}

type options struct {
	src string // the front end's package folder
	out string // the folder internal/webui embeds
	// iconPackage ships icons/svg/, served under a stamped folder whose
	// URL the app reads as __ICON_BASE__.
	iconPackage string
}

// The placeholders index.html names, each replaced by its stamped output.
const (
	appScript      = `src="/app.js"`
	appStylesheet  = `href="/app.css"`
	prepaintScript = `src="/prepaint.js"`
	indexName      = "index.html"
	licenseDir     = "licenses"
	keepName       = ".gitkeep"
)

func run(given *options) error {
	o, err := absolute(given)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(filepath.Join(o.src, "package.json")); statErr != nil {
		return fmt.Errorf("run from the repo root: %w", statErr)
	}
	if cleanErr := clean(o.out); cleanErr != nil {
		return cleanErr
	}
	iconBase, err := copyIcons(o)
	if err != nil {
		return err
	}
	app, err := build(o, api.FormatESModule, iconBase,
		api.EntryPoint{InputPath: filepath.Join(o.src, "src", "main.ts"), OutputPath: "app"},
		api.EntryPoint{InputPath: filepath.Join(o.src, "src", "app.css"), OutputPath: "app"})
	if err != nil {
		return err
	}
	prepaint, err := build(o, api.FormatIIFE, iconBase,
		api.EntryPoint{InputPath: filepath.Join(o.src, "src", "prepaint.ts"), OutputPath: "prepaint"})
	if err != nil {
		return err
	}
	if err := writeIndex(o, map[string]string{
		appScript:      `src="/` + app[".js"] + `"`,
		appStylesheet:  `href="/` + app[".css"] + `"`,
		prepaintScript: `src="/` + prepaint[".js"] + `"`,
	}); err != nil {
		return err
	}
	return copyNotices(o)
}

// absolute makes o's folders absolute, since esbuild resolves entries
// and writes its metafile against AbsWorkingDir.
func absolute(o *options) (*options, error) {
	src, err := filepath.Abs(o.src)
	if err != nil {
		return nil, err
	}
	out, err := filepath.Abs(o.out)
	if err != nil {
		return nil, err
	}
	return &options{src: src, out: out, iconPackage: o.iconPackage}, nil
}

// clean empties out but keeps keepName, which lets internal/webui's embed
// compile on a clone that has never bundled.
func clean(out string) error {
	entries, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == keepName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(out, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// build bundles entries into o.out under content-stamped names and returns
// each output's out-relative path by its extension.
func build(o *options, format api.Format, iconBase string, entries ...api.EntryPoint) (map[string]string, error) {
	result := api.Build(api.BuildOptions{
		AbsWorkingDir:       filepath.Dir(o.out),
		EntryPointsAdvanced: entries,
		Outdir:              o.out,
		Bundle:              true,
		Format:              format,
		Target:              api.ESNext,
		EntryNames:          "[name]-[hash]",
		AssetNames:          "assets/[name]-[hash]",
		Loader:              map[string]api.Loader{".woff2": api.LoaderFile, ".svg": api.LoaderFile},
		Define:              map[string]string{"__ICON_BASE__": strconv.Quote(iconBase)},
		MinifyWhitespace:    true,
		MinifyIdentifiers:   true,
		MinifySyntax:        true,
		Charset:             api.CharsetUTF8,
		LogLevel:            api.LogLevelSilent,
		Metafile:            true,
		Write:               true,
	})
	for _, w := range api.FormatMessages(result.Warnings, api.FormatMessagesOptions{Kind: api.WarningMessage}) {
		fmt.Fprint(os.Stderr, w)
	}
	if len(result.Errors) > 0 {
		msgs := api.FormatMessages(result.Errors, api.FormatMessagesOptions{Kind: api.ErrorMessage})
		return nil, fmt.Errorf("esbuild:\n%s", strings.Join(msgs, "\n"))
	}
	var meta struct {
		Outputs map[string]struct {
			EntryPoint string `json:"entryPoint"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(result.Metafile), &meta); err != nil {
		return nil, fmt.Errorf("esbuild metafile: %w", err)
	}
	outs := map[string]string{}
	for written, info := range meta.Outputs {
		if info.EntryPoint == "" {
			continue
		}
		// Metafile paths are relative to AbsWorkingDir.
		rel, err := filepath.Rel(o.out, filepath.Join(filepath.Dir(o.out), written))
		if err != nil {
			return nil, err
		}
		outs[filepath.Ext(rel)] = filepath.ToSlash(rel)
	}
	if len(outs) != len(entries) {
		return nil, fmt.Errorf("esbuild wrote %d entry outputs for %d entries: %v", len(outs), len(entries), outs)
	}
	return outs, nil
}

// stamp is esbuild's [hash] form, 8 base32 letters, so every
// content-addressed name in o.out reads alike.
func stamp(sum []byte) string {
	return base32.StdEncoding.EncodeToString(sum)[:8]
}

// copyIcons copies the icon package's icons/svg into assets/icons-<stamp>/,
// the stamp covering every name and byte, and returns its URL.
func copyIcons(o *options) (string, error) {
	pkg, err := os.OpenRoot(filepath.Join(o.src, "node_modules", filepath.FromSlash(o.iconPackage)))
	if err != nil {
		return "", fmt.Errorf("icons: %w", err)
	}
	defer pkg.Close()
	names, err := fs.Glob(pkg.FS(), "icons/svg/*.svg")
	if err != nil {
		return "", fmt.Errorf("icons: %w", err)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("icons: %s ships no icons/svg/*.svg", o.iconPackage)
	}
	slices.Sort(names)
	sum := sha256.New()
	files := make(map[string][]byte, len(names))
	for _, n := range names {
		data, err := pkg.ReadFile(n)
		if err != nil {
			return "", fmt.Errorf("icons: %w", err)
		}
		files[path.Base(n)] = data
		// Each name and length is written, so no two sets can hash alike by
		// moving bytes between files.
		fmt.Fprintf(sum, "%s\x00%d\x00", path.Base(n), len(data))
		sum.Write(data)
	}
	dir := "icons-" + stamp(sum.Sum(nil))
	dst := filepath.Join(o.out, "assets", dir)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", err
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dst, name), data, 0o600); err != nil {
			return "", err
		}
	}
	return "/assets/" + dir + "/", nil
}

// writeIndex copies index.html with each placeholder, which must appear
// exactly once, replaced.
func writeIndex(o *options, swaps map[string]string) error {
	page, err := os.ReadFile(filepath.Join(o.src, indexName))
	if err != nil {
		return err
	}
	for from, to := range swaps {
		if n := bytes.Count(page, []byte(from)); n != 1 {
			return fmt.Errorf("%s names %s %d times, want once", indexName, from, n)
		}
		page = bytes.Replace(page, []byte(from), []byte(to), 1)
	}
	out, err := os.OpenRoot(o.out)
	if err != nil {
		return err
	}
	defer out.Close()
	return out.WriteFile(indexName, page, 0o600)
}

// copyNotices copies each bundled dependency's licence files into
// licenses/<name>/: its top-level LICENSE*, NOTICE* and
// THIRD_PARTY_NOTICES*, and the LICENSE* of each folder it ships assets
// from (fonts under OFL 1.1, icons under ISC). Apache-2.0 section 4 asks
// for them with every copy.
func copyNotices(o *options) error {
	manifest, err := os.ReadFile(filepath.Join(o.src, "package.json"))
	if err != nil {
		return err
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(manifest, &pkg); err != nil {
		return fmt.Errorf("package.json: %w", err)
	}
	for name := range pkg.Dependencies {
		if err := copyPackageNotices(o, name); err != nil {
			return err
		}
	}
	return nil
}

var noticePatterns = []string{"LICENSE*", "NOTICE*", "THIRD_PARTY_NOTICES*", "*/LICENSE*"}

func copyPackageNotices(o *options, name string) error {
	src, err := os.OpenRoot(filepath.Join(o.src, "node_modules", filepath.FromSlash(name)))
	if err != nil {
		return fmt.Errorf("notices: %w", err)
	}
	defer src.Close()
	found, err := noticeFiles(src, name)
	if err != nil {
		return err
	}
	dst, err := openLicenseDir(o, name)
	if err != nil {
		return err
	}
	defer dst.Close()
	for _, f := range found {
		data, readErr := src.ReadFile(f)
		if readErr != nil {
			return fmt.Errorf("notices: %w", readErr)
		}
		if mkErr := dst.MkdirAll(path.Dir(f), 0o755); mkErr != nil {
			return mkErr
		}
		if writeErr := dst.WriteFile(f, data, 0o600); writeErr != nil {
			return writeErr
		}
	}
	return nil
}

func noticeFiles(src *os.Root, name string) ([]string, error) {
	var found []string
	for _, p := range noticePatterns {
		m, err := fs.Glob(src.FS(), p)
		if err != nil {
			return nil, fmt.Errorf("notices: %w", err)
		}
		found = append(found, m...)
	}
	if !slices.ContainsFunc(found, func(f string) bool { return strings.HasPrefix(f, "LICENSE") }) {
		return nil, fmt.Errorf("notices: %s ships no LICENSE file", name)
	}
	return found, nil
}

// openLicenseDir opens licenses/<name without its scope>/ under o.out.
func openLicenseDir(o *options, name string) (*os.Root, error) {
	_, short, _ := strings.Cut(name, "/")
	if short == "" {
		short = name
	}
	dir := filepath.Join(o.out, licenseDir, short)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}
