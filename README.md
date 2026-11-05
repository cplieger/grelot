# grelot

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/grelot/badges/size.json)](https://github.com/cplieger/grelot/pkgs/container/grelot) [![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)](https://github.com/cplieger/grelot/pkgs/container/grelot) [![base: scratch](https://img.shields.io/badge/base-scratch-000000)](https://github.com/cplieger/grelot/blob/main/Dockerfile) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/grelot/badges/mutation.json)](https://github.com/cplieger/grelot/issues?q=label%3Agremlins-tracker) [![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/grelot/releases)

<!-- hub-overview BEGIN -->
grelot will let the coding agents on your machines message each other, share a thread per folder and say which files they are editing. It is not usable yet, because this release only answers its health check.

## ⚠ Pre-release: not ready for use

This release runs the server's health endpoint and the provider file check, and nothing else. The message API, the agent client and their documentation arrive in the next releases, and every setting may change until then.

## What it does

Today grelot runs a server that reports it is up.

- `GET /healthz` answers 200 while the server runs.
- `grelot health` checks that endpoint, and the image uses it as its healthcheck.
- `grelot provider check <file>` checks a provider file, which describes one coding tool's hooks and wake-ups as data: its keys, its templates rendered against sample data, and every output meant as JSON parsed. It prints one line per problem and exits 1 when the file is invalid.

## Who it is for

grelot is being built for people who run several coding agents, such as Kiro, Claude Code or Codex, on their own machines and want them to coordinate instead of working blind on the same code.

You need a Docker host.

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

    ports:
      # Publish this port only to a network you trust, see README "Security".
      - "8080:8080"
```

1. Replace the token with a long random string, for example the output of `openssl rand -hex 32`.
2. Create the `data` folder and give it to the image's user: `mkdir data && sudo chown 65532:65532 data`.
3. In the folder that holds `compose.yaml`, run `docker compose up -d`.

Run `docker logs grelot`. You should see `"msg":"listening"`. If you see `address already in use`, another program on the host uses port 8080, so change the first number of the port mapping.

## Configuration reference

| Variable | Default | Description |
| --- | --- | --- |
| `GRELOT_TOKEN` | required | The bearer token every client sends. `GRELOT_TOKEN_FILE` names a file that holds it instead |
| `GRELOT_PATH_ROOT` | `/workspace` | The root that repository paths are written under, `<root>/<repo>/<file>`, whatever folder each clone sits in |
| `STORE_PATH` | `/data/grelot.db` | The store file |
| `STORE_MAX_BYTES` | `536870912` (512 MiB) | The ceiling on the bytes the store uses; posts are refused past 90 % of it |
| `THREAD_TTL` | `72h` | How long a thread, a message and an event live after they are written |
| `DORMANT_TTL` | `72h` | How long an expired agent identity stays revivable |
| `RESERVATION_MAX_TTL` | `8h` | The longest lease a file reservation may ask for |
| `PROVIDERS_DIR` | `/providers` | The folder of provider files whose `http` doors the server pushes through |

A setting that is malformed stops the server at startup with a `"msg":"config invalid"` line naming it.

| Port | Description |
| --- | --- |
| `8080` | The HTTP API, and `GET /healthz`, which needs no token |

## Security

Every `/v1` request needs the bearer token, and every holder of it is trusted equally, so publish port 8080 only to a network you trust and keep the token out of the compose file with `GRELOT_TOKEN_FILE`. The image runs as user 65532 on a `scratch` base and writes only the store and its health marker.

## Troubleshooting

The healthcheck runs `grelot health`, which checks the marker file the server refreshes after each pass of its store sweeper, every 15 seconds. Unhealthy means the server has stopped sweeping, or never opened its store.

- The container exits at once. Run `docker logs grelot`, and the `"msg":"config invalid"` or `"msg":"server stopped"` line names the error.
- `permission denied` on `/data/grelot.db`: the `data` folder is not owned by user 65532.

## Contributing

Issues and pull requests are welcome. The [shared contributing rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) apply.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE). The image carries the license text of every bundled component under `/usr/share/licenses/`.
