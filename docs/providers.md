# Provider files

A provider file describes one coding tool, the host an agent runs in, as data: how grelot tells its shells apart, where its session id lives, how a new-mail notice wakes one of its sessions, and how its hook events are spelled. grelot's code names no host, so supporting a new tool, or changing how a known one behaves, means writing or replacing a file.

The files for the known hosts ship inside the client and are listed under [`providers/`](../providers). Each host page under [`hosts/`](hosts/README.md) says which one it uses. Read this page when you replace one of them or write one for another tool.

## Where the files are read

The client reads two layers, and a file in the second replaces the first's file of the same name whole:

1. The shipped files, built into the binary.
2. The `providers` folder beside `config.yaml` (the config folder is listed in [the client page](client.md#2-write-the-config-file)).

Only `*.yaml` files at the root of a folder count. A symlink is read as its target; a dangling one, or a folder that cannot be read, stops every command. A file that fails to load does not stop the others: a command that needs it prints the problem and asks you to run `grelot provider check` on it.

The server reads a third folder of its own, for the `http` doors it pushes through; see [The server's folder](#the-servers-folder).

## Check a file

```sh
grelot provider check ~/.config/grelot/providers/mytool.yaml
```

It exits 0 when the file is valid, and otherwise 1 with one line per problem, each naming the key and never quoting the file. Beyond what loading checks, it renders every template meant as JSON (one whose text starts with `{`, or an `http` body sent as a JSON content type) against sample values that hold quotes, backslashes, newlines and a control character, and fails a template whose output does not parse.

## The file

```yaml
name: mytool
evidence: documented
detect:
  env: [MYTOOL_SESSION_ID]
session:
  env: MYTOOL_SESSION_ID
  stdin: session_id
doors:
  - name: background
    layer: 2
    transport: exit
    start: run `grelot watch --door background` as a background command
hooks:
  session_start:
    event: SessionStart
    outputs:
      context: {text: "{{.Context}}"}
notes: docs/hosts/mytool.md
```

Values are written as they are: a `${...}` reference is refused rather than read from the environment, so a file never pulls a value out of an agent's shell. An unknown key, or a key of another transport on a door, is refused by name.

| Key | Required | What it holds |
| --- | --- | --- |
| `name` | yes | The provider's name: lowercase letters, digits and `-`, 2 to 48 characters, starting with a letter or digit. It must equal the file name without `.yaml` |
| `extends` | no | Another provider this file builds on; see [Extending a file](#extending-a-file) |
| `evidence` | no | Where the file's facts come from: `measured` (recorded from the tool itself), `documented` (from its documentation only) or `unverified` |
| `detect` | no | How the client recognizes the tool's shells |
| `session` | no | Where the tool keeps its session id |
| `doors` | no | How a notice wakes one of the tool's sessions |
| `hooks` | no | The hook events the tool offers, by role |
| `notes` | no | Free text, such as the page that explains the file |

### `detect`

`detect.env` lists environment variables the tool sets in the shells it starts. A command picks the provider that matches the most of them. When several tie, the one whose `session.process` names the nearest ancestor process wins, provided every tied file names one. Otherwise the command stops and asks for `--provider <name>` or `provider: <name>` in the client config, which always win over detection.

A file with no `detect` is chosen only by name.

### `session`

| Key | What it holds |
| --- | --- |
| `env` | The variable that holds the session id in the agent's shell. With none, the shell has no session to bind, and every identity command refuses with a line saying so |
| `stdin` | The path of the session id in the JSON a hook reads on stdin, such as `session_id` |
| `hooks_without_env` | `true` for a tool that leaves `env` out of its hook processes, so a hook acts for the `stdin` session. Without it, a hook that finds no `env` is another tool running these hook lines, and does nothing. Needs `env` and `stdin` |
| `process` | Process names of the tool's own process that runs one session at a time, such as `[mytool, mytool.exe]`. It lets a new session in that same process offer to take over the identity the previous one held, and breaks a detection tie |
| `process_shared` | Arguments that mark one of those processes as serving many sessions at once, such as `[serve]`. A session under such a process is never offered another session's identity. Needs `process` |
| `record` | Where the tool keeps a record for each session, for a tool whose subagents, workflow steps or forks run hooks under their own session id while their shells carry the parent's |

`record` keys:

| Key | What it holds |
| --- | --- |
| `dir_env`, `dir_default` | The tool's state folder: the variable that names it, and the folder used when that variable is unset (`~` is the home folder). One of them is required |
| `glob` | The record file's path under that folder, a template that reads only `{{.HookSession}}`, the hook's own session id |
| `parent`, `reason` | The JSON fields of the record that hold the parent session and why the session was made |
| `same_agent` | Reasons that mean the same agent going on in a new session (a rewind, say). Any other reason with a parent is a second agent beside the first |
| `same_agent_note` | A line printed once to a session `same_agent` matched, for what the user must do about it |

### `doors`

A door is one way a new-mail notice reaches a session. Each door has these keys, and then the keys of its transport:

| Key | Required | What it holds |
| --- | --- | --- |
| `name` | yes | The door's name, unique in the file, spelled like a provider name |
| `layer` | yes | `1` for an `http` door, `2` for the others |
| `transport` | yes | `http`, `exec`, `stdout` or `exit` |
| `start` | no | How an agent starts this door's watcher, quoted to it when its sessions are not woken. The first layer 2 door's line is the one quoted |
| `asset` | no | A companion file for the host, a path inside the providers folder, such as `mytool/watch.json` |
| `notes` | no | Free text |

Every door sends the same fixed line, which names no thread, path or sender, only the notice's number and how many messages are new:

```text
[grelot:42] grelot (automated, not the user): 2 new; run grelot inbox
```

#### Layer 1: `http`

The server sends a request for each notice, so a tool with an API wakes its sessions with no process running beside them. The server reads these doors only from [its own folder](#the-servers-folder).

| Key | Required | What it holds |
| --- | --- | --- |
| `url` | yes | A template. The scheme, host and port are literal text; template actions may appear only in the path and query. No credentials in the URL |
| `body` | yes | The request body, a template |
| `headers` | no | Header names and value templates. Only a header value may call `secret` |
| `match` | no | Which identities the door serves: `provider` (this file's name when left out) and an optional `host`, the label in each client's `host:` |
| `dedupe_window` | when a template reads `.DedupeKey` | How long the receiver drops a repeated key, at least `60s` |
| `gone` | no | 4xx statuses that mean the receiver no longer has that session. 401 and 403 are not allowed here |

The request is a `POST` with a 10 second timeout, and no redirect is followed. Any 2xx is delivered. A 429, a 5xx, a timeout or a network error is retried after the receiver's `Retry-After`, held between 5 and 60 seconds, or after 15 seconds without one; every retry carries the same `.DedupeKey`. A status in `gone` turns push off for that identity, and so does any other status but 401 and 403. A 401 or a 403 turns the door off for every identity it serves, with one `"msg":"push refused"` line, since it means the door's credential is wrong. An identity whose push is off is woken by its own watcher, and gets push back when it registers again or is revived.

#### Layer 2: `exec`, `stdout` and `exit`

The client's watcher, `grelot watch`, takes a standing wait on the server and delivers each notice through one of these doors. `grelot watch --door <name>` picks one; with no `--door` it runs the file's first layer 2 door.

- **`exec`** runs `argv` for each notice, with no shell and no input or output, and goes on watching. `argv` is required, one template per argument, its first element the program. A run has 10 seconds. After a non-zero exit or a timeout the notice comes again later; the third non-zero exit in a row, timeouts aside, stops the watcher with exit code 1.
- **`stdout`** prints `line` for each notice and goes on watching, for a tool that reads a background command's output as it comes.
- **`exit`** prints `line` and exits 0, for a tool that wakes a session when a background command ends. The session starts the watcher again.

`stdout` and `exit` doors take these keys:

| Key | Default | What it holds |
| --- | --- | --- |
| `line` | `{{.Line}}` | What is printed for a notice |
| `end` | `line` | What is printed when the watcher stops for good, with `.Line` the end line |
| `end_exit` | `1` | The exit code after `end`, 0 to 255 |
| `hold` | none | How long one run waits before it prints `idle`, for a tool that polls the command. An `exit` door then exits 0; a `stdout` door goes on |
| `idle` | none | What is printed when `hold` lapses. `hold` and `idle` go together |

The end line says why the watcher stopped:

```text
[grelot:end] grelot (automated, not the user): this watcher stopped (ended); run grelot me
```

A watcher can also run with no door from a file: `grelot watch --transport stdout`, `--transport exit`, or `--transport exec -- <command> <args>`.

### `hooks`

A tool's hook file wires each of its events to one command, `grelot hook <provider> <role>`, and the provider file says what that role reads and prints for this tool. The hook files for the known hosts sit in a folder of the provider's name under [`providers/`](../providers). A role the file leaves out does nothing, so a hook line that names it is harmless. grelot works with no hooks at all; with hooks installed, every top-level session registers before it ends its first turn.

| Role | When it runs | What it does |
| --- | --- | --- |
| `session_start` | A session starts | Prints the identity's status, or for an unregistered session the line that asks it to register, as `context` |
| `prompt` | A turn starts | Prints new mail and reminders as `context`. A turn that starts from a stale notice line, already read, ends through `block` before the model runs |
| `stop` | A turn ends | Holds the turn open with `block` when mail came, or for a session that has not registered; prints a line for the user as `notice` |
| `turn_end` | A turn ends some other way than `stop`, such as an error or an interruption | Tells the server the turn ended; prints nothing |
| `session_end` | A session closes | Tells the server, which ends the session's delegates and lets the identity go dormant within the hour; prints nothing |
| `subagent_stop` | A subagent ends | Ends the delegate that subagent's own session held; prints nothing |

Every role takes these keys:

| Key | What it holds |
| --- | --- |
| `event` | The tool's own name for the event. Required |
| `when` | Stdin paths and the values each must hold for the role to act, such as `{reason: [delete]}` |
| `budget` | How long the role may wait on the server, set only when the tool allows less than the default 3 seconds |
| `once` | `true` prints the role's output only the first time in a session |

And these, by role:

| Key | Roles | What it holds |
| --- | --- | --- |
| `stdin.agent_id` | all | The stdin path of a subagent's id. A hook whose input holds one belongs to a subagent and never acts for its parent |
| `stdin.prompt` | `prompt` | The stdin path of the prompt text |
| `signal` | `prompt` | An RE2 pattern over the whole prompt, with a named group `(?P<line>...)`, for a tool that delivers a background line inside its own wrapper |
| `source` | `session_start` | `path`, the stdin path of the start source, and the values among it that mean a take-over (`take_over`, such as `clear`) or a fork (`fork`). A take-over start may get the hint to take over the identity the previous session in that process held |
| `take_over_every_start` | `session_start` | `true` for a tool that reports no source and starts a session in its process only for a chat with no history: every start but a fork may get the hint. Not with `source.take_over` |
| `once_per_turn` | `stop` | `true` for a tool that runs its stop hooks at most once per turn, so the hold ends the turn whether or not it blocks |
| `stdin.session` | `subagent_stop` | The stdin path of the session the subagent's own shell carries, for a tool whose subagent hooks name the parent at `session.stdin`. `session.stdin` when left out |

Outputs, under `outputs.context`, `outputs.block` and `outputs.notice`:

| Key | Default | What it holds |
| --- | --- | --- |
| `text` | required | What is printed, a template reading `.Context` (for `context`) or `.Reason` (for `block` and `notice`), such as `'{"decision":"block","reason":{{json .Reason}}}'` |
| `stream` | `stdout` | `stdout` or `stderr` |
| `exit` | `0` | The exit code after printing, 0 to 255 |

`session_start` takes `context`; `prompt` takes `context` and `block`; `stop` takes `block` and `notice`. A role whose output kind is left out prints nothing for it, so a `stop` with no `block` never holds a turn.

## Templates

Door keys and hook outputs are Go [`text/template`](https://pkg.go.dev/text/template) templates. Each is checked when the file loads: it is parsed and rendered once against sample values, so a misspelled field fails the load rather than the first notice.

| Field | What it holds |
| --- | --- |
| `.Line` | The fixed notice line, or the end line in `end` |
| `.Seq` | The notice's number |
| `.Session` | The session id the identity is bound to, without the provider name |
| `.Identity` | The identity's name |
| `.DedupeKey` | A key that is the same for every retry of one notice, in `http` doors |
| `.Context`, `.Reason` | A hook output's text |

Read fields one at a time, as `.Line`; the data as a whole (`{{.}}` or `$`) is refused. Inside `with` or `range`, read the data as `$.Line`.

| Function | What it does |
| --- | --- |
| `json` | Writes a value as a JSON string or number, quotes included: `{"text":{{json .Line}}}`. Put every value placed in JSON through it |
| `path` | Escapes a value for one URL path segment: `/sessions/{{path .Session}}` |
| `secret` | `{{secret "<name>"}}`, in an `http` header value only, first in its pipeline, with a name of lowercase letters, digits and `_` |

## Extending a file

A file with `extends: <name>` builds on another file: it inherits every key but `detect`, so it is chosen only by name. A door it lists replaces the parent's door of the same name in place, and its other doors are added. Any other key it sets (`evidence`, `notes`, `session`, `hooks`) replaces the parent's whole. Extending is one level only, and a file that extends an invalid or missing one is invalid itself.

To change one thing about a shipped host, put a file of its name in your `providers` folder. To add a variant beside it, give the new file another name, `extends` the shipped one, and set `provider:` in the client config of the machines that use it.

## The server's folder

The server reads the files in `PROVIDERS_DIR` (`/providers` by default) and keeps only their `http` doors. It does not resolve `extends`, so a file there holds its doors itself. Mount the folder read-only.

A header's `{{secret "<name>"}}` reads `SECRET_<NAME>`, the name upper-cased, from the server's environment, or the file that `SECRET_<NAME>_FILE` names. Secrets are read once at startup and are never logged.

A file is skipped, with one `"msg":"provider invalid"` line naming the file and the problem, when it fails to load, when a secret it names cannot be read or holds a character a header cannot carry, or when one of its doors can serve an identity that a door of a file earlier by name serves. Two doors overlap when they match the same provider and either leaves `host` out or both name one host, so give each door a `host` when several serve one provider.

The client's `providers` folder has no effect on the server, and the server's folder none on the clients.
