# Getting started

From install to a terminal on your phone.

## 1. Requirements

- Linux or macOS (amd64 or arm64).
- `claude` (Claude Code) and/or `opencode` installed, signed in, and on your
  `PATH`. agentflow does not install or sign in engines; it runs them as you.
- A [Tailscale](https://tailscale.com) account for reaching the computer from
  your phone, ideally with Tailscale installed and signed in on this computer.
  Not needed if you only use agentflow on this computer (`AF_REMOTE=off`).
- Linux: a systemd user session (`systemctl --user` must work) for the
  background service. Without one, run `agentflow serve` in the foreground.

## 2. Install

```sh
curl -fsSL https://raw.githubusercontent.com/arthurobo/agentflow/main/install.sh | sh
```

The installer:

1. picks the archive for your OS and CPU:
   `agentflow_<version>_<os>_<arch>.tar.gz`;
2. finds the latest release through the GitHub API (or uses
   `AGENTFLOW_VERSION`);
3. downloads the archive and `checksums.txt`;
4. if `cosign` is installed, downloads `checksums.txt.sig` and
   `checksums.txt.pem` and verifies the signature against this repository's
   release workflow; any failure stops the install;
5. verifies the archive's SHA-256 against `checksums.txt`; a mismatch stops the
   install;
6. installs `~/.local/bin/agentflow` and tells you if that directory is not on
   your `PATH`;
7. runs `agentflow start` (skip with `AGENTFLOW_NO_START=1`).

To build from source instead, see [CONTRIBUTING.md](../CONTRIBUTING.md). A
binary from plain `go install` has no web UI.

## 3. Start and pair

```sh
agentflow start
```

What you'll see, in order:

1. **Email** (optional; skipped when you're already signed in). "Email for
   your agentflow link and dashboard (optional, press Enter to skip):". Press
   Enter to skip it; everything works without it. If you enter one, a 6-digit
   code is emailed to you; type it in. From then on agentflow emails you this
   machine's address when it changes and lists it on the dashboard. If the
   account service can't be reached, `start` says so and carries on; run
   `agentflow account login` later.
2. **Service.** "Installed the agentflow service; it starts again at login."
   (or "agentflow service is installed and started." when nothing changed),
   the path of your settings file, and on Linux a note to run
   `loginctl enable-linger $USER` if the service would stop when you log out.
3. **Tailscale**, one step at a time, each with a link and QR code where
   there is one:
   - Tailscale not installed: the download link, and a note that
     `AF_TAILSCALE=embedded` uses agentflow's built-in node instead.
   - Tailscale signed out or stopped: `tailscale up` to run, or a sign-in link.
     If your tailnet requires new machines to be approved, a link to the admin
     console.
   - On Linux, permission to publish through your Tailscale:
     `sudo tailscale set --operator=$USER`, once. agentflow notices and
     continues on its own.
   - Funnel approval (Funnel mode, first time only): "One click to allow
     public HTTPS (Tailscale Funnel) for this machine." and an approval link.
4. **Address.** "Remote URL: https://<machine>.<tailnet>.ts.net:8443 (Tailscale
   funnel)" (with the embedded node,
   `https://agentflow-xxxxxx.<tailnet>.ts.net`). A
   brand-new address can take a few minutes to resolve and get a certificate;
   `start` waits (up to 3 minutes) until it answers.
5. **Pairing.** "Scan this with your phone camera to pair it:", a QR code, the
   same link as text, when it expires, and how to pair without a camera.
6. **Access requests.** "Watching for access requests; press Ctrl-C to stop."
   Each phone that taps **Request access** shows up as
   "iPhone · Safari (code 4821) wants access — approve? [y/N]".

Scan the code with the phone's camera and open the link. The pairing page
stores a device token in that browser and takes you to the sessions list. You
can add the page to your home screen.

**No camera?** Open the address on the phone. The pairing page offers
**Request access**: tap it, and the phone shows a 4-digit code. Check that the
computer shows the same code, then approve it at the prompt above, with
`agentflow approve 4821`, or from the pairing page in a browser on this
computer that is already paired. The phone pairs itself within a few seconds.
A request expires after 5 minutes.

Ctrl-C is safe at any point; agentflow keeps running. Pick up later with
`agentflow remote status` and `agentflow pair`. If remote access isn't ready
after 10 minutes, `start` stops waiting and tells you so.

To pair another device, run `agentflow pair`. Each link pairs one device.
While remote access isn't running yet, `agentflow pair` doesn't print a link a
phone couldn't open: it shows what remote access is waiting for and asks you
to run it again once it's running.

With `AF_REMOTE=off`, `start` prints "Remote access is off (AF_REMOTE=off);
pairing works on this computer only." and "Open this on this computer to pair
its browser:" with a link on `http://127.0.0.1:4344`.

Without a terminal (a script), `start` asks nothing: there's no email prompt
unless you pass `--email`.

```sh
agentflow start --email you@example.com   # the code is still read from stdin
agentflow start --no-email                # never offer the sign-in
```

## 4. Use it

In the web UI:

