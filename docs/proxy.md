# Putting it behind a reverse proxy

Optional, and worth it for TLS and a memorable hostname. Three notes from running it
this way:

- Give it **its own config file** rather than appending to a shared routes file.
  Appending can silently land a router definition inside a `services` block, where it
  is ignored and the URL just 404s.
- If the machine is a **laptop, define two backends and health-check them.** A laptop
  has one address on its home network and a different one over a VPN, and typically
  only one of the two is reachable at a time. A single backend URL is therefore wrong
  half the time; with a health check on `/healthz` the proxy follows the laptop
  between locations with no edit. An example is in
  [`deploy/traefik-xirp-remote.yml`](../deploy/traefik-xirp-remote.yml).
- `/healthz` is unauthenticated on purpose, so an uptime monitor works even when a
  key is set.
- **Do not buffer `/api/events`, and do not time it out.** It is a server-sent event
  stream that stays open, sending a comment line every 25 seconds. The bridge sets
  `X-Accel-Buffering: no`, which nginx honours; Traefik does not buffer responses by
  default, so nothing is needed there. What does bite is a read or idle timeout shorter
  than the heartbeat: the phone reconnects every time it fires, and the app falls back to
  polling in between, so the symptom is "it works on the LAN and feels slow through the
  proxy". Give that route a timeout above 30 seconds.
