# Architecture

agentflow is one Go binary. `agentflow serve` (what the background service
runs) is the daemon; every other subcommand is a short-lived CLI that talks to
the daemon over a Unix socket or reads the database directly.

```
  phone browser                                                       browser on this computer
      │ https://<machine>.<tailnet>.ts.net:8443   (system Tailscale)       │ http://127.0.0.1:4344
      │ https://agentflow-xxxxxx.<tailnet>.ts.net (embedded node)          │
      ▼                                                                    │
  Tailscale Funnel ingress (forwards TLS, no decryption) or the tailnet    │
      │                                                                    │
┌─────┼────────────────────────────────────────────────────────────────────┼──────────────────┐
│     ▼                     agentflow serve (runs as you)                  ▼                  │
│  internal/remote Transport:                                    local listener (AF_ADDR)     │
│   system: tailscaled :8443 ──► loopback listener ─┐            plain HTTP ──► local root    │
│   embedded: tsnet TLS :443 ───────────────────────┴► public root              (httpserve)   │
│                                                      (httpserve, Public:      │             │
│                                                       hidden from unpaired)   │             │
│                                        ┌──────────────────────────────────────┘             │
│                                        ▼                                                    │
│   agentapi (sessions, all-sessions, terminals, devices, pairing, access requests, engines)  │
│   loopapi  (loops, tools, prompts)      mailapi (agent mail, local root only)               │
│   webui    (embedded static export)                                                         │
│                                                                                             │
│   spawner ── PTYs ── claude / opencode     store (SQLite)      rtc + uploads                │
│   courier / sweeper / reapers              account reporter ──► account service (optional)  │
│                                                                                             │
│   adminsock: <data dir>/run/agentflow.sock  ◄── agentflow pair/approve/revoke/remote/account│
└─────────────────────────────────────────────────────────────────────────────────────────────┘
```

## Startup

`runServe` in `cmd/agentflow/main.go`, in order:

1. The env file has already been loaded (every command does that first).
2. Take the single-instance lock: an exclusive `flock` on `<AF_DB>.lock`, which
   records the PID. A second daemon on the same database exits with
   "agentflow is already running (pid N; lock …)".
3. Open the database: create the file `0600`, run the embedded migrations, and
   refuse a database whose schema is newer than this binary.
4. Register engines: Claude Code always, OpenCode if `opencode` is on `PATH`.
5. Mark runs and loops left over from a previous daemon as ended (the reapers),
   before anything serves.
6. Start background work: OpenCode session indexing, the attachment sweeper,
   the loop courier and sweeper.
7. Build the local and public roots, listen on `AF_ADDR`, start remote access
   (it never blocks the local listener), start the account reporter when an
   email sign-in is stored, open the admin socket.

On SIGINT/SIGTERM the daemon closes the local listener, closes remote access,
asks every agent to stop (10 seconds in total), closes live WebSockets, shuts
the HTTP servers down and removes the socket. The systemd unit allows 20
seconds (`TimeoutStopSec=20`, `KillMode=mixed`).

## The two roots

`internal/httpserve.Root` builds the handler for each listener from the same
server objects, so both share the device registry, the WebSocket connection
registry and the pairing rate limiter. Around the API and the UI it applies, in
one place:

- `..` path segments refused;
- a request-body cap on `/api/` (1 MB; 4 MB for the local-only agent mail API
  and approvals hook);
- a `Host` allow-list: loopback names on the local port (plus the configured
  `AF_ADDR` host, or any IP literal for a wildcard bind) locally, the node's own
  name publicly (421 otherwise);
- on the public root only: an empty 404 for everything but the pairing page, its
  assets, `/health` and `/api/v1/agentd/pair/*` unless the request carries a
  valid `af_device` cookie (set when a browser pairs over the public listener);
  per-client rate limits (20 requests/s, burst 40); and a 15-minute block after
  20 401 responses in 10 minutes;
- security headers on every response, HSTS on the public root, a
  Content-Security-Policy on UI pages;
