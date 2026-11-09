package providers_test

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/grelot/internal/provider"
	"github.com/cplieger/grelot/providers"
)

func TestFS_everyShippedFileLoads(t *testing.T) {
	files, err := fs.Glob(providers.FS, "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("the embedded folder holds no provider file")
	}
	set, err := provider.LoadSet(providers.FS)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	var want []string
	for _, f := range files {
		want = append(want, strings.TrimSuffix(f, ".yaml"))
	}
	if got := set.Names(); !slices.Equal(got, want) {
		t.Errorf("set names %v, want one per file %v", got, want)
	}
	for _, name := range want {
		if _, err := set.Get(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestFixtures_indexNamesEachFixture keeps testdata/golden/index.json, the
// record of where each fixture was recorded, in step with the fixtures.
func TestFixtures_indexNamesEachFixture(t *testing.T) {
	root := filepath.Join("testdata", "golden")
	data, err := os.ReadFile(filepath.Join(root, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index map[string]string
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("index.json: %v", err)
	}
	var files []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == filepath.Join(root, "index.json") {
			return err
		}
		rel, err := filepath.Rel(root, path)
		files = append(files, filepath.ToSlash(rel))
		if !json.Valid(mustRead(t, path)) {
			t.Errorf("%s is not JSON", rel)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	if got := slices.Sorted(maps.Keys(index)); !slices.Equal(got, files) {
		t.Errorf("index.json names %v, want the fixtures %v", got, files)
	}
	for name, source := range index {
		if source == "" {
			t.Errorf("index.json gives %s no source", name)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
