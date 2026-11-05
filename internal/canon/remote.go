package canon

import (
	"net/url"
	"path"
	"strings"
)

// NormalizeRemote turns a git remote URL into <host>/<owner path>/<name>: no
// scheme, user or port, the host lowercased, no trailing ".git" or "/", and
// the scp form host:owner/name read as host/owner/name. It answers false for
// a remote with no host, such as a local path, so a clone of one is local.
func NormalizeRemote(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	var host, p string
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", false
		}
		host, p = u.Hostname(), u.Path
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	} else {
		var ok bool
		if host, p, ok = scpForm(s); !ok {
			return "", false
		}
	}
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	p = strings.TrimRight(p, "/")
	if host == "" || p == "" || strings.ContainsAny(host, "/@") {
		return "", false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	norm := strings.ToLower(host) + "/" + p
	if strings.ContainsFunc(norm, badRemoteRune) {
		return "", false
	}
	return norm, true
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

// badRemoteRune refuses what would make the normalized form read back as
// another remote: a query or fragment mark, an escape, a backslash, a control.
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
