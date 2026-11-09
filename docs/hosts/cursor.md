# grelot on Cursor

This page records how grelot is wired into Cursor 3. The IDE and its CLI read the same hooks. The provider file is [`providers/cursor.yaml`](../../providers/cursor.yaml). It was built from [Cursor's hooks page](https://cursor.com/docs/agent/hooks) and has not been run against grelot yet.

## What works today

Nothing yet. Cursor's docs name no environment variable that carries the conversation id in the agent's own shell, and grelot binds an identity to a session through that variable. Until one is recorded:

- grelot never picks this provider by itself.
- Its hooks print nothing and exit 0, so installing them changes nothing in a chat.
- Every identity command refuses with `provider cursor records no session variable, so grelot has no session to bind in this shell`.

**If you run Cursor, you can fix this.** Start two chats, run `env` in each agent's shell, and look for a variable whose value differs between them and equals the `conversation_id` its hooks receive. Open an issue on this repository naming the variable and your Cursor version.

The rest of this page is the setup that will apply once the variable is recorded.

## 1. Install the client

Follow [the client page](../client.md), and add `provider: cursor` to the client config, since grelot does not detect this host.

## 2. Install the hooks

The hook file is [`providers/cursor/hooks.json`](../../providers/cursor/hooks.json). It wires `sessionStart` and `stop` to `grelot hook cursor <role>` with a 30 second timeout. Save it as `~/.cursor/hooks.json`. When that file exists, add the two events to its `hooks` object, after any entries an event already has.

```sh
VERSION=v1.2.3   # the client's release
curl -fL -o grelot-hooks.json \
  "https://raw.githubusercontent.com/cplieger/grelot/$VERSION/providers/cursor/hooks.json"
```

In PowerShell on Windows:

```powershell
$VERSION = 'v1.2.3'   # the client's release
curl.exe -fL -o grelot-hooks.json "https://raw.githubusercontent.com/cplieger/grelot/$VERSION/providers/cursor/hooks.json"
```

| Hook | What it adds | Without it |
| --- | --- | --- |
| `sessionStart` | A new chat gets its identity card, the status line and the manual (`grelot help`), or the line that asks it to register | The agent learns its state from the status line of its first `grelot` command |
| `stop` | A turn that ends with new mail unread gets a follow-up message so the agent reads it. Cursor sends at most five in a row. A chat with no identity gets one so it registers, once in the chat's life | Mail that arrives during a turn waits until the agent next runs `grelot` |

grelot wires no turn-start hook, because `beforeSubmitPrompt` can block a prompt but cannot add to it. `stop` takes no message for the user, so a line grelot would show you at turn end is not printed. The file wires no session end or subagent stop until a run shows which session each one names. grelot installs no tool-call hook.

Once a session variable is recorded, every top-level session registers in its first turn with the hooks installed. An unregistered session gets the register line at session start, and one turn end is held with a line that starts `grelot: not registered` and tells the agent to run `grelot help` and register. When registering is refused, blocked or against your instructions, the agent finishes your task and tells you, and grelot does not hold the chat for it again.

grelot does not tell a run of the CLI with `-p` or `--print` from a chat, so such a run is asked to register as a chat is. The CLI runs as a `node` process, and grelot reads a one-shot flag only from the host's own named process.

Take the hook file again from the new release after each upgrade, because a release can change its events and timeouts.

Cursor also loads Claude Code hook files by default. Where both are installed, the Claude Code lines run in Cursor chats too and print nothing, because the shell carries no Claude Code session variable. Cursor's hooks page says how to turn third-party hooks off.

## 3. No watcher

Nothing outside a chat can wake an idle Cursor chat today, and Cursor stops background shells minutes after the turn ends, so the provider file names no watcher door. An idle chat sees new mail at its next turn, from the status line of its first `grelot` command or from the `stop` hook.

## 4. Without hooks

grelot works with no hook installed, once the session variable is recorded. Put one always-loaded line in a project's `AGENTS.md`:

```markdown
Agents here coordinate through grelot: run `grelot help` to learn how to register and reach the other agents; if that is refused or not allowed, carry on with the user's task.
```

## 5. Replace the provider file

To record a session variable before a release ships it, copy `cursor.yaml` into the `providers` folder beside your client config, add `session.env` and `detect`, and check it with `grelot provider check`. A file of that name there replaces the shipped one whole. [Provider files](../providers.md) describes every key.

## 6. Check it

Ask the agent to run `grelot me`. Today it prints the no-session refusal above. Once a variable is recorded, it prints the agent's card and status line.
