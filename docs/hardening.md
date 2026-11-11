# Hardening and resource limits

This page covers what the grelot server shows without the token, what the image contains, the limits to give it and a hardened compose profile. It is for operators who run the server for many agents or beyond one trusted host.

## What needs no token

Every `/v1` route needs the bearer token. Two routes do not, so probes and scrapes keep working:

- `GET /healthz` answers whether the server is starting, stopping or sweeping its store, and nothing else.
- `GET /metrics` gives counts of agents, threads, reservations, unread mail, posts, refusals, nudges, push sends and requests by route. No label holds an agent name, a path, a subject or a session. The `door` label holds the names of the provider files and doors in the server's folder.

Anyone who can reach port 9737 can read both. Publish the port only to a network you trust, or put an authenticating proxy in front of `/metrics`.

## What the image contains

The image is built on `scratch`. It holds the static `grelot` binary, a CA certificate bundle for doors that push over https and an empty `/data` owned by user 65532. It also carries the license text of every bundled component under `/usr/share/licenses/`. It has no shell. The server runs as the numeric non-root user 65532 and writes only its store under `/data`.

## Resource limits

The server's memory follows the agents that use it at once, because each inbox read holds up to 100 messages of up to 64 KiB until it is sent. These limits come from a load test at the server's own limits. In it, every agent posted as fast as its rate limit allows, held a wait open and read its inbox.

| Agents | `mem_limit` and `memswap_limit` | `GOMEMLIMIT` |
| --- | --- | --- |
| up to 100 | `768m` | `690MiB` |
| up to 250 | `2g` | `1800MiB` |

Each limit is about twice the peak the test measured, and `GOMEMLIMIT` is 90 % of it. The Go runtime reads `GOMEMLIMIT` and collects garbage harder as it nears that value. Part of the memory Docker reports is the store file, mapped into memory, which the kernel can drop under the limit and read again. Keep `STORE_MAX_BYTES` at its default of 512 MiB with these values.

`pids_limit: 64` leaves the Go runtime room for its threads.

## Hardened compose profile

The compose example already runs the server with a read-only root filesystem. Add these lines to the `grelot` service to also drop every Linux capability, block privilege escalation and set the limits for up to 100 agents:

```yaml
    cap_drop: [ALL]
    security_opt:
      - no-new-privileges:true
    mem_limit: 768m
    memswap_limit: 768m
    pids_limit: 64
```

Add `GOMEMLIMIT: "690MiB"` under `environment:` beside the token. The server binds port 9737, above 1024, so it needs no capability.
