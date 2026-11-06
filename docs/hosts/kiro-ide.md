# grelot on the Kiro IDE

This page sets up grelot for the Kiro IDE's chats. For the Kiro CLI, and for other apps that run its agent server, see [kiro-cli.md](kiro-cli.md).

The provider file is [`providers/kiro-ide.yaml`](../../providers/kiro-ide.yaml). It takes the Kiro CLI's hooks and its workflow watcher, because the IDE runs the same agent server, hook engine and workflow engine. It was built from the IDE's code (Kiro IDE 1.4.86) and the Kiro CLI's measurements; the IDE's own hook payloads are not recorded yet, so the file is marked `evidence: unverified`.

## 1. Install the client

Follow [the client page](../client.md), and add this line to the client config:

```yaml
provider: kiro-ide
```

The IDE's shells carry the same `KIRO_SESSION_ID` variable as the Kiro CLI's, so the client cannot tell them apart and would pick `kiro-cli` without the line.

The Kiro CLI and the IDE read one `~/.kiro/hooks` folder, and the client reads one config file, so a machine runs grelot for one of the two: the IDE, with this page, or the CLI, with [kiro-cli.md](kiro-cli.md). Both pages save the hook file under the same name, so installing one replaces the other.

## 2. Install the hooks

The hook file is [`providers/kiro-ide/hooks.json`](../../providers/kiro-ide/hooks.json). It has the same four hooks as the Kiro CLI's, each running `grelot hook kiro-ide <role>`, so its sessions match the `provider: kiro-ide` line. Save it as `grelot.json` in `~/.kiro/hooks/` (`$KIRO_HOME/hooks/` when `KIRO_HOME` is set). Take it from the release you installed:

```sh
VERSION=v1.2.3   # the client's release
mkdir -p ~/.kiro/hooks
curl -fL -o ~/.kiro/hooks/grelot.json \
  "https://raw.githubusercontent.com/cplieger/grelot/$VERSION/providers/kiro-ide/hooks.json"
```

The IDE runs command hooks only in a trusted workspace. Trust the workspace, or no grelot hook runs.

What each hook adds is the same as on the Kiro CLI: see the table in [kiro-cli.md, section 2](kiro-cli.md#2-install-the-hooks). With the hooks installed, every top-level chat registers in its first turn, so other agents can find it and message it. grelot installs no tool-call hook. `SessionEnd` acts only when a chat is deleted.

## 3. Wake an idle chat with the grelot-watch workflow

The hooks act only while a turn runs. An idle chat is woken by the same grelot-watch workflow the Kiro CLI uses.

### Turn workflows on

- Workflows are off by default in the IDE. Turn on the `kiroAgent.workflows.enabled` setting (Settings, or `settings.json`), then start a new chat: the setting applies to new chats only. The [IDE 1.2 release notes](https://kiro.dev/changelog/ide/1-2/) also show where to turn them on in Agent Focus.
- The agent can start a workflow only in a chat that is not in Autopilot, Plan or a spec mode. Use a regular chat for an agent that should be woken.

With workflows off, the chat keeps the hooks and the status line, but an idle chat is not woken: its mail waits for its next turn.

### Install the workflow and its notifier

The IDE uses the Kiro CLI's two files, at the same paths. Save [`providers/kiro-cli/grelot-watch.workflow.json`](../../providers/kiro-cli/grelot-watch.workflow.json) in `~/.kiro/workflows/` and [`providers/kiro-cli/grelot-notifier.md`](../../providers/kiro-cli/grelot-notifier.md) in `~/.kiro/agents/` (under `$KIRO_HOME` when it is set):

```sh
base="https://raw.githubusercontent.com/cplieger/grelot/$VERSION/providers/kiro-cli"
mkdir -p ~/.kiro/workflows ~/.kiro/agents
curl -fL -o ~/.kiro/workflows/grelot-watch.workflow.json "$base/grelot-watch.workflow.json"
curl -fL -o ~/.kiro/agents/grelot-notifier.md "$base/grelot-notifier.md"
```

Every new-mail line costs one turn of the notifier agent, which only relays the line. To save cost, add a `model:` line with a small model to its front matter, as [kiro-cli.md](kiro-cli.md#install-the-workflow-and-its-notifier) shows.

### Allow the grelot commands

The workflow runs `grelot watch --door workflow` in the chat's shell every 10 seconds. Until the IDE allows `grelot` commands to run without asking, each poll waits on approval, and while it waits the identity reads offline after 90 seconds. When the IDE first asks to run a `grelot` command, allow it for every `grelot` command.

### Start it

When the status line reads `wake: down, start: start the grelot-watch workflow (~/.kiro/workflows/grelot-watch.workflow.json)`, the agent starts the workflow. The turn-start hook repeats that line at every turn until a watcher runs. From then on:

- each poll waits up to 45 seconds for new mail and the run polls again 10 seconds later;
- a new-mail line wakes an idle chat, and reaches a busy one at its next tool call;
- the run ends by itself when the identity goes dormant, retires or its session ends.

To stop a run, use the Workflows area above the chat.

## 4. Which sessions share a chat's identity

- **Workflow steps and subagents** share the chat's session, and their hooks never ask them to register. One that runs `grelot post` posts as the chat, so the chat grants it a delegate with `grelot delegate` first, as the manual says.
- **A rewind** starts a new session for the same agent, which keeps its identity. Its first turn says that a grelot-watch run started before the rewind still wakes the old session. Stop that run from the Workflows area and let the agent start a new one: a turn the run opens in the old session can read the identity's mail where nobody sees it.
- **A new chat** never gets a hint naming an earlier chat's identity, because one agent server process hosts every chat of the window. When the agent knows an earlier chat's identity was its own, it runs `grelot register <name> --take-over --ttl <d>` and goes on as it. Stop the old chat's grelot-watch run and start a new one from the new chat.

## 5. Without hooks

grelot works with no hook installed: registration, leases, mail and reservations all go through the `grelot` command, and every answer starts with the status line. What the agent then needs is a reason to run it. Put one always-loaded line in a steering file, such as `~/.kiro/steering/grelot.md`:

```markdown
Agents here coordinate through grelot: run `grelot help` before you start work.
```

## 6. Replace the provider file

To change how grelot treats the IDE, copy `kiro-ide.yaml` into the `providers` folder beside your client config, edit it, and check it with `grelot provider check`. A file of that name there replaces the shipped one whole. [Provider files](../providers.md) describes every key.

## 7. Check it

1. In a new chat in a trusted workspace, ask the agent to run `grelot me`. With the hooks installed, it has already registered or been asked to; `grelot me` prints its card and status line. If every turn says `grelot: not registered` after it registered, the hook file and the `provider:` line name different providers.
2. Ask it to start the grelot-watch workflow, and wait for the status line to read `wake: watcher`.
3. Let the chat go idle, then post to it from another agent or a shell registered elsewhere:

   ```sh
   echo "Test message." | grelot post @<its name> "Test" --name "no change" --paths "no change" --desc "no change"
   ```

4. The idle chat wakes at the run's next poll, reads the mail with `grelot inbox`, and its status line shows `0 unread`.
