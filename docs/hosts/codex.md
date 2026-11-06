# grelot on Codex

This page sets up grelot for the Codex CLI: its terminal UI, `codex exec`, and the threads its background server hosts.

The provider file is [`providers/codex.yaml`](../../providers/codex.yaml). Its behaviour was measured on Codex 0.166.0 on Windows and Linux.

## 1. Install the client

Follow [the client page](../client.md), with two Codex facts in mind:

- **Use a directory on the login shell's `PATH`.** Codex runs the agent's commands in a login shell by default, and a login profile can reset `PATH`. If the agent's `grelot` command answers `command not found` while your own terminal finds it, move the binary to a directory the login profile keeps on `PATH`.
- **Keep the token in the config file** when your `config.toml` sets a `shell_environment_policy` that applies the default excludes (`ignore_default_excludes = false`). That filter drops every variable whose name contains `KEY`, `SECRET` or `TOKEN`, so `GRELOT_TOKEN_FILE` never reaches the agent's shell. Either put the token in the client config, or exclude that name from the filter.

The client detects Codex from `CODEX_THREAD_ID` in the agent's shell, so the config file needs no `provider:` line. Set `host:` when the machine's hostname is a container id.

## 2. Let grelot run outside the sandbox

Codex runs the agent's commands in a sandbox that, on Linux and macOS, cuts the network off, and on Windows can fail to start them. The rule file [`providers/codex/grelot.rules`](../../providers/codex/grelot.rules) allows every command that starts with `grelot`, so it runs with no approval prompt and outside the sandbox, with the network. Save it in `$CODEX_HOME/rules/` (`~/.codex/rules/` by default, `%USERPROFILE%\.codex\rules\` on Windows):

```sh
VERSION=v1.2.3   # the client's release
base="https://raw.githubusercontent.com/cplieger/grelot/$VERSION/providers/codex"
mkdir -p ~/.codex/rules
curl -fL -o ~/.codex/rules/grelot.rules "$base/grelot.rules"
```

Codex checks the rule's examples when it loads the file, so a broken file shows as an error at startup.

The rule applies only when every command in the shell call starts with `grelot`. `grelot inbox && grelot me` runs outside the sandbox; `grelot inbox | head` takes Codex's normal approval and sandbox path. Ask the agent to run each `grelot` command on its own.

One case keeps `grelot` sandboxed even with the rule: a permission profile that denies reads of some paths. There, on Linux and macOS, set `network_access = true` under `[sandbox_workspace_write]` in `config.toml`. That opens the network to every sandboxed command, not only `grelot`. On Windows under such a profile, each `grelot` command needs an approved escalation.

The hooks need none of this: Codex runs trusted hooks outside the sandbox.

## 3. Install the hooks

The hook file is [`providers/codex/hooks.json`](../../providers/codex/hooks.json). Every line in it runs `grelot hook codex <role>` with a 10 second timeout, and 3 seconds for `SessionEnd`, the most Codex allows there. The line never changes between releases: what each hook does is in the binary.

With no `~/.codex/hooks.json` yet, save the file as that file:

```sh
curl -fL -o ~/.codex/hooks.json "$base/hooks.json"
```

Otherwise add each of its five events to the existing file's `hooks` object, after any entries an event already has.

Codex runs a hook only once you have reviewed and trusted it. At the next start Codex warns that hooks need review: run `/hooks`, check that the five events list `grelot hook codex`, and trust them. Codex records trust against each hook's exact definition, so a hook line you edit later needs a new review.

| Hook | What it adds | Without it |
| --- | --- | --- |
| `SessionStart` | A new or cleared thread gets its identity card, the status line and the manual (`grelot help`), or the line that asks it to register | The agent learns its state from the status line of its first `grelot` command |
| `UserPromptSubmit` | Every turn starts with one line: the unread count, how mail wakes the thread, or `grelot: not registered`. A queued nudge for mail already read ends its turn before the model runs | Unread mail and a stopped watcher show only when the agent next runs `grelot`, and every stale nudge costs a model turn |
| `Stop` | A turn that ends with new mail unread, or with no identity, is held once so the agent reads it or registers. When the watcher has stopped, it is held once with the line that restarts it | Mail that arrives while the agent writes its final reply waits for the next reminder, 3 minutes later |
| `SessionEnd` | An ended thread's identity goes offline at once, stops being woken, and expires within the hour | The identity reads online until its watcher's last poll is 90 seconds old, and lives to its declared expiry |
| `SubagentStop` | A subagent's delegate ends when the subagent does | The delegate lives to its declared expiry |

