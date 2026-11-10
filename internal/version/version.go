// Package version reads grelot's version strings: the one a build
// reports, the one it sends and answers as Grelot-Version, and their order.
package version

import (
	"runtime/debug"
	"strconv"
	"strings"
)

// Dev is the version of a build that carries no release version. Every
// server speaks it, and no skew line compares it.
const Dev = "dev"

// devel is debug.BuildInfo's main module version for a build that is not
// of a module version and has no VCS stamp.
const devel = "(devel)"

// V is a parsed vX.Y.Z or vX.Y.Z-dev.N.
type V struct {
	Release [3]int
	// Dev is N of a -dev.N build, -1 for a release.
	Dev int
}

// Parse reads s as vX.Y.Z or vX.Y.Z-dev.N, each number of at most 9
// digits with no leading zero. Anything else, Dev and a pseudo-version
// included, is not a version.
func Parse(s string) (V, bool) {
	out := V{Dev: -1}
	base, pre, hasPre := strings.Cut(s, "-")
	if hasPre {
		n, found := strings.CutPrefix(pre, "dev.")
		if !found || !isNumber(n) {
			return V{}, false
		}
		out.Dev, _ = strconv.Atoi(n)
	}
	rest, found := strings.CutPrefix(base, "v")
	parts := strings.Split(rest, ".")
	if !found || len(parts) != len(out.Release) {
		return V{}, false
	}
	for i, p := range parts {
		if !isNumber(p) {
			return V{}, false
		}
		out.Release[i], _ = strconv.Atoi(p)
	}
	return out, true
}

// Install is the instruction that gets the client of version v: go install
// at its tag, and for a release also its release page, which carries the
// client binaries. A dev build's tag has no release, so the page is left out.
func Install(v string) string {
	line := "install grelot " + v + " with go install github.com/cplieger/grelot@" + v
	if p, ok := Parse(v); ok && p.Dev < 0 {
		line += " or from https://github.com/cplieger/grelot/releases/tag/" + v
	}
	return line + ", then check both with grelot version"
}

func isNumber(s string) bool {
	if s == "" || len(s) > 9 || (len(s) > 1 && s[0] == '0') {
		return false
	}
	return strings.Trim(s, "0123456789") == ""
}

// Build is the version a binary reports: stamp, the -X main.version a
// release build carries, unless it is empty or Dev; else the main module's
// version from read, as go version -m shows it (a tag, a -dev.N tag, a
// pseudo-version, a +dirty checkout); else Dev.
func Build(stamp string, read func() (*debug.BuildInfo, bool)) string {
	if stamp != "" && stamp != Dev {
		return stamp
	}
	if info, ok := read(); ok && info.Main.Version != "" && info.Main.Version != devel {
		return info.Main.Version
	}
	return Dev
}

// Wire is the Grelot-Version of a build of version build: build when Parse
// reads it, else Dev, since a server answers any other client version with
// a 426.
func Wire(build string) string {
	if _, ok := Parse(build); ok {
		return build
	}
	return Dev
}
