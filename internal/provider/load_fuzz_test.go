package provider

import (
	"errors"
	"regexp"
	"slices"
	"testing"
)

var envRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// FuzzLoad_refusesOrValid checks that every input is either refused with a
// LoadError or yields a file named as asked, with no environment reference
// left in its notes and its doors ordered by layer.
func FuzzLoad_refusesOrValid(f *testing.F) {
	f.Add([]byte(full))
	f.Add([]byte("name: example\nnotes: ${HOME}\n"))
	f.Add([]byte("name: example\n---\n"))
	f.Add([]byte("name: example\ndoors:\n  - {name: aa, layer: 2, transport: exit, hold: 1s}\n"))
	f.Add([]byte("name: example\nhooks: {prompt: {event: P, signal: '(?P<line>x)'}}\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := Load("example", data)
		if err != nil {
			if _, ok := errors.AsType[*LoadError](err); !ok {
				t.Fatalf("error %T is not a LoadError: %v", err, err)
			}
			return
		}
		if p.Name != "example" {
			t.Fatalf("accepted a file named %q", p.Name)
		}
		if envRef.MatchString(p.Notes) || envRef.MatchString(p.Session.Env) {
			t.Fatalf("accepted an environment reference: notes %q", p.Notes)
		}
		if !slices.IsSortedFunc(p.Doors, func(a, b Door) int { return a.Layer - b.Layer }) {
			t.Fatalf("doors not ordered by layer: %+v", p.Doors)
		}
	})
}
