# Ten improvements, in the order worth doing them

Written 2026-09-03 against Xirp 0.22.0 and xirp-remote v0.4.0. All ten shipped in
v0.5.0; kept as the record of why each one was worth doing, and of the three
measurements that turned out to contradict the plan:

- `squab --since` saves bytes and not time, so the transcript work targets the
  5.2 MB pipe rather than the parse.
- `message:added` is not the transcript signal for a tmux session, so the cache
  is driven by the session row instead.
- `permission:request` is broadcast, which turned item 7 from pattern-matching a
  terminal blind into being told a prompt is live first.

Two decisions moved while building. Item 1 needed no new argument: the request
already carries the session id, so a reply simply has to be about it, and that
covered a fifth call nobody had noticed. And item 8 writes to a temp folder
rather than the worktree, which answers its own open question about polluting a
repository.

Four phases. Phase A is a defect and a waste, both small. Phase B is the one
change that alters how the app feels, and it needs A first. Phase C is cost.
Phase D is four features that need no daemon change.

## Receipts this plan rests on

Measured on this machine, 2026-09-03, against the running 0.22.0 daemon:

- **The daemon broadcasts what a live cache needs.** `session:updated` from 43
  call sites, `session:created` from 6, plus `session:deleted`,
  `session:creating`, `session:agent-swapped`, `session:title-generating`,
  `project:added`, `project:updated`, `project:removed`, `sessions:restorable`,
  `permission:request`, `permission:resolved`, `message:added`.
- **Six authenticated connections work at once**, on top of the desktop app's
  own, and each got `server:hello`.
- **A reply goes only to the connection that asked**, 1 of 6, so a pool keeps
  per-connection type matching correct.
- **`squab session-parse --since` returns 90x fewer bytes and saves no time.**
  A 1035-message session parses to 5,244,612 bytes in 0.12 to 0.23 s; with
  `--since` set to the 30th-from-last timestamp it returns 29 messages and
  57,781 bytes in 0.10 to 0.11 s. Every non-message field is identical and the
  returned tail matches the full parse's tail. Node startup and reading the
  JSONL dominate, so the win is bytes moved between squab and the bridge.
- **`message:added` is not the transcript signal.** It fires when the daemon
  records a message itself, which for tmux sessions it does not: that is why
  squab exists. `session:updated` is the signal that a session moved.

## Phase A: a defect and a waste

### A1. Match the session id, not just the reply type

Four calls wait for `session:updated`, which the daemon broadcasts for every
session's every status change, so a stranger's frame satisfies the wait:

| Call site | Effect of the wrong frame |
|---|---|
| `main.go:495` stop | reports another session's status as the stopped one's |
| `features.go:620` rename | returns another session's row, which the UI writes into the header |
| `features.go:604` regenerate title | same |
| `features.go:234` acknowledge | reports success on someone else's update |

Change: `Call` takes an optional expectation, `wantSessionID`, and keeps reading
when a frame of the wanted type carries a different `session.id` or `sessionId`.
The four sites pass the id they asked about. Everything else passes nothing and
behaves as today.

- Files: `daemon.go`, `main.go`, `features.go`, `daemon_test.go`, `handlers_test.go`.
- Verified by: a test that sends session B's `session:updated` first and then
  session A's, and asserts the stop handler reports A's status. Red before the
  change.
- Size: small. Roughly 40 lines and four tests.

### A2. Stop polling for permission requests

`refreshApprovals()` runs at the end of every `refreshList()`, so an open app
asks `permission:list` every 5 seconds: about 720 daemon calls an hour for a
queue the daemon's own 500 ms cap keeps empty. Phase D1 replaces this properly;
until then, ask once when a session opens.

- Files: `web/app.js`.
- Verified by: recording the paths the app requests over 20 seconds and
  asserting `/api/permissions` appears zero times on the list screen.
- Size: small. One call site.

## Phase B: subscribe to the daemon instead of polling it

This is the change that alters how the app feels. Do it after A1, because
concurrency makes cross-talk more likely, not less.

### B1. A pool of connections

