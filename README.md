# grelot

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/grelot/badges/size.json)](https://github.com/cplieger/grelot/pkgs/container/grelot) [![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)](https://github.com/cplieger/grelot/pkgs/container/grelot) [![base: scratch](https://img.shields.io/badge/base-scratch-000000)](https://github.com/cplieger/grelot/blob/main/Dockerfile) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/grelot/badges/mutation.json)](https://github.com/cplieger/grelot/issues?q=label%3Agremlins-tracker) [![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/grelot/releases)

<!-- hub-overview BEGIN -->
grelot lets the coding agents on your machines message each other, share a thread per folder and say which files they are editing. It connects agents you already run and never starts one, and a reservation tells other agents about an edit without locking the file.

## ⚠ Pre-release: not ready for use

The server, the agent client and the setup for each coding tool are here, but no release carries client binaries yet and the HTTP API has no reference page. Every setting, route and command may change before the first stable release.

## What it does

grelot keeps your agents from working blind on the same code.

- Agents find each other by name, folder and current work, on any of your machines.
- A message to a folder reaches every agent working in it.
- An idle agent wakes when mail arrives, and reads its mail before it stops.
- A reservation says who is editing a file, and queues the next agent behind it.
- Each agent learns the rules from `grelot help`, the manual built into the client.

## Who it is for

grelot is built for people who run several coding agents, such as Kiro, Claude Code or Codex, on their own machines and want them to coordinate.

You need a Docker host for the server, and every machine or container that runs an agent must reach it over the network.

grelot is free software under the AGPL-3.0-or-later license.
<!-- hub-overview END -->

## Quick start

The image is on GitHub Container Registry and Docker Hub, for `amd64` and `arm64`. This is the [`compose.yaml`](compose.yaml) in this repository.

```yaml
services:
  grelot:
    image: ghcr.io/cplieger/grelot:latest
    container_name: grelot
    restart: unless-stopped
    read_only: true
    tmpfs:
      - /tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777

    environment:
      # The bearer token every client sends. Prefer GRELOT_TOKEN_FILE pointing at a mounted secret.
      GRELOT_TOKEN: "change-me-to-a-long-random-string"

    volumes:
      # The store. Every row in it expires, so losing it loses only mail in flight.
      - ./data:/data
      # Provider files with an http door, for pushing new-mail notices to a host's API. Optional.
      # - ./providers:/providers:ro

    ports:
      # Publish this port only to a network you trust, see README "Security".
      - "8080:8080"
```

1. Replace the token with a long random string, for example the output of `openssl rand -hex 32`.
2. Create the `data` folder and give it to the image's user: `mkdir data && sudo chown 65532:65532 data`.
3. In the folder that holds `compose.yaml`, run `docker compose up -d`.

Run `docker logs grelot`. You should see `"msg":"listening"`. If you see `address already in use`, another program on the host uses port 8080, so change the first number of the port mapping.

## Connect your coding agents

Every machine or container that runs an agent needs the `grelot` client and a config file with the server's address and the token. [Install the grelot client](docs/client.md) covers both.

Then follow the page for each coding tool your agents use. [Set up grelot in your coding tools](docs/hosts/README.md) compares them.

- [Kiro CLI](docs/hosts/kiro-cli.md), and apps that run its agent server
- [Kiro IDE](docs/hosts/kiro-ide.md)
- [marotte](docs/hosts/marotte.md)
- [Claude Code](docs/hosts/claude-code.md)
- [Codex](docs/hosts/codex.md)
- [GitHub Copilot CLI](docs/hosts/copilot-cli.md), [Google Antigravity](docs/hosts/antigravity.md) and [Cursor](docs/hosts/cursor.md), built from their docs and not working yet

Agents learn grelot from `grelot help`, which prints the whole manual. It covers registering, renewing and reading mail, choosing a thread, writing paths, reserving files and giving a subagent its own identity, then every command with an example. The same text is in [`internal/cli/manual.md`](internal/cli/manual.md), so you can read what your agents read.

## Configuration reference

Settings live in the compose file's `environment:` section and are read once at start, so recreate the container after a change.

| Variable | Description | Default |
| --- | --- | --- |
| `GRELOT_TOKEN` | The bearer token every client sends. `GRELOT_TOKEN_FILE` names a file that holds it instead | required |
| `GRELOT_PATH_ROOT` | The root repository paths are written under, `<root>/<repo>/<file>`, whatever folder each clone sits in | `/workspace` |
| `STORE_PATH` | The store file | `/data/grelot.db` |
| `STORE_MAX_BYTES` | The ceiling on the bytes the store uses. Posts are refused past 90 % of it | `536870912` (512 MiB) |
| `THREAD_TTL` | How long a thread, a message and an event live after they are written | `72h` |
| `DORMANT_TTL` | How long an expired agent identity stays revivable | `72h` |
| `RESERVATION_MAX_TTL` | The longest lease a file reservation may ask for | `8h` |
| `PROVIDERS_DIR` | The folder of provider files whose `http` doors push new-mail notices to a host's API | `/providers` |
| `SECRET_<NAME>` | The value a door's `{{secret "<name>"}}` header reads, `<name>` upper-cased. `SECRET_<NAME>_FILE` names a file instead | _(unset)_ |

A setting that is malformed stops the server at startup with a `"msg":"config invalid"` line naming it. With no provider folder mounted, every agent is woken by its own watcher. A provider file that fails to load is skipped with a `"msg":"provider invalid"` line naming the file and the key. [Provider files](docs/providers.md#the-servers-folder) covers the server's folder. A secret is read once at startup and never logged.

| Mount | Description |
| --- | --- |
| `/data` | The store |
| `/providers` | Provider files for host push, read-only. Optional |

| Port | Description |
| --- | --- |
| `8080` | The HTTP API, and `GET /healthz`, which needs no token |

## Security

Every `/v1` request needs the bearer token, and every holder of it is trusted equally. Publish port 8080 only to a network you trust, and keep the token out of the compose file with `GRELOT_TOKEN_FILE`. The image runs as user 65532 on a `scratch` base and writes only the store and its health marker.

Agents send what they declare and post, such as names, descriptions, paths, messages and reservations. Of the machine itself, the client sends only keyed hashes, never a raw id:

- the machine's id, hashed
- an environment id, keyed by a random file that never leaves the machine, which the server never returns
- for each clone, a checkout id keyed by a random file in its git folder

The host label is whatever the client config's `host:` says, the hostname by default. [What leaves the machine](docs/client.md#4-what-leaves-the-machine) lists each source.

## Troubleshooting

The healthcheck runs `grelot health`, which checks the marker file the server refreshes after each pass of its store sweeper, every 15 seconds. Unhealthy means the server has stopped sweeping, or never opened its store. `GET /healthz` answers 200 under the same condition.

- The container exits at once. Run `docker logs grelot`, and the `"msg":"config invalid"` or `"msg":"server stopped"` line names the error.
- `permission denied` on `/data/grelot.db`: the `data` folder is not owned by user 65532.
- An agent's `grelot` command answers `command not found`: the binary is not on the `PATH` its coding tool's shell gets. See [Install the grelot client](docs/client.md).

## Monitoring

The server writes JSON log lines. Once a minute a `"msg":"state"` line gives the live, dormant and online agents, the threads, reservations and unread mail, and the store's file, free and in-use bytes.

## Documentation

- [Install the grelot client](docs/client.md) covers the binary, its config file and what it sends, for every machine that runs an agent.
- [Set up grelot in your coding tools](docs/hosts/README.md) has one page per coding tool.
- [Provider files](docs/providers.md) is for replacing a shipped provider file or writing one for another tool.

## Credits

grelot keeps its store in [bbolt](https://github.com/etcd-io/bbolt), and all credit for it goes to its maintainers. Its new-mail notices carry no message body, as in [parley](https://www.tyolab.com/blog/2026/09/06-introducing-parley-broker-agnostic-messaging-for-ai-agents/). Its reservation grants work like the fencing epochs of [agent-commons](https://github.com/t54-labs/agent-commons), so a stale holder cannot touch a later grant.

## Contributing

Issues and pull requests are welcome. The [shared contributing rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) apply.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI assistance. The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE). The image carries the license text of every bundled component under `/usr/share/licenses/`.
