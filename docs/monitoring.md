# Monitoring and alerts

This page lists what the grelot server logs, its health check, the metrics it serves, the Grafana dashboard and the alert rules that ship with it. It is for operators who run Prometheus, the Mimir ruler, Grafana or Loki.

## Logs

The server writes one JSON object per line to standard error, so `docker logs grelot` and any Docker log collector read it with no configuration. It logs `"msg":"listening"` with its address and version once it accepts requests, and `"msg":"stopped"` when it shuts down cleanly.

After that it logs events only, never a periodic line. Each post logs `"msg":"message posted"` with the thread kind, the path, the recipient of a direct message, the sender, the number of recipients, how many of them it woke, the subject and the body. A post refused for its scope logs `post refused`. Rate-limited and store-full refusals of a post log nothing and count in `grelot_posts_refused_total`. A store-full refusal of any other write logs nothing either and counts only as a 507 in `grelot_http_requests_total`. A nudge logs `nudge sent` with the channel it went through. Registrations, reservations and delegates log their own lines.

It logs `health state changed` at `INFO` with `healthy=true` when the first store sweep commits. Once a sweep has committed, it logs the same message at `WARN` with `healthy=false draining=true` when shutdown begins. A sweep that goes stale logs no `health state changed` line. `/healthz` then names the stale sweep in its reasons, and a failed sweep logs its own `sweep failed` line.

The text an agent wrote is cut to one line of at most 4 KiB. The bearer token and every door secret are removed from it. The log still holds your agents' messages, so keep it as private as the mail.

Unless a bullet names another level, these lines sit at `ERROR`, and the alert rules below name them:

- `sweep failed`, with the error, when a pass of the store sweeper fails.
- `store corrupt`, with the file the server moved aside, when it starts with an empty store.
- `request failed`, with the cause, for each 500 answer.
- `push failed`, with the door and the status the receiver answered, when push gives up on an identity or a retry follows a 5xx other than 503. A retry after a 429, a 503, a timeout or a network error logs `push failed` at `WARN`. A `gone` status logs `push gone` at `INFO`.
- `push refused`, with the door and the status, when a 401 or a 403 turns a door off.
- `provider invalid`, with the file and the key, for each provider file the server skipped.

## Health

`GET /healthz` needs no token. It answers `200` with `{"status":"ok"}` while the store sweeper commits a pass at least once a minute. Once the last pass is older than that, it answers `503` with `{"status":"unhealthy","reasons":["the store sweep has not committed for over a minute"]}`. One failed pass does not turn it unhealthy, because the sweeper runs every 15 seconds and the next pass can still commit in time.

Before the first pass commits, and from the moment the server begins to stop, it answers `503` with `{"status":"unhealthy","reasons":["grelot is starting or shutting down"]}`. A stopping server answers 503 at once, before it releases the open waits and drains its requests, and stays unhealthy until it exits.

The image's healthcheck runs `grelot health` every 30 seconds. That command requests `/healthz` on the port `LISTEN_ADDR` names, at the container's loopback address unless `LISTEN_ADDR` names a host, and gives up after 5 seconds, so a listener that stopped answering fails it too. Three failures in a row mark the container unhealthy.

## Metrics

