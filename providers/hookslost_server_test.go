//go:build !windows

package providers_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// codexHooksLost is the status line's segment for a Codex session whose
// hooks never ran, with codex.yaml's fix.
const codexHooksLost = "grelot's hooks did not run in this session; ask the user to re-trust grelot's hooks in /hooks"

// onMachine puts sh on other's machine: the same client config directory,
// which holds the hooks' marks.
func onMachine(sh, other *shell) *shell {
	sh.config = other.config
	return sh
}

// A Codex thread whose hooks never ran, on a machine where Codex's hooks
// ran in another thread, carries the re-trust hint on its own commands,
// and loses it at its first hook run.
func TestCodexHooksLost(t *testing.T) {
	srv := newServer(t)
	hookedThread := codexShell(t, srv, sessionOf(t, "codex", "session_start-startup-tui"))
	lost := onMachine(codexShell(t, srv, sessionOf(t, "codex", "session_start-startup-after-new")), hookedThread)
	hookedThread.register("hooked-dev")
	lost.register("lost-dev")
	if out := lost.must("", "me"); strings.Contains(out, "hooks did not run") {
		t.Errorf("before any hook ran on the machine, me printed %q; want no hint", out)
	}

	if got := hookedThread.hook("session_start", hookedThread.fixture("session_start-startup-tui")); got.Code != 0 {
		t.Fatalf("the hooked thread's session start exited %d", got.Code)
	}
	if out := lost.must("", "me"); !strings.Contains(out, codexHooksLost) {
		t.Errorf("a thread whose hooks never ran printed %q; want the hint %q", out, codexHooksLost)
	}
	if out := hookedThread.must("", "who"); strings.Contains(out, "hooks did not run") {
		t.Errorf("the thread whose hooks ran printed %q; want no hint", out)
	}

	if got := lost.hook("session_start", lost.fixture("session_start-startup-after-new")); got.Code != 0 || strings.Contains(got.Stdout, "hooks did not run") {
		t.Errorf("the lost thread's own session start: exit %d, stdout %q; want 0 and no hint", got.Code, got.Stdout)
	}
	if out := lost.must("", "me"); strings.Contains(out, "hooks did not run") {
		t.Errorf("after its own hook ran, me printed %q; want no hint", out)
	}
}

// A subagent thread's shell carries its own thread id while its hooks
// carry the root's session_id, so the subagent's hook run marks its own
// thread too.
func TestCodexHooksLost_subagentThread(t *testing.T) {
	srv := newServer(t)
	root := codexShell(t, srv, sessionOf(t, "codex", "prompt-subagent"))
	root.hook("session_start", root.fixture("session_start-startup-tui"))
	sub := onMachine(codexShell(t, srv, "01a19ef2-1c3d-7a52-8e4f-3b6d9c0a7e15"), root)
	sub.register("sub-dev")
	if out := sub.must("", "me"); !strings.Contains(out, codexHooksLost) {
		t.Fatalf("before its own prompt hook, the subagent printed %q; want the hint", out)
	}
	if got := root.hook("prompt", root.fixture("prompt-subagent")); got.Code != 0 {
		t.Fatalf("the subagent's prompt hook exited %d", got.Code)
	}
	if out := sub.must("", "me"); strings.Contains(out, "hooks did not run") {
		t.Errorf("after the subagent's prompt hook, me printed %q; want no hint", out)
	}
}

// A delegate's commands (grelot as) and a one-shot run never carry the
// hint: neither needs its own session's hooks.
func TestCodexHooksLost_delegateAndOneShot(t *testing.T) {
	srv := newServer(t)
	lead := codexShell(t, srv, sessionOf(t, "codex", "session_start-startup-tui"))
	lead.register("lead-dev")
	lead.hook("session_start", lead.fixture("session_start-startup-tui"))
	out := lead.must("", "delegate", "worker", "--ttl", "1h")
	i := strings.Index(out, "grelot-link:")
	if i < 0 {
		t.Fatalf("grelot delegate printed %q, no link", out)
	}

	worker := onMachine(codexShell(t, srv, "worker-thread"), lead)
	worker.must("", "register", "--delegate", strings.Fields(out[i:])[0], "--desc", "answering one question")
	if got := worker.must("", "as", "lead-dev/worker", "inbox"); strings.Contains(got, "hooks did not run") {
		t.Errorf("as lead-dev/worker inbox printed %q; want no hint", got)
	}
	if got := worker.must("", "me"); !strings.Contains(got, codexHooksLost) {
		t.Errorf("the worker's own me printed %q; want the hint, the as gate's control", got)
	}

	exec := onMachine(codexShell(t, srv, sessionOf(t, "codex", "session_start-startup-exec")), lead)
	exec.procs = codexExecChain
	exec.register("exec-dev")
	if got := exec.must("", "me"); strings.Contains(got, "hooks did not run") {
		t.Errorf("codex exec's me printed %q; want no hint", got)
	}
}

// A host whose file declares no lost_fix keeps no hooked marks.
func TestHooksLost_onlyAHostThatDeclaresIt(t *testing.T) {
	srv := newServer(t)
	sh := newShell(t, srv, "claude-code", map[string]string{"CLAUDE_CODE_SESSION_ID": sessionOf(t, "claude-code", "session_start-startup-tui")})
	sh.register("claude-dev")
	if got := sh.hook("session_start", sh.fixture("session_start-startup-tui")); got.Code != 0 {
		t.Fatalf("session start exited %d", got.Code)
	}
	for _, kind := range []string{"hooks", "hooked"} {
		files, err := filepath.Glob(filepath.Join(sh.config, "grelot", "state", "marks", kind+"-*"))
		if err != nil || len(files) != 0 {
			t.Errorf("%s marks: %v, %v; want none", kind, files, err)
		}
	}
}
