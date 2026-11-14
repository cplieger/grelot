package canon

import (
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// NormalizeRemote turns a git remote URL into <host>/<owner path>/<name>: no
// scheme, user or port, the host lowercased, no trailing ".git" or "/", and
// the scp form host:owner/name read as host/owner/name. It answers false for
// a remote with no host, such as a local path, so a clone of one is local, and
// for one whose normalized form would not read back as itself.
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
	if !urlHost(host) {
		return "", false
	}
	return host + "/" + p, true
}

// splitRemote reads the host and path of a URL remote or, without a scheme,
// of git's scp form.
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

// trimRepoPath drops slashes and ".git" from both ends until none is left, so
// a reread of the result trims nothing more.
func trimRepoPath(p string) string {
	for {
		q := strings.TrimSuffix(strings.Trim(p, "/"), ".git")
		if q == p {
			return p
		}
		p = q
	}
}

// readable reports whether s is valid UTF-8 with no rune badRemoteRune refuses.
func readable(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, badRemoteRune)
}

// cleanPath reports whether p has at least one segment and none is empty, "."
// or "..".
func cleanPath(p string) bool {
	if p == "" {
		return false
	}
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
	if len(host) < 2 || strings.ContainsAny(host, "[]") {
		return "", "", false
	}
	return host, s[c+1:], true
}

// urlHost reports whether host reads back unchanged as the host of a URL, so
// a host taken from either remote form passes by the URL form's rule.
func urlHost(host string) bool {
	u, err := url.Parse("https://" + host + "/")
	return host != "" && err == nil && u.Host == host
}

// badRemoteRune refuses what would make the normalized form read back as
// another remote: a query or fragment mark, an escape, a backslash, a control,
// or whitespace, which a reread trims or a URL host refuses.
func badRemoteRune(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(`?#%\`, r)
}

// repoName is the <repo> of a normalized remote: its alias in repos, or its
// last component.
func repoName(norm string, repos map[string]string) string {
	if r, ok := repos[norm]; ok {
		return r
	}
	return path.Base(norm)
}
