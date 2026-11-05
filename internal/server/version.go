package server

import (
	"fmt"
	"strconv"
	"strings"
)

// minClient is the release base of the oldest client this server speaks.
// Raise it in the release whose API an older client can no longer use.
const minClient = "v1.1.0"

// devVersion is an unstamped build's version, which speaks every version.
const devVersion = "dev"

// releaseBase is v with any -dev.N suffix removed, and ok false when v is
// not vX.Y.Z or vX.Y.Z-dev.N.
func releaseBase(v string) (base string, ok bool) {
	base, pre, hasPre := strings.Cut(v, "-")
	if hasPre {
		n, found := strings.CutPrefix(pre, "dev.")
		if !found || !isNumber(n) {
			return "", false
		}
	}
	if _, ok := versionParts(base); !ok {
		return "", false
	}
	return base, true
}

func versionParts(base string) ([3]int, bool) {
	var out [3]int
	rest, found := strings.CutPrefix(base, "v")
	parts := strings.Split(rest, ".")
	if !found || len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if !isNumber(p) {
			return out, false
		}
		out[i], _ = strconv.Atoi(p)
	}
	return out, true
}

func isNumber(s string) bool {
	if s == "" || len(s) > 9 || (len(s) > 1 && s[0] == '0') {
		return false
	}
	return strings.Trim(s, "0123456789") == ""
}

// speaks reports whether this server speaks a client of version v.
func speaks(v string) bool {
	if v == devVersion {
		return true
	}
	base, ok := releaseBase(v)
	if !ok {
		return false
	}
	got, _ := versionParts(base)
	floor, _ := versionParts(minClient)
	for i := range got {
		if got[i] != floor[i] {
			return got[i] > floor[i]
		}
	}
	return true
}

// upgradeLine is the 426's one line: the client to install for a server of
// version server.
func upgradeLine(client, server string) string {
	if client == "" {
		client = "with no Grelot-Version"
	}
	base, ok := releaseBase(server)
	if !ok {
		return fmt.Sprintf("this grelot server (%s) does not speak client %s; install the client built from the same source as the server", server, client)
	}
	return fmt.Sprintf("this grelot server (%s) does not speak client %s; install grelot %s from https://github.com/cplieger/grelot/releases/tag/%s",
		server, client, base, base)
}
