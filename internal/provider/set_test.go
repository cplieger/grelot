package provider

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func files(kv ...string) fstest.MapFS {
	m := fstest.MapFS{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]+".yaml"] = &fstest.MapFile{Data: []byte(kv[i+1])}
	}
	return m
}

func mustSet(t *testing.T, layers ...fstest.MapFS) *Set {
	t.Helper()
	fss := make([]fs.FS, len(layers))
	for i, l := range layers {
		fss[i] = l
	}
	s, err := LoadSet(fss...)
	if err != nil {
		t.Fatalf("LoadSet: %v", err)
	}
	return s
}

func doorNames(p *Provider) []string {
	var names []string
	for _, d := range p.Doors {
		names = append(names, d.Name)
	}
	return names
}

const parentFile = `name: base
evidence: measured
notes: docs/base.md
detect: {env: [BASE_ID]}
session: {env: BASE_ID, process: [base]}
doors:
  - {name: queue, layer: 2, transport: exec, argv: [base, queue]}
  - {name: watch, layer: 2, transport: exit}
  - {name: out, layer: 2, transport: stdout}
hooks:
  prompt: {event: Prompt}
  stop: {event: Stop}
`

func TestLoadSetLayers(t *testing.T) {
	shipped := files("base", parentFile, "other", "name: other\nnotes: shipped\n")
	user := files("other", "name: other\nnotes: user\n", "extra", "name: extra\n")
	s := mustSet(t, shipped, user)
	t.Run("a later layer replaces by name", func(t *testing.T) {
		p, err := s.Get("other")
		if err != nil {
			t.Fatal(err)
		}
		if p.Notes != "user" {
			t.Errorf("notes = %q, want the user layer's", p.Notes)
		}
	})
	t.Run("names of every layer", func(t *testing.T) {
		if got, want := s.Names(), []string{"base", "extra", "other"}; !slices.Equal(got, want) {
			t.Errorf("Names = %v, want %v", got, want)
		}
	})
	t.Run("an invalid user file replaces a valid shipped one", func(t *testing.T) {
		s := mustSet(t, shipped, files("base", "name: base\nsurprise: 1\n"))
		if _, err := s.Get("base"); err == nil || !strings.Contains(err.Error(), "surprise") {
			t.Errorf("Get(base) = %v, want the user file's load error", err)
		}
		if !slices.Contains(s.Names(), "base") {
			t.Errorf("Names %v lacks the invalid provider", s.Names())
		}
	})
	t.Run("unknown name", func(t *testing.T) {
		if _, err := s.Get("nowhere"); err == nil || !strings.Contains(err.Error(), "nowhere") {
			t.Errorf("Get(nowhere) = %v", err)
		}
	})
	t.Run("only yaml files at the root", func(t *testing.T) {
		m := files("base", parentFile)
		m["base/hooks.json"] = &fstest.MapFile{Data: []byte("{}")}
		m["README.md"] = &fstest.MapFile{Data: []byte("x")}
		if got := mustSet(t, m).Names(); !slices.Equal(got, []string{"base"}) {
			t.Errorf("Names = %v, want [base]", got)
		}
	})
	t.Run("a symlinked file replaces by name", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "kept.yaml")
		if err := os.WriteFile(target, []byte("name: other\nnotes: linked\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, target, filepath.Join(dir, "other.yaml"))
		s, err := LoadSet(shipped, os.DirFS(dir))
		if err != nil {
			t.Fatalf("LoadSet: %v", err)
		}
		if p, err := s.Get("other"); err != nil || p.Notes != "linked" {
			t.Errorf("Get(other) = %+v, %v; want the linked file", p, err)
		}
	})
	t.Run("a dangling symlink fails the set", func(t *testing.T) {
		dir := t.TempDir()
		mustSymlink(t, filepath.Join(dir, "absent.yaml"), filepath.Join(dir, "other.yaml"))
		if _, err := LoadSet(shipped, os.DirFS(dir)); err == nil || !strings.Contains(err.Error(), "other") {
			t.Errorf("LoadSet = %v, want a read error naming other", err)
		}
	})
	t.Run("a directory named like a file is ignored", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "other.yaml"), 0o700); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, filepath.Join(dir, "other.yaml"), filepath.Join(dir, "extra.yaml"))
		s, err := LoadSet(shipped, os.DirFS(dir))
		if err != nil {
			t.Fatalf("LoadSet: %v", err)
		}
		if p, err := s.Get("other"); err != nil || p.Notes != "shipped" || slices.Contains(s.Names(), "extra") {
			t.Errorf("Names = %v, other = %+v, %v; want the shipped file and no extra", s.Names(), p, err)
		}
	})
	t.Run("a missing layer is empty", func(t *testing.T) {
		s, err := LoadSet(shipped, os.DirFS(filepath.Join(t.TempDir(), "absent")))
		if err != nil {
			t.Fatalf("LoadSet: %v", err)
		}
		if !slices.Equal(s.Names(), []string{"base", "other"}) {
			t.Errorf("Names = %v", s.Names())
		}
	})
}