- JSON error bodies for unknown `/api/` routes.

### Route exposure

| Routes | Local | Public | Auth |
|---|---|---|---|
| Web UI (everything outside `/api/`) | yes | pairing page and assets for anyone; the rest only with a valid `af_device` cookie | none locally (the UI itself requires a paired token) |
| `GET /api/v1/agentd/health` | yes: status, machine ID, claude version, session counts | yes: status only | none |
| `POST /api/v1/agentd/pair/complete` | yes | yes | pairing token; 5 attempts/min per client; sets `af_device` on the public listener |
| `POST /api/v1/agentd/pair/request`, `GET /api/v1/agentd/pair/request/{id}` | yes | yes | none to ask (shares the pairing limit; at most 5 pending, 2 per client); `X-Pair-Secret` to poll; the approved poll returns the device token once and sets `af_device` publicly |
| `GET /api/v1/agentd/pair/requests`, `POST …/pair/requests/{id}/approve`, `…/deny` | yes | no (404) | device token |
| `POST /api/v1/agentd/pair/cookie` | yes (no effect) | yes | device token; sets `af_device` |
| `GET /api/v1/agentd/all-sessions` | yes | yes | device token |
| `/api/v1/agentd/sessions…`, `runs…`, `devices…`, `engines`, `doctor`, `cwds`, `rtc/config` | yes | yes | device token |
| `GET /api/v1/agentd/sessions/{id}/tty/ws`, `…/rtc/ws` (WebSockets) | yes | yes | device token in `?token=`, then Origin allow-list |
| `/api/v1/agentd/approvals` (list, decide), `/api/v1/agentd/policies` | yes | yes | device token |
| `POST /api/v1/agentd/approvals/request` (approvals hook) | yes | no (404) | `X-AgentFlow-Hook-Secret` |
| `/api/v1/loops…`, `/api/v1/tools…`, `/api/v1/prompts…` | yes | yes | device token |
| `/api/v1/agent/*` (agent mail) | yes | no (404) | member token |
| Pairing-token minting, access-request decisions from the CLI | no HTTP route | no HTTP route | admin socket only |

`internal/agentapi/hostile_test.go` checks this table, the WebSocket rules,
body caps, path traversal and headers.

## Remote access (`internal/remote`)

`AF_REMOTE` picks a `Transport` through `remote.Select`: `Off` or Tailscale
(the system or the embedded implementation); `Fake` stands in for tests. Each
publishes the same `Status` (state, transport, public URL, and the link or
command for whatever it is waiting on), mirrored to `<data dir>/remote.json`,
which `agentflow remote status` reads when the daemon is down. States:
`starting`, `needs_install`, `needs_login`, `needs_permission`,
`needs_funnel_approval`, `running`, `error`, `off`. Both Tailscale
implementations serve the public root on a listener of their own; the system
one's loopback listener (`ListenTunnel`) refuses the local listener's port and
any non-loopback address.

**System Tailscale** (`system.go`, `AF_REMOTE=tailscale`, the default):

1. Look for `tailscale` on `PATH` and ask `tailscaled`'s LocalAPI for its
   status. Missing or not answering publishes `needs_install` with
   `https://tailscale.com/download`. `NeedsLogin`/`Stopped` publishes
   `needs_login` with the sign-in URL or `tailscale up`; `NeedsMachineAuth`
   the admin console.
2. Funnel mode: until the node has the HTTPS and Funnel capabilities, publish
   the `funnel` feature's one-click approval URL (tailnet mode checks HTTPS
   through the `serve` feature).
3. Open a loopback listener (`AF_TUNNEL_ADDR` or a free port) whose handler
   takes the client address from `X-Forwarded-For` (which `tailscaled` sets on
   every request) and refuses requests without it.
