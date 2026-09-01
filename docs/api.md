# Configuration and HTTP API

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

## Notifications

Turn them on per device in settings. Three parties have to agree — the browser grants
permission, its push service issues a subscription, and this server keeps it and signs
each message — so the setting reports which step failed rather than a generic "off".

The VAPID keypair is generated on first use and kept in
`~/Library/Application Support/xirp-remote/push.json`, mode `0600`. It must persist:
a new keypair silently invalidates every existing subscription. Subscriptions that keep
failing are dropped after five attempts, and a `404` or `410` removes one immediately —
that is the push service saying the browser is gone.

The watcher asks the daemon nothing while no device is subscribed, so this costs nothing
until you switch it on. On iOS the app has to be installed to the home screen before the
browser will issue a subscription at all.

## HTTP endpoints

When a key is configured, endpoints take it as an `X-Xirp-Key` header or the
cookie from `POST /api/auth`. In open mode no credential is needed.

| Method | Path | Purpose |
|---|---|---|
| GET | `/healthz` | Liveness. No auth. |
| POST | `/api/auth` | `{key}` → sets cookie. |
| GET | `/api/sessions` | Projected session list. |
| GET | `/api/sessions/{id}?limit=N` | Session plus transcript. `transcript=0` returns the session alone, which is what the terminal view polls: parsing a transcript spawns a `node` process for something that view does not draw. |
| POST | `/api/sessions/{id}/message` | `{text}` → drives the agent. |
| POST | `/api/sessions/{id}/stop` | Stop the session. |
| GET | `/api/permissions` | Live permission requests (see the caveat in [`protocol.md`](protocol.md#why-there-is-no-remote-approval-of-permission-prompts)). |
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
| GET | `/api/diagnostics` | Daemon, tmux, database and module state. |
| GET | `/api/logs?level=&limit=` | Daemon log records at or above a pino level. |
| GET | `/api/sessions/{id}/changes` | Changed files: uncommitted and branch-vs-base, plus the PR. |
| GET | `/api/sessions/{id}/diff?path=&mode=` | One file's unified diff (`worktree` or `branch`). |
| POST | `/api/sessions/{id}/fork` | `{newBranch?, forkIntoWorktree?, agent?}`. |
| POST | `/api/sessions/{id}/swap` | `{agent, reason?}`. |
| GET | `/api/push/key` | VAPID public key, generated on first use. |
| POST | `/api/push/subscribe` | Store a browser subscription. |
| POST | `/api/push/unsubscribe` | Forget one, or all. |
| POST | `/api/push/test` | Send a notification now, to prove the chain. |
| GET | `/api/restorable` | Sessions left needing a restore-or-dismiss decision. |
| POST | `/api/restore` | `{restore: [], dismiss: []}`. |
| POST | `/api/sessions/{id}/rename` | `{name}` or `{regenerate: true}`. |
| GET | `/api/sessions/{id}/file?path=` | One file from the session's checkout. `files:stat` runs first, so the answer names a directory, a missing path or a file over 5 MB instead of reading it. |
| GET | `/api/prompts` | The saved prompts kept in Xirp's own settings. |
| POST | `/api/prompts` | `{name, prompt}` adds, `{id, name, prompt}` replaces, `{delete: id}` removes. |

## Traps in the daemon's API worth knowing

All of these cost real debugging time and none is visible from the message catalogue:

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
5. **A typed error frame is not in the catalogue.** `api:describe` reports
   `responseTypes: ["git:status","git:error"]` for `git:status` and `["git:log","git:error"]`
   for `git:log`, and no `git:error` at all for `git:fileDiff`, `git:branchDiff` or
   `git:branchFileDiff`. In the source every one of them routes through
   `resolveGitCwdOrSendError`, which answers `git:error` with `code:"DIRECTORY_MISSING"`
   when the worktree directory is gone. Trusting the catalogue means the call waits out
   its timeout: 75 seconds for this app's changes screen, which makes three of them.
   `git:branchPR` is the only git request here that cannot answer it.
6. **`files:read` reports failure inside the success frame.** It answers
   `{type:"files:read", path, error}` with no `content`, so a client that treats the
   frame as a success renders a missing file as an empty one. `files:stat`, added in
   0.20.1, is the cheap way to tell "missing" from "a directory" from "too big to read".
7. **`session:agent-swapped` carries no session object**, only `sessionId`, `from` and
   `to`. The daemon broadcasts `session:updated` with the row immediately before it, but
   a call that waits for one type discards the other, so the row has to be read back.
8. **The daemon answers an expired permission request with silence.** For a request it
   no longer holds, `permission:respond` writes a debug line and sends nothing at all.
   A request lives for about 500 ms, so that is the normal case from a phone: the call
   gives up in 3 seconds and says the request expired rather than reporting a timeout.
9. **A saved prompt has exactly six allowed keys and one timestamp format.** `id`,
   `name`, `prompt`, `projectId`, `createdAt`, `updatedAt`, nothing else, with the times
   matching JavaScript's `toISOString()` byte for byte. Go's `time.RFC3339Nano` is
   refused because it prints nanoseconds. The daemon rejects the whole write with the
   single word `validation`, which names neither the field nor the reason, and it refuses
   every write while the stored list holds an entry it considers invalid. Such a list
   reads back from `chirp:savedPrompts:get` as an empty one.
