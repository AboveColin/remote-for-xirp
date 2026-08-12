# Running it as a service

The [README](../README.md#install) covers the two commands you need. This is the
rest: every CLI subcommand, how to pin which address it serves on, and why the
service is a LaunchAgent rather than a LaunchDaemon.

## Installing

No build needed — download the release:

```sh
curl -fsSL https://raw.githubusercontent.com/AboveColin/remote-for-xirp/main/install.sh | sh
```

That drops a universal binary (Apple Silicon and Intel) into `~/.local/bin` and stops.
Nothing starts listening because you piped a script into a shell; starting it is one
explicit command:

```sh
xirp-remote interfaces                # which address can a phone reach?
xirp-remote install --generate-key    # start it, and print a QR code to scan
```

`install` writes a launchd user agent, so it comes back at login and after a crash.

<details>
<summary>Other ways in</summary>

```sh
go install github.com/AboveColin/remote-for-xirp@latest   # binary is named xirp-remote
```

or from a clone:

```sh
go build -o xirp-remote .
./xirp-remote install
```

The release binaries are unsigned. `install.sh` fetches them with curl rather than a
browser, so Gatekeeper does not quarantine them; if you download by hand in a browser,
run `xattr -d com.apple.quarantine xirp-remote` first.

</details>

The command is `xirp-remote`; the repository is `remote-for-xirp`.

| Command | Does |
|---|---|
| `install [--addr H:P] [--key K] [--no-copy]` | Copy the binary to `~/.local/bin`, write a LaunchAgent, start it, and wait until `/healthz` answers. Re-running it upgrades in place. |
| `uninstall` | Stop and remove the agent, leaving the binary and log. |
| `start` / `stop` / `restart` | Control the running agent. |
| `status` | Service, HTTP and Xirp daemon state, plus the session count. |
| `logs [-f]` | Tail `~/Library/Logs/xirp-remote.log`. |
| `qr` | Print a pairing QR code and its URL. |
| `interfaces` | List the addresses this can serve on, marking the default route. |
| `version` | Print the version. |

By default it listens on every interface and the pairing code advertises whichever
address holds the default route. On a laptop with Wi-Fi, a VPN and a few virtual
bridges that guess can be wrong, so it can be pinned:

```sh
xirp-remote interfaces                       # see what is available
xirp-remote install --interface en0          # serve on that interface only
xirp-remote install --bind 192.168.1.50      # or on one address
```

The bound address is also the one encoded in the pairing QR, so the code always points
at somewhere the phone can actually reach.

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