4. Merge one entry into the node's serve config: TCP 8443 as HTTPS,
   `<machine>.<tailnet>.ts.net:8443` → `http://127.0.0.1:<port>`, and Funnel
   allowed in funnel mode. The config is read and written with its ETag; any
   other user of port 8443 is a conflict and nothing is written. The entry is
   recorded in `<data dir>/tailscale-serve.json`, so a later start replaces it
   after a crash instead of calling it a conflict. Permission denied publishes
   `needs_permission` with `sudo tailscale set --operator=$USER` and retries.
5. Publish `running` with `https://<machine>.<tailnet>.ts.net:8443`, then keep
   checking: a reset serve config gets the entry back; a changed name or a node
   that stops sends it back to step 1.

On shutdown the entry is removed, and only if it is still exactly agentflow's.
`Logout` refuses: the computer's Tailscale isn't agentflow's to sign out.

**Embedded node** (`tsnet.go`, `AF_TAILSCALE=embedded`, or automatically when
an earlier node's `tailscale/tailscaled.state` exists and the system Tailscale
isn't installed or doesn't answer within 10 seconds):

1. Set `TS_NO_LOGS_NO_SUPPORT=true` unless `AF_TS_LOGS=on`; create
   `<data dir>/tailscale` (`0700`); load or generate the `agentflow-<6 hex>`
   hostname; start the node.
2. Poll the node status every second. `NeedsLogin` publishes the sign-in URL
   (asking the node for one if none shows up); `NeedsMachineAuth` publishes the
   admin console's machines page.
3. Funnel mode: until the node has the HTTPS and Funnel capabilities and a
   certificate domain, query the `funnel` feature every 3 seconds and publish
   its one-click approval URL.
4. Listen on `:443`: `ListenFunnel` (TLS 1.2+, the node's certificate; the
   listener also accepts tailnet peers) or, in tailnet mode, `ListenTLS`.
   Request the certificate right away rather than on the first phone
   connection.
5. Serve the public root and publish `running` with
   `https://<certificate domain>`.

`Logout` logs the node out and sends the loop back to step 2. On Funnel
connections `RemoteAddr` is Tailscale's ingress node; the real client address
comes from the underlying Funnel connection.

In every case a failure publishes `error` and retries with backoff.

## Account service (`internal/cloud`, `cmd/agentflow/accountreport.go`)

`agentflow account login`, and `agentflow start` when you don't skip it, sign in
with an email and a 6-digit code and store the session token in
`<data dir>/account.json`. While
the daemon runs with a stored account and remote access on, the account
reporter puts the machine (`<data dir>/machine-id`, host name, transport,
version, current URL) to the account service (`AF_CLOUD_URL`) when it starts
and whenever the transport reports a new working URL, and sends a heartbeat
every 10 minutes. Statuses without a URL are not reported, so a reconnect
doesn't look like a new address. The service emails the address when it
changes. The admin socket's `POST /account/reload` restarts the reporter from
the account file after a sign-in or sign-out.

## Admin socket (`internal/adminsock`)

JSON over HTTP on `<data dir>/run/agentflow.sock`: directory `0700`, socket
`0600`, no TCP. A stale socket from a crashed daemon is replaced (safe because
the instance lock is already held). Unix socket paths are limited to about 104
bytes, so when `<data dir>/run/agentflow.sock` would be longer (a deep home
directory or `AF_DATA_DIR`), the socket goes in
`$TMPDIR/agentflow-<uid>-<hash of the data dir>/` instead; the daemon refuses
that directory unless it is a real directory owned by the same user with no
access for anyone else.

| Endpoint | Used by |
|---|---|
| `GET /health` | `status`, `doctor`, `start` |
| `GET /remote/status` | `remote status`, `status`, `doctor`, `start`, `pair` |
| `POST /remote/logout` | `remote logout`, `uninstall --purge` (embedded node only) |
| `POST /pair/mint` | `pair`, `start` |
| `GET /pair/requests` | `devices`, and the access-request prompt in `pair` and `start` |
| `POST /pair/requests/{id or code}/approve`, `…/deny` | `approve`, `deny`, the prompt |
| `POST /account/reload` | `start`, `account login`, `account logout` |
| `POST /devices/{id}/revoke` | `revoke` (response includes `connectionsClosed`) |

