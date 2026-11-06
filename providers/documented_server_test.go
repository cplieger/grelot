//go:build !windows

package providers_test

import (
	"strings"
	"testing"
)

// With no session in the shell, every hook acts as nobody: it prints
// nothing, exits 0 and asks the server nothing.
func TestDocumentedHosts_hooksWriteNothing(t *testing.T) {
	for _, h := range documentedHosts {
		for _, role := range []string{"session_start", "stop"} {
			t.Run(h.name+"_"+role, func(t *testing.T) {
				sh := newShell(t, newServer(t), h.name, nil)
				got := sh.hook(role, sh.fixture(role))
				if got.Code != 0 || got.Stdout != "" || got.Stderr != "" || len(got.Requests) != 0 {
					t.Errorf("exit %d, stdout %q, stderr %q, requests %v; want 0, nothing printed and none sent", got.Code, got.Stdout, got.Stderr, got.Requests)
				}
			})
		}
	}
}

func TestDocumentedHosts_identityCommandsRefuse(t *testing.T) {
	for _, h := range documentedHosts {
		t.Run(h.name, func(t *testing.T) {
			sh := newShell(t, newServer(t), h.name, nil)
			got := sh.run("", "--provider", h.name, "me")
			want := "provider " + h.name + " records no session variable"
			if got.Code == 0 || !strings.Contains(got.Stderr, want) || len(got.Requests) != 0 {
				t.Errorf("exit %d, stderr %q, requests %v; want a failure naming %q and none sent", got.Code, got.Stderr, got.Requests, want)
			}
		})
	}
}
