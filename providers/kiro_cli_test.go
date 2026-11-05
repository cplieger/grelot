package providers_test

import (
	"net/http"
	"testing"
)

// kiroSession is the session the recorded kiro-cli headless run carries.
const kiroSession = "sess_fcd984d7-fd4e-4051-bbea-b41fd81e9391"

// registerLine is Ruling 17's turn-start text for a session with no
// identity.
const registerLine = "grelot: not registered. Register before you start work: run `grelot help`, then `grelot register` as it says."

func kiroShell(t *testing.T, srv *testServer) *shell {
	t.Helper()
	return newShell(t, srv, "kiro-cli", map[string]string{"KIRO_SESSION_ID": kiroSession})
}

func TestKiroCLISessionStart_unregistered(t *testing.T) {
	sh := kiroShell(t, newServer(t))
	got := sh.hook("session_start", sh.fixture("session_start"))
	if got.Code != 0 || got.Stdout != registerLine+"\n" || got.Stderr != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want 0 and only the register line as context", got.Code, got.Stdout, got.Stderr)
	}
	if len(got.Requests) != 1 {
		t.Fatalf("%d requests, want one GET /v1/me", len(got.Requests))
	}
	req := got.Requests[0]
	if req.Method != http.MethodGet || req.Path != "/v1/me" || req.Header.Get("Grelot-Session") != "kiro-cli:"+kiroSession {
		t.Errorf("request %s %s as %q, want GET /v1/me as kiro-cli:%s", req.Method, req.Path, req.Header.Get("Grelot-Session"), kiroSession)
	}
}