When the socket isn't there, `pair` mints straight into the database (with a
local link) and `revoke` updates the database directly. `devices` always reads
the database.

## Pairing and devices

- `POST /pair/mint` stores a pairing row (kind `pairing`, SHA-256 hash of a
  24-byte token). The CLI prints `<base>/pair/#token=…&expires=…` with the
  public URL as the base when remote access is running, or
  `http://127.0.0.1:<port>` (labelled for this computer) when it is off. While
  remote access is on but not running, `agentflow pair` mints nothing and
  shows what remote access is waiting for.
- **Access requests.** A browser without a link calls `POST /pair/request`
  and gets a request id, a poll secret and a 4-digit code; the daemon keeps
  the request in memory (5 minutes, at most 5 pending and 2 per client). The
  computer approves or denies it through the admin socket, or from an already
  paired browser on the local listener. The phone polls
  `GET /pair/request/{id}` with `X-Pair-Secret` every 2 seconds; the poll that
  finds it approved creates the device row and returns its token once.
- The web UI's `/pair/` page reads the fragment, strips it from the address
  bar, and calls `pair/complete`, which consumes the pairing row atomically and
  creates a device row (kind `device`) with a fresh token. The browser keeps
  the token in `localStorage`; on the public listener the response also sets
  the `af_device` cookie that lets it load the app there.
- Every API request goes through `VerifyClientDevice`: kind `device`, status
  `active`, used within the last 30 days. Each authenticated request updates
  `last_seen`.
- Revocation (`RevokeDevice`) clears the token hash, marks the row revoked and
  closes that device's registered WebSockets. Each WebSocket also re-verifies
  its token every minute.

## Terminals (`internal/spawner`, `internal/agentapi/tty.go`)

The spawner owns every child process. A terminal run starts the engine in a PTY
(`TERM=xterm-256color`), with `AF_*` (except `AF_CLAUDE`) and `TS_*` removed
from its environment, after marking the working directory as trusted in
`~/.claude.json`. Claude runs with `--dangerously-skip-permissions`, OpenCode
with `--auto`; OpenCode also gets a local control port that agentflow uses to
bind and follow its session.

