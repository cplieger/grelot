package canon

import (
	"fmt"
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
		{".git as the last segment", "git@github.com:cplieger/toolbelt/.git", "github.com/cplieger/toolbelt"},
		{"non-ASCII host", "git@bücher.example:team/docs.git", "bücher.example/team/docs"},
		{"space inside an scp name", "git@github.com:o/my repo", "github.com/o/my repo"},
		{"escaped space inside an https name", "https://github.com/o/my%20repo", "github.com/o/my repo"},
		{"Azure DevOps https names with spaces", "https://dev.azure.com/org/My%20Project/_git/My%20Repo", "dev.azure.com/org/My Project/_git/My Repo"},
		{"Azure DevOps scp names with spaces", "git@ssh.dev.azure.com:v3/org/My Project/My Repo", "ssh.dev.azure.com/v3/org/My Project/My Repo"},
		{"space segment", "git@github.com:o/ /r", "github.com/o/ /r"},
		{"no-break space segment", "git@github.com:o/\u00a0/r", "github.com/o/\u00a0/r"},
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

func TestNormalizeRemote_unreadable(t *testing.T) {
	tests := []struct{ name, raw string }{
		{"non-UTF-8 scp host", "git@a\x86:o/r"},
		{"non-UTF-8 https host", "https://a\x86/o/r"},
		{"non-UTF-8 escaped https host", "https://a%86/o/r"},
		{"non-UTF-8 scp path", "git@github.com:o/r\x86"},
		{"non-UTF-8 escaped https path", "https://github.com/o/r%86"},
		{"space before .git", "git@github.com:o/r .git"},
		{"escaped trailing space", "https://github.com/o/r%20"},
		{"escaped trailing no-break space", "https://github.com/o/r%C2%A0/"},
		{"space path", "a1: /"},
		{"repeated .git", "https://github.com/cplieger/toolbelt.git.git"},
		{".git after a slash", "git@github.com:cplieger/toolbelt.git/.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := NormalizeRemote(tt.raw); ok {
				t.Errorf("NormalizeRemote(%q) = %q, true; want refused", tt.raw, got)
			}
		})
	}
}

func TestNormalizeRemote_formsAgreeOnHosts(t *testing.T) {
	tests := []struct {
		host string
		ok   bool
	}{
		{"a]b", true},
		{"[ab]", false},
		{"a{b", false},
		{"a}b", false},
		{"a|b", false},
		{"a^b", false},
		{"a`b", false},
		{"a b", false},
		{"a\u00a0b", false},
		{"a\x86", false},
		{"a\u0085b", false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.host), func(t *testing.T) {
			want := ""
			if tt.ok {
				want = tt.host + "/o/r"
			}
			for _, raw := range []string{"git@" + tt.host + ":o/r", "https://" + tt.host + "/o/r"} {
				if got, ok := NormalizeRemote(raw); ok != tt.ok || got != want {
					t.Errorf("NormalizeRemote(%q) = %q, %v; want %q, %v", raw, got, ok, want, tt.ok)
				}
			}
		})
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
		"a{b:o/r",
		"h1:o/r.git.git",
		"https://dev.azure.com/org/My%20Project/_git/My%20Repo",
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
