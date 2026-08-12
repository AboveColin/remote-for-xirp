# How it talks to Xirp

The daemon listens on `127.0.0.1` only and authenticates with a token passed as a
query parameter (`ws://127.0.0.1:<port>/?token=<token>`). Its protocol is a typed
WebSocket API with 212 message types, self-describing through `api:list` and
`api:describe` — worth using if you extend this.

Two facts shape the implementation (four more traps are in [`api.md`](api.md#four-traps-in-the-daemons-api-worth-knowing)):

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

## Why there is no remote approval of permission prompts

This was the original motivation and it does not work against this daemon.
`permissionService.waitForDecision` caps its wait at
`Math.min(timeout, 500)` milliseconds before falling through to the agent's own
dialog. A pending request is therefore gone about half a second after it appears,
which no polling remote client can catch. The `/api/permissions` endpoints are
implemented and correct, and the UI renders a request if one is genuinely live,
but this is not an approval queue. Approving from your phone would need the app
to hold requests open longer.
