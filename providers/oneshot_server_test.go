//go:build !windows

package providers_test

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/grelot/internal/proctree"
)

// chain is a process table putting the client, pid 300, under ancestors,
// nearest first, each started before the one below it.
func chain(ancestors ...proctree.Process) processes {
	t := processes{}
	child := proctree.Process{Name: "grelot", Args: []string{"grelot"}, PID: 300, Start: 300}
	for i, p := range ancestors {
		p.PID, p.Start = 290-10*i, uint64(290-10*i)
		child.PPID = p.PID
		t[child.PID] = child
		child = p
	}
	t[child.PID] = child
	return t
}

func proc(name string, args ...string) proctree.Process {
	return proctree.Process{Name: name, Args: args}
}

// The ancestor chains recorded on Windows 11 under Claude Code 2.1.319 and
// Codex 0.167.0, paths shortened. Each host runs as three processes in a
// row, and a -p or exec sits on an outer one, never the innermost.
var (
	claudePrintChain = chain(
		proc("bash.exe", "bash.exe", "-c", "grelot hook claude-code stop"),
		proc("bash.exe", "bash.exe", "-c", "grelot hook claude-code stop"),
		proc("claude.exe", `\\?\C:\Users\user\.local\bin\claude.exe`, "--settings", `C:\Users\user\settings.json`),
		proc("claude.exe", `C:\Users\user\.local\bin\claude.exe`, "-p", "Reply with the single word OK. Do not run any command.", "--settings", `C:\Users\user\settings.json`),
		proc("claude.exe", "claude.exe", "-p", "Reply with the single word OK. Do not run any command."),
		proc("pwsh.exe", "pwsh.exe"),
	)
	claudeTUIChain = chain(
		proc("bash.exe", "bash.exe", "-c", "grelot hook claude-code stop"),
		proc("bash.exe", "bash.exe", "-c", "grelot hook claude-code stop"),
		proc("claude.exe", `\\?\C:\Users\user\.local\bin\claude.exe`, "--settings", `C:\Users\user\settings.json`),
		proc("claude.exe", `C:\Users\user\.local\bin\claude.exe`),
		proc("claude.exe", "claude.exe"),
		proc("pwsh.exe", "pwsh.exe"),
	)
	// claudeUnreadChain is the -p chain on a system that lets the hook read
	// no host process's command line.
	claudeUnreadChain = chain(
		proc("bash.exe", "bash.exe", "-c", "grelot hook claude-code stop"),
		proctree.Process{Name: "claude.exe"},
		proctree.Process{Name: "claude.exe"},
		proctree.Process{Name: "claude.exe"},
		proc("pwsh.exe", "pwsh.exe"),
	)
	// codexExecChain is the recorded exec run, launched from an interactive
	// Claude Code session's shell.
	codexExecChain = chain(
		proc("pwsh.exe", "pwsh.exe", "-NoProfile", "-Command", "grelot hook codex stop"),
		proc("codex.exe", `\\?\C:\Users\user\bin\codex.exe`, "-c", "features.hooks=true", "-c", "approval_policy=never"),
		proc("codex.exe", `C:\Users\user\bin\codex.exe`, "exec", "-s", "read-only", "--skip-git-repo-check", "--ephemeral", "-C", `C:\work`, "-o", `C:\work\out.txt`, "-"),
		proc("codex.exe", "codex.exe", "exec", "-s", "read-only", "--skip-git-repo-check", "--ephemeral", "-C", `C:\work`, "-o", `C:\work\out.txt`, "-"),
		proc("bash.exe", "bash.exe", "-c", "codex exec -s read-only -"),
		proc("claude.exe", `\\?\C:\Users\user\.local\bin\claude.exe`, "--settings", `C:\Users\user\settings.json`),
		proc("claude.exe", "claude.exe"),
	)
	codexTUIChain = codexTUIWith()
	// claudeSDKChain is an Agent SDK session: the SDK's node process starts
	// claude with its streaming options, never -p (claude-agent-sdk 0.3.319).
	claudeSDKChain = chain(
		proc("bash.exe", "bash.exe", "-c", "grelot hook claude-code stop"),
		proc("claude.exe", `C:\Users\user\.local\bin\claude.exe`, "--output-format", "stream-json", "--verbose", "--input-format", "stream-json"),
		proc("node.exe", "node.exe", "app.mjs"),
	)
)