The PTY is registered with a fan-out hub as soon as it exists. Viewers attach
over `…/tty/ws`: they get a replay of recent output, then live bytes; input
from any viewer goes to the same PTY. Terminal starts are serialized per Claude
session, so two viewers never start two processes on one session. An attach
never spawns for an unknown run ID, and never revives a run that was stopped
on purpose (`POST …/tty?restart=1` from the UI's **Restart session** does).

## Loops (`internal/loopapi`, `internal/mailapi`)

A loop is a set of agent sessions working on one task. The web UI creates and
watches loops through `/api/v1/loops` (device token). Member agents talk to
each other through the local-only mail API (`/api/v1/agent/*`, member tokens);
their instructions include `AF_AGENT_BASE_URL`. The courier
(`cmd/agentflow/courier.go`) types new mail into a member's terminal after
stripping control characters; the sweeper (`cmd/agentflow/sweeper.go`) expires
leases, notices stranded members and nudges them.

## Attachments (`internal/rtc`, `internal/uploads`)

The phone opens a WebRTC data channel to the daemon, signaled over
`…/rtc/ws`. ICE uses STUN servers from `AF_STUN_URLS`; TURN URLs are refused,
so file data never passes through a TURN server. The uploads store decides the path
(under `AF_UPLOADS_DIR`), sniffs the content (PNG, JPEG, WebP, GIF only),
enforces the size, count, disk and rate limits, and a sweeper deletes files
after `AF_UPLOADS_RETENTION_DAYS` or when their run is gone.

## Store (`internal/store`)

SQLite through `modernc.org/sqlite` (pure Go, so release builds use
`CGO_ENABLED=0`), WAL mode, 30-second busy timeout. Migrations are embedded
(`internal/store/migrations/*.sql`) and applied at startup. The database holds
managed runs, conversation events, loops and mail, devices, approvals and prompt
overrides.

## Web UI (`web/`, `internal/webui`)

A Next.js app built as a static export (`output: "export"`). `make web` copies
`web/out` into `internal/webui/dist`, which is embedded with `go:embed`. Pages
that need an ID take it as a query parameter (`/session/?id=…`). The UI calls
the API on its own origin, so the same bundle works on both listeners.

`/` is the sessions home: `GET /api/v1/agentd/all-sessions` merges Claude Code
transcripts under `~/.claude/projects` (read only; ordered by file time, with
titles and working directories read only for the returned page and cached),
OpenCode sessions from the store, and managed runs, which mark the session
they hold as live. `/session/?id=` attaches to a managed run id, or resumes an
engine session id into a terminal run and replaces the URL with the run's id.
`/session/config/` shows the rules in effect. `/pair/` pairs by link, QR code,
pasted token or **Request access**.

`internal/webui` serves only GET and HEAD, never serves `/api/`, rejects `..`,
caches `/_next/static/*` for a year and sends HTML with `no-cache`. A binary
built without the export serves a placeholder page instead.

The web app has a manifest (installable to the home screen) and a service
worker that caches only content-hashed build assets and icons; pages and API
responses are never cached.

## Service management (`cmd/agentflow`)

| | Linux | macOS |
|---|---|---|
| Definition | `~/.config/systemd/user/agentflow.service` (`systemd.go`) | `~/Library/LaunchAgents/com.arthurobo.agentflow.plist` (`launchd.go`) |
| Runs | `<resolved binary path> serve`, `Restart=on-failure` | `<resolved binary path> serve`, `KeepAlive` |
| Environment | `PATH` captured by `agentflow start` | same |
| Logs | journald | `~/Library/Logs/agentflow/agentflow.log` |

`agentflow start` rewrites the definition only when it changed, and restarts the
service when it did. The env file is read by the daemon itself, not by the
service manager.

## Releases

`.goreleaser.yaml` runs `make web`, then builds `linux` and `darwin` for
`amd64` and `arm64` with `CGO_ENABLED=0`. Archives are
`agentflow_<version>_<os>_<arch>.tar.gz`; `checksums.txt` is signed keyless
with cosign (`checksums.txt.sig`, `checksums.txt.pem`) in the release workflow.
`install.sh` and `agentflow update` build the same archive names and verify the
same way.

## Where to look

```
cmd/agentflow/main.go         daemon wiring (runServe), config, shutdown
cmd/agentflow/args.go         command parsing and usage text
cmd/agentflow/start.go        agentflow start (optional sign-in, Tailscale setup, pairing)
cmd/agentflow/pair.go         pairing links and QR codes
cmd/agentflow/access.go       approve, deny, and the access-request prompt
cmd/agentflow/account.go      agentflow account
cmd/agentflow/accountreport.go  reporting this machine to the account service
cmd/agentflow/commands.go     stop, restart, status, remote, devices, revoke, doctor
cmd/agentflow/envfile.go      env file parsing and the default file
cmd/agentflow/systemd.go      systemd unit
cmd/agentflow/launchd.go      launchd plist
cmd/agentflow/update.go       agentflow update
cmd/agentflow/uninstall.go    agentflow uninstall
cmd/agentflow/lock.go         instance lock
internal/httpserve/           roots, headers, host checks, rate limits, body caps
internal/agentapi/            device API, pairing and access requests, all-sessions, terminals, WebSockets
internal/remote/              remote access: system and embedded Tailscale
internal/cloud/               account service client
internal/adminsock/           admin socket server and client
internal/spawner/             child processes and PTYs
internal/engine/              claude and opencode engines
internal/loopapi/             loops API
internal/mailapi/             agent mail API (local only)
internal/rtc/, internal/uploads/  attachments
internal/store/               SQLite store and migrations
internal/webui/               embedded web UI
web/                          web UI source
```
