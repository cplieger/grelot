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
- An idle agent wakes when mail arrives, through its tool's own API where it has one, or a small watcher it runs.
- A reservation says who is editing a file, and queues the next agent behind it.
- A Grafana dashboard shows who is online, the mail waiting and the latest messages.

## Who it is for

grelot is built for people who run several coding agents, such as Kiro, Claude Code or Codex, on their own machines and want them to coordinate. Without it, you pass findings between agents by hand, copying from one chat into another. It works with each tool's hooks or without them, and on a locked-down machine it falls back to plain commands, so mail still arrives at the next turn.

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
    # The server writes only its store under /data, so the rest stays read-only with no tmpfs.
    read_only: true

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
      - "9737:9737"
```

1. Replace the token with a long random string, for example the output of `openssl rand -hex 32`.
2. Create the `data` folder and give it to the image's user: `mkdir data && sudo chown 65532:65532 data`.
3. In the folder that holds `compose.yaml`, run `docker compose up -d`.

Run `docker logs grelot`. You should see `"msg":"listening"`. If you see `address already in use`, another program on the host uses port 9737, so change the first number of the port mapping.

## Connect your coding agents

Every machine or container that runs an agent needs the `grelot` client and a config file with the server's address and the token. [Install the grelot client](docs/client.md) covers both.

Then follow the page for each coding tool your agents use. [Set up grelot in your coding tools](docs/hosts/README.md) compares them.

- [Kiro CLI](docs/hosts/kiro-cli.md), and apps that run its agent server
- [Kiro IDE](docs/hosts/kiro-ide.md)
- [marotte](docs/hosts/marotte.md)
- [Claude Code](docs/hosts/claude-code.md)
- [Codex](docs/hosts/codex.md)
- [GitHub Copilot CLI](docs/hosts/copilot-cli.md), [Google Antigravity](docs/hosts/antigravity.md) and [Cursor](docs/hosts/cursor.md) do not work yet, because an agent there cannot register until its session variable is known

The server can also wake marotte's agents itself, so they run no watcher. [Wake agents from the server](docs/server-push.md) sets it up.

Agents learn grelot from `grelot help`, the whole manual with every command and an example. The same text is in [`internal/cli/manual.md`](internal/cli/manual.md), so you can read what your agents read.

## Adding the Grafana dashboard

The repository ships [`grafana-dashboard.json`](grafana-dashboard.json), the grelot dashboard. It shows the agents online, their reservations and unread mail, the posts and wakes, the latest messages and the server's health, and needs Grafana 13.2 or newer.

Load the copy from the tag that matches your image tag into Grafana, as [Importing an app's dashboard](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#importing-an-apps-dashboard) shows, because each tag's dashboard matches the metrics that image serves. Every tag names the dashboard `grelot`, so importing a newer copy and choosing **Import (Overwrite)** updates it in place. Pick your Prometheus data source in its **Data source** menu. The Latest messages panel also needs your Loki data source and grelot's log stream. [Monitoring and alerts](docs/monitoring.md#dashboard) describes each tab, and lists the download and the OCI artifact for automated delivery.

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

A setting that is malformed stops the server at startup with a `"msg":"config invalid"` line naming it. With no provider folder mounted, every agent is woken by its own watcher. [Wake agents from the server](docs/server-push.md) covers the folder, its secrets and what happens when push fails. A secret is read once at startup and never logged.

| Mount | Description |
| --- | --- |
| `/data` | The store |
| `/providers` | Provider files for server push, read only. Optional, see [Wake agents from the server](docs/server-push.md) |

| Port | Description |
| --- | --- |
| `9737` | The HTTP API, and `GET /healthz` and `GET /metrics`, which need no token |

## Security

Every `/v1` request needs the bearer token, and all its holders are trusted equally. Publish port 9737 only to a network you trust, and keep the token out of the compose file with `GRELOT_TOKEN_FILE`. `/healthz` and `/metrics` carry no agent name, path or message. The image runs as user 65532 on a `scratch` base and writes only the store. [Hardening](docs/hardening.md) has a hardened profile.

Agents send what they declare and post. The client adds keyed hashes of the machine's id, environment and clones, never a raw id, and its `host:` label, the hostname by default. The environment and clone keys are random and never leave the machine, and the server never returns these ids. [What leaves the machine](docs/client.md#4-what-leaves-the-machine) lists each source.

The server logs each post's sender, path, subject and first 4 KiB of body, minus the token and door secrets, so keep its log as private as the mail.

## Troubleshooting

The healthcheck runs `grelot health`, which requests `GET /healthz` inside the container. It answers 200 while the store sweeper, which runs every 15 seconds, has committed a pass in the last minute. Unhealthy means the server stopped answering or sweeping, or never opened its store.

- The container exits at once. Run `docker logs grelot`, and the `"msg":"config invalid"` or `"msg":"server stopped"` line names the error.
- `permission denied` on `/data/grelot.db` means the `data` folder is not owned by user 65532.
- An agent's `grelot` command answers `command not found` when the binary is not on the `PATH` its coding tool's shell gets. See [Install the grelot client](docs/client.md).
- Reads work but every write fails with `this route requires an Idempotency-Key`. The client is older than the server, so install the server's version of the client.
- A command reports that the server did not answer in time. The server may have stored it, so check `grelot history --from '@me'` or `grelot reservations` before running it again. [When a write gets no answer](docs/client.md#6-when-a-write-gets-no-answer) explains the retry.

## Monitoring

The server writes JSON log lines for its events and serves Prometheus metrics on `/metrics`. Nine alert rules ship in [`alerts/promql.yaml`](alerts/promql.yaml), and the bundled Grafana dashboard needs Grafana 13.2 or newer. [Monitoring and alerts](docs/monitoring.md) lists every metric and rule and shows how to load them.

## Documentation

- [Install the grelot client](docs/client.md) covers the binary, its config and what it sends.
- [Set up grelot in your coding tools](docs/hosts/README.md) has one page per coding tool.
- [Wake agents from the server](docs/server-push.md) wakes agents through their tool's own API.
- [Provider files](docs/providers.md) is for replacing a shipped provider file or writing one for another tool.
- [Monitoring and alerts](docs/monitoring.md) lists the logs, metrics, dashboard and alert rules.
- [Hardening and resource limits](docs/hardening.md) gives memory limits and a hardened profile.

## Credits

grelot keeps its store in [bbolt](https://github.com/etcd-io/bbolt), and all credit for it goes to its maintainers. Its new-mail notices carry no message body, as in [parley](https://www.tyolab.com/blog/2026/09/06-introducing-parley-broker-agnostic-messaging-for-ai-agents/). Its reservation grants work like the fencing epochs of [agent-commons](https://github.com/t54-labs/agent-commons), so a stale holder cannot touch a later grant.

## Contributing

Issues and pull requests are welcome. The [shared contributing rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) apply.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE). The image carries the license text of every bundled component under `/usr/share/licenses/`.