// codexTUIWith is the recorded Codex TUI chain started as codex args.
func codexTUIWith(args ...string) processes {
	return chain(
		proc("pwsh.exe", "pwsh.exe", "-NoProfile", "-Command", "grelot hook codex stop"),
		proc("codex.exe", `\\?\C:\Users\user\bin\codex.exe`, "-c", "features.hooks=true", "-c", "approval_policy=never"),
		proc("codex.exe", append([]string{`C:\Users\user\bin\codex.exe`}, args...)...),
		proc("codex.exe", append([]string{"codex.exe"}, args...)...),
		proc("pwsh.exe", "pwsh.exe"),
	)
}

// kiroChain is the chain recorded on Linux under kiro-cli 3.1.0, one agent
// server between the hook's shell and the kiro-cli process given args.
func kiroChain(args ...string) processes {
	return chain(
		proc("sh", "/bin/sh", "-c", "grelot hook kiro-cli stop"),
		proc("node", "/home/user/.cache/kiro-cli-runtime/node", "--experimental-wasm-modules", "/home/user/.cache/kiro-cli-runtime/kas/node/index.js"),
		proc("kiro-cli", append([]string{"/usr/local/bin/kiro-cli"}, args...)...),
		proc("bash", "bash"),
	)
}

// kiroTUIChain is the TUI's chain recorded under kiro-cli 3.0.0, its
// terminal UI between the agent server and kiro-cli chat.
func kiroTUIChain(args ...string) processes {
	return chain(
		proc("sh", "/bin/sh", "-c", "grelot hook kiro-cli stop"),
		proc("node", "/home/user/.cache/kiro-cli-runtime/node", "--experimental-wasm-modules", "/home/user/.cache/kiro-cli-runtime/kas/node/index.js"),
		proc("bun", "/home/user/.cache/kiro-cli-runtime/bun"),
		proc("kiro-cli", append([]string{"kiro-cli", "chat"}, args...)...),
		proc("bash", "bash"),
	)
}

const oneShotPrompt = "Reply with the single word OK. Do not run any command."

// The hook environments recorded beside those chains. A Codex run started
// from a Claude Code shell inherits that session's variables.
var (
	claudePrintEnv       = map[string]string{"CLAUDE_CODE_ENTRYPOINT": "sdk-cli", "CLAUDE_CODE_SESSION_ATTENDED": "0", "CLAUDECODE": "1", "CLAUDE_CODE_CHILD_SESSION": "1"}
	claudeInteractiveEnv = map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli", "CLAUDE_CODE_SESSION_ATTENDED": "1", "CLAUDECODE": "1", "CLAUDE_CODE_CHILD_SESSION": "1"}
)

// oneShotRun is one run's hook environment and the payloads its roles read.
type oneShotRun struct {
	env                  map[string]string
	procs                processes
	start, prompt, stop  string
	provider, sessionVar string
	// homeVar names a variable the shell sets to a directory of its own.
	homeVar              string
	oneShot              bool
	omitSessionFromHooks bool
}

func (r *oneShotRun) shell(t *testing.T, srv *testServer, fixture string) *shell {
	t.Helper()
	vars := map[string]string{r.sessionVar: sessionOf(t, r.provider, fixture)}
	if r.homeVar != "" {
		vars[r.homeVar] = t.TempDir()
	}
	maps.Copy(vars, r.env)
	sh := newShell(t, srv, r.provider, vars)
	sh.procs = r.procs
	if r.omitSessionFromHooks {
		sh.hookOmits = []string{r.sessionVar}
	}
	return sh
}

func claudeRun(env map[string]string, procs processes, oneShot bool) oneShotRun {
	return oneShotRun{
		provider: "claude-code", sessionVar: "CLAUDE_CODE_SESSION_ID", env: env, procs: procs, oneShot: oneShot,
		start: "session_start-startup-print", prompt: "prompt-typed", stop: "stop-first",
	}
}

func codexRun(start string, env map[string]string, procs processes, oneShot bool) oneShotRun {
	return oneShotRun{
		provider: "codex", sessionVar: "CODEX_THREAD_ID", env: env, procs: procs, oneShot: oneShot, omitSessionFromHooks: true,
		start: start, prompt: "prompt-typed", stop: "stop-first",
	}
}

func kiroRun(procs processes, oneShot bool) oneShotRun {
	return oneShotRun{
		provider: "kiro-cli", sessionVar: "KIRO_SESSION_ID", homeVar: "KIRO_HOME", procs: procs, oneShot: oneShot,
		start: "session_start-no-interactive", prompt: "prompt-no-interactive", stop: "stop-no-interactive",
	}
}

