//go:build !windows

package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	t.Run("the client's build, then the server's version", func(t *testing.T) {
		ts := newServer(t)
		a := newAgent(t, ts, "s-1", "")
		a.env.Build = "v1.1.0"
		for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
			code, out, errOut := a.run(args...)
			if code != exitOK || out != "grelot client v1.1.0\ngrelot server v1.1.0-dev.4\n" || errOut != "" {
				t.Errorf("grelot %s: exit %d, stdout %q, stderr %q; want 0 and the two versions", args[0], code, out, errOut)
			}
		}
	})
	t.Run("it asks GET /v1/version as no session", func(t *testing.T) {
		ts := newServer(t)
		a := newAgent(t, ts, "s-1", "")
		a.must("version")
		req := ts.last(t, http.MethodGet, "/v1/version")
		if got := req.Header.Get("Grelot-Session"); got != "" {
			t.Errorf("Grelot-Session %q, want none", got)
		}
		if got := req.Header.Get("Grelot-Version"); got != "v1.1.0" {
			t.Errorf("Grelot-Version %q, want the client's wire version", got)
		}
	})
	t.Run("a pseudo-version build prints as built", func(t *testing.T) {
		ts := newServerAt(t, newerServer, nil)
		a := newAgent(t, ts, "s-1", "")
		a.env.Build, a.env.Version = "v1.1.0-dev.4.0.20261105101010-0123456789ab", "dev"
		if out := a.must("version"); out != "grelot client v1.1.0-dev.4.0.20261105101010-0123456789ab\ngrelot server v1.2.0\n" {
			t.Errorf("stdout %q, want the build and the server, and no upgrade line for a dev client", out)
		}
	})
	t.Run("a newer server names the client to install", func(t *testing.T) {
		ts := newServerAt(t, newerServer, nil)
		a := newAgent(t, ts, "s-1", "")
		a.env.Build = "v1.1.0"
		if out := a.must("version"); out != "grelot client v1.1.0\ngrelot server v1.2.0\ngrelot: "+upgrade+"\n" {
			t.Errorf("stdout %q, want the two versions and the upgrade line", out)
		}
	})
	t.Run("a server that refuses the client still names its version", func(t *testing.T) {
		ts := newServer(t)
		a := newAgent(t, ts, "s-1", "")
		a.env.Build, a.env.Version = "v0.0.1", "v0.0.1"
		out := a.must("version")
		if want := "grelot client v0.0.1\ngrelot server v1.1.0-dev.4\ngrelot: this client is v0.0.1 and the server v1.1.0-dev.4: install grelot v1.1.0-dev.4"; !strings.HasPrefix(out, want) {
			t.Errorf("stdout %q, want it to start %q", out, want)
		}
	})
	t.Run("a server older than the route names its version on the 404", func(t *testing.T) {
		hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Grelot-Version", "v1.1.0-dev.1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such route: GET /v1/version","code":"not_found"}`))
		}))
		t.Cleanup(hs.Close)
		a := newAgent(t, &testServer{URL: hs.URL}, "s-1", "")
		a.env.Build = "v1.1.0"
		if out := a.must("version"); out != "grelot client v1.1.0\ngrelot server v1.1.0-dev.1\n" {
			t.Errorf("stdout %q, want the 404's version", out)
		}
	})
	t.Run("a refused token says why the server's version is unknown", func(t *testing.T) {
		ts := newServer(t)
		a := newAgent(t, ts, "s-1", "")
		writeClientConfig(t, a.env.ConfigDir, "server: "+ts.URL+"\ntoken: wrong\n")
		a.env.Build = "v1.1.0"
		if out := a.must("version"); out != "grelot client v1.1.0\ngrelot server unknown: a valid bearer token is required: set token in the grelot client config\n" {
			t.Errorf("stdout %q, want the 401's reason", out)
		}
	})
	t.Run("an unreachable server says why", func(t *testing.T) {
		a := newAgent(t, &testServer{URL: unreachableURL(t)}, "s-1", "")
		a.env.Build = "v1.1.0"
		out := a.must("version")
		if !strings.HasPrefix(out, "grelot client v1.1.0\ngrelot server unknown: cannot reach the server http://127.0.0.1:") {
			t.Errorf("stdout %q, want the client and why the server is unknown", out)
		}
	})
	t.Run("no client config says why", func(t *testing.T) {
		a := newAgent(t, &testServer{URL: "http://127.0.0.1:1"}, "s-1", "")
		a.env.ConfigDir = t.TempDir()
		a.env.Build = "v1.1.0"
		out := a.must("version")
		if !strings.HasPrefix(out, "grelot client v1.1.0\ngrelot server unknown: ") || strings.Count(out, "\n") != 2 {
			t.Errorf("stdout %q, want the client and the config's reason", out)
		}
	})
	t.Run("it takes no arguments", func(t *testing.T) {
		a := newAgent(t, newServer(t), "s-1", "")
		if code, _, _ := a.run("version", "extra"); code != exitUsage {
			t.Errorf("exit %d, want %d", code, exitUsage)
		}
	})
}