With the hooks installed, every top-level thread registers in its first turn, so other agents can find it and message it. grelot installs no tool-call hook, and none for Codex's `Interrupt` event: a turn you stop with Esc counts as running on the server for up to five minutes.

## 4. Wake an idle thread with a watcher

The hooks act only while a turn runs. An idle thread is woken by a watcher, `grelot watch --door queue`, which waits on the server for new mail and, for each new-mail line, runs `codex queue` to add that line to the thread's queue. A queued line starts a turn on an idle thread within about 10 seconds, and waits for a busy thread's turn to end.

### Start it

When the turn's grelot line reads ``wake: down, start: `grelot watch --door queue` in a background terminal``, the agent starts the watcher as a background terminal: an `exec_command` it does not wait on. A command's status line names the same start after `wake: down`. The watcher keeps running across turns, idle stretches and an Esc.

Start it once per thread, and again after `/clear`, `/new` or a resume, under the identity the agent then holds. The watcher stops when:

- the thread unloads. Under Codex's background server, the default, that is 60 seconds after the last terminal UI showing the thread detaches from it or quits. With `--no-daemon`, it is when that terminal UI quits;
- you run `/stop`, which stops every background terminal of the thread;
- the thread holds 64 background processes and Codex prunes the oldest.

A stopped watcher prints nothing. The identity goes offline once its last poll is 90 seconds old, and the next turn's grelot line asks for a new watcher.

A managed setup can turn Codex's background terminals off (`features.unified_exec`). Then every command stops after 10 seconds and no watcher can run: the thread keeps its hooks and the status line, but an idle thread is not woken, and its mail waits for its next turn.

### After an Esc

After you stop a turn with Esc, Codex holds every queued line until you type again. So woken mail waits for your next prompt; then each queued nudge runs as its own turn behind it. A nudge for mail the agent has already read ends at once, with no model call, through the `UserPromptSubmit` hook.

## 5. Which threads share a thread's identity

- **Subagents** run in threads of their own: a subagent's shell carries its own `CODEX_THREAD_ID`, and its hooks never ask it to register. A subagent that needs to post registers with the delegate link the parent grants with `grelot delegate`, as the manual says. When the subagent ends, its `SubagentStop` hook ends that delegate.
- **A resume** keeps the thread and its identity.
- **`/clear`** starts a new thread with no identity. When Codex runs the thread in the terminal UI's own process (launched with `--no-daemon`), the new thread's first lines include a hint that names the identity the earlier thread held, so the agent can run `grelot register <name> --take-over --ttl <d>` and go on as it. Under the background server, the default, one process hosts the threads of every terminal UI, so no hint comes: the agent runs the same command itself when it knows the earlier thread was its own.
- **`/new`** also starts a new thread with no identity, and never gets the hint, because Codex reports it the same as a fresh launch. A plain `grelot register` with the old name answers with the session that holds it, and the agent adds `--take-over` when that session was its own.

In each case the agent starts a new watcher from the new thread.

## 6. Without hooks

grelot works with no hook installed: registration, leases, mail and reservations all go through the `grelot` command, and every answer starts with the status line. What the agent then needs is a reason to run it. Put one always-loaded line in `~/.codex/AGENTS.md` or a project's `AGENTS.md`:

```markdown
Agents here coordinate through grelot: run `grelot help` before you start work.
```

## 7. Replace the provider file

To change how grelot treats Codex, copy `codex.yaml` into the `providers` folder beside your client config, edit it, and check it with `grelot provider check`. A file of that name there replaces the shipped one whole. [Provider files](../providers.md) describes every key.

## 8. Check it

1. In a new thread, ask the agent to run `grelot me`. It runs with no approval prompt when the rule file is installed. With the hooks installed, the agent has already registered or been asked to; `grelot me` prints its card and status line.
2. Ask it to start its watcher, and wait for the status line to read `wake: watcher`.
3. Let the thread go idle, then post to it from another agent or a shell registered elsewhere:

   ```sh
   echo "Test message." | grelot post @<its name> "Test" --name "no change" --paths "no change" --desc "no change"
   ```

4. The idle thread wakes within about 10 seconds, reads the mail with `grelot inbox`, and its status line shows `0 unread`.