// A one-shot run is never asked to register nor held for it, and any other
// run is; the payloads alone never tell them apart.
func TestOneShot_unregistered(t *testing.T) {
	runs := map[string]oneShotRun{
		"claude -p":                                                     claudeRun(claudePrintEnv, claudePrintChain, true),
		"claude -p, its entrypoint alone":                               claudeRun(map[string]string{"CLAUDE_CODE_ENTRYPOINT": "sdk-cli"}, nil, true),
		"claude -p, its ancestors alone":                                claudeRun(nil, claudePrintChain, true),
		"claude -p, its ancestors unreadable":                           claudeRun(nil, claudeUnreadChain, false),
		"an interactive Claude Code session":                            claudeRun(claudeInteractiveEnv, claudeTUIChain, false),
		"a TypeScript Agent SDK session":                                claudeRun(map[string]string{"CLAUDE_CODE_ENTRYPOINT": "sdk-ts", "CLAUDE_CODE_SESSION_ATTENDED": "0"}, claudeSDKChain, false),
		"a Python Agent SDK session":                                    claudeRun(map[string]string{"CLAUDE_CODE_ENTRYPOINT": "sdk-py", "CLAUDE_CODE_SESSION_ATTENDED": "0"}, claudeSDKChain, false),
		"codex exec carrying Claude Code's session variables":           codexRun("session_start-startup-exec", claudeInteractiveEnv, codexExecChain, true),
		"codex exec --ephemeral":                                        codexRun("session_start-startup-exec-ephemeral", nil, codexExecChain, true),
		"codex exec after a profile, not known":                         codexRun("session_start-startup-exec", nil, codexTUIWith("-p", "review", "exec", "-"), false),
		"the Codex TUI with web search resuming a session named review": codexRun("session_start-startup-tui", nil, codexTUIWith("--search", "resume", "review"), false),
		"the Codex TUI":                                                 codexRun("session_start-startup-tui", nil, codexTUIChain, false),
		"the Codex TUI carrying Claude Code's -p variables":             codexRun("session_start-startup-tui", claudePrintEnv, codexTUIChain, false),
		"the Codex TUI with a profile named review":                     codexRun("session_start-startup-tui", nil, codexTUIWith("-p", "review"), false),
		"the Codex TUI with a profile named e":                          codexRun("session_start-startup-tui", nil, codexTUIWith("--profile", "e"), false),
		"the Codex TUI with a model named e":                            codexRun("session_start-startup-tui", nil, codexTUIWith("-m", "e"), false),
		"the Codex TUI in a directory named exec":                       codexRun("session_start-startup-tui", nil, codexTUIWith("-C", "exec"), false),
		"the Codex TUI resuming a session named review":                 codexRun("session_start-startup-tui", nil, codexTUIWith("resume", "review"), false),
		"the Codex TUI resuming a session with review to ask":           codexRun("session_start-startup-tui", nil, codexTUIWith("resume", "0198c1e2-7a3b-4c5d-9e8f-123456789abc", "review"), false),
		"the Codex TUI forking a session named exec":                    codexRun("session_start-startup-tui", nil, codexTUIWith("fork", "exec"), false),
		"kiro-cli chat --no-interactive":                                kiroRun(kiroChain("--v3", "chat", "--no-interactive", "--trust-workspace", oneShotPrompt), true),
		"kiro-cli chat --output-format stream-json":                     kiroRun(kiroChain("--v3", "chat", "--output-format", "stream-json", "--trust-workspace", oneShotPrompt), true),
		"kiro-cli chat --output-format=stream-json":                     kiroRun(kiroChain("chat", "--output-format=stream-json", oneShotPrompt), true),
		"the kiro-cli TUI":                                              kiroRun(kiroTUIChain("--trust-workspace", "-a", "--model", "claude-sonnet-5.5"), false),
		"the kiro-cli TUI given text output and a first prompt":         kiroRun(kiroTUIChain("--output-format", "text", oneShotPrompt), false),
		"the kiro-cli TUI whose first question is stream-json":          kiroRun(kiroTUIChain("stream-json"), false),
		"the kiro-cli TUI given text output and stream-json to ask":     kiroRun(kiroTUIChain("--output-format", "text", "stream-json"), false),
		"kiro-cli acp": kiroRun(kiroChain("acp", "--agent-engine", "v3"), false),
	}
	for name, r := range runs {
		t.Run(name, func(t *testing.T) {
			t.Run("session start", func(t *testing.T) {
				sh := r.shell(t, newServer(t), r.start)
				got := sh.hook("session_start", sh.fixture(r.start))
				want := registerStart + "\n"
				if r.oneShot {
					want = ""
				}
				if got.Code != 0 || got.Stdout != want || got.Stderr != "" {
					t.Errorf("exit %d, stdout %q, stderr %q; want 0 and stdout %q", got.Code, got.Stdout, got.Stderr, want)
				}
			})
			t.Run("prompt", func(t *testing.T) {
				sh := r.shell(t, newServer(t), r.prompt)
				got := sh.hook("prompt", sh.fixture(r.prompt))
				want := registerPrompt + "\n"
				if r.oneShot {
					want = ""
				}
				if got.Code != 0 || got.Stdout != want || got.Stderr != "" {
					t.Errorf("exit %d, stdout %q, stderr %q; want 0 and stdout %q", got.Code, got.Stdout, got.Stderr, want)
				}
			})
			t.Run("stop", func(t *testing.T) {
				sh := r.shell(t, newServer(t), r.stop)
				got := sh.hook("stop", sh.fixture(r.stop))
				if r.oneShot {
					if got.Code != 0 || got.Stdout != "" || got.Stderr != "" {
						t.Errorf("exit %d, stdout %q, stderr %q; want 0 and no hold", got.Code, got.Stdout, got.Stderr)
					}
					return
				}
				if b := parseBlock(t, got.Stdout); got.Code != 0 || b.Reason != registerHold {
					t.Errorf("exit %d, reason %q; want 0 and the register line's hold", got.Code, b.Reason)
				}
			})
		})
	}
}