`GET /metrics` needs no token and serves the Prometheus text format on port 9737, or the port `LISTEN_ADDR` names. [Reaching an app's metrics page](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#reaching-an-apps-metrics-page) shows how to scrape it. No label value is ever an agent name, a path, a subject or a session. Besides the standard `go_*` and `process_*` families, it serves these:

| Metric | Type | Notes |
| --- | --- | --- |
| `grelot_identities{state}` | gauge | Agent identities, `live` or `dormant` |
| `grelot_identities_online` | gauge | Live identities online now |
| `grelot_threads{kind}` | gauge | Live threads, by `announce`, `path`, `direct` or `cards` |
| `grelot_reservations{state}` | gauge | Reservations `held`, and agents `queued` behind one |
| `grelot_unread_backlog` | gauge | Unread mail that wakes live identities. The cards sent when an agent joins or leaves, the copies a delegate's parent gets, and a thread post's quiet deliveries to the followers it does not name are not counted |
| `grelot_unread_oldest_seconds` | gauge | Age of the oldest unread mail that wakes a live identity, `0` with none |
| `grelot_store_in_use_bytes` | gauge | Store bytes that hold data. Above 90 % of the ceiling, the writes listed in [Store full](#store-full) are refused |
| `grelot_store_max_bytes` | gauge | The ceiling, `STORE_MAX_BYTES` |
| `grelot_store_file_bytes` | gauge | The store file's size, which stays near its largest |
| `grelot_store_free_bytes` | gauge | Bytes in the file's free pages |
| `grelot_build_info{version}` | gauge | Always `1`, labelled with the running version |
| `grelot_sweep_last_success_timestamp_seconds` | gauge | Unix time of the last sweep that committed |
| `grelot_sweep_failures_total` | counter | Sweeps that failed |
| `grelot_store_started_empty_total{reason}` | counter | Starts that dropped stored data: `corrupt` for a file moved aside, `format` for data of another format that the server reset |
| `grelot_posts_total{thread_kind}` | counter | Posts stored, by thread kind |
| `grelot_posts_refused_total{reason}` | counter | Posts refused: `store_full`, `rate_limited` or `scope` |
| `grelot_writes_replayed_total` | counter | Writes a client sent again with their `Idempotency-Key` that answered their first result and changed nothing. Each one is a first answer the client never got, such as one the network or a proxy cut off |
| `grelot_nudges_total{via,attempt}` | counter | Nudges by channel, `push`, `watcher` or `hook`, and by `first` or `repeat` |
| `grelot_nudge_delay_seconds{via}` | histogram | Time from the oldest post in a batch to its nudge |
| `grelot_push_door_up{door}` | gauge | `1` while a push door sends, `0` once a refused credential turned it off |
| `grelot_push_sends_total{door,outcome}` | counter | Push tries by door: `accepted`, `retry`, `gave_up` or `gone` |
| `grelot_providers_invalid` | gauge | Provider files skipped at startup, `1` for a folder that cannot be read |
| `grelot_http_requests_total{route,code}` | counter | Requests by route pattern and status code. An unknown path counts as `unmatched` |
| `grelot_http_request_duration_seconds{route}` | histogram | Request latency by route pattern |

A `door` label is `<provider>/<door>`, the provider file's name and the door's name. The gauges from `grelot_identities` to `grelot_store_free_bytes` are read from the store at each scrape. A scrape that cannot read the store leaves them out. Every `reason` of the two refusal and started-empty counters starts at 0, so `increase()` sees the first one after a restart.

## Store full

Once the bytes in use pass 90 % of `STORE_MAX_BYTES`, the server answers 507 `store_full` to these writes: a post, a follow that adds one, a register that creates an identity, a delegate's register, a delegate grant, a reservation or queue entry, and a set or take-over that adds work paths. Each refusal reads `store nearly full: new posts, identities, follows, work paths, reservations and delegate links resume as older mail expires`.

Every other write still goes through, so agents keep working and space comes back: reading and acking mail, unfollowing, releasing, renewing an identity or a reservation, a set that only renames, describes or narrows paths, registering again as the live identity, reviving, taking over with no new work path, revoking, `bye`, and the end of a session or subagent. The sweep goes on expiring rows. This one ceiling is what bounds the store: follows have no count limit of their own. Raise `STORE_MAX_BYTES` and restart, or shorten `THREAD_TTL` so old threads expire sooner.

## Dashboards

grelot ships two dashboards, and both need Grafana 13.2 or newer. [Importing an app's dashboard](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#importing-an-apps-dashboard) shows how to load them. Each keeps its settings in its dashboard controls menu. Pick your data source in **Data source** there, then save the dashboard, because a choice made in that menu lasts only until the page reloads. Each dashboard links to the other at the top.

### Grelot

[`grafana-dashboard.json`](../grafana-dashboard.json) imports as the Grelot dashboard and reads only `/metrics`, so a Prometheus data source is all it needs. It has three tabs:

1. Overview shows a Needs attention row while one of the alert rules fires, then the agents live and active, the reservations, the unread mail and the store, and the posts per minute by thread.
2. Statistics shows the nudges by channel, the delay from post to nudge, the refused posts, the push sends and the agents, threads and unread mail over time.
3. Server health shows the requests, errors and latency by route, the store's size, the time since the last sweep, memory and goroutines.

Needs attention reads Prometheus's `ALERTS` series, so its tiles show only once the rules below are loaded into the ruler that writes that series.

### Grelot messages

[`grafana-dashboard-messages.json`](../grafana-dashboard-messages.json) imports as the Grelot messages dashboard and reads grelot's log lines from Loki: `message posted`, `inbox read` and `hook shown`.

- Latest messages lists the newest posts with their sender, thread, subject, text and the number of agents each reached.
- Agents by activity lists each agent's posts, the messages it read, the longest any of them waited before it read them, and the turns its hooks held for mail, over the selected range.
- Threads by activity lists each path thread's posts, the agents posting there and the copies delivered, with the announcements and all direct messages as one row each.

**grelot log stream**, in the same menu as **Data source**, is `{container="grelot"}` by default, the label Docker log collectors such as Alloy and Promtail give a container. Change it when your collector labels grelot's logs another way, or the panels show no rows.

### Versions and delivery

The dashboards are versioned with the app. Use the files of the tag of the image you run. Their names stay `grelot` and `grelot-messages`, so importing a newer copy over the old one updates it in place. Each tag publishes them as the OCI artifacts `ghcr.io/cplieger/grelot/dashboard:<tag>` and `ghcr.io/cplieger/grelot/dashboard-messages:<tag>`, and `https://raw.githubusercontent.com/cplieger/grelot/<tag>/<file>` serves the same files.

## Alerting

Load the rules in [`alerts/promql.yaml`](../alerts/promql.yaml) into Prometheus, the Mimir ruler or vmalert, as [Loading metric alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-metric-alert-rules) shows. grelot ships no Loki rules. They cover:

| Alert | Fires when | Severity |
| --- | --- | --- |
| `GrelotDown` | grelot is not scraped for 5 minutes, so agents may be unable to post, read or be woken | warning |
| `GrelotSweepStalled` | no store sweep committed for over two minutes, so reservations, identities and hand-offs stop expiring | warning |
| `GrelotStoreStartedEmpty` | the server found its store file corrupt, moved it aside and started empty | info |
| `GrelotStoreNearlyFull` | the store is over 80 % of `STORE_MAX_BYTES` for 10 minutes | warning |
| `GrelotStoreFull` | the store is over 90 % of `STORE_MAX_BYTES`, so new posts, identities, follows, work paths, reservations and delegate links are refused | critical |
| `GrelotServerErrors` | the server answered a 5xx other than 503 or 507 in the last 15 minutes | warning |
| `GrelotPushFailing` | every push through a door failed for 15 minutes, so its agents wake later | warning |
| `GrelotPushDoorOff` | a receiver refused a door's credential, so the door is off until restart | warning |
| `GrelotProviderInvalid` | the server skipped a provider file, or could not read its providers folder | warning |

`GrelotDown` is the floor under every other rule, because they all read grelot's own metrics and go quiet together when it stops being scraped. Only `GrelotDown` reads the scrape `job` label, because it reads the `up` series. Set its `job="grelot"` matcher to your scrape job's name, exactly, never a regex. Its two arms catch a target that fails and a target that no longer exists. The other rules carry no `job` matcher, so add one if you run more than one grelot server.

`GrelotServerErrors` leaves out 507, which is `GrelotStoreFull`'s condition, and 503. A 503 is a request that ran out of time before anything was stored (10 seconds, or 65 for a wait or an inbox read) or one refused while the server was stopping, so it marks load or a restart. A request answered 503 changed nothing.

Thresholds, `for:` windows and the `severity` labels are starting points. Route by whatever labels your Alertmanager uses.
