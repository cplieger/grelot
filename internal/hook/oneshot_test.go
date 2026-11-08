package hook

import (
	"testing"

	"github.com/cplieger/grelot/internal/proctree"
	"github.com/cplieger/grelot/internal/provider"
)

func TestOneShot(t *testing.T) {
	p := &provider.Provider{
		Session: provider.Session{Process: []string{"host", "host.exe"}},
		OneShot: provider.OneShot{Env: map[string][]string{"HOST_ENTRY": {"batch", "sdk"}}, Args: []string{"-p", "exec"}},
	}
	shell := proctree.Process{Name: "bash", Args: []string{"bash", "-c", "grelot hook"}}
	inner := proctree.Process{Name: "host.exe", Args: []string{`C:\x\host.exe`, "-c", "a=1"}}
	outer := proctree.Process{Name: "host.exe", Args: []string{"host.exe", "exec", "-"}}
	tui := proctree.Process{Name: "host", Args: []string{"host"}}
	unread := proctree.Process{Name: "host.exe"}
	tests := []struct {
		name      string
		env       map[string]string
		ancestors []proctree.Process
		want      bool
	}{
		{"a listed value", map[string]string{"HOST_ENTRY": "sdk"}, []proctree.Process{shell, tui}, true},
		{"an unlisted value", map[string]string{"HOST_ENTRY": "cli"}, []proctree.Process{shell, tui}, false},
		{"another variable holding a listed value", map[string]string{"OTHER_ENTRY": "batch"}, []proctree.Process{shell, tui}, false},
		{"a listed argument on an outer process of the run", nil, []proctree.Process{shell, inner, outer}, true},
		{"no listed argument in the run", nil, []proctree.Process{shell, inner, tui}, false},
		{"a listed argument past the run's end", nil, []proctree.Process{shell, inner, shell, outer}, false},
		{"a listed argument on a process of another name", nil, []proctree.Process{{Name: "other", Args: []string{"other", "exec"}}, tui}, false},
		{"unreadable arguments", nil, []proctree.Process{shell, unread, unread}, false},
		{"an argument matched only whole", nil, []proctree.Process{{Name: "host", Args: []string{"host", "-pc", "--print=x", "execute"}}}, false},
		{"a process named as the argument", nil, []proctree.Process{{Name: "host", Args: []string{"exec"}}}, false},
		{"no ancestors", nil, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(name string) (string, bool) { v, ok := tt.env[name]; return v, ok }
			if got := OneShot(p, lookup, func() []proctree.Process { return tt.ancestors }); got != tt.want {
				t.Errorf("OneShot = %v, want %v", got, tt.want)
			}
		})
	}
}

// An entry ending in "=" matches the option given with an attached value,
// and only that option.
func TestOneShot_attachedValue(t *testing.T) {
	p := &provider.Provider{
		Session: provider.Session{Process: []string{"host"}},
		OneShot: provider.OneShot{Args: []string{"--prompt", "--prompt="}},
	}
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"the option with an attached value", []string{"host", "--prompt=fix the tests"}, true},
		{"the option with an empty attached value", []string{"host", "--prompt="}, true},
		{"the option before its value", []string{"host", "--prompt", "fix the tests"}, true},
		{"another option sharing the prefix", []string{"host", "--prompt-file=x"}, false},
		{"the entry inside another argument", []string{"host", "x--prompt=y"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ancestors := []proctree.Process{{Name: "host", Args: tt.args}}
			if got := OneShot(p, func(string) (string, bool) { return "", false }, func() []proctree.Process { return ancestors }); got != tt.want {
				t.Errorf("OneShot = %v, want %v", got, tt.want)
			}
		})
	}
}

// A word is a subcommand, the first operand, and counts only when every
// option before it carries its value: an option written alone may take
// the next argument (-p review) or none (--search resume review), and a
// later operand belongs to the subcommand before it.
func TestOneShot_subcommandPlace(t *testing.T) {
	p := &provider.Provider{
		Session: provider.Session{Process: []string{"host"}},
		OneShot: provider.OneShot{Args: []string{"exec", "review"}},
	}
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"after the program name", []string{"host", "exec", "-"}, true},
		{"after options carrying their values", []string{"host", "-c=a=b", "--profile=work", "review"}, true},
		{"after an option written alone and its value", []string{"host", "-c", "a=b", "exec"}, false},
		{"after a flag that takes no value", []string{"host", "--search", "exec"}, false},
		{"as another subcommand's operand after a flag", []string{"host", "--search", "resume", "review"}, false},
		{"after a flag and another subcommand", []string{"host", "--flag", "fork", "exec"}, false},
		{"after the stdin operand", []string{"host", "-", "exec"}, false},
		{"as another subcommand's operand", []string{"host", "resume", "review"}, false},
		{"as another subcommand's second operand", []string{"host", "resume", "0198c1e2-7a3b-4c5d-9e8f-123456789abc", "review"}, false},
		{"after another subcommand's option", []string{"host", "fork", "--last", "-c", "a=b", "exec"}, false},
		{"as a short option's value", []string{"host", "-p", "review"}, false},
		{"as a long option's value", []string{"host", "--profile", "exec"}, false},
		{"as an option's value, then a subcommand", []string{"host", "-p", "review", "exec"}, false},
		{"after --", []string{"host", "--", "exec"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ancestors := []proctree.Process{{Name: "host", Args: tt.args}}
			if got := OneShot(p, func(string) (string, bool) { return "", false }, func() []proctree.Process { return ancestors }); got != tt.want {
				t.Errorf("OneShot(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// An option given with its value matches written either way, and only
// with that value.
func TestOneShot_optionValue(t *testing.T) {
	p := &provider.Provider{
		Session: provider.Session{Process: []string{"host"}},
		OneShot: provider.OneShot{Args: []string{"--format=json"}},
	}
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"as one argument", []string{"host", "--format=json"}, true},
		{"as the option then the value", []string{"host", "chat", "--format", "json", "hello"}, true},
		{"with another value", []string{"host", "--format", "text", "json"}, false},
		{"the value alone, as a prompt", []string{"host", "chat", "json"}, false},
		{"the option last, with no value", []string{"host", "--format"}, false},
		{"after --", []string{"host", "--", "--format=json"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ancestors := []proctree.Process{{Name: "host", Args: tt.args}}
			if got := OneShot(p, func(string) (string, bool) { return "", false }, func() []proctree.Process { return ancestors }); got != tt.want {
				t.Errorf("OneShot(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// A variable that says one-shot settles it without reading the ancestors.
func TestOneShot_envReadsNoAncestors(t *testing.T) {
	p := &provider.Provider{
		Session: provider.Session{Process: []string{"host"}},
		OneShot: provider.OneShot{Env: map[string][]string{"HOST_ENTRY": {"batch"}}, Args: []string{"-p"}},
	}
	read := false
	got := OneShot(p, func(string) (string, bool) { return "batch", true }, func() []proctree.Process { read = true; return nil })
	if !got || read {
		t.Errorf("OneShot = %v with the ancestors read %v; want true without reading them", got, read)
	}
}

// A provider declaring no one_shot never calls a run one-shot.
func TestOneShot_undeclared(t *testing.T) {
	p := &provider.Provider{Session: provider.Session{Process: []string{"host"}}}
	ancestors := []proctree.Process{{Name: "host", Args: []string{"host", "exec"}}}
	if OneShot(p, func(string) (string, bool) { return "batch", true }, func() []proctree.Process { return ancestors }) {
		t.Error("OneShot = true for a provider with no one_shot")
	}
}