func TestLoadSetExtends(t *testing.T) {
	child := `name: child
extends: base
evidence: unverified
doors:
  - {name: watch, layer: 2, transport: stdout}
  - {name: push, layer: 1, transport: http, url: 'http://receiver/x', body: '{}'}
  - {name: tail, layer: 2, transport: exit}
hooks:
  stop: {event: Halt}
`
	s := mustSet(t, files("base", parentFile, "child", child))
	p, err := s.Get("child")
	if err != nil {
		t.Fatalf("Get(child): %v", err)
	}
	t.Run("doors merge by name, by layer then inherited first", func(t *testing.T) {
		if got, want := doorNames(p), []string{"push", "queue", "watch", "out", "tail"}; !slices.Equal(got, want) {
			t.Errorf("doors = %v, want %v", got, want)
		}
		if p.Doors[2].Transport != TransportStdout {
			t.Errorf("door watch transport = %q, want the child's stdout", p.Doors[2].Transport)
		}
	})
	t.Run("a key the child sets replaces the parent's whole", func(t *testing.T) {
		if p.Hooks.Prompt != nil || p.Hooks.Stop == nil || p.Hooks.Stop.Event != "Halt" {
			t.Errorf("hooks = prompt %+v, stop %+v; want only the child's stop", p.Hooks.Prompt, p.Hooks.Stop)
		}
		if p.Evidence != EvidenceUnverified {
			t.Errorf("evidence = %q", p.Evidence)
		}
	})
	t.Run("a key the child leaves out is inherited", func(t *testing.T) {
		if p.Session.Env != "BASE_ID" || p.Notes != "docs/base.md" {
			t.Errorf("session %+v, notes %q", p.Session, p.Notes)
		}
		if !p.has("session") {
			t.Error("Has(session) = false for an inherited key")
		}
	})
	t.Run("detect is not inherited", func(t *testing.T) {
		if len(p.Detect.Env) != 0 || p.has("detect") {
			t.Errorf("detect = %+v", p.Detect)
		}
	})
	t.Run("the name is the child's", func(t *testing.T) {
		if p.Name != "child" || p.Extends != "base" {
			t.Errorf("name %q extends %q", p.Name, p.Extends)
		}
	})
	t.Run("the parent is unchanged", func(t *testing.T) {
		b, err := s.Get("base")
		if err != nil {
			t.Fatal(err)
		}
		if got := doorNames(b); !slices.Equal(got, []string{"queue", "watch", "out"}) || b.Doors[1].Transport != TransportExit {
			t.Errorf("base doors = %v", got)
		}
	})
	t.Run("an inherited door renders", func(t *testing.T) {
		argv, err := p.Doors[1].RenderArgv(&Data{})
		if err != nil || !slices.Equal(argv, []string{"base", "queue"}) {
			t.Errorf("RenderArgv = %v, %v", argv, err)
		}
	})
}

func TestLoadSetExtendsOneShot(t *testing.T) {
	oneShotParent := parentFile + "one_shot: {env: {BASE_MODE: [batch]}, args: [--once]}\n"
	t.Run("args read the inherited session.process", func(t *testing.T) {
		p, err := mustSet(t, files("base", parentFile, "child", "name: child\nextends: base\none_shot: {args: [--once]}\n")).Get("child")
		if err != nil {
			t.Fatalf("Get(child): %v", err)
		}
		if !slices.Equal(p.OneShot.Args, []string{"--once"}) || !slices.Equal(p.Session.Process, []string{"base"}) {
			t.Errorf("one_shot %+v, session.process %v", p.OneShot, p.Session.Process)
		}
	})
	t.Run("a child that leaves one_shot out inherits it", func(t *testing.T) {
		p, err := mustSet(t, files("base", oneShotParent, "child", "name: child\nextends: base\n")).Get("child")
		if err != nil {
			t.Fatalf("Get(child): %v", err)
		}
		if !slices.Equal(p.OneShot.Args, []string{"--once"}) || !slices.Equal(p.OneShot.Env["BASE_MODE"], []string{"batch"}) {
			t.Errorf("one_shot = %+v", p.OneShot)
		}
	})
	t.Run("a child's one_shot replaces the parent's whole", func(t *testing.T) {
		p, err := mustSet(t, files("base", oneShotParent, "child", "name: child\nextends: base\none_shot: {args: [-q]}\n")).Get("child")
		if err != nil {
			t.Fatalf("Get(child): %v", err)
		}
		if !slices.Equal(p.OneShot.Args, []string{"-q"}) || p.OneShot.Env != nil {
			t.Errorf("one_shot = %+v, want only the child's args", p.OneShot)
		}
	})
	t.Run("a session without process under inherited args is refused", func(t *testing.T) {
		_, err := mustSet(t, files("base", oneShotParent, "child", "name: child\nextends: base\nsession: {env: CHILD_ID}\n")).Get("child")
		if le, ok := errors.AsType[*LoadError](err); !ok || le.Provider != "child" || le.Key != "one_shot.args" {
			t.Errorf("Get(child) = %v, want a one_shot.args LoadError for child", err)
		}
	})
}

func TestLoadSetExtendsRefuses(t *testing.T) {
	cases := []struct {
		name  string
		layer fstest.MapFS
		want  string
	}{
		{"unknown parent", files("child", "name: child\nextends: nowhere\n"), "extends: no provider nowhere"},
		{"a chain", files("base", parentFile, "mid", "name: mid\nextends: base\n", "child", "name: child\nextends: mid\n"), "extends: provider mid extends another"},
		{"an invalid parent", files("base", "name: base\nsurprise: 1\n", "child", "name: child\nextends: base\n"), "extends: provider base is invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := mustSet(t, c.layer).Get("child")
			if err == nil {
				t.Fatal("Get(child) accepted the file")
			}
			if le, ok := errors.AsType[*LoadError](err); !ok || le.Provider != "child" {
				t.Errorf("error %T %v is not a LoadError for child", err, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q lacks %q", err, c.want)
			}
		})
	}
}
