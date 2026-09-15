# agentflow

[![CI](https://github.com/arthurobo/agentflow/actions/workflows/ci.yml/badge.svg)](https://github.com/arthurobo/agentflow/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/arthurobo/agentflow.svg)](https://pkg.go.dev/github.com/arthurobo/agentflow)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

agentflow is a background daemon that runs coding-agent terminals
([Claude Code](https://docs.claude.com/en/docs/claude-code) and
[OpenCode](https://opencode.ai)) in real PTYs on your computer and lets you
drive them from your phone's browser.

Start a terminal from your phone, leave it running, and come back to the same
terminal later from any paired device. agentflow does not replace the agents: it runs
the `claude` and `opencode` you already have, logged in as you, and serves a
web UI (built into the binary) for talking to them.

- **A stable address with nothing to set up.** By default the agentflow
  account service gives this machine its own Cloudflare tunnel at
  `https://<adjective>-<noun>-<4 digits>.useagentflow.xyz`; no account and no
  sign-in are needed. Cloudflare terminates TLS for that address and can see
  the traffic. To get the tunnel the account service learns this machine's
  random id, its hostname and a hash of a secret the machine keeps.
- **Or your own Tailscale account.** `AF_REMOTE=tailscale` uses
  [Tailscale](https://tailscale.com) instead: agentflow's built-in node
  (`https://agentflow-<random>.<tailnet>.ts.net`) or the Tailscale already
  installed on this computer (`https://<machine>.<tailnet>.ts.net:8443`).
  Tailscale can't see the traffic, and nothing goes through servers run by
  this project.
- **Only paired devices see anything.** To anyone who hasn't paired, the
  public address shows nothing but the pairing page; everything else is a
  blank 404. Set `AF_REMOTE=off` (or run `agentflow remote off`) to keep
  everything on this computer.
- **Per-device access.** There is no password. Each phone or browser pairs
  once, with a one-time QR code or by tapping **Request access** and approving
  it on the computer, and gets its own revocable token.
- **Optional email sign-in.** Sign in with an email and agentflow emails you
  this machine's address when it changes and lists it on a dashboard. Press
  Enter to skip it; everything works without it. The account service only
  learns your email, the machine's id, name, transport, address and agentflow
  version; never tokens or terminal data.
- **Runs in the background.** A systemd user service on Linux, a launchd agent
  on macOS.

> A paired device can run any command as you: agent terminals run with
> `claude --dangerously-skip-permissions` (OpenCode: `--auto`). Pair only
> devices you control, and read [SECURITY.md](SECURITY.md).

## Requirements

- Linux or macOS, amd64 or arm64.
- `claude` and/or `opencode` installed, logged in, and on your `PATH`.
- Nothing else for the default remote access. `AF_REMOTE=tailscale` needs a
  Tailscale account.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/arthurobo/agentflow/main/install.sh | sh
```

The installer downloads the release archive for your OS and CPU, verifies its
SHA-256 against the release's `checksums.txt` (and, if `cosign` is installed,
the signature on `checksums.txt`; a failed signature aborts the install),
installs `~/.local/bin/agentflow` and runs `agentflow start`.

Installer options: `AGENTFLOW_VERSION=v0.5.0` pins a release,
`AGENTFLOW_NO_START=1` installs without starting.

**From source.** `make build` builds `./bin/agentflow` with the web UI
embedded; it needs Go 1.26 and Node.js (the CI uses Node 22).
`go install github.com/arthurobo/agentflow/cmd/agentflow@latest` also works,
but that binary has **no web UI**: the web export is not in the Go module, so
the daemon serves a page saying "Web UI not included in this build". Use a
release binary or `make build` for a usable UI.

## Quick start

```sh
agentflow start
```

`agentflow start`:

1. writes `~/.config/agentflow/agentflow.env` (every setting commented out at
   its default) if it doesn't exist;
2. unless you're already signed in, offers the optional email sign-in (press
   Enter to skip; otherwise a 6-digit code is sent to it);
3. installs or refreshes the agentflow service and starts it;
4. follows remote access as it comes up: the Cloudflare tunnel (the default)
   needs nothing from you; with `AF_REMOTE=tailscale` it walks you through
   whatever Tailscale still needs (installing it, signing in, a one-time
   permission command, or a one-click Funnel approval), each with a link and
   QR code;
5. waits until the public address answers;
6. prints a pairing QR code, then keeps asking about **Request access**
   requests until you press Ctrl-C.

The phone opens the web UI in its normal browser; you can add it to the home
screen. Run `agentflow pair` for each additional device (a pairing link works
once and expires after 15 minutes), or open the address on the phone, tap
**Request access**, and approve the 4-digit code with
`agentflow approve <code>`. Scripts can use `agentflow start --email ADDR`
(the code is still read from stdin) or `--no-email`; without a terminal and
without `--email`, no email is asked for.

Ctrl-C at any step is safe: the service keeps running, and
`agentflow remote status` shows where it is.

On Linux, user services stop when you log out unless lingering is on;
`agentflow start` tells you when to run `loginctl enable-linger $USER`.

## Remote access modes

| Setting | Address | Reachable from | Phone needs |
|---|---|---|---|
| `AF_REMOTE=cloudflare` (default) | `https://<adjective>-<noun>-<4 digits>.useagentflow.xyz` | the internet, through Cloudflare | a browser |
| `AF_REMOTE=tailscale`, `AF_REMOTE_MODE=funnel` | `https://agentflow-<name>.<tailnet>.ts.net` (embedded, default) or `https://<machine>.<tailnet>.ts.net:8443` (`AF_TAILSCALE=system`) | the internet, through Tailscale Funnel, and your tailnet | a browser |
| `AF_REMOTE=tailscale`, `AF_REMOTE_MODE=tailnet` | same | devices in your tailnet only | the Tailscale app, signed in to the same tailnet |
| `AF_REMOTE=off` | `http://127.0.0.1:4344` | this computer only | not applicable |

The Cloudflare tunnel is created for this machine by the account service
(`AF_CLOUD_URL`) the first time the daemon starts, and keeps its address across
restarts. agentflow runs `cloudflared` itself, downloading a pinned, SHA-256
verified release into `~/.local/share/agentflow/bin` when there is none on
`PATH`. A machine set up with Tailscale before Cloudflare became the default
keeps using Tailscale while `AF_REMOTE` is unset.

With `AF_REMOTE=tailscale` agentflow runs its **own** Tailscale node (`tsnet`)
by default — no install and no root, just a one-time browser sign-in it prints
at `agentflow start`. To use
the Tailscale already installed on this computer instead, set
`AF_TAILSCALE=system` (that mode needs a one-time
`sudo tailscale set --operator=$USER`, which `agentflow start` explains).

Settings live in `~/.config/agentflow/agentflow.env`; run `agentflow restart`
after editing. `agentflow remote on|off` sets `AF_REMOTE` to the default
transport (`cloudflare`, or `tailscale` on a machine already set up with
Tailscale) or `off`, and restarts the service for you. See [docs/configuration.md](docs/configuration.md).

**Hosted relay: coming soon.** An optional hosted relay, end-to-end encrypted
so the relay can't read or control your machine, is coming. It is not in this
release.

## Commands

| Command | What it does |
|---|---|
| `agentflow [serve] [--addr ADDR] [--db PATH]` | run the daemon in the foreground (the default with no command) |
| `agentflow start [--email ADDR \| --no-email]` | offer the optional email sign-in, install and start the background service, bring up remote access (and walk through Tailscale's steps with `AF_REMOTE=tailscale`), pair a phone, then approve access requests |
| `agentflow stop` | stop the service and keep it from starting at login |
| `agentflow restart` | restart the service |
| `agentflow status` | service, PID, local URL, database, daemon health, remote access, email sign-in, engines |
| `agentflow doctor` | check `claude`, `opencode`, the service, the email sign-in, the daemon and remote access |
| `agentflow pair` | print a one-time pairing link and QR code once remote access is running (or, with it off, a link for this computer); then ask about access requests |
| `agentflow devices` | list paired devices and access requests waiting for approval |
| `agentflow approve CODE` / `agentflow deny CODE` | approve or deny a **Request access** request by its 4-digit code or request id |
| `agentflow revoke ID` | revoke a device and close its live connections |
| `agentflow remote status` | show remote-access state, transport, address, and any install, sign-in, permission or approval step |
| `agentflow remote logout` | log agentflow's own Tailscale node out (agentflow never signs the computer's Tailscale out) |
| `agentflow account login` / `logout` / `status` | sign in with an email (a 6-digit code), sign out, or show the account and reporting state |
| `agentflow remote on` / `off` | set `AF_REMOTE` to the default transport (`cloudflare`, or `tailscale` on a machine already set up with it) / `off` and restart the service |
| `agentflow update` | download, verify and install the latest release, restart the service |
| `agentflow uninstall [--purge] [--yes]` | remove the service (and the binary if it is in `~/.local/bin`); `--purge` also deletes agentflow's data and settings |
| `agentflow version` | version, commit and build date |
| `agentflow make-resumable ID` | make a session show up in `claude -r` (backs up the transcript first) |
| `agentflow undo-make-resumable ID` | restore that transcript from its backup |

The CLI talks to the running daemon over a Unix socket in its data directory
(`~/.local/share/agentflow/run/agentflow.sock`, owner-only), so `pair`,
`approve`, `revoke` and `remote` work the same for the service and for
`agentflow serve`.

## Platforms

| | Linux | macOS |
|---|---|---|
| Release builds | amd64, arm64 | amd64, arm64 |
| Background service | systemd user unit (`~/.config/systemd/user/agentflow.service`) | launchd agent (`~/Library/LaunchAgents/com.arthurobo.agentflow.plist`) |
| Logs | `journalctl --user -u agentflow` | `~/Library/Logs/agentflow/agentflow.log` |

Other platforms have no release builds or service support.

## Uninstall

```sh
agentflow uninstall                 # remove the service; keep your data
agentflow uninstall --purge         # also delete data and settings (and log agentflow's own Tailscale node out)
agentflow uninstall --purge --yes   # don't ask
```

`uninstall` lists what it will remove and asks first. It removes the agentflow
service and deletes the binary only when it lives in `~/.local/bin`. `--purge` deletes only paths
agentflow names: the database and its `-wal`/`-shm` files, the lock file, the
hook secret, `remote.json`, `account.json`, `machine-id`,
`tailscale-serve.json`, `cloudflare-tunnel.json`, the downloaded
`bin/cloudflared`, the admin socket, the
reference copy of the built-in defaults, the embedded Tailscale state directory
(after logging that node out), the uploads directory and the env file, then
removes the directories it leaves empty. It never touches the project trust
entries it added to `~/.claude.json` or the `.agentflow-bak-*` transcript
backups written by `make-resumable`; remove those yourself if you want them
gone. It doesn't delete the account on the account service (sign in on the
dashboard to do that), and an embedded node stays listed in your Tailscale
admin console until you remove it there. The Cloudflare tunnel itself is not
deleted; a reinstall after `--purge` gets a new address.

## Local development

End users need **neither Go nor Node** — they install a prebuilt binary. Only
contributors building from source do: **Go 1.26** and **Node 22** (pinned in
`web/.nvmrc`).

Run the daemon against an isolated, gitignored data dir so development never
touches your real install's database, port, or service:

```sh
make dev        # daemon on 127.0.0.1:4400, data under ./.dev, remote off
```

For the web UI with hot reload, run the Next.js dev server and let it call the
dev daemon:

```sh
AF_DEV_CORS_ORIGIN=http://localhost:3000 make dev   # terminal 1: the daemon
make dev-web                                         # terminal 2: Next.js on :3000
```

Fast Go-only rebuilds (no UI): `make build-go`. Full binary with the embedded
UI: `make build`. Run what CI runs with `make test`, `make vet`, `make lint`.

## Documentation

- [Getting started](docs/getting-started.md)
- [Configuration](docs/configuration.md)
- [Architecture](docs/architecture.md)
- [Security model](SECURITY.md)
- [Contributing](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md)
- [Changelog](CHANGELOG.md)

Report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). Third-party notices are in [NOTICE](NOTICE).
