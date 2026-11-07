# Provider files

A provider file describes one coding tool as data. It says how grelot tells the tool's shells apart, where its session id lives, how a notice wakes a session, and how its hook events are spelled. No host is named in grelot's code, so a new tool, or a change to a known one, needs only a new or replaced file.

The known hosts' files ship inside the client, listed under [`providers/`](../providers). Each host page under [`hosts/`](hosts/README.md) says which one it uses. Read this page to replace one or write one for another tool.

## Where the files are read

The client reads two layers, and a file in the second replaces the first's file of the same name whole:

1. The shipped files, built into the binary.
2. The `providers` folder beside `config.yaml`, in the config folder that [the client page](client.md#2-write-the-config-file) lists.

Only `*.yaml` files at the root of a folder count. A symlink is read as its target. A dangling one, or a folder that cannot be read, stops every command. A file that fails to load stops only the commands that need it, which print the problem and ask for `grelot provider check` on it.

The server reads a third folder, [its own](#the-servers-folder), for the `http` doors it pushes through.

## Check a file

```sh
grelot provider check ~/.config/grelot/providers/mytool.yaml
```

It exits 0 when the file is valid, and otherwise 1 with one line per problem, each naming the key and never quoting the file. It also renders every JSON template against sample values holding quotes, backslashes, newlines and a control character, and fails one whose output does not parse. A JSON template is one whose text starts with `{`, or an `http` body sent as a JSON content type.

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
    start: run `grelot watch --door background` in the background
hooks:
  session_start:
    event: SessionStart
    outputs:
      context: {text: "{{.Context}}"}
notes: docs/hosts/mytool.md
```

Values are read as written. A `${...}` reference is refused, so a file never pulls a value out of an agent's shell. An unknown key, or a key of another transport on a door, is refused by name.

| Key | Required | What it holds |
| --- | --- | --- |
| `name` | yes | The provider's name: lowercase letters, digits and `-`, 2 to 48 characters, starting with a letter or digit. It must equal the file name without `.yaml` |
| `extends` | no | Another provider this file [builds on](#extending-a-file) |
| `evidence` | no | Where the file's facts come from: `measured` (recorded from the tool itself), `documented` (from its documentation only) or `unverified` |
| `detect` | no | How the client recognizes the tool's shells |
| `session` | no | Where the tool keeps its session id |
| `doors` | no | How a notice wakes one of the tool's sessions |
| `hooks` | no | The hook events the tool offers, by role |
| `notes` | no | Free text, such as the page that explains the file |

### `detect`

`detect.env` lists environment variables the tool sets in the shells it starts. A command picks the provider that matches the most of them. When several tie, the one whose `session.process` names the nearest ancestor process wins, provided every tied file names one. Otherwise the command stops and asks for `--provider <name>` or `provider: <name>` in the client config, which always win.

A file with no `detect` is chosen only by name.

### `session`

| Key | What it holds |
| --- | --- |
| `env` | The variable that holds the session id in the agent's shell. With none, every identity command refuses, because the shell has no session to bind |
| `stdin` | The path of the session id in the JSON a hook reads on stdin, such as `session_id` |
| `hooks_without_env` | `true` for a tool that leaves `env` out of its hook processes, so a hook acts for the `stdin` session. Without it, a hook that finds no `env` belongs to another tool and does nothing. Needs `env` and `stdin` |
| `process` | Names of the tool's process when it runs one session at a time, such as `[mytool, mytool.exe]`. A new session in that process may get the hint to take over the previous one's identity. It also breaks a detection tie |
| `process_shared` | Arguments that mark one of those processes as serving many sessions at once, such as `[serve]`. A session under such a process is never offered another session's identity. Needs `process` |
| `record` | Where the tool keeps a record for each session, for a tool whose subagents, workflow steps or forks run hooks under their own session id while their shells carry the parent's |

`record` keys:

| Key | What it holds |
| --- | --- |
| `dir_env`, `dir_default` | The variable that names the tool's state folder, and the folder used when it is unset, where `~` is the home folder. One of them is required |
| `glob` | The record file's path under that folder, a template that reads only `{{.HookSession}}`, the hook's own session id |
| `parent`, `reason` | The JSON fields of the record that hold the parent session and why the session was made |
| `same_agent` | Reasons that mean the same agent going on in a new session, such as a rewind. Any other reason with a parent is a second agent beside the first |
| `same_agent_note` | A line printed once to a session `same_agent` matched, for what the user must do about it |

### `doors`

A door is one way a new-mail notice reaches a session. Each door has these keys, and then the keys of its transport:

| Key | Required | What it holds |
| --- | --- | --- |
| `name` | yes | The door's name, unique in the file, spelled like a provider name |
| `layer` | yes | `1` for an `http` door, `2` for the others |
| `transport` | yes | `http`, `exec`, `stdout` or `exit` |
| `start` | no | How an agent starts this door's watcher. The first layer 2 door's line is quoted to a session that is not woken |
| `asset` | no | A companion file for the host, a path inside the providers folder, such as `mytool/watch.json` |
| `notes` | no | Free text |

Every door sends one fixed line, with the notice's number and new-message count but no thread, path or sender:

```text
[grelot:42] grelot (automated, not the user): 2 new; run grelot inbox
```

#### Layer 1: `http`

The server sends a request for each notice, so a tool with an API needs no watcher process. The server reads these doors only from [its own folder](#the-servers-folder).

| Key | Required | What it holds |
| --- | --- | --- |
| `url` | yes | A template with a literal scheme, host and port, and actions only in the path and query. No credentials in the URL |
| `body` | yes | The request body, a template |
| `headers` | no | Header names and value templates. Only a header value may call `secret` |
| `match` | no | Which identities the door serves: `provider` (this file's name when left out) and an optional `host`, the label in each client's `host:` |
| `dedupe_window` | when a template reads `.DedupeKey` | How long the receiver drops a repeated key, at least `60s` |
| `gone` | no | 4xx statuses that mean the receiver no longer has that session. 401 and 403 are not allowed here |

The request is a `POST` with a 10 second timeout and no redirects. The answer decides what comes next:

- Any 2xx is delivered.
- A 429, a 5xx, a timeout or a network error is retried after the receiver's `Retry-After`, held between 5 and 60 seconds, or after 15 seconds without one. Every retry carries the same `.DedupeKey`.
- A status in `gone`, or any other status but 401 and 403, turns push off for that identity.
- A 401 or a 403 means the door's credential is wrong. It turns the door off for every identity it serves, with one `"msg":"push refused"` line, until the server restarts. Registering again or reviving does not turn it back on.

An identity whose push is off is woken by its own watcher. After any other give-up, registering again or reviving gives it push back.

#### Layer 2: `exec`, `stdout` and `exit`

The client's watcher, `grelot watch`, waits on the server and delivers each notice through one of these doors. `grelot watch --door <name>` picks one. With no `--door`, it runs the file's first layer 2 door.

- **`exec`** runs `argv` for each notice, with no shell and no input or output, and goes on watching. `argv` is required, one template per argument, its first element the program. A run has 10 seconds. After a non-zero exit or a timeout the notice comes again later. The third non-zero exit in a row stops the watcher with exit code 1, and timeouts do not count.
- **`stdout`** prints `line` for each notice and goes on watching, for a tool that reads a background command's output as it comes.
- **`exit`** prints `line` and exits 0, for a tool that wakes a session when a background command ends. The session starts the watcher again.

`stdout` and `exit` doors take these keys:

| Key | Default | What it holds |
| --- | --- | --- |
| `line` | `{{.Line}}` | What is printed for a notice |
| `end` | `line` | What is printed when the watcher stops for good, with `.Line` the end line |
| `end_exit` | `1` | The exit code after `end`, 0 to 255 |
| `hold` | none | How long one run waits before it prints `idle`, for a tool that polls the command. An `exit` door then exits 0, and a `stdout` door goes on |
| `idle` | none | What is printed when `hold` lapses. `hold` and `idle` go together |

The end line says why the watcher stopped:

```text
[grelot:end] grelot (automated, not the user): this watcher stopped (ended); run grelot me
```

A watcher can also run with no door from a file: `grelot watch --transport stdout`, `--transport exit`, or `--transport exec -- <command> <args>`.

### `hooks`

A tool's hook file wires each event to `grelot hook <provider> <role>`, and the provider file says what that role reads and prints. The hook files for the known hosts sit in a folder of the provider's name under [`providers/`](../providers). A role the file leaves out does nothing, so a hook line that names it is harmless.

| Role | When it runs | What it does |
| --- | --- | --- |
| `session_start` | A session starts | Prints the identity's status, or for an unregistered session the line that asks it to register, as `context` |
| `prompt` | A turn starts | Prints the unread count, an hour-old description's reminder and a stopped watcher's start line as `context`, never a message. A turn started by a notice line already read ends through `block` before the model runs |
| `stop` | A turn ends | Holds the turn open with `block` when mail came, or for a session that has not registered. Prints a line for the user as `notice` |
| `turn_end` | A turn ends some other way than `stop`, such as an error or an interruption | Tells the server the turn ended. Prints nothing |
| `session_end` | A session closes | Tells the server, which ends the session's delegates and lets the identity go dormant within the hour. Prints nothing |
| `subagent_stop` | A subagent ends | Ends the delegate that subagent's own session held. Prints nothing |

Every role takes these keys:

| Key | What it holds |
| --- | --- |
| `event` | The tool's own name for the event. Required |
| `when` | Stdin paths and the values each must hold for the role to act, such as `{reason: [delete]}` |
| `budget` | How long the role may wait on the server, for a tool that allows less than the default 10 seconds. A role that gets no answer within it prints that it could not check the session and holds nothing. Give the tool's hook a timeout of at least the budget plus the time the tool takes to start the hook, which is up to about 10 seconds on a slow Windows machine. A hook the tool stops at its timeout holds nothing either, and the next command's status line shows the state |
| `once` | `true` prints the role's output only the first time in a session |

And these, by role:

| Key | Roles | What it holds |
| --- | --- | --- |
| `stdin.agent_id` | all | The stdin path of a subagent's id. A hook whose input holds one belongs to a subagent and never acts for its parent |
| `stdin.prompt` | `prompt` | The stdin path of the prompt text |
| `signal` | `prompt` | An RE2 pattern over the whole prompt, with a named group `(?P<line>...)`, for a tool that delivers a background line inside its own wrapper |
| `source` | `session_start` | `path`, the stdin path of the start source, and the values among it that mean a take-over (`take_over`, such as `clear`) or a fork (`fork`). A take-over start may get the hint for the identity of the previous session in that process |
| `take_over_every_start` | `session_start` | `true` for a tool that reports no source and starts a session in its process only for a chat with no history. Every start but a fork may then get the hint. Not with `source.take_over` |
| `once_per_turn` | `stop` | `true` for a tool that runs its stop hooks at most once per turn, so the hold ends the turn whether or not it blocks |
| `stdin.session` | `subagent_stop` | The stdin path of the session the subagent's own shell carries, for a tool whose subagent hooks name the parent at `session.stdin`. `session.stdin` when left out |

Outputs, under `outputs.context`, `outputs.block` and `outputs.notice`:

| Key | Default | What it holds |
| --- | --- | --- |
| `text` | required | What is printed, a template reading `.Context` in `context` and `.Reason` in `block` and `notice`, such as `'{"decision":"block","reason":{{json .Reason}}}'` |
| `stream` | `stdout` | `stdout` or `stderr` |
| `exit` | `0` | The exit code after printing, 0 to 255 |

`session_start` takes `context`, `prompt` takes `context` and `block`, and `stop` takes `block` and `notice`. A role whose output kind is left out prints nothing for it, so a `stop` with no `block` never holds a turn.

## Templates

Door keys and hook outputs are Go [`text/template`](https://pkg.go.dev/text/template) templates. Each is parsed and rendered once against sample values when the file loads, so a misspelled field fails the load rather than the first notice.

| Field | What it holds |
| --- | --- |
| `.Line` | The fixed notice line, or the end line in `end` |
| `.Seq` | The notice's number |
| `.Session` | The session id the identity is bound to, without the provider name |
| `.Identity` | The identity's name |
| `.DedupeKey` | A key that is the same for every retry of one notice, in `http` doors |
| `.Context`, `.Reason` | A hook output's text |

Read fields one at a time, as `.Line`. Reading the whole data, as `{{.}}` or `$`, is refused. Inside `with` or `range`, read the data as `$.Line`.

| Function | What it does |
| --- | --- |
| `json` | Writes a value as a JSON string or number, quotes included: `{"text":{{json .Line}}}`. Put every value placed in JSON through it |
| `path` | Escapes a value for one URL path segment: `/sessions/{{path .Session}}` |
| `secret` | `{{secret "<name>"}}`, in an `http` header value only, first in its pipeline, with a name of lowercase letters, digits and `_` |

## Extending a file

A file with `extends: <name>` inherits every key of that file but `detect`, so it is chosen only by name. A door it lists replaces the parent's door of the same name in place, and its other doors are added. Any other key it sets, `evidence`, `notes`, `session` or `hooks`, replaces the parent's whole. Extending is one level only, and a file that extends an invalid or missing one is invalid itself.

To change one thing about a shipped host, put a file of its name in your `providers` folder. To add a variant beside it, give the new file another name, `extends` the shipped one, and set `provider:` in the client config of the machines that use it.

## The server's folder

The server reads the files in `PROVIDERS_DIR`, by default `/providers`, and keeps only their `http` doors. It does not resolve `extends`, so a file there holds its doors itself. Mount the folder read-only.

A header's `{{secret "<name>"}}` reads `SECRET_<NAME>`, the name upper-cased, from the server's environment, or the file that `SECRET_<NAME>_FILE` names. Secrets are read once at startup and are never logged.

The server skips a file, logging `"msg":"provider invalid"` with the file and the problem, when:

- The file fails to load.
- A secret it names cannot be read, or holds a character a header cannot carry.
- One of its doors can serve an identity that a door of a file earlier by name serves.

Two doors overlap when they match the same provider and either leaves `host` out or both name one host, so give each door a `host` when several serve one provider.
