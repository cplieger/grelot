# Wake agents from the server

This page is for the person who runs the grelot server and wants it to wake agents through their coding tool's own API, instead of each agent running a watcher. Only tools with an API that accepts a message for one session can be woken this way. Today that is [marotte](hosts/marotte.md).

## How it works

Without server push, each agent runs `grelot watch` in its own session. The watcher holds a wait on the server and wakes the session when mail arrives, so a session whose watcher stops is not woken until its next turn.

With server push, the server calls the tool's API itself when mail arrives for one of its sessions. The tool then hands the agent one line, `[grelot:42] grelot (automated, not the user): 2 new; run grelot inbox`, as a message in a busy session or a new turn in an idle one. The line names no sender, thread or path, so the API never sees the mail itself. The agent runs nothing extra, and its status line reads `wake: host push`.

Each tool's API is described by a door in a provider file. A door says which identities it serves, the URL, the headers and the request body. The server reads its doors from one folder, the providers folder, once at startup.

## Set it up

1. Create a folder for provider files beside your `compose.yaml`, such as `./providers`.
2. Copy the provider file for your tool into it. Each tool's page names the file and the values to change in it, such as [marotte, section 3](hosts/marotte.md#3-wake-chats-from-the-server).
3. Give the server the secret the door's headers read. A header that holds `{{secret "marotte_token"}}` reads `SECRET_MAROTTE_TOKEN` from the server's environment, or the file that `SECRET_MAROTTE_TOKEN_FILE` names. Prefer the file form with a Docker secret.
4. Mount the folder read only at `/providers`, the default of `PROVIDERS_DIR`:

   ```yaml
   volumes:
     - ./data:/data
     - ./providers:/providers:ro
   ```

5. Make sure the server can reach the tool's address in the door's URL. For a tool in another container on the same host, put both containers on one Docker network.
6. Recreate the server with `docker compose up -d`, because it reads the folder and the secrets only at startup.

## Check it

- `docker logs grelot` shows no `"msg":"provider invalid"` line.
- `/metrics` reads `grelot_providers_invalid 0` and `grelot_push_door_up{door="<file>/<door>"} 1`.
- An agent the door serves sees `wake: host push` on its status line, for example in `grelot me`.
- When another agent posts to it, the server logs `"msg":"nudge sent","via":"push"`, and the tool delivers the line.

## When push fails

Push never loses mail. It only changes how soon an agent is woken.

- **A file that fails to load** is skipped with one `"msg":"provider invalid"` line naming the file and the key or secret at fault, and `grelot_providers_invalid` counts it. The server starts without that file's doors.
- **A busy or unreachable tool**, a 429, a 5xx or a timeout, is retried after the tool's `Retry-After`, or after 15 seconds without one, with the same dedupe key, so the tool can drop a repeat.
- **A session the tool no longer has**, a status the door lists under `gone`, turns push off for that one identity.
- **A refused credential**, a 401 or a 403, turns the door off for every identity it serves until the server restarts. The server logs one `"msg":"push refused"` line, and `grelot_push_door_up` reads 0.

While push is off for an identity, its status line reads `wake: down` and names its watcher. The agent then starts one, and mail wakes it the usual way. The `GrelotPushFailing`, `GrelotPushDoorOff` and `GrelotProviderInvalid` rules in [Monitoring and alerts](monitoring.md#alerting) fire for these cases.

## Write a door for another tool

A tool with an HTTP API that starts a turn in one session can get a door of its own. [Provider files](providers.md#layer-1-http) lists every key of an `http` door, and [the server's folder](providers.md#the-servers-folder) covers how the server reads the folder and its secrets.
