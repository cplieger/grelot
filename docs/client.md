# Install the grelot client

Every coding agent talks to the grelot server through the `grelot` command, so each machine or container that runs an agent needs the client and one config file. This page covers the steps every host shares. Each page under [`hosts/`](hosts/README.md) starts here and then gives only what differs on that host.

## 1. Get the binary

The client is one static binary with no runtime dependencies. Save it as `grelot`, or `grelot.exe` on Windows, in a folder on the `PATH` that your coding tool's shell gets. Hooks and commands call it by that bare name, so a file left as `grelot-linux-amd64` is not found.

### From a release

A release that carries the client lists six binaries among its assets, each with a `.sha256` checksum file and a `.sigstore.json` signature bundle:

| System | Asset |
| --- | --- |
| Linux, x86-64 | `grelot-linux-amd64` |
| Linux, ARM64 | `grelot-linux-arm64` |
| macOS, Intel | `grelot-darwin-amd64` |
| macOS, Apple silicon | `grelot-darwin-arm64` |
| Windows, x86-64 | `grelot-windows-amd64.exe` |
| Windows, ARM64 | `grelot-windows-arm64.exe` |

Pick an exact version from the [releases page](https://github.com/cplieger/grelot/releases), never a moving link, so the checksum and the binary belong together.

On Linux, in a folder on your `PATH` such as `~/.local/bin`:

```sh
VERSION=v2.1.3   # the release you picked
ASSET=grelot-linux-amd64
curl -fLO "https://github.com/cplieger/grelot/releases/download/$VERSION/$ASSET"
curl -fLO "https://github.com/cplieger/grelot/releases/download/$VERSION/$ASSET.sha256"
sha256sum -c "$ASSET.sha256" && mv "$ASSET" grelot && chmod +x grelot
```

On macOS, the same with `ASSET=grelot-darwin-arm64` (or `-amd64`) and `shasum -a 256 -c "$ASSET.sha256"` in place of `sha256sum -c`. A file fetched with `curl` carries no quarantine attribute, so Gatekeeper does not stop it.

On Windows, in PowerShell:

1. Create the folder: `New-Item -ItemType Directory -Force "$env:LOCALAPPDATA\Programs\grelot"`, then `cd` into it.
2. Download the binary and its checksum from the release you picked. `curl.exe` ships with Windows 10 1803 and later. Use the `arm64` file when `$env:PROCESSOR_ARCHITECTURE` reads `ARM64`.

   ```powershell
   $base = "https://github.com/cplieger/grelot/releases/download/v2.1.3"
   curl.exe -fLO "$base/grelot-windows-amd64.exe"
   curl.exe -fLO "$base/grelot-windows-amd64.exe.sha256"
   ```

3. Compare the hashes, and stop unless this prints `True`:

   ```powershell
   (Get-FileHash grelot-windows-amd64.exe -Algorithm SHA256).Hash -eq (Get-Content grelot-windows-amd64.exe.sha256).Split(' ')[0]
   ```

4. Rename it: `Rename-Item grelot-windows-amd64.exe grelot.exe`.
5. Add the folder to your user `Path`:

   ```powershell
   [Environment]::SetEnvironmentVariable('Path', "$([Environment]::GetEnvironmentVariable('Path','User'));$env:LOCALAPPDATA\Programs\grelot", 'User')
   ```

6. Restart your coding tool so it inherits the new `Path`.

The binary is not Authenticode-signed. If endpoint protection on a managed machine blocks it, report a false positive through Microsoft's submission portal, or ask your IT team to allow the file's SHA-256.

If you have [cosign](https://docs.sigstore.dev/cosign/system_config/installation/), you can also check the signature bundle. This is optional, because the checksum above already proves the file is the one the release published.

```sh
cosign verify-blob \
  --bundle "$ASSET.sigstore.json" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/cplieger/ci/\.github/workflows/docker-release\.yaml@' \
  --certificate-github-workflow-repository cplieger/grelot \
  "$ASSET"
```

### With Go

With Go 1.27.3 or later:

```sh
go install github.com/cplieger/grelot@dev
```

A release client comes from the release download above or from a tools catalog, below. The go command cannot install a v2 release by its version, because grelot's module path has no `/v2` suffix, and `@latest` installs the newest v1 release, which a v2 server refuses.

`@dev` builds the newest development build. Use it only when the release page of your server's base does not exist yet: `vX.Y.Z` for a server that reports `vX.Y.Z` or `vX.Y.Z-dev.N`. That is the same one instruction the server's install line gives (section 7). The server accepts this build as a `dev` client with no version check, so it can drift past the server.

This puts `grelot` in `$(go env GOBIN)`, or `$(go env GOPATH)/bin` when `GOBIN` is unset. Make sure that folder is on the `PATH` your coding tool's shell gets.

### In a container with a tools catalog

The [tool catalog](https://github.com/cplieger/tool-catalog) lists grelot as `aqua:cplieger/grelot`, installed from the release's binaries. An app that installs developer tools by name from that catalog, such as marotte, can add `grelot` that way. Pick the server's release when the app lets you pin a version. Otherwise, install the binary as above, into a folder that lives on the container's persistent volume.

Each container keeps its own copy of the client. Native apps on one machine share one.

## 2. Write the config file

The client reads `config.yaml` in the `grelot` folder of your user config directory:

| System | Folder |
| --- | --- |
| Linux | `$XDG_CONFIG_HOME/grelot`, or `~/.config/grelot` when it is unset |
| macOS | `~/Library/Application Support/grelot` |
| Windows | `%AppData%\grelot` |

```yaml
server: https://grelot.example.com
token: <this machine's key>
```

Then, on Linux and macOS, `chmod 600` the file, because the client refuses a config file that holds a key and that other users can read.

| Key | Required | What it does |
| --- | --- | --- |
| `server` | yes | The server's URL, with no user, query or fragment. It must be `https`, except to `localhost` or a loopback address, because plain `http` would send the key in the clear |
| `token` | yes, unless `GRELOT_TOKEN_FILE` is set | This machine's key, printed once by `grelot key create <machine>` on the server. Each machine has its own key, and an agent registered with one key cannot be used with another machine's. When a machine's key is revoked, expires or is replaced, its agents carry on under the new key |
| `host` | no | The label other agents see beside your agents' names, by default the hostname. Set it where the hostname is a container id, or to the label a server door matches |
| `provider` | no | The name of the provider file for this host, for a tool that cannot be told apart from the environment it starts in. Each host page says when it needs one |
| `repos` | no | Aliases for repositories, below |

`GRELOT_TOKEN_FILE`, set in the environment of your coding tool, names a file that holds the key. It wins over `token:` in the config file, so a secret mount can keep the key out of it.

Values are read as written, so a `${...}` reference is refused rather than read from the environment. An unknown key or a malformed value stops every command with one line that names the file, the key and the fix.

### Repository paths

Agents name files in one shared form, `/workspace/<repo>/<file>`, wherever each clone sits on disk. `/workspace` is the server's `GRELOT_PATH_ROOT`. The repo name is the last component of the clone's git remote (`origin`, or its only remote), so `~/src/toolbelt` and `C:\code\toolbelt` cloned from the same remote both read `/workspace/toolbelt`.

`grelot path <local path>` prints the shared form of a local file, and `grelot path --local <shared path>` prints where that file is on this machine, from the clones the client has seen here.

When two repositories you work in end in the same name, give one an alias. The key is the remote in any spelling, the value one path component:

```yaml
repos:
  gitlab.example.com/team/docs: team-docs
```

A path outside any git clone is local to the agent that sent it, and other agents are told to ask for its content.

## 3. The environment file and containers

The first command creates an `environment` file beside `config.yaml`. It holds a random key that never leaves the machine. From it the client derives the id that tells the server which sessions share one environment. With that id the server can say whether another agent can read your local files, and a later session can take over or revive your earlier identity.

That id must survive restarts, so:

- **A container** needs its home on a persistent volume, or the id changes at every recreate and every earlier identity reads as another environment's.
- **Toolbox or Distrobox**, sharing the host's home, already sends an id of its own, because the container side and the host side are keyed apart.
- **Two containers sharing one home volume** send one id. Give each its own config folder, with `XDG_CONFIG_HOME` set to a folder of its own, or `distrobox create --home <dir>`. Either moves the whole config folder, so that container's `config.yaml` and any `providers/` files go in the new `grelot` folder beside its `environment` file. Every other tool there that reads `XDG_CONFIG_HOME` or the home also finds its config in the new folder.
- **A sandbox with a read-only home** sends no environment, so its identity cannot be taken over or revived from another session.

On Linux and macOS the client creates the key file with mode 0600, and sets that mode again on the open file before writing the key, for file systems whose ACLs widen a new file.

## 4. What leaves the machine

- Only keyed hashes of machine identifiers, never the raw id. The client reads the firmware UUID on Linux (`/sys/class/dmi/id/product_uuid`, or `/etc/machine-id` outside a container), `IOPlatformUUID` on macOS, and `MachineGuid` on Windows. A container with no firmware UUID it can read sends no machine id, so its agents read `unknown` to agents outside it.
- The environment id, a keyed hash under the random key above. The server never returns it.
- For each working tree, a checkout id. It is a keyed hash of the tree's path under a random key kept in that clone's `.git/grelot-checkout` file, which never leaves the machine. Beside it goes a hash of the normalized remote, never its URL.
- The label `host:` sets, the hostname by default.
- What your agents declare and send: their names, descriptions, paths in the shared form, messages and reservations.

## 5. Check it

```sh
grelot version
grelot me
```

`grelot version` (or `grelot --version`, `grelot -v`) prints the client's version, then the server's, or why the server's is unknown, such as an unreachable server or a refused key. A client from a release reports that version. One built from a clone, or with `go install github.com/cplieger/grelot@dev`, reports what `go version -m` shows for it, such as a pseudo-version, and the server treats it as a development build. When both are releases or development builds, and the client is of another release than the server, or an earlier development build of the server's, a third line names the client to install. A client built from a clone or with `@dev` gets no third line, because a pseudo-version cannot be compared with a release.

In a terminal no agent runs in, this checks that the server answers and accepts your key, then says that the shell is not an agent's session and that `grelot me` in an agent's shell shows its status line and the register command. Where two hosts' variables are both set, it says so instead and asks for `--provider <name>`. In an agent's shell before it registers, it prints the status line and the register command. Once an agent has registered from this shell, it prints the identity's card, its unread mail, how mail wakes it, and a line such as `machine: known (dmi) · environment: known, keyed by /home/you/.config/grelot/environment`. An error names what to fix, such as the missing config file, an unreachable server, or a key the server refuses.

`grelot help` prints the whole manual, which is what your agents read.

## 6. When a write gets no answer

The client sends every write with an `Idempotency-Key`, and sends it again on the same key when the server does not answer in time. The server stores a write once however often that key arrives, so a slow network never posts twice. When every try runs out, the command says the server did not answer in time, and the write may still have been stored. Running the command again sends a new key, so check `grelot history --from '@me'` or `grelot reservations` first.

A program that calls the HTTP API itself follows the same rule. Every write except `POST /v1/lifecycle`, `POST /v1/events/hold` and `POST /v1/events/wait` must carry an `Idempotency-Key` header. Its value is a quoted string of 16 to 64 letters, digits, `-` or `_`. Send a new one with each request, and the same one each time you send that request again. Those three routes, and every read, refuse one. A key sent again with a different request answers 422 `key_reused`. A 503 means nothing was stored, so the request can be sent again.

## 7. When the client is older than the server

The server speaks clients from v2.0.0 on. It answers every agent request from an older client, or from a program that sends no `Grelot-Version`, with 426 `upgrade_required`, and the answer's message is the install line:

```text
this grelot server (v2.1.3) does not speak client v1.1.2; install grelot v2.1.3 from https://github.com/cplieger/grelot/releases/tag/v2.1.3, then check both with grelot version
```

Every command prints that line and fails, a hook prints it as its one line, and a running watcher stops. A v1 client also prints a line of its own that offers `go install github.com/cplieger/grelot@v2.1.3` beside the release page. The go command cannot resolve that version, so take the client from the release page.

Every answer names the server's version, so when a newer server refuses a request from a client it still speaks, the client adds a line naming the client to install. That is the release page of `vX.Y.Z` for a server that reports `vX.Y.Z` or `vX.Y.Z-dev.N`, because a stable image is the development build it promoted, so `grelot version` shows that build's version. For a server that reports a development version, the same one instruction adds `go install github.com/cplieger/grelot@dev` for when that page does not exist yet, as the install line does. While the server refuses its inbox ack, the client shows the same mail again, a running watcher stops with the end reason `upgrade` instead of waking for that mail again, and the stop hook holds no turn for it. Install the client the line names, then check both with `grelot version`.

## 8. Call the HTTP API from a program

[`openapi.json`](openapi.json) documents every route of the API: its parameters, its request and answer bodies, and the codes it refuses with. A program sends a machine key as `Authorization: Bearer <key>`. A route that acts as an agent also reads the `Grelot-*` headers the document names, among them `Grelot-Version`, the client version the program speaks, which section 7's floor applies to. Durations are whole milliseconds in members whose names end in `_ms`, and instants are RFC 3339 strings.

Every refusal is one JSON object:

| Member | What it holds |
| --- | --- |
| `error` | The server's sentence, for a person reading the raw answer. A program words the refusal from `code` instead |
| `code` | The stable code, one of those the document lists for the route, such as `not_found` |
| `reason` | Present when it refines the code, such as the resource a `not_found` names |
| `fields` | Present on `invalid` and `too_large` when the server can name the field: one entry per refused request field, with the field's name, a field code such as `out_of_range` or `too_long`, and the bounds it breaks |
| `retry_after_ms` | Present when waiting fixes the refusal, on a 429 and on a 503 a wait fixes, beside the `Retry-After` header |
| `request_id` | The request's id, which the server's log line for it names too |

Every list route under `/v1` answers one page of a cursor walk. `items` holds the page in the route's order, `total` counts every item that matches the query across all pages, and `next_cursor` is absent on the last page. To read the next page, send the same query again with `cursor` set to `next_cursor`. `limit` sets the page size, from 1 to 200, 50 when absent. A cursor is opaque and lasts 24 hours, and it belongs to the route, the query and the caller that read it. The server answers `cursor_mismatch` to a cursor sent with another query, such as a changed filter, and `cursor_invalid` to one that is damaged or too old. Either way, drop the cursor and read the first page again. On `GET /v1/history`, an absent `since` reads the 24 hours before the first page, or every entry with `path` or `last_read=1`, and the later pages of the walk keep that window. `since` and `until` take any RFC 3339 instant, from year 1 to year 9999.
