package canon

import (
	"testing"

	"github.com/cplieger/grelot/internal/where"
)

func TestNormalizeRemote(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{"scp form", "git@github.com:cplieger/toolbelt.git", "github.com/cplieger/toolbelt"},
		{"https", "https://github.com/cplieger/toolbelt.git", "github.com/cplieger/toolbelt"},
		{"no .git", "https://github.com/cplieger/toolbelt", "github.com/cplieger/toolbelt"},
		{"mixed-case host", "https://GitHub.COM/cplieger/toolbelt", "github.com/cplieger/toolbelt"},
		{"ssh with port and trailing slash", "ssh://git@github.com:22/cplieger/toolbelt.git/", "github.com/cplieger/toolbelt"},
		{"user and password", "https://u:secret@github.com/cplieger/toolbelt/", "github.com/cplieger/toolbelt"},
		{"surrounding space", " git@github.com:cplieger/toolbelt.git\n", "github.com/cplieger/toolbelt"},
		{"nested owner path", "https://gitlab.example.com/team/sub/docs.git", "gitlab.example.com/team/sub/docs"},
		{"owner case kept", "git@GitLab.example.com:Team/Docs.git", "gitlab.example.com/Team/Docs"},
		{"scp form without user", "example.org:repos/x.git", "example.org/repos/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NormalizeRemote(tt.raw)
			if !ok || got != tt.want {
				t.Errorf("NormalizeRemote(%q) = %q, %v; want %q, true", tt.raw, got, ok, tt.want)
			}
		})
	}
}

func TestNormalizeRemote_noHost(t *testing.T) {
	for _, raw := range []string{
		"",
		"/srv/git/toolbelt.git",
		"../toolbelt",
		"file:///srv/git/toolbelt.git",
		`C:\repos\toolbelt`,
		"C:/repos/toolbelt",
		"https://github.com/",
		"https://github.com/.git",
		"https://github.com/a/../b",
		"dir/with:colon",
	} {
		if got, ok := NormalizeRemote(raw); ok {
			t.Errorf("NormalizeRemote(%q) = %q, true; want no remote", raw, got)
		}
	}
}

func TestNormalizeRemote_spellingsShareOneHash(t *testing.T) {
	// printf '%s' github.com/cplieger/toolbelt | openssl dgst -sha256 -hmac 'grelot remote', cut to 32 hex.
	const want = "3b9c6e0d9248a2015132854ae815633b" // gitleaks:allow
	for _, raw := range []string{
		"git@github.com:cplieger/toolbelt.git",
		"https://github.com/cplieger/toolbelt.git",
		"https://github.com/cplieger/toolbelt",
		"https://GITHUB.com/cplieger/toolbelt.git",
		"ssh://git@github.com/cplieger/toolbelt",
	} {
		norm, _ := NormalizeRemote(raw)
		if got := where.Remote(norm); got != want {
			t.Errorf("%q: hash %q, want %q", raw, got, want)
		}
	}
}

func TestRepoName(t *testing.T) {
	repos := map[string]string{"gitlab.example.com/team/docs": "team-docs"}
	if got := repoName("github.com/cplieger/docs", repos); got != "docs" {
		t.Errorf("no alias: got %q, want docs", got)
	}
	if got := repoName("gitlab.example.com/team/docs", repos); got != "team-docs" {
		t.Errorf("alias: got %q, want team-docs", got)
	}
}

func FuzzNormalizeRemote_idempotent(f *testing.F) {
	for _, s := range []string{
		"git@github.com:cplieger/toolbelt.git",
		"https://u:p@GitHub.com:443/a/b/c.git/",
		"ssh://[::1]:22/x/y",
		"host:~u/r",
		"file:///x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		norm, ok := NormalizeRemote(raw)
		if !ok {
			return
		}
		again, ok := NormalizeRemote("https://" + norm)
		if !ok || again != norm {
			t.Errorf("NormalizeRemote(%q) = %q, but its https form gives %q, %v", raw, norm, again, ok)
		}
	})
}
