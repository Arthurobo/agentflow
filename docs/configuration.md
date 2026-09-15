# Configuration

agentflow is configured with environment variables. It reads them from the
environment and from `~/.config/agentflow/agentflow.env`.

## The env file

`agentflow start` creates `~/.config/agentflow/agentflow.env` (mode `0600`) if
it doesn't exist, with every setting commented out at its default. It never
rewrites an existing file, except that `agentflow remote on|off` sets the
`AF_REMOTE` line. The same text is in [`.env.example`](../.env.example).

Every `agentflow` command reads the file when it starts, in the foreground and
under the service manager alike; the service definition does not load it
separately. The rules:

- `KEY=VALUE` per line; blank lines and lines starting with `#` are ignored; an
  `export ` prefix and one layer of matching quotes are removed.
- `$HOME` and `${HOME}` are expanded. No other variables are.
- A variable already set in the environment wins over the file.

After editing, run `agentflow restart` (the daemon reads settings only when it
starts).

## Settings

### Local listener and storage

| Variable | Default | Meaning |
|---|---|---|
| `AF_ADDR` | `127.0.0.1:4344` | Address of the local HTTP listener (web UI and API). `agentflow serve --addr` overrides it. A non-loopback address makes plain HTTP reachable from your network; the daemon warns at startup. See below. |
| `AF_DB` | `~/.local/share/agentflow/agentflow.db` | SQLite database. The single-instance lock is `<AF_DB>.lock` next to it. `agentflow serve --db` overrides it. |
| `AF_DATA_DIR` | the directory holding `AF_DB` | Other state: `hook.secret`, `remote.json`, `run/agentflow.sock`, `cloudflare-tunnel.json`, `bin/cloudflared`, `tailscale/`, `tailscale-serve.json`, `account.json`, `machine-id`, `defaults/`. |
| `AF_UPLOADS_DIR` | `~/.local/share/agentflow/uploads` | Where phone attachments are stored. Not derived from `AF_DATA_DIR`. |
| `AF_RETENTION_DAYS` | `90` | Days the session index keeps a session nobody has touched. A prune runs once a day and deletes that session's indexed events, search documents and unreferenced content blobs; sessions with a live run are kept. `0` keeps everything; an unreadable value falls back to 90. |

### Engines

| Variable | Default | Meaning |
|---|---|---|
| `AF_CLAUDE` | `claude` on `PATH` | Path to the `claude` binary. |
| `AF_AGENT_BASE_URL` | `http://` + `AF_ADDR` (`http://127.0.0.1:<port>` when `AF_ADDR` has no host) | Base URL written into the instructions loop members get for reaching the local agent mail API. |

`opencode` is looked up on `PATH` when the daemon starts; if it isn't found,
the OpenCode engine is not registered until the next restart.

The background service runs with the `PATH` that `agentflow start` was run
with, recorded in the service definition. If you install an engine somewhere
new, run `agentflow start` again. `agentflow doctor` and `agentflow status`
look for engines in that recorded `PATH`.

### Remote access

| Variable | Default | Meaning |
|---|---|---|
| `AF_REMOTE` | `cloudflare` | `cloudflare` publishes through a Cloudflare named tunnel the account service (`AF_CLOUD_URL`) creates for this machine, at `https://<adjective>-<noun>-<4 digits>.useagentflow.xyz`; no sign-in needed, and Cloudflare can see the traffic. `tailscale` publishes through Tailscale (see `AF_TAILSCALE`); `off` disables remote access. While unset, a machine already set up with Tailscale (`<AF_DATA_DIR>/tailscale/tailscaled.state` or `<AF_DATA_DIR>/tailscale-serve.json` exists) keeps `tailscale`. Any other value is treated as `off`, with a warning. |
| `AF_TUNNEL_ADDR` | `127.0.0.1:4345` (Cloudflare); a free loopback port (system Tailscale) | Loopback address of the listener `cloudflared` or `tailscaled` forwards public requests to. Every provisioned Cloudflare tunnel's ingress points at `127.0.0.1:4345`, so change it only together with the tunnel. It must be a loopback IP address and port and must not be the `AF_ADDR` port; any other value is ignored, with a warning. Not used by the embedded Tailscale node. |
| `AF_TAILSCALE` | `embedded` | Tailscale only. `embedded` (default): agentflow runs its own Tailscale node (tsnet) in the daemon — no install, no root, just a one-time browser sign-in; URL `https://agentflow-<name>.<tailnet>.ts.net`. `system`: use the Tailscale already installed and signed in on this computer; agentflow adds one serve entry (HTTPS 8443, forwarded to it) and removes it on a clean shutdown, and it needs a one-time `sudo tailscale set --operator=$USER`. Any other value falls back to `embedded`, with a warning. An existing `<AF_DATA_DIR>/tailscale/tailscaled.state` from an earlier embedded node is reused. |
| `AF_REMOTE_MODE` | `funnel` | Tailscale only. `funnel`: public HTTPS through Tailscale Funnel (tailnet devices can use the same URL). `tailnet`: only devices in your tailnet. Any other value is treated as `tailnet`, with a warning. |
| `AF_TS_HOSTNAME` | generated `agentflow-<6 hex>` | Embedded node only. Machine name in your tailnet, and so the first label of the URL. Must be lowercase letters, digits and dashes. The generated name is saved in `<AF_DATA_DIR>/tailscale/hostname` and reused. Changing the name changes the URL, and every device has to pair again (its token is stored per origin). |
| `AF_TS_LOGS` | `off` | Embedded node only. `on` lets the Tailscale client upload its logs to Tailscale. Otherwise the daemon sets `TS_NO_LOGS_NO_SUPPORT=true` before the node starts. |
| `AF_CLOUD_URL` | `https://cloud.useagentflow.xyz` | The account service that provides this machine's Cloudflare tunnel (with `AF_REMOTE=cloudflare`, no sign-in) and, after `agentflow account login`, emails this machine's address and lists it on the dashboard. Plain `http` is refused except to a loopback address. |
| `AF_STUN_URLS` | `stun:stun.l.google.com:19302` | Comma-separated STUN servers offered for the direct phone-to-computer attachment channel. `turn:` and `turns:` entries are dropped (media stays peer to peer). |