// Only the hooks go quiet in a one-shot run: a worker a parent started
// registers with the parent's delegate link, posts and waits as it.
func TestOneShot_delegate(t *testing.T) {
	runs := map[string]oneShotRun{
		"claude -p":  claudeRun(claudePrintEnv, claudePrintChain, true),
		"codex exec": codexRun("session_start-startup-exec", nil, codexExecChain, true),
	}
	for name, r := range runs {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t)
			parent := newShell(t, srv, r.provider, map[string]string{r.sessionVar: "parent-session"})
			parent.register("lead-dev")
			out := parent.must("", "delegate", "worker", "--ttl", "1h")
			i := strings.Index(out, "grelot-link:")
			if i < 0 {
				t.Fatalf("grelot delegate printed %q, no link", out)
			}
			worker := r.shell(t, srv, r.start)
			if got := worker.hook("session_start", worker.fixture(r.start)); got.Code != 0 || got.Stdout != "" {
				t.Fatalf("worker's session start: exit %d, stdout %q; want 0 and nothing", got.Code, got.Stdout)
			}
			worker.must("", "register", "--delegate", strings.Fields(out[i:])[0], "--desc", "answering one question")
			worker.must("the answer", "as", "lead-dev/worker", "post", "@lead-dev", "Answer: shape", "--desc", "no change")
			if inbox := parent.must("", "--self", "inbox"); !strings.Contains(inbox, "the answer") {
				t.Errorf("the parent's inbox %q, want the worker's post", inbox)
			}
			parent.post("@lead-dev/worker", "a follow-up")
			if got := worker.must("", "as", "lead-dev/worker", "wait", "--timeout=1s"); !strings.Contains(got, "a follow-up") {
				t.Errorf("the worker's wait printed %q, want the parent's follow-up", got)
			}
		})
	}
}

// An identity registered from a one-shot run's session keeps its card and
// its hold for unread mail.
func TestOneShot_registered(t *testing.T) {
	runs := map[string]oneShotRun{
		"claude -p":  claudeRun(claudePrintEnv, claudePrintChain, true),
		"codex exec": codexRun("session_start-startup-exec", nil, codexExecChain, true),
	}
	for name, r := range runs {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t)
			sh := r.shell(t, srv, r.start)
			sh.register("oneshot-dev")
			got := sh.hook("session_start", sh.fixture(r.start))
			if got.Code != 0 || !strings.HasPrefix(got.Stdout, "@oneshot-dev · box · ") {
				t.Errorf("session start: exit %d, stdout %q; want 0 and the card of @oneshot-dev", got.Code, got.Stdout)
			}

			writer := newShell(t, srv, r.provider, map[string]string{r.sessionVar: "writer-session"})
			writer.register("writer-dev")
			writer.post("@oneshot-dev", "a question")
			srv.clock.advance(time.Second)
			got = sh.hook("stop", sh.fixture(r.stop))
			if b := parseBlock(t, got.Stdout); got.Code != 0 || !strings.HasPrefix(b.Reason, "grelot (automated, not the user): unread ") {
				t.Errorf("stop: exit %d, reason %q; want 0 and the unread hold", got.Code, b.Reason)
			}
		})
	}
}

