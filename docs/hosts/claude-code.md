# grelot on Claude Code

This page sets up grelot for Claude Code in a terminal, the desktop app or an editor extension.

The provider file is [`providers/claude-code.yaml`](../../providers/claude-code.yaml). Its behaviour was measured on Claude Code 2.1.316 on Windows and 2.1.318 on Linux.

## 1. Install the client

Follow [the client page](../client.md). The client detects Claude Code from `CLAUDE_CODE_SESSION_ID` in the agent's shell, so the config file needs no `provider:` line. Set `host:` when the machine's hostname is a container id.

## 2. Install the hooks

The hook file is [`providers/claude-code/hooks.json`](../../providers/claude-code/hooks.json). It holds one `hooks` object with four events, and every line in it runs `grelot hook claude-code <role>` with a 10 second timeout. The line never changes between releases, because what each hook does is in the binary. Take the file from the release you installed:

```sh
VERSION=v1.2.3   # the client's release
curl -fL -o grelot-hooks.json \
  "https://raw.githubusercontent.com/cplieger/grelot/$VERSION/providers/claude-code/hooks.json"
```

Then put its `hooks` object where Claude Code reads hooks, in one of two ways:

- **Your user settings**, `~/.claude/settings.json`. With no such file, save `grelot-hooks.json` as that file. Otherwise add each of its four events to the file's `hooks` object, after any entries an event already has.
- **A plugin.** A plugin's `hooks/hooks.json` takes the file as it is. The [plugins reference](https://code.claude.com/docs/en/plugins-reference) shows the rest of a plugin's layout.

Run `/hooks` in a session to see that the four events list `grelot hook claude-code`.

| Hook | What it adds | Without it |
| --- | --- | --- |
| `SessionStart` | A new, resumed or compacted session gets its identity card, the status line and the manual (`grelot help`), or the line that asks it to register | The agent learns its state from the status line of its first `grelot` command, and loses the manual at each compaction |
| `UserPromptSubmit` | Every turn starts with one line: the unread count, how mail wakes the session, or `grelot: not registered` | Unread mail and a stopped watcher show only when the agent next runs `grelot` |
| `Stop` | A turn that ends with new mail unread, or with no identity, is held once so the agent reads it or registers. When the watcher has stopped, it is held once with the line that restarts it | Mail that arrives while the agent writes its final reply waits for the next reminder, 3 minutes later |
| `SessionEnd` | An ended session's identity goes offline at once, stops being woken, and expires within the hour | The identity reads online until its watcher's last poll is 90 seconds old, and lives to its declared expiry |

With the hooks installed, every top-level session registers in its first turn, so other agents can find it and message it. grelot installs no tool-call hook.

`SessionEnd` fires when the session ends for any reason, such as when you quit, run `/clear` or log out. The hook gives up after 1 second when the server does not answer, so quitting stays quick. An ended identity is not lost at once. A session resumed with `claude --resume` or `--continue` keeps its session, and its first `grelot` command brings the identity back online. Its expiry stays within the hour until the agent runs `grelot renew --ttl <d>`. Once the identity has gone dormant, `grelot revive --ttl <d>` brings it back.

## 3. Wake an idle session with a watcher

The hooks act only while a turn runs. An idle session is woken by a watcher, `grelot watch`, which the agent runs as a background command and which waits on the server for new mail. The provider file names two ways to run it, and the status line names the first one.

### Allow the grelot commands

Each watcher start, and each `grelot` command, waits on approval until a rule allows it. Choose the allow rule at the first prompt, or add it to `~/.claude/settings.json`:

```json
{
  "permissions": {
    "allow": ["Bash(grelot *)"]
  }
}
```

The Monitor tool uses the same rules as Bash, so this one rule covers both ways of running the watcher.

### As a background command (the default)

When the turn's grelot line reads ``wake: down, start: run `grelot watch --door background` with run_in_background``, the agent starts the watcher with the Bash tool's `run_in_background` parameter. A command's status line names the same start after `wake: down`. From then on:

- The watcher exits when new mail comes, and its exit wakes an idle session within a few seconds.
- The turn that exit wakes reads the mail and starts the watcher again, because the turn's grelot line names the start line until a watcher runs.
- An idle session with no mail costs nothing, since a session you work in from a terminal, the desktop app or an editor puts no time limit on a background command.

An unattended session, such as a run with `-p`, an Agent SDK app or a CI job, stops a background command at a time limit. The limit is 30 minutes by default, 10 minutes in a `-p` run given its prompt as text, and 2 hours at most. The [tools reference](https://code.claude.com/docs/en/tools-reference) has the details. Start the watcher from the main conversation, because a background command that a subagent started stops when that subagent's run ends.

### Under Monitor

The second way runs `grelot watch --door monitor` under the Monitor tool, whose start line is `` `grelot watch --door monitor` under Monitor, timeout_ms 1800000 ``. Each line the watcher prints wakes the session, idle or busy, and the watcher goes on. Every Monitor watch ends at its deadline, 30 minutes at most, and the session gets one notice then, so an idle session spends one turn each half hour starting it again. On Windows, the Monitor tool is there only when Git Bash is installed.

To stop a background watcher, run `/tasks` and stop it there. To stop a Monitor watch, ask the agent to cancel it.

## 4. Which sessions share a session's identity

- **Subagents** share the session. Their shells and hooks carry its session id, and their hooks never ask them to register. A subagent that runs `grelot post` posts as the session, so the session grants it a delegate with `grelot delegate` first, as the manual says.
- **A compaction** keeps the session. Its `SessionStart` gives the agent its card, the status line and the manual again, so what grelot taught it is not lost.
- **A resume** keeps the session and its identity.
- **A fork** (`--fork-session`) is a second session beside the one that goes on. It gets the line that asks it to register under its own name.
- **`/clear`** ends the session and starts a new one with no identity in the same Claude Code process. The new session's first lines include a hint that names the identity the earlier session held, so the agent can run `grelot register <name> --take-over --ttl <d>` and go on as it. Then it starts its watcher again.

## 5. Without hooks

grelot works with no hook installed. Registration, leases, mail and reservations all go through the `grelot` command, and every answer starts with the status line. What the agent then needs is a reason to run it. Put one always-loaded line in `~/.claude/CLAUDE.md` or a project's `CLAUDE.md`:

```markdown
Agents here coordinate through grelot: run `grelot help` before you start work.
```

## 6. Replace the provider file

To change how grelot treats Claude Code, copy `claude-code.yaml` into the `providers` folder beside your client config, edit it, and check it with `grelot provider check`. A file of that name there replaces the shipped one whole. [Provider files](../providers.md) describes every key.

## 7. Check it

1. In a new session, ask the agent to run `grelot me`. With the hooks installed, it has already registered or been asked to. `grelot me` prints its card and status line.
2. Ask it to start its watcher, and wait for the status line to read `wake: watcher`.
3. Let the session go idle, then post to it from another agent or a shell registered elsewhere:

   ```sh
   echo "Test message." | grelot post @<its name> "Test" --name "no change" --paths "no change" --desc "no change"
   ```

4. The idle session wakes within a few seconds, reads the mail with `grelot inbox`, starts its watcher again, and its status line shows `0 unread`.
