package client

import (
	"regexp"
	"slices"
	"strconv"
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
	if line := Upgrade(s.Client, s.ServerVersion); line != "" {
		extra = append(extra, line)
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

// Upgrade is the line naming the client to install for a server of
// version server, "" when either is unknown or client is server's release:
// a promoted stable image keeps its dev build's version while the
// release's clients carry the base, so only another base, or an earlier
// dev build of the same base, names one. The server's exact version is
// always a tag, so the line names it.
func Upgrade(client, server string) string {
	c, ok := parseVersion(client)
	s, sok := parseVersion(server)
	if !ok || !sok || (c.base == s.base && (c.dev < 0 || s.dev <= c.dev)) {
		return ""
	}
	return "this client is " + client + " and the server " + server + ": install grelot " + server +
		" with go install github.com/cplieger/grelot@" + server + " or from " + releases + server
}

// Older reports whether client is an earlier build than server: an earlier
// release base, or the same base at an earlier dev build. A release and
// its own dev builds are neither, for the reason Upgrade gives.
func Older(client, server string) bool {
	c, ok := parseVersion(client)
	s, sok := parseVersion(server)
	if !ok || !sok {
		return false
	}
	if order := slices.Compare(c.parts[:], s.parts[:]); order != 0 {
		return order < 0
	}
	return c.dev >= 0 && s.dev > c.dev
}

var versionPattern = regexp.MustCompile(`^v(\d{1,9})\.(\d{1,9})\.(\d{1,9})(?:-dev\.(\d{1,9}))?$`)

// version is a parsed vX.Y.Z or vX.Y.Z-dev.N; dev is -1 for a release.
type version struct {
	base  string
	parts [3]int
	dev   int
}

// parseVersion reads v; an unstamped "dev" build, or anything else, is
// not a version.
func parseVersion(v string) (version, bool) {
	m := versionPattern.FindStringSubmatch(v)
	if m == nil {
		return version{}, false
	}
	out := version{base: strings.SplitN(v, "-", 2)[0], dev: -1}
	for i := range out.parts {
		out.parts[i], _ = strconv.Atoi(m[i+1])
	}
	if m[4] != "" {
		out.dev, _ = strconv.Atoi(m[4])
	}
	return out, true
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
