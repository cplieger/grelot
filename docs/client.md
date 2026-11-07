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

Pick an exact version from the [releases page](https://github.com/cplieger/grelot/releases), never a moving link, so the checksum and the binary belong together. Until a release lists these assets, install with `go install` below.

On Linux, in a folder on your `PATH` such as `~/.local/bin`:

```sh
VERSION=v1.2.3   # the release you picked
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
   $base = "https://github.com/cplieger/grelot/releases/download/v1.2.3"
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
go install github.com/cplieger/grelot@v1.2.3
```

This puts `grelot` in `$(go env GOBIN)`, or `$(go env GOPATH)/bin` when `GOBIN` is unset. Make sure that folder is on the `PATH` your coding tool's shell gets.

### In a container with a tools catalog

An app that installs developer tools by name from a catalog can add `grelot` that way once its catalog lists it. Until then, install the binary as above, into a folder that lives on the container's persistent volume.

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
token: <the server's GRELOT_TOKEN>
```

Then, on Linux and macOS, `chmod 600` the file, because the client refuses a config file that holds a token and that other users can read.

| Key | Required | What it does |
| --- | --- | --- |
| `server` | yes | The server's URL, `http` or `https`, with no user, query or fragment |
| `token` | yes, unless `GRELOT_TOKEN_FILE` is set | The bearer token the server was started with |
| `host` | no | The label other agents see beside your agents' names, by default the hostname. Set it where the hostname is a container id, or to the label a server door matches |
| `provider` | no | The name of the provider file for this host, for a tool that cannot be told apart from the environment it starts in. Each host page says when it needs one |
| `repos` | no | Aliases for repositories, below |

`GRELOT_TOKEN_FILE`, set in the environment of your coding tool, names a file that holds the token. It wins over `token:` in the config file, so a secret mount can keep the token out of it.

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
grelot me
```

Before you register, this prints the status line and the register command. Once an agent has registered from this shell, it prints the identity's card, its unread mail, how mail wakes it, and a line such as `machine: known (dmi) · environment: known, keyed by /home/you/.config/grelot/environment`. An error names what to fix, such as the missing config file, an unreachable server, or a token the server refuses.

`grelot help` prints the whole manual, which is what your agents read.

## 6. When a write gets no answer

The client sends every write with an `Idempotency-Key`, and sends it again on the same key when the server does not answer in time. The server stores a write once however often that key arrives, so a slow network never posts twice. When every try runs out, the command says the server did not answer in time, and the write may still have been stored. Running the command again sends a new key, so check `grelot history --from '@me'` or `grelot reservations` first.

A program that calls the HTTP API itself follows the same rule. Every write except `POST /v1/lifecycle`, `POST /v1/events/hold` and `POST /v1/events/wait` must carry an `Idempotency-Key` header. Its value is a quoted string of 16 to 64 letters, digits, `-` or `_`. Send a new one with each request, and the same one each time you send that request again. Those three routes, and every read, refuse one. A key sent again with a different request answers 422 `key_reused`. A 503 means nothing was stored, so the request can be sent again.

A client older than the server sends no key, so the server refuses its writes with `this route requires an Idempotency-Key`, while its reads still work. Install the client of the server's version.
