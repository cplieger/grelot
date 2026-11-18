package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const regenerate = "run go run ./cmd/wire-codegen from the repo root and commit the result"

// The committed client, OpenAPI document and edges file match what generate
// writes, byte for byte.
func TestGenerate_matchesTheCommittedFiles(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	repo := "."
	tmp := t.TempDir()
	tsDir := filepath.Join("internal", "webui", "static-src", "src", "wire")
	for _, d := range []string{tsDir, "docs"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// The edges' TypeScript side is named by package.json. The committed edges
	// file is not copied: generating merges into it, so a copy would pass
	// with the edges write gone.
	pkg := filepath.Join("internal", "webui", "static-src", "package.json")
	b, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, pkg), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := generate(t.Context(), tmp); err != nil {
		t.Fatalf("generate: %v", err)
	}
	generated := genFiles(t, filepath.Join(tmp, tsDir))
	if committed := genFiles(t, filepath.Join(repo, tsDir)); !slices.Equal(committed, generated) {
		t.Errorf("committed %s holds %q, the generator writes %q: %s", tsDir, committed, generated, regenerate)
	}
	rels := []string{filepath.Join("docs", "openapi.json"), "deadset-edges.json"}
	for _, f := range generated {
		rels = append(rels, filepath.Join(tsDir, f))
	}
	for _, rel := range rels {
		want, err := os.ReadFile(filepath.Join(tmp, rel))
		if err != nil {
			t.Fatalf("generate wrote no %s: %v", rel, err)
		}
		got, err := os.ReadFile(filepath.Join(repo, rel))
		switch {
		case err != nil:
			t.Errorf("committed %s: %v: %s", rel, err, regenerate)
		case !bytes.Equal(got, want):
			t.Errorf("%s differs from what the generator writes: %s", rel, regenerate)
		}
	}
}

// genFiles lists dir's *.gen.ts files, sorted.
func genFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.gen.ts"))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(m))
	for _, p := range m {
		out = append(out, filepath.Base(p))
	}
	slices.Sort(out)
	return out
}
