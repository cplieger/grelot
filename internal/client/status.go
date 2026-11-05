package client

import (
	"regexp"
	"strings"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/grelot/internal/provider"
)

// wakeDown is how the server's status line says no wake reaches the
// identity.
const wakeDown = " · wake: down"

// releases is where the status line sends a client of another release.
const releases = "https://github.com/cplieger/grelot/releases/tag/"

// Status is the line every command prints first: the server's
// Grelot-Status and what only the client knows.
type Status struct {
	// Server is the server's line, empty when the answer carried none.
	Server string
	// Start is the start line of the provider's first layer-2 door,
	// appended when the server says the wake is down.
	Start string
	// Client and ServerVersion are compared by release base.
	Client        string
	ServerVersion string
	// Invalid lists the provider files that failed to load.
	Invalid []string
}

// Line is the status line, or "" when there is nothing to say.
func (s *Status) Line() string {
	var extra []string
	if s.Start != "" && strings.Contains(s.Server, wakeDown) {
		extra = append(extra, textsafe.SanitizeSingleLine(s.Start))
	}
	if c, ok := releaseBase(s.Client); ok {
		if v, ok := releaseBase(s.ServerVersion); ok && v != c {
			extra = append(extra, "this client is "+c+" and the server "+v+": install grelot "+v+" from "+releases+v)
		}
	}
	for _, name := range s.Invalid {
		extra = append(extra, "provider "+name+" invalid: run grelot provider check on its file")
	}
	switch {
	case s.Server == "" && len(extra) == 0:
		return ""
	case s.Server == "":
		return "grelot: " + strings.Join(extra, " · ")
	}
	return strings.Join(append([]string{textsafe.SanitizeSingleLine(s.Server)}, extra...), " · ")
}

var releasePattern = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// releaseBase is v with any -dev.N suffix removed: a promoted stable image
// keeps its dev build's version while the release's clients carry the
// base. An unstamped "dev" build, or anything else, has none.
func releaseBase(v string) (string, bool) {
	base, _, _ := strings.Cut(v, "-")
	return base, releasePattern.MatchString(base)
}

// FirstStart is the start line of p's first layer-2 door, "" when it has
// none.
func FirstStart(p *provider.Provider) string {
	if p == nil {
		return ""
	}
	for i := range p.Doors {
		if p.Doors[i].Layer == 2 {
			return p.Doors[i].Start
		}
	}
	return ""
}

// Invalid lists the providers in set whose file failed to load.
func Invalid(set *provider.Set) []string {
	var out []string
	for _, name := range set.Names() {
		if _, err := set.Get(name); err != nil {
			out = append(out, name)
		}
	}
	return out
}
