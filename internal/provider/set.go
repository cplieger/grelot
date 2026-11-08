package provider

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
)

const yamlExt = ".yaml"

// Set is the providers a client knows, with extends resolved. A file that
// failed to load stays in the set under its name, so a caller can tell an
// invalid provider from an unknown one.
type Set struct {
	valid   map[string]*Provider
	invalid map[string]error
}

// LoadSet reads every *.yaml file at the root of each layer, a later layer
// replacing an earlier layer's file of the same name, then resolves extends.
// A symlink is read as its target. A layer that does not exist is empty;
// another read error, a dangling symlink included, fails the set.
func LoadSet(layers ...fs.FS) (*Set, error) {
	files, err := readLayers(layers)
	if err != nil {
		return nil, err
	}
	loaded := map[string]*Provider{}
	s := &Set{valid: map[string]*Provider{}, invalid: map[string]error{}}
	for name, data := range files {
		p, err := Load(name, data)
		if err != nil {
			s.invalid[name] = err
			continue
		}
		loaded[name] = p
	}
	for name, p := range loaded {
		merged, err := resolve(p, loaded, s.invalid)
		if err != nil {
			s.invalid[name] = err
			continue
		}
		s.valid[name] = merged
	}
	return s, nil
}

func readLayers(layers []fs.FS) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, layer := range layers {
		if err := readLayer(layer, files); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// readLayer adds the layer's *.yaml files to files, replacing by name. A
// layer that does not exist adds nothing.
func readLayer(layer fs.FS, files map[string][]byte) error {
	entries, err := fs.ReadDir(layer, ".")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read providers: %w", err)
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), yamlExt)
		if !ok || e.IsDir() {
			continue
		}
		data, regular, err := readFile(layer, e.Name())
		if err != nil {
			return fmt.Errorf("read provider %s: %w", name, err)
		}
		if regular {
			files[name] = data
		}
	}
	return nil
}

// readFile reads a regular file, or answers false for anything else. Stat
// follows a symlink, as a mounted config file often is; a dangling one is an
// error, and a pipe is never opened.
func readFile(layer fs.FS, name string) (data []byte, regular bool, err error) {
	info, err := fs.Stat(layer, name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, err
	}
	data, err = fs.ReadFile(layer, name)
	return data, err == nil, err
}

// Get returns the named provider. An invalid file answers its load error; a
// name no layer holds answers an error saying so.
func (s *Set) Get(name string) (*Provider, error) {
	if p, ok := s.valid[name]; ok {
		return p, nil
	}
	if err, ok := s.invalid[name]; ok {
		return nil, err
	}
	return nil, &LoadError{Provider: name, Problem: "no such provider"}
}

// Names lists every provider in the set, valid or not, sorted.
func (s *Set) Names() []string {
	names := append(slices.Collect(maps.Keys(s.valid)), slices.Collect(maps.Keys(s.invalid))...)
	slices.Sort(names)
	return names
}

// resolve applies one level of extends. invalid holds the files that failed
// to load, so a child of one is refused naming its parent as invalid rather
// than as missing.
func resolve(p *Provider, loaded map[string]*Provider, invalid map[string]error) (*Provider, error) {
	if p.Extends == "" {
		return p, nil
	}
	fail := func(problem string) error {
		return &LoadError{Provider: p.Name, Key: keyExtends, Problem: problem}
	}
	parent, ok := loaded[p.Extends]
	switch {
	case !ok && invalid[p.Extends] != nil:
		return nil, fail(fmt.Sprintf("provider %s is invalid", p.Extends))
	case !ok:
		return nil, fail(fmt.Sprintf("no provider %s", p.Extends))
	case parent.Extends != "":
		return nil, fail(fmt.Sprintf("provider %s extends another; extends is one level only", p.Extends))
	}
	return merge(parent, p), nil
}

// merge builds child over parent: every key but detect is inherited, doors
// merge by name with the child's replacing the parent's in place, and every
// other key the child sets replaces the parent's whole.
func merge(parent, child *Provider) *Provider {
	m := *parent
	m.Name, m.Extends, m.Detect = child.Name, child.Extends, Detect{}
	m.set = maps.Clone(parent.set)
	maps.Copy(m.set, child.set)
	delete(m.set, keyDetect)
	if child.has("evidence") {
		m.Evidence = child.Evidence
	}
	if child.has("notes") {
		m.Notes = child.Notes
	}
	if child.has("session") {
		m.Session = child.Session
	}
	if child.has("hooks") {
		m.Hooks = child.Hooks
	}
	m.Doors = slices.Clone(parent.Doors)
	for i := range child.Doors {
		name := child.Doors[i].Name
		if j := slices.IndexFunc(m.Doors, func(p Door) bool { return p.Name == name }); j >= 0 {
			m.Doors[j] = child.Doors[i]
		} else {
			m.Doors = append(m.Doors, child.Doors[i])
		}
	}
	slices.SortStableFunc(m.Doors, func(a, b Door) int { return a.Layer - b.Layer })
	return &m
}