`Client` holds one connection under one mutex, so every request queues behind
every other. A slow `git:branchFileDiff` blocks the session list for its whole
duration, and the push watcher's `sessions:list` blocks it every 20 seconds.

Change: `Client` keeps N connections, each with its own mutex, and `Call` takes
the first free one. Type matching stays per connection, which the 1-of-6
measurement above says is correct. N = 4, sized to the app's own concurrency:
one phone's detail poll, its pane poll, the push watcher, and one spare. The
message when the pool is exhausted names N and waits rather than failing.

- Files: `daemon.go`, `daemon_test.go`.
- Verified by: a test that starts 4 slow calls and asserts the 5th waits rather
  than crossing replies, plus a test that two concurrent `session:get` calls for
  different ids each get their own answer.
- Size: medium. The dial, drop and reconnect logic becomes per connection.

### B2. A subscriber connection and an in-memory store

One more connection, used for nothing but reading. A goroutine applies
broadcasts to a store: sessions by id, projects by id, the restorable list, live
permission requests.

Rules that matter:

- On connect, and after any read error, fetch `sessions:list` and
  `projects:list` in full, then apply broadcasts. A gap must never leave a
  stale row.
- Resync in full every 60 seconds anyway, and count the rows that differ from
  what the store held. That counter goes in `/api/diagnostics`: it is the only
  way anyone will notice drift.
- The store is the answer to `/api/sessions`, not a cache in front of it.

- Files: new `events.go`, `daemon.go`, `diagnostics.go`, new `events_test.go`.
- Verified by: a test that applies a scripted `session:updated`,
  `session:created` and `session:deleted` and asserts the store matches; a test
  that a dropped connection triggers a full resync; a test that the drift
  counter rises when the fake daemon's list disagrees with the store.
- Size: large. This is the centre of the phase.

### B3. Serve the list from the store

`/api/sessions` becomes a map read plus the tmux overlay. `projects:list`,
`modules:list` and both tmux calls leave the request path. In the steady state
an open phone costs the daemon nothing until something happens.

- Files: `main.go`, `diagnostics.go`, `handlers_test.go`.
- Verified by: the existing session-list tests, plus a test asserting the
  handler makes zero daemon calls when the store is warm.
- Size: small, once B2 exists.

### B4. Server-sent events to the phone

`GET /api/events` streams what the store changed. The phone applies the frames
and keeps the 5 second poll only as the reconnect fallback, because a PWA loses
its connection whenever the phone sleeps.

Things that will bite:

- A reverse proxy buffers SSE unless told not to. `docs/proxy.md` needs the
  `X-Accel-Buffering: no` note and the read-timeout note.
- iOS suspends the connection on background. The client must resync on
  `visibilitychange`, which the app already does for polling.
- Every open phone holds a connection. Cap it, name the cap in the refusal, and
  count the open streams in diagnostics.

- Files: new `stream.go`, `web/app.js`, `docs/api.md`, `docs/proxy.md`.
- Verified by: a Go test reading two frames off the stream through
  `httptest.NewServer`; in the browser, asserting the list updates with the
  poll interval set to zero.
- Size: medium.

## Phase C: stop moving 5 MB every four seconds

### C1. Cache the transcript in the bridge and top it up with `--since`

The phone already receives a projected tail, so the 5.2 MB moves between squab
and the Go process only, every 4 seconds per open session. Keep the parsed
messages per session in the bridge, ask squab only for what is newer than the
last message held, and drop the cache when `session:updated` says that session
moved. The HTTP response shape does not change, so the front end does not
change at all.

Bound it: sessions by last use, and a total byte budget across the cache, both
named in the refusal and reported in diagnostics. Measure before choosing the
numbers, per the rule about limits.

- Files: `daemon.go`, new `transcript.go`, `handlers_test.go`.
- Verified by: a test with a fake squab, via `CHIRP_SQUAB_PATH`, asserting the
  second read passes `--since` with the newest timestamp and that the merged
  result equals a full parse; a measurement of bytes read per poll before and
  after.
- Size: medium. The merge is where the bugs will be: duplicate ids at the
  boundary, and a session whose file was rewritten.

