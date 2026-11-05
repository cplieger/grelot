package provider

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func env(kv ...string) func(string) (string, bool) {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

// Hosts that name their CLI process (twin names outer's), one that names
// none, one that extends outer and so is chosen only by name, and one with
// no session.
var detectSet = files(
	"outer", "name: outer\ndetect: {env: [OUTER_SESSION, OUTER_HOME]}\nsession: {env: OUTER_SESSION, process: [outer]}\n",
	"inner", "name: inner\ndetect: {env: [INNER_THREAD, INNER_HOME]}\nsession: {env: INNER_THREAD, process: [inner, inner.exe]}\n",
	"plain", "name: plain\ndetect: {env: [PLAIN_ID, PLAIN_HOME]}\nsession: {env: PLAIN_ID}\n",
	"variant", "name: variant\nextends: outer\n",
	"doorless", "name: doorless\ndetect: {env: [DOORLESS_ID]}\n",
	"twin", "name: twin\ndetect: {env: [TWIN_ID, TWIN_HOME]}\nsession: {env: TWIN_ID, process: [outer]}\n",
)

func TestDetect(t *testing.T) {
	s := mustSet(t, detectSet)
	cases := []struct {
		name      string
		override  string
		lookup    func(string) (string, bool)
		ancestors []string
		want      string
	}{
		{"one match", "", env("INNER_THREAD", "t1"), nil, "inner"},
		{"the most variables wins", "", env("OUTER_SESSION", "s", "OUTER_HOME", "/h", "PLAIN_ID", "p"), nil, "outer"},
		{"a variable set empty is present", "", env("INNER_THREAD", ""), nil, "inner"},
		{"a nested host picks the inner one", "", env("OUTER_SESSION", "s", "INNER_THREAD", "t"), []string{"bash", "inner.exe", "zsh", "outer"}, "inner"},
		{"the same shell under the outer host", "", env("OUTER_SESSION", "s", "INNER_THREAD", "t"), []string{"bash", "outer", "inner"}, "outer"},
		{"the override wins over detection", "inner", env("OUTER_SESSION", "s", "OUTER_HOME", "/h"), nil, "inner"},
		{"the override chooses a file that extends", "variant", env("OUTER_SESSION", "s"), nil, "variant"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := s.Detect(c.override, c.lookup, c.ancestors)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if p.Name != c.want {
				t.Errorf("Detect = %s, want %s", p.Name, c.want)
			}
		})
	}
}

func TestDetectRefuses(t *testing.T) {
	s := mustSet(t, detectSet)
	cases := []struct {
		name      string
		lookup    func(string) (string, bool)
		ancestors []string
		matched   []string
	}{
		{"no match", env("HOME", "/h"), nil, nil},
		{"a tie where one names no process", env("OUTER_SESSION", "s", "PLAIN_ID", "p"), []string{"outer"}, []string{"outer", "plain"}},
		{"a tie no ancestor breaks", env("OUTER_SESSION", "s", "INNER_THREAD", "t"), []string{"bash", "init"}, []string{"inner", "outer"}},
		{"a tie whose nearest process two providers name", env("OUTER_SESSION", "s", "TWIN_ID", "t"), []string{"bash", "outer"}, []string{"outer", "twin"}},
		{"a tie with no ancestors", env("OUTER_SESSION", "s", "INNER_THREAD", "t"), nil, []string{"inner", "outer"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := s.Detect("", c.lookup, c.ancestors)
			if err == nil {
				t.Fatalf("Detect = %s, want a refusal", p.Name)
			}
			de, ok := errors.AsType[*DetectError](err)
			if !ok {
				t.Fatalf("error %T %v is not a DetectError", err, err)
			}
			if !slices.Equal(de.Matched, c.matched) {
				t.Errorf("matched = %v, want %v", de.Matched, c.matched)
			}
			msg := err.Error()
			for _, w := range append([]string{"--provider <name>", "provider: <name>"}, c.matched...) {
				if !strings.Contains(msg, w) {
					t.Errorf("line %q lacks %q", msg, w)
				}
			}
		})
	}
	t.Run("an invalid file is named beside none matched", func(t *testing.T) {
		broken := files("zeta", "name: zeta\ndetect: {env: [ZETA_ID]}\ncolour: red\n", "alpha", "name: alpha\nsurprise: 1\n")
		_, err := mustSet(t, detectSet, broken).Detect("", env("ZETA_ID", "z"), nil)
		de, ok := errors.AsType[*DetectError](err)
		if !ok {
			t.Fatalf("error %T %v is not a DetectError", err, err)
		}
		if !slices.Equal(de.Invalid, []string{"alpha", "zeta"}) {
			t.Errorf("invalid = %v, want [alpha zeta]", de.Invalid)
		}
		msg := err.Error()
		for _, w := range []string{"none matched", "providers alpha, zeta invalid", "grelot provider check on each file", "--provider <name>"} {
			if !strings.Contains(msg, w) {
				t.Errorf("line %q lacks %q", msg, w)
			}
		}
	})
	t.Run("one invalid file is named in the singular", func(t *testing.T) {
		broken := files("alpha", "name: alpha\nsurprise: 1\n")
		_, err := mustSet(t, detectSet, broken).Detect("", env("ZETA_ID", "z"), nil)
		msg := err.Error()
		for _, w := range []string{"; provider alpha invalid", "grelot provider check on its file"} {
			if !strings.Contains(msg, w) {
				t.Errorf("line %q lacks %q", msg, w)
			}
		}
	})
	t.Run("no invalid file names none", func(t *testing.T) {
		_, err := s.Detect("", env("HOME", "/h"), nil)
		if strings.Contains(err.Error(), "invalid") {
			t.Errorf("line %q names an invalid provider in a set with none", err)
		}
	})
	t.Run("an unknown override", func(t *testing.T) {
		if _, err := s.Detect("nowhere", env("INNER_THREAD", "t"), nil); err == nil || !strings.Contains(err.Error(), "nowhere") {
			t.Errorf("Detect(nowhere) = %v", err)
		}
	})
}

func TestSessionID(t *testing.T) {
	s := mustSet(t, detectSet)
	inner, err := s.Get("inner")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("the session variable", func(t *testing.T) {
		id, err := inner.SessionID(env("INNER_THREAD", "t-42"))
		if err != nil || id != "t-42" {
			t.Errorf("SessionID = %q, %v", id, err)
		}
	})
	refusals := []struct {
		name     string
		provider string
		lookup   func(string) (string, bool)
		want     []string
	}{
		{"a file with no session.env", "doorless", env("DOORLESS_ID", "d"), []string{"provider doorless", "records no session variable"}},
		{"the variable unset", "inner", env(), []string{"provider inner", "INNER_THREAD"}},
		{"the variable empty", "inner", env("INNER_THREAD", ""), []string{"provider inner", "INNER_THREAD"}},
	}
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			p, err := s.Get(c.provider)
			if err != nil {
				t.Fatal(err)
			}
			id, err := p.SessionID(c.lookup)
			if id != "" {
				t.Errorf("SessionID = %q, want no session", id)
			}
			if _, ok := errors.AsType[*NoSessionError](err); !ok {
				t.Fatalf("error %T %v is not a NoSessionError", err, err)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("line %q lacks %q", err, w)
				}
			}
		})
	}
}