// A codex started from a registered thread's shell inherits that thread's
// CODEX_THREAD_ID into its hooks, which still act as their own stdin
// session: an exec run stays quiet and a TUI is asked to register, neither
// shown the parent's card nor held for its mail.
func TestOneShot_inheritedThread(t *testing.T) {
	runs := map[string]oneShotRun{
		"codex exec":    codexRun("session_start-startup-exec", nil, codexExecChain, true),
		"the Codex TUI": codexRun("session_start-startup-tui", nil, codexTUIChain, false),
	}
	for name, r := range runs {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t)
			parent := newShell(t, srv, "codex", map[string]string{"CODEX_THREAD_ID": "lead-thread-0001"})
			parent.register("lead-dev")
			writer := newShell(t, srv, "codex", map[string]string{"CODEX_THREAD_ID": "writer-thread-0001"})
			writer.register("writer-dev")
			writer.post("@lead-dev", "a question")
			srv.clock.advance(time.Second)

			r.env = map[string]string{"CODEX_THREAD_ID": "lead-thread-0001"}
			r.omitSessionFromHooks = false
			startWant, promptWant := registerStart+"\n", registerPrompt+"\n"
			if r.oneShot {
				startWant, promptWant = "", ""
			}
			for role, c := range map[string]struct{ fixture, want string }{
				"session_start": {r.start, startWant},
				"prompt":        {r.prompt, promptWant},
			} {
				sh := r.shell(t, srv, c.fixture)
				if got := sh.hook(role, sh.fixture(c.fixture)); got.Code != 0 || got.Stdout != c.want || got.Stderr != "" {
					t.Errorf("%s: exit %d, stdout %q, stderr %q; want 0 and stdout %q", role, got.Code, got.Stdout, got.Stderr, c.want)
				}
			}
			sh := r.shell(t, srv, r.stop)
			got := sh.hook("stop", sh.fixture(r.stop))
			if r.oneShot {
				if got.Code != 0 || got.Stdout != "" || got.Stderr != "" {
					t.Errorf("stop: exit %d, stdout %q, stderr %q; want 0 and no hold", got.Code, got.Stdout, got.Stderr)
				}
				return
			}
			if b := parseBlock(t, got.Stdout); got.Code != 0 || b.Reason != registerHold {
				t.Errorf("stop: exit %d, reason %q; want 0 and the register line's hold", got.Code, b.Reason)
			}
		})
	}
}

// A server that cannot be reached leaves a one-shot run's output its own,
// while any other run is told the check failed.
func TestOneShot_serverUnreachable(t *testing.T) {
	runs := map[string]oneShotRun{
		"claude -p":                          claudeRun(claudePrintEnv, claudePrintChain, true),
		"codex exec":                         codexRun("session_start-startup-exec", nil, codexExecChain, true),
		"kiro-cli chat --no-interactive":     kiroRun(kiroChain("--v3", "chat", "--no-interactive", oneShotPrompt), true),
		"an interactive Claude Code session": claudeRun(claudeInteractiveEnv, claudeTUIChain, false),
		"the Codex TUI":                      codexRun("session_start-startup-tui", nil, codexTUIChain, false),
	}
	down := &testServer{clock: &clock{now: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}, URL: "http://127.0.0.1:1"}
	for name, r := range runs {
		t.Run(name, func(t *testing.T) {
			for role, fixture := range map[string]string{"session_start": r.start, "prompt": r.prompt, "stop": r.stop} {
				sh := r.shell(t, down, fixture)
				got := sh.hook(role, sh.fixture(fixture))
				told := strings.Contains(got.Stdout+got.Stderr, "could not check this session's identity")
				if got.Code != 0 || r.oneShot && got.Stdout+got.Stderr != "" || !r.oneShot && !told {
					t.Errorf("%s: exit %d, stdout %q, stderr %q; want 0, and the could-not-check line only outside a one-shot run", role, got.Code, got.Stdout, got.Stderr)
				}
			}
		})
	}
}
