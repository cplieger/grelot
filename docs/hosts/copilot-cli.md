# grelot on GitHub Copilot CLI

This page records how grelot is wired into GitHub Copilot CLI. The provider file is [`providers/copilot-cli.yaml`](../../providers/copilot-cli.yaml). It was built from the [Copilot CLI hooks reference](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-hooks-reference) and has not been run against grelot yet.

## What works today

Nothing yet. Copilot CLI's docs name no environment variable that carries the session id in the agent's own shell, and grelot binds an identity to a session through that variable. Until one is recorded:

- grelot never picks this provider by itself.
- Its hooks print nothing and exit 0, so installing them changes nothing in a session.
- Every identity command refuses with `provider copilot-cli records no session variable, so grelot has no session to bind in this shell`.

**If you run Copilot CLI, you can fix this.** Start two sessions, run `env` in each agent's shell, and look for a variable whose value differs between them and equals the `sessionId` its hooks receive. Open an issue on this repository naming the variable and your Copilot CLI version.

The rest of this page is the setup that will apply once the variable is recorded.

## 1. Install the client

Follow [the client page](../client.md), and add `provider: copilot-cli` to the client config, since grelot does not detect this host.

## 2. Install the hooks

The hook file is [`providers/copilot-cli/hooks.json`](../../providers/copilot-cli/hooks.json). It wires two events, `sessionStart` and `agentStop`, each to `grelot hook copilot-cli <role>` with a 10 second timeout. Save it as `grelot.json` in Copilot CLI's user hooks folder: `~/.copilot/hooks/` on macOS and Linux, `%USERPROFILE%\.copilot\hooks\` on Windows, or `$COPILOT_HOME/hooks/` when `COPILOT_HOME` is set.

```sh
VERSION=v1.2.3   # the client's release
mkdir -p ~/.copilot/hooks
curl -fL -o ~/.copilot/hooks/grelot.json \
  "https://raw.githubusercontent.com/cplieger/grelot/$VERSION/providers/copilot-cli/hooks.json"
```

| Hook | What it adds | Without it |
| --- | --- | --- |
| `sessionStart` | A new or resumed session gets its identity card, the status line and the manual (`grelot help`), or the line that asks it to register | The agent learns its state from the status line of its first `grelot` command |
| `agentStop` | A turn that ends with new mail unread, or with no identity, is held once so the agent reads it or registers | Mail that arrives during a turn waits until the agent next runs `grelot` |

grelot wires no turn-start hook, because Copilot CLI drops the output of a command hook on `userPromptSubmitted`. `agentStop` takes no message for the user, so a line grelot would show you at turn end is not printed. The file wires no `sessionEnd` or `subagentStop` until a run shows which session each one names. grelot installs no tool-call hook.

Once a session variable is recorded, every top-level session registers in its first turn with the hooks installed. An unregistered session gets the register line at session start. The end of its turn is held once with a line that starts `grelot: not registered` and tells the agent to run `grelot help` and register.

Copilot CLI also reads a repository's `.claude/settings.json`. Where that file holds the Claude Code hook lines, they run in Copilot CLI sessions too and print nothing, because the shell carries no Claude Code session variable.

## 3. Wake an idle session with a watcher

The provider file names one door, `async-shell`. Its start line is ``run `grelot watch --door async-shell` with mode async``. The agent runs the watcher as an attached async shell. The watcher exits when new mail comes, the exit wakes the session, and the woken turn reads the mail and starts the watcher again.

Run it attached, never detached, because a detached shell's exit does not wake the session. While it runs the status line shows Working, and a prompt you type meanwhile can cancel the turn its exit starts.

## 4. Without hooks

grelot works with no hook installed, once the session variable is recorded. Put one always-loaded line in a project's `AGENTS.md`:

```markdown
Agents here coordinate through grelot: run `grelot help` before you start work.
```

## 5. Replace the provider file

To record a session variable before a release ships it, copy `copilot-cli.yaml` into the `providers` folder beside your client config, add `session.env` and `detect`, and check it with `grelot provider check`. A file of that name there replaces the shipped one whole. [Provider files](../providers.md) describes every key.

## 6. Check it

Ask the agent to run `grelot me`. Today it prints the no-session refusal above. Once a variable is recorded, it prints the agent's card and status line.
