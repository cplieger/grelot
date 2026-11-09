package hook

import (
	"slices"
	"strings"

	"github.com/cplieger/grelot/internal/proctree"
	"github.com/cplieger/grelot/internal/provider"
)

// OneShot reports whether the hook runs in a one-shot run of p's host, by
// p's one_shot: one of its variables holding a listed value, or a listed
// argument, in one of provider.OneShot.Args' forms, on a process of the
// host's own run of ancestors (proctree.Host). ancestors is called only
// when no variable matches. It answers false when neither is seen, so an
// ancestor whose arguments cannot be read keeps the run interactive.
func OneShot(p *provider.Provider, lookup func(string) (string, bool), ancestors func() []proctree.Process) bool {
	for name, values := range p.OneShot.Env {
		if v, ok := lookup(name); ok && slices.Contains(values, v) {
			return true
		}
	}
	if len(p.OneShot.Args) == 0 {
		return false
	}
	for _, proc := range proctree.Host(ancestors(), p.Session.Process) {
		for _, want := range p.OneShot.Args {
			if holds(proc.Args, want) {
				return true
			}
		}
	}
	return false
}

// holds reports whether argv, the program name first, holds want. Nothing
// after "--" counts, since there every argument is an operand.
func holds(argv []string, want string) bool {
	if !strings.HasPrefix(want, "-") {
		i := subcommand(argv)
		return i > 0 && argv[i] == want
	}
	for i := 1; i < len(argv) && argv[i] != "--"; i++ {
		if optionAt(argv, i, want) {
			return true
		}
	}
	return false
}

// optionAt reports whether want, an option in its form, starts at argv[i].
func optionAt(argv []string, i int, want string) bool {
	a := argv[i]
	name, value, valued := strings.Cut(want, "=")
	switch {
	case !valued:
		return a == want
	case value == "":
		return strings.HasPrefix(a, want)
	}
	return a == want || a == name && i+1 < len(argv) && argv[i+1] == value
}

// subcommand returns the index of argv's first operand before "--", the
// only place a subcommand stands (later operands are its own: resume
// review names a session), or 0 when it is not known. Only an option's
// own grammar says whether an option written alone takes the next
// argument (-p review names a profile, --search resume takes none), so
// after one the first operand is not known and the run stays interactive.
func subcommand(argv []string) int {
	for i := 1; i < len(argv) && argv[i] != "--"; i++ {
		switch a := argv[i]; {
		case !isOption(a):
			return i
		case !strings.Contains(a, "="):
			return 0
		}
	}
	return 0
}

// isOption reports whether a is an option; "-" alone is the stdin operand.
func isOption(a string) bool {
	return len(a) > 1 && a[0] == '-'
}