## Phase D: four features that need no daemon change

### D1. Live permission requests, answered through the pane

`permission:request` is broadcast, so with B2 the bridge learns about a prompt
within milliseconds rather than missing it between 5 second polls. A human still
cannot beat the 500 ms cap, so `permission:respond` will usually report the
request as expired. That is the point of the design: the broadcast tells the app
a prompt is live and which tool it is for, and the pane is how it gets answered.
When respond reports expired, offer the numbered choices as keystrokes into the
pane.

The rule: show nothing when the app is not certain what is on screen. A wrong
button on a permission prompt is worse than no button.

- Files: `events.go`, `stream.go`, `web/app.js`, `docs/protocol.md`.
- Verified by: a Go test asserting a broadcast request reaches the stream; a
  manual check per agent, because the prompt's shape is the agent's, not Xirp's.
- Size: medium, and the manual part does not shrink.

### D2. Send a photo into a session

On a phone the natural thing to want is "here is a screenshot of the bug". The
daemon has no upload, so the bridge writes the file into the session's worktree
and types its path into the pane with `session:message`.

Decisions to make before writing it: where the file lands, whether it is
`.gitignore`d, who deletes it, and the size cap. My proposal: a
`.xirp-remote/` directory in the worktree, a 10 MB cap named in the refusal, and
no automatic deletion, because deleting a file an agent may still be reading is
worse than leaving it.

- Files: new `upload.go`, `main.go`, `web/index.html`, `web/app.js`, `docs/api.md`.
- Verified by: a test posting a small PNG and asserting the file exists under
  the worktree and that the pane got the path; a test that 11 MB is refused with
  the cap named.
- Size: medium.

### D3. Saved prompts in the create sheet

The goal field is where a saved prompt belongs, and the endpoint shipped in
v0.4.0.

- Files: `web/index.html`, `web/app.js`.
- Verified by: the CI id check, and a browser check that picking a prompt fills
  the goal.
- Size: small.

### D4. Show stale data instead of an error

An unreachable Mac gives an error page today. Keep the last session list per
host in `localStorage` and show it with an "as of 14:32" stamp that is never
hidden. The stamp is the whole feature: stale data without it is the bug this
app exists to avoid.

- Files: `web/app.js`, `web/style.css`.
- Verified by: a browser check with the bridge stopped, asserting the list still
  renders and the stamp shows.
- Size: small.

## Order, and what depends on what

```
A1 ──► B1 ──► B2 ──► B3
                │
                ├──► B4 ──► D1
                └──► C1
A2 ──────────────────► (replaced by D1)
D2, D3, D4 independent
```

A1 and A2 are worth doing on their own, this week. B is one coherent piece and
should land as one release, because B3 without B2 is nothing and B4 without B3
has nothing to stream. C1 and D can follow in any order.

## Risks

- **The store drifts from the daemon.** A broadcast this plan did not account
  for, or a frame lost in a gap, and the phone shows a session that ended. The
  60 second full resync plus the drift counter is the guard; if the counter is
  ever non-zero in normal use, the event rules are wrong and polling should come
  back until they are right.
- **A pool multiplies the reply-crossing bug**, which is why A1 comes first.
- **SSE behind a proxy** silently buffers. It will look like the stream works on
  the LAN and fails through the tunnel.
- **The transcript merge** has an off-by-one at the `--since` boundary. The test
  compares against a full parse for exactly that reason.
- **D1 pattern-matches an agent's TUI.** It will break when an agent changes its
  prompt. It must fail closed, showing nothing.

## Open questions

1. Does B4 replace the poll, or run beside it permanently? I would keep the poll
   as the fallback and never remove it, because a PWA on a sleeping phone cannot
   rely on a stream.
2. Is D2 worth the worktree pollution? It writes files into a repo an agent is
   working in, which is a real cost for a feature nobody has asked for yet.
3. Should `/api/events` require the key on the URL? EventSource cannot set
   headers, so a cross-origin machine would need the key as a query parameter,
   which is exactly what the pairing design avoids for the fragment. A cookie
   works for the local host only.
