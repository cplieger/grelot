//go:build !windows

package providers_test

import (
	"strings"
	"testing"
)

// A client never runs as marotte: a marotte chat's shell is detected as
// kiro-cli, and naming marotte refuses with the no-session line.
func TestMarotte_clientNeverSelectsIt(t *testing.T) {
	sh := kiroShell(t, newServer(t), "kiro-cli", kiroSession)
	got := sh.run("", "me")
	if len(got.Requests) == 0 || got.Requests[0].Header.Get("Grelot-Session") != "kiro-cli:"+kiroSession {
		t.Errorf("me: requests %v, want them sent as kiro-cli:%s", got.Requests, kiroSession)
	}
	got = sh.run("", "--provider", "marotte", "me")
	if want := "provider marotte records no session variable"; got.Code == 0 || !strings.Contains(got.Stderr, want) || len(got.Requests) != 0 {
		t.Errorf("--provider marotte me: exit %d, stderr %q, requests %v; want a failure naming %q and none sent", got.Code, got.Stderr, got.Requests, want)
	}
	got = sh.run(string(sh.fixture("prompt")), "hook", "marotte", "prompt")
	if got.Code != 0 || got.Stdout != "" || got.Stderr != "" || len(got.Requests) != 0 {
		t.Errorf("hook marotte prompt: exit %d, stdout %q, stderr %q, requests %v; want 0 and nothing", got.Code, got.Stdout, got.Stderr, got.Requests)
	}
}