With Cloudflare the URL is `https://<adjective>-<noun>-<4 digits>.useagentflow.xyz`,
the same across restarts. The daemon asks the account service for it at every
start and every 6 hours, and keeps the last answer so the tunnel still comes up
while the service can't be reached. `cloudflared` on `PATH` is used when there
is one; otherwise agentflow downloads the pinned release (2026.9.1), checks its
SHA-256 and keeps it in `<AF_DATA_DIR>/bin/cloudflared`.

With the system Tailscale the URL is `https://<machine>.<tailnet>.ts.net:8443`
(this computer's own name). With the embedded node it is
`https://agentflow-<6 hex>.<tailnet>.ts.net`.

### Phone attachments

Values that don't parse, or aren't positive, keep the default.

| Variable | Default | Meaning |
|---|---|---|
| `AF_UPLOADS_MAX_BYTES` | `10485760` (10 MB) | Largest single file. |
| `AF_UPLOADS_SESSION_BYTES` | `209715200` (200 MB) | Total bytes per session. |
| `AF_UPLOADS_SESSION_FILES` | `100` | Files per session. |
| `AF_UPLOADS_TOTAL_BYTES` | `2147483648` (2 GB) | Total across all sessions. |
| `AF_UPLOADS_RATE_PER_MINUTE` | `20` | Uploads per run per minute. |
| `AF_UPLOADS_RETENTION_DAYS` | `7` | Days before an attachment is deleted. The sweep runs at startup and every 6 hours. |

### Development

| Variable | Default | Meaning |
|---|---|---|
| `AF_DEV_CORS_ORIGIN` | unset | One extra origin (for example `http://localhost:3000` for `next dev`) that may call the API from a browser and open WebSockets. |

### Installer and `agentflow update`

These are read by `install.sh` and `agentflow update`, not by the daemon.

| Variable | Used by | Meaning |
|---|---|---|
| `AGENTFLOW_VERSION` | `install.sh`, `agentflow update` | Release tag to install, for example `v0.5.0`. Default: the latest release. |
| `AGENTFLOW_RELEASE_BASE` | `install.sh`, `agentflow update` | URL that directly holds the release files (archive, `checksums.txt`, `.sig`, `.pem`). Default: the GitHub release download URL. |
| `AGENTFLOW_NO_START` | `install.sh` | `1` installs the binary without running `agentflow start`. |

## Local only

With `AF_REMOTE=off` there is no tunnel. The web UI is at
`http://127.0.0.1:4344`, and `agentflow pair` prints a link on that address
("Open this on this computer to pair its browser"), which only works in a
browser on the same computer. Everything else (terminals,
loops, the CLI) works the same.

To reach the daemon from other devices without a tunnel you can set `AF_ADDR`
to a LAN address (for example `192.168.1.20:4344`), and `agentflow pair` then
prints a link on that address. Traffic, including device tokens, is then plain
HTTP on your network, and some browser features (camera access for scanning a
QR code on the pairing page) need HTTPS. With `AF_ADDR=0.0.0.0:4344` the
listener accepts any IP address in the `Host` header, but the pairing link still
points at `127.0.0.1`. `AF_REMOTE_MODE=tailnet` is the safer way to keep access
private.

## Paths at a glance

| Path | What |
|---|---|
| `~/.config/agentflow/agentflow.env` | settings |
| `~/.local/share/agentflow/agentflow.db` (+ `-wal`, `-shm`, `.lock`) | database and instance lock |
| `~/.local/share/agentflow/hook.secret` | approvals hook secret |
| `~/.local/share/agentflow/remote.json` | last remote-access status |
| `~/.local/share/agentflow/run/agentflow.sock` | admin socket for the CLI |
| `~/.local/share/agentflow/cloudflare-tunnel.json` | Cloudflare tunnel (mode `0600`): the provision secret, the hostname and the last tunnel token |
| `~/.local/share/agentflow/bin/cloudflared` | `cloudflared` downloaded by agentflow (pinned release, SHA-256 verified) when none is on `PATH` |
| `~/.local/share/agentflow/tailscale/` | embedded Tailscale node state and generated hostname |
| `~/.local/share/agentflow/tailscale-serve.json` | the serve entry agentflow added to the system Tailscale |
| `~/.local/share/agentflow/account.json` | email sign-in: email and session token for the account service |
| `~/.local/share/agentflow/machine-id` | random id this machine is known by on the account service (for its tunnel and, once signed in, its link) |
| `~/.local/share/agentflow/defaults/` | read-only copy of the built-in loop plays and rules |
| `~/.local/share/agentflow/uploads/` | phone attachments |
| `~/.config/systemd/user/agentflow.service` | Linux service |
| `~/Library/LaunchAgents/com.arthurobo.agentflow.plist` | macOS service |
| `~/Library/Logs/agentflow/agentflow.log` | macOS service log (Linux: `journalctl --user -u agentflow`) |

## Checking the result

```sh
agentflow restart   # apply changes
agentflow status    # service, database, daemon, remote access, email sign-in, engines
agentflow doctor    # PASS/FAIL checks; exits 1 if any fail
```
