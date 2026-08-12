# Remote For Xirp

A mobile web control surface for the [Xirp](https://xirp.dev) desktop app's agent
daemon. Runs on the Mac that runs Xirp, exposes a phone-sized UI on your own
network, and lets you read what your sessions are doing and send them messages
without being at the desk.

Single Go binary with the UI embedded. No build step for the frontend, no
node_modules, no database.

> **Unofficial.** Not affiliated with, endorsed by, or derived from Xirp. This
> repository contains no code, stylesheets, icons or other assets from the Xirp
> application — only this project's own implementation of a client for the
> daemon's local WebSocket API, described below. "Xirp" is used to say what this
> talks to.

## Requirements

- **macOS**, and not incidentally. The daemon's access token exists only in the Xirp
  app's process environment, which `ps -E` exposes to the same user and nobody else, so
  this has to run as you, on that machine, as a launchd user agent. It also drives
  `tmux` for the terminal view.
- **Xirp** installed and running. Sessions come from its local daemon; when the app is
  quit there is nothing to control.
- **Go 1.26+** to build. Nothing else — no Node, no package manager, no database.
- A phone on the same network, or reaching the machine over a VPN.

## What it does

- Lists every session with status, project, branch, agent, context use, cost and
  last activity, filtered to active sessions by default.
- Opens a session: metadata plus recent transcript entries, as a plain chat or with
  the agent's tool calls included.
- Sends a message to a session (this drives the agent — same as typing into it).
- Stops a session.
- **Searches every session** — metadata, messages and JSONL transcripts — including
  completed ones the list no longer shows.
- **Starts a new session**: pick a project, an installed agent and a model, give it a
  goal, optionally a new branch — or start a plain shell instead.
- **Shows the working tree state** for a session: branch, staged, modified and
  untracked counts, so you can see from a phone whether an agent left work
  uncommitted.
- **Shows recent commits** from the session's worktree, and any **URLs the agent
  printed** (dev servers, previews) as tappable links — loopback addresses are
  filtered out, since they would resolve to the phone.
- **Picks the model** when creating a session, listed with context window and input
  price so the choice is informed. 35 models for Claude alone.
- **Acknowledges** a finished or failed session to clear its attention state.
- **Machines first.** The app opens on the machines it knows about — one card each,
  with a live dot, project and running counts, and round-trip time — then folders
  (projects) inside a machine, then that folder's sessions, then the session. A session
  list only means something once you have said which machine you mean.
- **Welcome screen** for a cold start, with **Scan QR code** as the primary action;
  manual entry is there but secondary.
- **Camera QR scanning** via the platform barcode detector, with an explicit fallback to
  manual entry where that is missing or the camera is refused, rather than a dead
  viewfinder.
- **Terminal mode: the session's actual tmux pane**, ANSI colours and all. Because it
  is the real pane, slash commands, model pickers and permission prompts work without
  this app knowing they exist. A key row supplies Escape, Tab, arrows, Enter and
  Ctrl-C, which a text field cannot send.
- **Multiple hosts.** Each host is one machine running Xirp; add others in settings and
  switch between them. Remote hosts are reached cross-origin, which their bridge allows
  only when it requires a key.
- **Pairing by QR.** `xirp-remote install --generate-key` mints a key and prints a
  scannable code; settings shows the same code for adding another device. The link
  carries the key in the URL *fragment*, so it never reaches a server log, and the page
  strips it from the address bar as soon as it has a cookie.
- **Installable PWA** with a service worker, so it lives on the home screen and opens
  full-screen. See `deploy/android/` to wrap it as an APK.
- **Settings screen** for transcript mode (Chat / Full / Terminal), how many messages
  load, the default filter, timestamps, hosts and pairing. Stored per browser.
- **Markdown rendering** for agent replies: fenced code blocks that scroll sideways
  rather than stretching the page, inline code, headings, lists and links.
- **Grouped chat layout** — one speaker label per run of messages, tool activity
  collapsed into a tappable "12 steps · Bash" line rather than hidden, a working
  indicator while the agent is mid-turn, and a "jump to latest" button so new output
  never yanks the page while you are reading.
- **Enter sends**; Shift+Enter (or Alt+Enter) inserts a newline, and the composer asks
  mobile keyboards for a Send key.
- **Two ways to deliver text**: **Submit** types it and presses Enter so the agent acts
  on it, **Type** leaves it in the agent's input unsent. The daemon draws this
  distinction itself, as `enter` on `session:message`.

## Running it as a service

```sh
go build -o xirp-remote .
./xirp-remote install          # installs a launchd user agent and starts it
./xirp-remote status
```

| Command | Does |
|---|---|
| `install [--addr H:P] [--key K] [--no-copy]` | Copy the binary to `~/.local/bin`, write a LaunchAgent, start it, and wait until `/healthz` answers. Re-running it upgrades in place. |
| `uninstall` | Stop and remove the agent, leaving the binary and log. |
| `start` / `stop` / `restart` | Control the running agent. |
| `status` | Service, HTTP and Xirp daemon state, plus the session count. |
| `logs [-f]` | Tail `~/Library/Logs/xirp-remote.log`. |
| `qr` | Print a pairing QR code and its URL. |

Re-running `install` preserves the existing key, URL and address; flags override them
and `--no-key` removes the key deliberately. That is not a nicety: an earlier version
read the flags over a blank slate, so a plain `install` silently dropped the access key
and turned authentication off.

It installs as a **LaunchAgent in your GUI session, not a LaunchDaemon**, and that
is not a style choice: the daemon token is read from the Xirp app's process
environment, which `ps -E` only reveals to the same user. A root LaunchDaemon in
another session would see neither the app nor its environment.

`RunAtLoad` starts it at login and `KeepAlive` restarts it if it dies — verified by
killing the process and watching launchd bring it back with a new pid. The plist
also pins a `PATH` that includes Homebrew, because transcripts are read by running
`node`, and a LaunchAgent's inherited `PATH` does not include it.

`install` refuses to start if something else already holds the port, naming the
process, rather than fighting another supervisor for it.

## How it talks to Xirp

The daemon listens on `127.0.0.1` only and authenticates with a token passed as a
query parameter (`ws://127.0.0.1:<port>/?token=<token>`). Its protocol is a typed
WebSocket API with 212 message types, self-describing through `api:list` and
`api:describe` — worth using if you extend this.

Two facts shape the implementation (two more are under "traps" below):

1. **Neither the port nor the token is stable.** The token is minted on each app
   launch and injected into the environment of the app's own processes; it is not
   written to disk. `discover()` in `daemon.go` reads it back out of the running
   process environment via `ps -E`, which only works for processes owned by the
   same user. So the bridge must run as the user running Xirp, and it rediscovers
   credentials on every reconnect rather than caching them. Restarting Xirp does
   not require restarting the bridge.

2. **Replies are matched by `type`, not by a request id.** Requests are therefore
   serialized under a mutex, each waiting for the one response type it expects.
   One in-flight call is far above what a phone UI generates.

Transcripts do not come from the daemon. `messages:list` reads the daemon's own
database, which is empty for harness-driven sessions; the canonical reader is
`squab session-parse` (schema `squab.session-parsed/v1`), the orchestrator CLI
shipped inside the app bundle.

### Why there is no remote approval of permission prompts

This was the original motivation and it does not work against this daemon.
`permissionService.waitForDecision` caps its wait at
`Math.min(timeout, 500)` milliseconds before falling through to the agent's own
dialog. A pending request is therefore gone about half a second after it appears,
which no polling remote client can catch. The `/api/permissions` endpoints are
implemented and correct, and the UI renders a request if one is genuinely live,
but this is not an approval queue. Approving from your phone would need the app
to hold requests open longer.

## Configuration

```sh
./xirp-remote                                   # foreground, open, no auth
XIRP_REMOTE_KEY=<32+ hex chars> ./xirp-remote    # foreground, key required
./xirp-remote install --key <32+ hex chars>      # as a service, key required
```

| Variable | Default | Meaning |
|---|---|---|
| `XIRP_REMOTE_KEY` | *(unset)* | Access key. Unset means open, no authentication. A key shorter than 16 chars is refused rather than half-honoured. |
| `XIRP_REMOTE_ADDR` | `0.0.0.0:8790` | Listen address. |

`install` bakes both into the LaunchAgent's `EnvironmentVariables`, so the plist is
the record of how the service is configured.

## Security

**As deployed it runs open, with no authentication** — that was a deliberate
choice: on this network the boundary is the network.

This service can type into agent sessions, which is arbitrary code execution on
the Mac as the user running Xirp. So what actually contains it is:

- It is reachable only from the LAN and the WireGuard tunnel. There is no port
  forward and no Cloudflare tunnel to it. Off the LAN, connect the tunnel first.
  Do not give it a public hostname without turning the key back on.
- The terminal view can type into a pane, which is the same power the message composer
  already had, plus a fixed set of control keys. Key names are allowlisted: passing
  arbitrary strings to `tmux send-keys` would let anything type anything anywhere.
- Cross-origin access is enabled **only when a key is required**. On an open instance,
  echoing an origin would let any web page you happen to visit read and drive your
  sessions.
- Only an explicit allowlist of daemon messages is reachable — 16 of the daemon's
  212 message types. It can list, read, search, message, stop and create sessions,
  and read git status. It cannot commit, push, open a PR, attach to a terminal,
  delete a session or change any app setting: `git:` writes, `terminal:`,
  `session:delete` and all of `settings:` are simply not proxied.
- Creating sessions **is** reachable, which is worth stating plainly: anyone who can
  reach this can start an agent with a goal of their choosing on any project the app
  knows about.
- Setting `XIRP_REMOTE_KEY` re-enables the login gate at any time; the key path is
  still there, compared in constant time, wrong keys delayed 400 ms, held in an
  `HttpOnly` `SameSite=Lax` cookie. Use `xirp-remote install --key <key>`.

## Putting it behind a reverse proxy

Optional, and worth it for TLS and a memorable hostname. Two notes from running it
this way:

- Give it **its own config file** rather than appending to a shared routes file.
  Appending can silently land a router definition inside a `services` block, where it
  is ignored and the URL just 404s.
- If the machine is a **laptop, define two backends and health-check them.** A laptop
  has one address on its home network and a different one over a VPN, and typically
  only one of the two is reachable at a time. A single backend URL is therefore wrong
  half the time; with a health check on `/healthz` the proxy follows the laptop
  between locations with no edit. An example is in
  [`deploy/traefik-xirp-remote.yml`](deploy/traefik-xirp-remote.yml).
- `/healthz` is unauthenticated on purpose, so an uptime monitor works even when a
  key is set.

## API

When a key is configured, endpoints take it as an `X-Xirp-Key` header or the
cookie from `POST /api/auth`. In open mode no credential is needed.

| Method | Path | Purpose |
|---|---|---|
| GET | `/healthz` | Liveness. No auth. |
| POST | `/api/auth` | `{key}` → sets cookie. |
| GET | `/api/sessions` | Projected session list. |
| GET | `/api/sessions/{id}?limit=N` | Session plus transcript. |
| POST | `/api/sessions/{id}/message` | `{text}` → drives the agent. |
| POST | `/api/sessions/{id}/stop` | Stop the session. |
| GET | `/api/permissions` | Live permission requests (see caveat above). |
| POST | `/api/permissions/{id}` | `{behavior: allow\|deny, message?}`. |
| GET | `/api/search?q=` | Full-text search across sessions, deduplicated, capped at 40. |
| GET | `/api/meta` | Projects and installed agents, for the create form. |
| POST | `/api/sessions` | Create: `{projectId, goal, name?, agent?, newBranch?, useTerminal?}`. |
| GET | `/api/sessions/{id}/git` | Branch and staged/modified/untracked counts. |
| GET | `/api/sessions/{id}/log` | Last 10 commits in the session's worktree. |
| GET | `/api/sessions/{id}/urls` | URLs the daemon detected in the session's output. |
| POST | `/api/sessions/{id}/ack` | Acknowledge a completed or failed session. |
| GET | `/api/models?agent=` | Models for an agent, with context window and pricing. |
| GET | `/api/sessions/{id}/pane?lines=N` | The session's tmux pane, ANSI intact. |
| POST | `/api/sessions/{id}/keys?key=` | Send one allowlisted key (escape, tab, up, down, enter, ctrl-c, …). |
| GET | `/api/pair[?format=png]` | Pairing URL, or a QR PNG of it. |

### Four traps in the daemon's API worth knowing

All four cost real debugging time and none is visible from the message catalogue:

1. **`agents:list` replies under the key `harnesses`, not `agents`.** Reading the
   obvious key returns an empty list with no error, so the create form silently
   offered no agents.
2. **A model is not a top-level create field.** The daemon derives a session's
   `requestedModel` from `agent.options.model`; passing `model` at the top level is
   silently ignored. Verified by creating a session on `claude-haiku-4-5` and reading
   `requestedModel` back.
3. **`useTerminal` does not mean "terminal instead of agent".** On `session:create`
   it selects the tmux transport, which is the default; setting it to `false` selects
   Claude-only SDK mode. Passing `true` hoping for a shell yields a full agent session
   — the pane came up running Claude Code. A standalone shell is a different message:
   `project:create-terminal`.
4. **Session search answers from three sources, each marking its own completion.**
   `metadata` and `messages` return in ~4ms already flagged `done: true`, while
   `jsonl` streams a frame per matching transcript for seconds afterwards. Stopping
   at the first `done` ends the search before the source that finds anything has
   started — it returned 0 results in 4ms instead of 36 in 3s. `CallStream` instead
   waits until every source seen so far is done and the wire has been idle briefly,
   so a new upstream source cannot silently truncate results.
