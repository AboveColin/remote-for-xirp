# How it talks to Xirp

The daemon listens on `127.0.0.1` only and authenticates with a token passed as a
query parameter (`ws://127.0.0.1:<port>/?token=<token>`). Its protocol is a typed
WebSocket API with 223 message types, self-describing through `api:list` and
`api:describe`, which is worth using if you extend this. The count is what `api:list`
returns on Xirp 0.22.0 with every module of the app edition active.

Three facts shape the implementation (more traps are in [`api.md`](api.md#traps-in-the-daemons-api-worth-knowing)):

1. **Neither the port nor the token is stable.** The token is minted on each app
   launch and injected into the environment of the app's own processes; it is not
   written to disk. `discover()` in `daemon.go` reads it back out of the running
   process environment via `ps -E`, which only works for processes owned by the
   same user. So the bridge must run as the user running Xirp, and it rediscovers
   credentials on every reconnect rather than caching them. Restarting Xirp does
   not require restarting the bridge.

2. **Replies are matched by `type`, not by a request id.** Two consequences, and the
   second cost real time to find.

   A socket can carry one call at a time, or two calls would read each other's answers.
   So the client keeps six sockets and takes a free one per call. Six is this app's own
   peak demand, two per open phone for the detail and the pane plus the watcher and a
   resync, and it is what the daemon was measured to allow: six authenticated clients at
   once alongside the desktop app, each answered on the socket that asked. One socket
   under one mutex was the earlier design, and it made a 30-second branch diff block the
   session list for its full duration.

   And the type alone is not enough to know a reply is yours. The daemon broadcasts
   `session:updated` from 43 places, `session:urls` and `session:created` too, so another
   session's status change satisfied a wait: stopping session A reported session B's
   status, and renaming A returned B's row. A reply now has to be about the session the
   request named, with `session:created` the one exception, because a fork asks about the
   source and is answered with the copy.

3. **Failures arrive in three shapes, and two of them are easy to miss.** A rejected
   request answers `{type:"error", originalType:<request>}`. A git request whose
   worktree directory is gone answers `git:error` instead, and a failed
   `session:swap-agent` answers `session:swap-agent:error`, both carrying a `code` and
   sometimes a `hint` written for the person reading it. A client that waits only for
   the success type waits out its whole timeout and then reports the wrong reason: this
   one waited 75 seconds on the changes screen before those types were named. Some
   module handlers report a third way, inside the success frame: `files:read` answers
   its own type with an `error` field and no content. `Client.Call` takes the error
   types per call for that reason, and the file read checks the field.

Transcripts do not come from the daemon. `messages:list` reads the daemon's own
database, which is empty for harness-driven sessions; the canonical reader is
`squab session-parse` (schema `squab.session-parsed/v1`), the orchestrator CLI
shipped inside the app bundle.

It is read once per session and topped up with `--since` after that, because a whole
read is every message in the session: 5,244,612 bytes for a 1035-message one, against
57,781 for the same session's newest 29. Three measured facts decide the merge. `--since`
is strictly after the timestamp given, so appending what comes back is correct. Message
ids are not unique, since a tool call and its result share one, so merging by id drops
every tool result. And `messageCount` counts the whole session in every answer, including
a `--since` answer, so kept plus dropped has to equal it, and when it does not, something
rewrote the file and the honest answer is to read it whole again.

## The daemon says what changed, so nothing needs polling

It broadcasts to every connected client: `session:updated` from 43 call sites,
`session:created`, `session:deleted`, `project:added`, `project:updated`,
`project:removed`, `sessions:restorable`, `permission:request` and
`permission:resolved`. A call reads until it sees the type it asked for and skips the
rest, so all of that used to be discarded and the phone polled instead.

One socket follows those broadcasts into a store, and the store answers the session list,
the project names, the restorable set and the live permission requests. A full resync
runs on connect and every minute, and it counts the rows it had to correct: that number
is in `/api/diagnostics`, and anything but zero means a broadcast was missed and the
phone was shown something untrue.

The frames are worth reading cheaply. The daemon broadcasts one per database query, so
the watcher reads the `type` on its own first and decodes nothing else unless it is one
of the nine it keeps.

## Why there is no remote approval of permission prompts

`permissionService.waitForDecision` caps its wait at `Math.min(timeout, 500)`
milliseconds before falling through to the agent's own dialog, so a request is gone about
half a second after it appears. No human beats that, and no amount of polling helps.

Two things make it useful anyway. The daemon broadcasts `permission:request`, so the
prompt reaches the phone in milliseconds rather than being missed between polls: that is
what the old `permission:list` poll could never do, since it asked every five seconds for
a queue guaranteed to be empty. And once the window has passed, the agent's own dialog is
on screen in the pane, so the app reads the numbered menu the agent drew and types the
number. It reads the digits without knowing what any of them mean, offers nothing when it
cannot see a menu, and only looks while the daemon says a request is live, which is what
stops it offering buttons for a numbered list an agent merely printed.
