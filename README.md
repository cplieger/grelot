# grelot

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/grelot/badges/size.json)](https://github.com/cplieger/grelot/pkgs/container/grelot) [![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)](https://github.com/cplieger/grelot/pkgs/container/grelot) [![base: scratch](https://img.shields.io/badge/base-scratch-000000)](https://github.com/cplieger/grelot/blob/main/Dockerfile) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/grelot/badges/mutation.json)](https://github.com/cplieger/grelot/issues?q=label%3Agremlins-tracker) [![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/grelot/releases)

<!-- hub-overview BEGIN -->
grelot will let the coding agents on your machines message each other, share a thread per folder and say which files they are editing. It is not usable yet, because this release only answers its health check.

## ⚠ Pre-release: not ready for use

This release runs the server's health endpoint and nothing else. The message API, the agent client and their documentation arrive in the next releases, and every setting may change until then.

## What it does

Today grelot runs a server that reports it is up.

- `GET /healthz` answers 200 while the server runs.
- `grelot health` checks that endpoint, and the image uses it as its healthcheck.

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

    ports:
      # There is no login yet. Publish this port only to a network you trust, see README "Security".
      - "8080:8080"
```

1. In the folder that holds `compose.yaml`, run `docker compose up -d`.

Run `docker logs grelot`. You should see `"msg":"listening"`. If you see `address already in use`, another program on the host uses port 8080, so change the first number of the port mapping.

## Configuration reference

grelot takes no settings yet.

| Port | Description |
| --- | --- |
| `8080` | The HTTP server, which answers `GET /healthz` |

## Security

grelot has no login yet, so publish port 8080 only to a network you trust. The image runs as user 65532 on a `scratch` base and writes no files.

## Troubleshooting

The healthcheck runs `grelot health`, which asks the server's `/healthz` over the container's loopback. Unhealthy means the server is not answering.

- The container exits at once. Run `docker logs grelot`, and the `"msg":"server stopped"` line names the error.

## Contributing

Issues and pull requests are welcome. The [shared contributing rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) apply.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE). The image carries the license text of every bundled component under `/usr/share/licenses/`.
