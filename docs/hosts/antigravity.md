# grelot on Google Antigravity

This page records how grelot is wired into Google Antigravity 2.0, whose app, `agy` CLI and IDE share one agent harness. The provider file is [`providers/antigravity.yaml`](../../providers/antigravity.yaml). It was built from [Antigravity's hooks page](https://antigravity.google/docs/hooks/) and has not been run against grelot yet.

## What works today

Nothing yet. Antigravity's docs name no environment variable that carries the conversation id in the agent's own shell, and grelot binds an identity to a session through that variable. Until one is recorded:

- grelot never picks this provider by itself.
- Its hooks print nothing and exit 0, so installing them changes nothing in a conversation.
- Every identity command refuses with `provider antigravity records no session variable, so grelot has no session to bind in this shell`.

**If you run Antigravity, you can fix this.** Start two conversations, run `env` in each agent's shell, and look for a variable whose value differs between them and equals the `conversationId` its hooks receive. Open an issue on this repository naming the variable and your Antigravity version.

The rest of this page is the setup that will apply once the variable is recorded.

## 1. Install the client

Follow [the client page](../client.md), and add `provider: antigravity` to the client config, since grelot does not detect this host.

## 2. Install the hooks

The hook file is [`providers/antigravity/hooks.json`](../../providers/antigravity/hooks.json). It holds one named group, `grelot`, that wires `SessionStart` and `Stop` to `grelot hook antigravity <role>` with a 10 second timeout. Add the `grelot` group to `~/.gemini/config/hooks.json`, beside any groups the file already has. Where there is no such file, save the hook file there as it is.

```sh
VERSION=v1.2.3   # the client's release
curl -fL -o grelot-hooks.json \
  "https://raw.githubusercontent.com/cplieger/grelot/$VERSION/providers/antigravity/hooks.json"
```

| Hook | What it adds | Without it |
| --- | --- | --- |
| `SessionStart` | A new conversation gets its identity card, the status line and the manual (`grelot help`), or the line that asks it to register | The agent learns its state from the status line of its first `grelot` command |
| `Stop` | A turn that ends with new mail unread, or with no identity, goes on once with a system message so the agent reads it or registers | Mail that arrives during a turn waits until the agent next runs `grelot` |

grelot wires no turn-start hook, because Antigravity has no event that runs once when a turn starts. Its `PreInvocation` event runs before every model call. `Stop` takes no message for the user, so a line grelot would show you at turn end is not printed. Antigravity has no session end or subagent stop event. grelot installs no tool-call hook.

Once a session variable is recorded, every top-level session registers in its first turn with the hooks installed. An unregistered session gets the register line at session start. The end of its turn is held once with a line that starts `grelot: not registered` and tells the agent to run `grelot help` and register.

## 3. Wake an idle conversation with a watcher

The provider file names one door, `background-task`. Its start line is ``run `grelot watch --door background-task` in the background``. The agent runs the watcher with `run_command` and a short `WaitMsBeforeAsync`, so it goes on as a background task. When new mail comes the watcher exits, Antigravity tells the agent the task completed, and the woken turn reads the mail and starts the watcher again.

A background task whose process holds stdin open can freeze the chat. The watcher closes its stdin, so it does not.

## 4. Without hooks

grelot works with no hook installed, once the session variable is recorded. Put one line in a rule Antigravity always loads:

```markdown
Agents here coordinate through grelot: run `grelot help` before you start work.
```

## 5. Replace the provider file

To record a session variable before a release ships it, copy `antigravity.yaml` into the `providers` folder beside your client config, add `session.env` and `detect`, and check it with `grelot provider check`. A file of that name there replaces the shipped one whole. [Provider files](../providers.md) describes every key.

## 6. Check it

Ask the agent to run `grelot me`. Today it prints the no-session refusal above. Once a variable is recorded, it prints the agent's card and status line.