- **Sessions** (`/`) lists every Claude Code and OpenCode session on this
  computer, newest first, including ones you started in a terminal without
  agentflow: name, folder, engine and when it was last active, with a dot on
  sessions that are running now. Search filters by name or folder. Tap one to
  open its terminal; a session nothing is running is resumed. **New session**
  opens a form: engine, session name, prompt, working directory, model, agent.
  The terminal opens ready to type into, with a composer pinned above the
  phone keyboard.
- A terminal keeps running when you close the page; open it again from any
  paired device. A session that was stopped stays stopped until you choose
  **Restart session**.
- You can attach images from the phone. They upload to the computer over the
  same secure connection the app already uses, and are kept for 7 days.
- **Loops** (`/loop/`) runs several agent sessions on one task, passing
  messages between them.

## 5. Manage devices

```sh
agentflow devices          # ID, name, status, created, last seen; then waiting access requests
agentflow approve <code>   # let a Request access request in (4-digit code or request id)
agentflow deny <code>      # turn it down
agentflow revoke <id>      # revoke and close its live connections
```

`revoke` prints "Revoked <id> and closed N live connection(s). Other devices
are unaffected." A device that goes 30 days without using its token has to pair
again.

## 6. Change how it's reachable

```sh
agentflow remote status    # state, transport, URL, and any pending install/sign-in/permission/approval step
agentflow remote off       # AF_REMOTE=off, restarts the service
agentflow remote on        # AF_REMOTE=tailscale, restarts; then run agentflow start
agentflow remote logout    # log agentflow's own Tailscale node out (AF_TAILSCALE=embedded)
```

For tailnet-only access, set `AF_REMOTE_MODE=tailnet` in
`~/.config/agentflow/agentflow.env` and run `agentflow restart`; the phone then
needs the Tailscale app signed in to the same tailnet. See
[configuration.md](configuration.md).

### Email sign-in

```sh
agentflow account login    # email, then the 6-digit code; a running agentflow picks it up at once
agentflow account status   # email, machine id, dashboard, reporting
agentflow account logout   # end the session on the account service and forget it here
```

## 7. Update

```sh
agentflow update
```

Downloads the latest release (or `AGENTFLOW_VERSION`), verifies the cosign
signature when `cosign` is installed and always the SHA-256, replaces the
running binary in place, and restarts the service if it is running.

Restarting agentflow
(`agentflow restart`, `update`, a reboot) stops the terminals it runs; the daemon gives them up to 10 seconds to exit. They stay
stopped until you open them and choose **Restart session**.

## 8. Uninstall

```sh
agentflow uninstall                 # remove the service; keep data
agentflow uninstall --purge         # also delete data and settings, and log agentflow's own Tailscale node out
agentflow uninstall --purge --yes   # without the confirmation prompt
```

The agentflow service is removed. The binary is deleted only if it is in `~/.local/bin`. Not touched: project trust
entries in `~/.claude.json`, `.agentflow-bak-*` transcript backups, the
computer's own Tailscale, and your account on the account service. If
agentflow ran its own Tailscale node, remove that machine from your Tailscale
admin console yourself.

## Troubleshooting

Start with:

```sh
agentflow status
agentflow doctor
```

`doctor` checks `claude` (found and `--version` works), `opencode` (optional),
the service, the optional email sign-in, whether the daemon answers on its
admin socket,
and remote access (running, and the public URL reachable from this machine).

Logs: `journalctl --user -u agentflow` on Linux,
`~/Library/Logs/agentflow/agentflow.log` on macOS.

- **`claude` not found** ("not found in the service PATH"). The service uses
  the `PATH` recorded when you ran `agentflow start`. Make sure `claude` is on
  your shell's `PATH` and run `agentflow start` again. Or set `AF_CLAUDE` to its
  full path.
- **OpenCode doesn't show up as an engine.** It is detected when the daemon
  starts: `agentflow start` again after installing it.
- **"not reachable from this machine yet"**. New `ts.net` addresses can take a
  few minutes to resolve and get a certificate. Check again with
  `agentflow doctor`.
- **The phone shows a blank page on the public address.** Only paired browsers
  get the app there. Open `<address>/pair/` on the phone: a phone paired before
  this check existed gets what it needs there automatically; otherwise pair it
  again.
- **Remote access says `needs_permission`.** Run
  `sudo tailscale set --operator=$USER` once; agentflow continues on its own.
- **Remote access says "port 8443 … is already served by something else".**
  Something else uses port 8443 in your Tailscale serve settings
  (`tailscale serve status`). agentflow leaves it alone; free the port.
- **"agentflow is already running (pid N; lock …)"**. Another daemon holds the
  database. You probably ran `agentflow serve` while the service is running;
  use the service, or `agentflow stop` first.
- **The pairing link says it expired.** Links expire 15 minutes after they are
  minted. Run `agentflow pair` again.
- **Phone lost its pairing after an address change.** Device tokens are stored
  per address, so a new `AF_TS_HOSTNAME`, or switching between the system
  Tailscale and the embedded node, means pairing again.
- **Service stops when you log out (Linux).** `loginctl enable-linger $USER`.
