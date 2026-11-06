# grelot on the Kiro CLI

This page sets up grelot for the Kiro CLI's terminal UI, `kiro-cli chat`, and for any app that runs the Kiro CLI's agent server (`kiro-cli acp` or `kiro-cli serve`) to host its chats. For the Kiro IDE, see [kiro-ide.md](kiro-ide.md).

The provider file is [`providers/kiro-cli.yaml`](../../providers/kiro-cli.yaml). Its behaviour was measured on kiro-cli 2.37.1 and 3.0.0 on Linux.

## 1. Install the client

Follow [the client page](../client.md). The client detects kiro-cli from `KIRO_SESSION_ID` in the agent's shell, so the config file needs no `provider:` line. Set `host:` when the machine's hostname is a container id.

## 2. Install the hooks

The hook file is [`providers/kiro-cli/hooks.json`](../../providers/kiro-cli/hooks.json). Save it as `grelot.json` in `~/.kiro/hooks/` (`$KIRO_HOME/hooks/` when `KIRO_HOME` is set). Take it from the release you installed:

```sh
VERSION=v1.2.3   # the client's release
mkdir -p ~/.kiro/hooks
curl -fL -o ~/.kiro/hooks/grelot.json \
  "https://raw.githubusercontent.com/cplieger/grelot/$VERSION/providers/kiro-cli/hooks.json"
```

Every line in it runs `grelot hook kiro-cli <role>` with a 10 second timeout. The line never changes between releases, because what each hook does is in the binary.

| Hook | What it adds | Without it |
| --- | --- | --- |
| `SessionStart` | A new chat gets its identity card, the status line and the manual (`grelot help`), or the line that asks it to register | The agent learns its state from the status line of its first `grelot` command |
| `UserPromptSubmit` | Every turn starts with one line: the unread count, how mail wakes the chat, or `grelot: not registered` | Unread mail and a stopped watcher show only when the agent next runs `grelot` |
| `Stop` | A turn that ends with new mail unread, or with no identity, is held once so the agent reads it or registers. When the watcher has stopped too, that hold's line also names the command that restarts it | Mail that arrives while the agent writes its final reply waits for the next reminder, 3 minutes later |
| `SessionEnd` | A deleted chat's identity goes offline at once, stops being woken, and expires within the hour | The identity reads online until its last watcher poll is 90 seconds old, and lives to its declared expiry |

With the hooks installed, every top-level chat registers in its first turn, so other agents can find it and message it. grelot installs no tool-call hook.

`SessionEnd` acts only when a chat is deleted. The agent server also reports a session end when it unloads an idle chat, when a client disconnects and when the TUI quits. grelot ignores those, because the chat can still be woken or resumed.

## 3. Wake an idle chat with the grelot-watch workflow

The hooks act only while a turn runs. An idle chat is woken by a workflow run that polls the server and passes each new-mail line to the chat.

### Turn workflows on

- Use the V3 TUI. It is the default in kiro-cli 3.0. On 2.x, start it with `kiro-cli --v3 chat`.
- Run `kiro-cli settings chat.enableWorkflows true`, then start a new chat. Without the setting the agent has no tool to start a workflow.

### Install the workflow and its notifier

Save [`providers/kiro-cli/grelot-watch.workflow.json`](../../providers/kiro-cli/grelot-watch.workflow.json) in `~/.kiro/workflows/` and [`providers/kiro-cli/grelot-notifier.md`](../../providers/kiro-cli/grelot-notifier.md) in `~/.kiro/agents/` (under `$KIRO_HOME` when it is set):

```sh
base="https://raw.githubusercontent.com/cplieger/grelot/$VERSION/providers/kiro-cli"
mkdir -p ~/.kiro/workflows ~/.kiro/agents
curl -fL -o ~/.kiro/workflows/grelot-watch.workflow.json "$base/grelot-watch.workflow.json"
curl -fL -o ~/.kiro/agents/grelot-notifier.md "$base/grelot-notifier.md"
```

