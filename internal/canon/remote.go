package canon

import (
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// NormalizeRemote turns a git remote URL into <host>/<owner path>/<name>: no
// scheme, user or port, the host lowercased, one trailing ".git" and the
// trailing "/" stripped, and the scp form host:owner/name read as
// host/owner/name. It answers false for a remote with no host, such as a local
// path, so a clone of one is local, and for one it cannot name canonically:
// invalid UTF-8, whitespace in the host, a backslash, or a normalized form that
// would not read back as itself, such as a name still ending in ".git".
func NormalizeRemote(raw string) (string, bool) {
	host, p, ok := splitRemote(strings.TrimSpace(raw))
	if !ok {
		return "", false
	}
	p = trimRepoPath(p)
	if !readable(host) || !readable(p) || !cleanPath(p) {
		return "", false
	}
	host = strings.ToLower(host)
	norm := host + "/" + p
	// A reread trims whitespace from the ends of the form and strips a ".git".
	if !urlHost(host) || strings.TrimSpace(norm) != norm || strings.HasSuffix(p, ".git") {
		return "", false
	}
	return norm, true
}

func splitRemote(s string) (host, p string, ok bool) {
	if !strings.Contains(s, "://") {
		return scpForm(s)
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", "", false
	}
	host = u.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host, u.Path, true
}

// trimRepoPath strips slashes from both ends and one trailing ".git", the
// suffix git drops from a clone's name, so name.git.git stays name.git.
func trimRepoPath(p string) string {
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	return strings.TrimRight(p, "/")
}

func readable(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, badRemoteRune)
}

func cleanPath(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// scpForm splits git's [user@]host:path, which git recognizes only when no
// slash comes before the first colon; a one-letter host is a Windows drive.
func scpForm(s string) (host, p string, ok bool) {
	c := strings.IndexByte(s, ':')
	if c < 0 || strings.ContainsRune(s[:c], '/') {
		return "", "", false
	}
	host = s[:c]
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	if len(host) < 2 {
		return "", "", false
	}
	return host, s[c+1:], true
}

// urlHost reports whether host reads back unchanged as the host of a URL, so
// a host taken from either remote form passes by the URL form's rule. A URL
// host holds no whitespace, though url.Parse lets a non-ASCII space through.
func urlHost(host string) bool {
	u, err := url.Parse("https://" + host + "/")
	return host != "" && err == nil && u.Host == host && !strings.ContainsFunc(host, unicode.IsSpace)
}

// badRemoteRune refuses what a reread or a canonical path would change: a
// reread cuts the path at "?" or "#", decodes "%" into another path and fails
// on an ASCII control, and Windows reads "\" in a canonical path as a
// separator.
func badRemoteRune(r rune) bool {
	return r < 0x20 || r == 0x7f || strings.ContainsRune(`?#%\`, r)
}

// repoName is the <repo> of a normalized remote: its alias in repos, or its
// last component.
func repoName(norm string, repos map[string]string) string {
	if r, ok := repos[norm]; ok {
		return r
	}
	return path.Base(norm)
}
