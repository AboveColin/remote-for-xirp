# Security

## What this can do

This service can type into agent sessions and create new ones. That is arbitrary code
execution on the machine running Xirp, as the user running it. Treat access to it as
equivalent to a terminal on that machine.

## How it is contained

- **An access key, if you set one.** `xirp-remote install --generate-key` requires one;
  without it the service runs open. Keys are compared in constant time, wrong keys are
  delayed, and the key is held in an `HttpOnly` `SameSite=Lax` cookie.
- **A narrow allowlist.** Sixteen of the daemon's 212 message types are reachable. Git
  writes, terminal attach, session deletion and every settings mutation are not proxied.
  Control keys sent to a pane are allowlisted by name.
- **Cross-origin only with a key.** Origins are echoed only when a key is required, so an
  open instance cannot be driven by a page you happen to visit.
- **No API caching.** The service worker caches the UI shell and never API responses.

## How to run it safely

Put it on a network you trust — a LAN, or a VPN — and do not give it a public hostname
without an access key. It listens on `0.0.0.0:8790` by default; `--addr` narrows that.

## Reporting something

Open an issue. If you would rather not do that in public, say so in an issue without
details and we can take it from there.