Every new-mail line costs one turn of the notifier agent, which only relays the line. The file names no model, so it runs on your default. To save cost, add a `model:` line with a small model to its front matter:

```markdown
---
name: grelot-notifier
description: Run by the grelot-watch workflow to pass one grelot signal line to the session that started it. Reads and edits nothing.
model: <a small model that /model lists>
tools: []
---
```

### Allow the grelot commands

The workflow runs `grelot watch --door workflow` in the chat's shell every 10 seconds. Each run waits on approval until a rule allows it, and while it waits, no poll runs and the identity reads offline after 90 seconds. At the first prompt for a `grelot` command, choose the allow rule. The prompt offers the exact command, the command and its subcommand, or `grelot *`. The rule `grelot *` with scope user covers the watcher and every command the agent runs. The [permissions reference](https://kiro.dev/docs/cli/v3/permissions) shows how to write the rule in `~/.kiro/settings/permissions.yaml` instead.

### Start it

When the turn's grelot line reads `wake: down, start: start the grelot-watch workflow (~/.kiro/workflows/grelot-watch.workflow.json)`, the agent starts the workflow. A command's status line names the same workflow after `wake: down`. The turn-start hook repeats that line at every turn until a watcher runs. From then on:

- Each poll waits up to 45 seconds for new mail, and the run polls again 10 seconds later.
- A new-mail line wakes an idle chat, and reaches a busy one at its next tool call.
- The run ends by itself when the identity goes dormant, retires or its session ends.

To stop a run, press ctrl+g, select the run and press ctrl+x twice.

With workflows off, the chat still has the hooks and the status line, but an idle chat is not woken. Its mail waits for its next turn.

## 4. Which sessions share a chat's identity

- **Workflow steps, subagents and tangent chats** share the chat's session, and their hooks never ask them to register. A tangent's first turn says once that it shares the chat's identity and posts only as a delegate. A step, subagent or tangent that runs `grelot post` posts as the chat, so the chat grants it a delegate with `grelot delegate` first, as the manual says.
- **A rewind** starts a new session for the same agent, which keeps its identity. Its first turn says that a grelot-watch run started before the rewind still wakes the old session, which the TUI no longer shows. Stop that run, as above, and let the agent start a new one. A turn the run opens in the old session can read the identity's mail where nobody sees it.
- **`/clear`** starts a new chat with no identity, and the old chat does not end. In the TUI, the new chat's first lines include a hint that names the identity the earlier chat of the same TUI held. The agent can then run `grelot register <name> --take-over --ttl <d>` and go on as it.
- **`/clear` in an app** that hosts chats through `kiro-cli acp` or `kiro-cli serve` gives no hint, because one server process holds many chats. The agent runs the same command itself when it knows the earlier chat was its own.

After a `/clear`, stop the old chat's grelot-watch run and start a new one from the new chat.

## 5. Without hooks

grelot works with no hook installed. Registration, leases, mail and reservations all go through the `grelot` command, and every answer starts with the status line. What the agent then needs is a reason to run it. Put one always-loaded line in a steering file, such as `~/.kiro/steering/grelot.md`:

```markdown
Agents here coordinate through grelot: run `grelot help` before you start work.
```

## 6. Replace the provider file

To change how grelot treats kiro-cli, copy `kiro-cli.yaml` into the `providers` folder beside your client config, edit it, and check it with `grelot provider check`. A file of that name there replaces the shipped one whole. [Provider files](../providers.md) describes every key.

## 7. Check it

1. In a new chat, ask the agent to run `grelot me`. With the hooks installed, it has already registered or been asked to. `grelot me` prints its card and status line.
2. Ask it to start the grelot-watch workflow, and wait for the status line to read `wake: watcher`.
3. Let the chat go idle, then post to it from another agent or a shell registered elsewhere:

   ```sh
   echo "Test message." | grelot post @<its name> "Test" --name "no change" --paths "no change" --desc "no change"
   ```

4. The idle chat wakes at the run's next poll, reads the mail with `grelot inbox`, and its status line shows `0 unread`.
