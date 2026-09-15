# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.5.1]

### Changed

- **`agentflow uninstall` now erases everything by default** — the service, the
  binary, all data and settings, and it logs agentflow's own Tailscale node out.
  Pass `--keep-data` to keep your data and settings for a reinstall. Previously
  uninstall kept data unless you added `--purge`, and because it deleted the
  binary first, a follow-up `--purge` could not run. `--purge` is still accepted.

## [0.5.0] — first open-source release

### Added

- **Daemon.** `agentflow serve` runs Claude Code and OpenCode terminals in PTYs
  and serves the web UI and API on `127.0.0.1:4344`. A single-instance lock
  (`<database>.lock`) stops a second daemon from taking over the same database.
- **Web UI in the binary.** The Next.js static export is embedded and served on
  the same origin as the API: a sessions home at `/` listing every Claude Code
  and OpenCode session on the machine (including ones started without
  agentflow), full-screen terminals at `/session/` with a phone-friendly
  composer, explicit **Restart session** for stopped sessions, loops, and a
  pairing page with a QR scanner and **Request access**. Installable to the home screen;
  the service worker caches only build assets and icons. Binaries built without
  the export serve a placeholder page.
- **Remote access through a Cloudflare tunnel**, on by default:
  `AF_REMOTE=cloudflare` gets this machine a named tunnel at
  `https://<adjective>-<noun>-<4 digits>.useagentflow.xyz` from the account
  service (`POST /v1/provision`, no sign-in; the machine id is guarded by a
  secret the daemon generates and keeps in `cloudflare-tunnel.json`), runs a
  pinned, SHA-256 verified `cloudflared` with the token in its environment,
  and serves the public root on `127.0.0.1:4345` with client addresses from
  `CF-Connecting-IP`. The last tunnel is kept so it comes up while the service
  is down. Machines already set up with Tailscale keep it while `AF_REMOTE` is
  unset. Cloudflare can see the traffic.
- **Remote access through your own Tailscale account** (opt-in):
  `AF_REMOTE=tailscale` publishes `https://<machine>.<tailnet>.ts.net:8443`
  through the Tailscale installed on the computer (one serve entry, removed on
  shutdown), or `https://agentflow-<random>.<tailnet>.ts.net` through a
  built-in node (`AF_TAILSCALE=embedded`), through Funnel
  (`AF_REMOTE_MODE=funnel`) or to your tailnet only (`AF_REMOTE_MODE=tailnet`).
  `AF_REMOTE=off` disables remote access. The public address shows unpaired
  visitors only the pairing page.
- **`agentflow start`** offers an optional email sign-in (Enter skips it),
  installs or refreshes the background service (systemd user unit on Linux,
  launchd agent on macOS), follows remote access as it comes up (with
  Tailscale: installing or signing in, permission and Funnel approval) with links and terminal QR codes,
  waits until the public URL answers, prints a pairing QR code and asks about
  access requests. `--email` and `--no-email` make it scriptable.
- **Optional account service** (`agentflow account login|logout|status`,
  `AF_CLOUD_URL`): emails this machine's address when it changes and lists it
  on a dashboard. It receives the email, machine id and name, transport,
  address and version; never tokens or terminal data.
- **Commands:** `stop`, `restart`, `status`, `doctor`, `pair`, `devices`,
  `approve`, `deny`, `revoke`, `remote status|logout|on|off`, `account
  login|logout|status`, `update`, `uninstall [--purge] [--yes]`, `version`,
  `make-resumable`, `undo-make-resumable`.
- **Per-device access.** One-time pairing links (`<base>/pair/#token=…`, token
  in the URL fragment), **Request access** approved on the computer with a
  4-digit code (`agentflow approve`), and revocable device tokens that expire
  after 30 days without use. `agentflow revoke` closes the device's live terminals at once;
  open WebSockets also re-check their token every minute.
- **Admin socket** (`<data dir>/run/agentflow.sock`, owner-only) for the CLI:
  remote status and logout, pairing, access-request decisions, revocation,
  account reload, health.
- **Settings file** `~/.config/agentflow/agentflow.env`, read by the daemon
  itself, with `$HOME` expansion; written with every setting commented at its
  default.
- **Image attachments from the phone** over a direct WebRTC data channel (STUN
  only; TURN servers are refused), with type sniffing, size, count, disk and
  rate limits, and 7-day retention.
- **Loops:** several agent sessions working on one task, exchanging mail
  through a local-only API; mail is delivered into member terminals with
  control characters stripped.
- **Releases** for Linux and macOS (amd64, arm64) as
  `agentflow_<version>_<os>_<arch>.tar.gz` with `checksums.txt` signed keyless
  with cosign. `install.sh` and `agentflow update` always verify the SHA-256,
  and verify the signature when `cosign` is installed, refusing to install if
  it fails. When the GitHub API is unavailable or rate-limited (the
  unauthenticated limit is 60 requests/hour per IP, which a shared office or CI
  network can exhaust), `install.sh` resolves the latest tag from the release
  page redirect instead, so installs keep working; set `GITHUB_TOKEN` to use the
  API at its 5000/hour authenticated limit.
- **Uninstall** removes only named paths and the service; `--purge` also logs
  agentflow's own Tailscale node out and deletes agentflow's data and settings.

### Security

- Remote listener serves only the web UI (to paired browsers, marked by an
  `HttpOnly; Secure; SameSite=Lax` cookie the API ignores) and the device-token
  API; the agent mail API, the approvals hook and access-request approval are
  local-only, and its `/health` reports only status. Client addresses behind
  the system Tailscale come from the `X-Forwarded-For` tailscaled sets, and
  requests without it are refused.
- `Host` allow-lists on both listeners (DNS-rebinding defense), WebSockets that
  always require a device token and an allowed `Origin`, CORS off, security
  headers and a Content-Security-Policy, 1 MB API body cap, per-client rate
  limits and a lockout after repeated 401s on the public listener, 5 pairing
  attempts a minute per client, at most 5 pending access requests (2 per
  client).
- Database, hook secret, env file and uploads are owner-only; `AF_*` and `TS_*`
  variables are removed from agent environments.

See [SECURITY.md](SECURITY.md) for the threat model.

### Not included

- A hosted relay. One, end-to-end encrypted so the relay can't read or control
  your machine, is coming soon.
