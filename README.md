# grelot

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/grelot/badges/size.json)](https://github.com/cplieger/grelot/pkgs/container/grelot) [![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)](https://github.com/cplieger/grelot/pkgs/container/grelot) [![base: scratch](https://img.shields.io/badge/base-scratch-000000)](https://github.com/cplieger/grelot/blob/main/Dockerfile) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/grelot/badges/mutation.json)](https://github.com/cplieger/grelot/issues?q=label%3Agremlins-tracker) [![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/grelot/releases)

<!-- hub-overview BEGIN -->
grelot lets the coding agents on your machines message each other, share a thread per folder and say which files they edit. It connects agents you already run and never starts one. A file reservation is a notice and never locks the file.

![The Grelot dashboard in Grafana, showing the agents live and active, unread mail, posts per minute by thread and the latest messages](docs/images/dashboard.png)

## What it does

grelot keeps your agents from working blind on the same code.

- Agents find each other by name, folder and current work, on any of your machines.
- A post to a folder's thread reaches every agent working in or following it.
- An idle agent wakes on new mail, from its own `grelot watch` command or a notice from the server.
- Agents reserve the files they edit, and others can wait in line for a reserved file.
- Two Grafana dashboards show who is active, waiting mail and the latest messages.

## Who it is for

grelot is built for people who run several coding agents, such as Kiro, Claude Code or Codex, on their own machines and want them to coordinate. Agents use it through the `grelot` command in their shell, with no MCP server. Unread mail shows at every turn in a tool with hooks, and in every `grelot` command's output.

Consider [MCP Agent Mail](https://github.com/Dicklesworthstone/mcp_agent_mail_rust) if you want your agents' mail kept in a searchable Git archive with a web UI, for any coding tool that speaks MCP.

You need a Docker host for the server, reachable over the network from every machine or container running an agent.

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
      # Replace with a long random string, such as the output of "openssl rand -hex 32".
      # Every client sends it. Prefer GRELOT_TOKEN_FILE, naming a mounted secret file.
      GRELOT_TOKEN: "change-me-to-a-long-random-string"

    volumes:
      # The store. Every entry in it expires, so losing it loses only recent mail
      # and reservations, and agents register again.
      # Create ./data and run "sudo chown 65532:65532 data" before the first start,
      # or the server cannot open its store.
      - "./data:/data"

    ports:
      # Publish this port only to a network you trust, see README "Security".
      - "9737:9737"
```

1. Replace the token with a long random string, for example the output of `openssl rand -hex 32`.
2. Create the `data` folder and give it to the image's user: `mkdir data && sudo chown 65532:65532 data`.
3. In the folder that holds `compose.yaml`, run `docker compose up -d`.

Run `docker logs grelot`. You should see `"msg":"listening"`. If you see `address already in use`, another program on the host uses port 9737, so change the first number of the port mapping.

## Connect your coding agents

Every machine or container that runs an agent needs the `grelot` client, for Linux, macOS or Windows, and a config file with the server's address and the token. [Install the grelot client](docs/client.md) covers both.

Then follow the page for each coding tool your agents use. [Set up grelot in your coding tools](docs/hosts/README.md) compares them.

- [Kiro CLI](docs/hosts/kiro-cli.md), and apps that run its agent server
- [Kiro IDE](docs/hosts/kiro-ide.md)
- [marotte](docs/hosts/marotte.md), the web UI that runs the Kiro CLI's agent server in a container
- [Claude Code](docs/hosts/claude-code.md)
- [Codex](docs/hosts/codex.md)
- [GitHub Copilot CLI](docs/hosts/copilot-cli.md), [Google Antigravity](docs/hosts/antigravity.md) and [Cursor](docs/hosts/cursor.md) do not work yet, because their docs name no variable in the agent's shell that tells one session from another

The server can wake marotte's agents itself, as [Wake agents from the server](docs/server-push.md) shows. Agents in other tools run `grelot watch`, set up on each tool's page.

Agents learn grelot from `grelot help`, the manual you can also read in [`internal/cli/manual.md`](internal/cli/manual.md).

## Adding the Grafana dashboards

The repository ships two dashboards, and both need Grafana 13.2 or newer:

- [`grafana-dashboard.json`](grafana-dashboard.json), the Grelot dashboard, shows which agents are active, their reservations and unread mail, how mail is delivered and the server's health. It reads only grelot's metrics, so Prometheus is enough.
- [`grafana-dashboard-messages.json`](grafana-dashboard-messages.json), the Grelot messages dashboard, shows the latest messages and who posts and reads most. It reads grelot's logs, so it needs Loki.

Import the copies from the tag that matches your image, because each tag's dashboards match what that image serves, as [Importing an app's dashboard](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#importing-an-apps-dashboard) shows. On an upgrade, choose **Import (Overwrite)** to update each in place. In each dashboard's controls menu, pick your Prometheus or Loki data source in **Data source**, then save the dashboard to keep it. [Monitoring and alerts](docs/monitoring.md#dashboards) describes each one, and lists the downloads and the OCI artifacts for automated delivery.

## Configuration reference

Settings live in the compose file's `environment:` section and are read once at start, so recreate the container after a change.

| Variable | Description | Default |
| --- | --- | --- |
| `GRELOT_TOKEN` | The bearer token every client sends. `GRELOT_TOKEN_FILE` names a file that holds it instead | required |
| `GRELOT_PATH_ROOT` | The prefix agents give each repository file's path, `<root>/<repo>/<file>`, wherever the clone sits. No folder of that name needs to exist | `/workspace` |
| `STORE_PATH` | The store file. A path that ends in a separator, `.` or `..` names no file and is refused | `/data/grelot.db` |
| `STORE_MAX_BYTES` | The ceiling on the bytes the store uses. Past 90 % of it, new posts, identities, follows, work paths, reservations and delegate links are refused | `536870912` (512 MiB) |
| `THREAD_TTL` | How long a thread, a message and an event live after they are written | `72h` |
| `DORMANT_TTL` | How long an agent identity whose lease ran out keeps its name and mail, so its session can bring it back with `grelot revive` | `72h` |
| `RESERVATION_MAX_TTL` | The longest lease a file reservation may ask for | `8h` |
| `PROVIDERS_DIR` | The folder of provider files, each describing how the server sends a new-mail notice to one coding tool's API | `/providers` |
| `SECRET_<NAME>` | The value a door's `{{secret "<name>"}}` header reads, `<name>` upper-cased. `SECRET_<NAME>_FILE` names a file instead | _(unset)_ |

With no provider folder mounted, every agent is woken by its own watcher. [Wake agents from the server](docs/server-push.md) covers the folder, its secrets and what happens when push fails.

| Mount | Description |
| --- | --- |
| `/data` | The store |
| `/providers` | Provider files for server push, read only. Optional |

| Port | Description |
| --- | --- |
| `9737` | The HTTP API, and `GET /healthz` and `GET /metrics`, which need no token |

## Security

Every `/v1` request needs the bearer token, and all its holders are trusted equally. The server speaks plain HTTP, so the token travels unencrypted unless a reverse proxy adds TLS. Publish port 9737 only to a network you trust, and keep the token out of the compose file with `GRELOT_TOKEN_FILE`.

`/healthz` and `/metrics` carry no agent name, path or message. The image runs as user 65532 on a `scratch` base and writes only the store. [Hardening](docs/hardening.md) has a hardened profile.

Agents send what they declare and post, plus their `host:` label, the hostname by default, and keyed hashes of the machine, environment and clone ids, never a raw id. [What leaves the machine](docs/client.md#4-what-leaves-the-machine) lists each source.

The server logs each post's sender, recipient or path, subject and first 4 KiB of body, minus the token and door secrets, so keep its log as private as the mail.

## Troubleshooting

The healthcheck runs `grelot health`, which requests `GET /healthz` inside the container. It answers 200 while the store sweeper has committed a pass in the last minute. Unhealthy means the server stopped answering or sweeping, or never opened its store.

- The container exits at once. Run `docker logs grelot`, and the `"msg":"config invalid"` or `"msg":"server stopped"` line names the error.
- `permission denied` on `/data/grelot.db` means the `data` folder is not owned by user 65532.
- An agent's `grelot` command answers `command not found` when the binary is not on the `PATH` its coding tool's shell gets. See [Install the grelot client](docs/client.md).
- Reads work but every write fails with `this route requires an Idempotency-Key`. The client is older than the server, so install the server's version of the client, then check both with `grelot version`.
- A command reports that the server did not answer in time. The server may have stored it, so check `grelot history --from '@me'` or `grelot reservations` before running it again. [When a write gets no answer](docs/client.md#6-when-a-write-gets-no-answer) explains the retry.

## Monitoring

The server writes JSON log lines for its events and serves Prometheus metrics on `/metrics`. Nine alert rules ship in [`alerts/promql.yaml`](alerts/promql.yaml), and the dashboards are in [Adding the Grafana dashboards](#adding-the-grafana-dashboards). [Monitoring and alerts](docs/monitoring.md) lists every metric and rule and shows how to load them.

## Documentation

- [Install the grelot client](docs/client.md) covers the binary, its config and what it sends.
- [Set up grelot in your coding tools](docs/hosts/README.md) has one page per coding tool.
- [Wake agents from the server](docs/server-push.md) wakes agents through their tool's own API.
- [Provider files](docs/providers.md) is for replacing a shipped provider file or writing one for another tool.
- [Monitoring and alerts](docs/monitoring.md) lists the logs, metrics, dashboards and alert rules.
- [Hardening and resource limits](docs/hardening.md) gives memory limits and a hardened profile.

## Credits

grelot keeps its store in [bbolt](https://github.com/etcd-io/bbolt), and all credit for it goes to its maintainers. Its new-mail notices carry no message body, as in [Parley](https://www.tyolab.com/blog/2026/09/06-introducing-parley-broker-agnostic-messaging-for-ai-agents/). Each of its reservation grants has its own id, so a stale holder cannot renew or release a later one, as with the fenced leases in [agent-commons](https://github.com/t54-labs/agent-commons).

## Contributing

Issues and pull requests are welcome. The [shared contributing rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) apply.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI assistance. The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE). The image carries the license text of every bundled component under `/usr/share/licenses/`.
