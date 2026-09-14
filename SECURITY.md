# Security

## Reporting a vulnerability

**Please do not report security vulnerabilities in public GitHub issues.**

Use GitHub's private vulnerability reporting:

1. Open the **Security** tab of
   [arthurobo/agentflow](https://github.com/arthurobo/agentflow).
2. Click **Report a vulnerability**
   ([direct link](https://github.com/arthurobo/agentflow/security/advisories/new)).
3. Describe the issue, its impact, and how to reproduce it, including
   `agentflow version`.

You should get an acknowledgement within a few days. We'll work with you on a
fix and a disclosure timeline, and credit you when the fix ships unless you
prefer to stay anonymous.

## Supported versions

agentflow is pre-1.0. Security fixes land on `main` and in the next release.
Please check that the issue reproduces on the latest release
(`agentflow update`) before reporting.

## What agentflow is, from a security point of view

agentflow runs coding agents in terminals on your computer, as your user, and
lets paired devices type into those terminals from a browser. **A paired device
is a remote shell as you.** Agent terminals run with
`claude --dangerously-skip-permissions` (OpenCode: `--auto`), so the agent does
not ask before running commands, and anyone who can type into a terminal can
also just ask it to run anything.

Everything below is about keeping unpaired parties out and making a pairing
easy to take back. None of it limits what a paired device, or an agent, can do
on your machine.

### Components

- **Local listener**: HTTP on `AF_ADDR` (default `127.0.0.1:4344`). Serves the
  web UI and the full API.
- **Remote listener**: serves the public root: the web UI (to paired browsers
  only) and the device API, but not the agent mail API or the approvals hook.
  How it is reached with `AF_REMOTE=tailscale`:
  - the system Tailscale (the default): `tailscaled` on this
    computer terminates HTTPS for `https://<machine>.<tailnet>.ts.net:8443`
    and forwards plain HTTP to a loopback listener agentflow opens for it.
  - `AF_TAILSCALE=embedded`: a Tailscale node inside the
    daemon (tsnet) terminates HTTPS itself on
    `https://agentflow-<6 hex>.<tailnet>.ts.net`.

  In `funnel` mode (default) it is reachable from the internet through
  Tailscale Funnel and from your tailnet; in `tailnet` mode only from your
  tailnet.
- **Admin socket**: a Unix socket at `<data dir>/run/agentflow.sock` that the
  CLI uses for remote status and logout, minting pairing tokens, deciding
  access requests, revoking devices, reloading the email sign-in and health. When that path is too long for a Unix socket, it lives
  in a per-user `0700` directory under the system temp directory, which the
  daemon refuses unless it owns it.
- **Engines**: `claude` and `opencode` child processes in PTYs.

- **Account service** (optional): if you sign in with an email, the daemon
  tells the account service (`AF_CLOUD_URL`, default
  `https://cloud.useagentflow.xyz`) this machine's id, name, transport, public
  address and agentflow version, so it can email you the address when it
  changes and list it on a dashboard. It is never in the path between a phone
  and the computer and never receives device tokens, pairing tokens or
  terminal data.

agentflow sends no telemetry. The only network calls it makes on its own are
Tailscale's (when remote access is on), the account service's (only when you
signed in), STUN requests when a phone sends an attachment (default server
`stun:stun.l.google.com:19302`), and, when you run `agentflow update` or
`install.sh`, GitHub release downloads. The engines talk to their own
providers as they always do.

## Threat model and controls

### 1. Anyone on the internet who finds the public URL

In `funnel` mode the URL is public. It is also published in Certificate
Transparency logs as soon as its certificate is issued (true in `tailnet` mode
too), so assume it will be found and scanned. The random `agentflow-<6 hex>`
node name of the embedded node is not a secret.

Controls on the remote listener (`internal/httpserve`, `internal/agentapi`):

- **The app is hidden from unpaired visitors.** A browser without a valid
  `af_device` cookie (see below) gets an empty, uncacheable 404, with no
  agentflow branding and no redirect, for everything except what pairing
  needs: the pairing page (`/pair/`) and its build assets (`/_next/static/*`,
  `/icons/*`, `/manifest.json`, `/sw.js`), `GET /api/v1/agentd/health` (which
  returns only `{"status":"ok"}` on this listener) and
  `/api/v1/agentd/pair/*`. A revoked or unused device makes its cookie
  invalid at once.
- **Unauthenticated surface.** Past that gate every API route except health
  and pairing answers 401 without a device token in the `Authorization`
  header; the cookie is never accepted in its place. The agent mail API
  (`/api/v1/agent/*`), the approvals hook (`/api/v1/agentd/approvals/request`)
  and the access-request approval routes are not mounted and answer 404.
- **Device tokens.** Every API call carries `Authorization: Bearer <token>`,
  checked against a SHA-256 hash in the database. Only rows of kind `device`
  and status `active` authenticate; pairing tokens and revoked devices do not.
- **Pairing.** `pair/complete` accepts only a pairing token, consumes it
  atomically (a second use gets 401), and is limited to 5 attempts a minute per
  client address. Tokens are 24 random bytes.
- **Access requests** (`POST /pair/request`, "Request access" on the pairing
  page) share that limit. A request gets a 128-bit id, a 192-bit poll secret
  (kept only as a SHA-256 hash) and a 4-digit code shown on the phone and next
  to the request on the computer. It waits at most 5 minutes; at most 5 wait at
  once, and at most 2 from one client address. It can only be approved on the
  computer: through the admin socket (`agentflow approve`, the prompt in
  `agentflow start` and `agentflow pair`) or by an already-paired browser on
  the local listener. Polling with a wrong or missing secret gets the same 404
  as an unknown id, and an approved request hands out its device token exactly
  once. The 4-digit code is not a secret; it is there so the person approving
  checks they approve the phone in their hand. Requests, approvals and denials
  are logged.
- **Rate limits** per client address: 20 requests a second (burst 40); a client
  that collects 20 401 responses within 10 minutes is blocked for 15 minutes.
  The client address is the phone's real address: on embedded-node Funnel
  connections it is taken from the Funnel connection; on the system
  Tailscale's loopback listener it comes from `X-Forwarded-For`, which
  `tailscaled` always overwrites, and a request without that header is refused
  (421) rather than counted under `127.0.0.1` with everyone else.
- **Host check.** Requests whose `Host` is not the current public host (with
  its port, `:8443`, for the system Tailscale) are refused with 421.
- **Limits.** 1 MB request bodies on the API, 64 KB headers, 10 s to send
  headers, 120 s idle timeout.
- **Headers.** `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`,
  `X-Frame-Options: DENY`, `Permissions-Policy: camera=(self), microphone=()`,
  `Strict-Transport-Security: max-age=31536000`, and on web UI pages a
  Content-Security-Policy that restricts everything to the daemon's own origin
  (`script-src` and `style-src` allow `'unsafe-inline'`, which the static
  Next.js export needs), `frame-ancestors 'none'`, `base-uri 'none'`.
- **CORS is off.** No `Access-Control-Allow-Origin` header is sent unless you
  set `AF_DEV_CORS_ORIGIN` for web development.

`AF_REMOTE_MODE=tailnet` removes the internet from this list; `AF_REMOTE=off`
removes the remote listener entirely.

The `af_device` cookie holds the device token and is set only by pairing over
the public listener (and by `POST /api/v1/agentd/pair/cookie`, which requires
the token in the header, for browsers paired before the cookie existed). It is
`HttpOnly`, `Secure`, `SameSite=Lax` and `Path=/`. It only decides whether the
app's files are served; the API ignores it, so a cross-site request that
carries it gains nothing. `Lax` rather than `Strict` lets a paired phone open
the address from a link in a mail or chat app.

### 2. A malicious web page in a browser on the same computer

A page on another site can make your browser send requests to
`127.0.0.1:4344` or to your public URL.

- **No token, no access.** The device token lives in the web UI's
  `localStorage`, which only the agentflow origin can read. The `af_device`
  cookie a browser may also carry on the public listener is not accepted by the
  API. Requests a foreign
  page can send without a CORS preflight cannot carry an `Authorization` header,
  so they get 401, and with CORS off the page can't read responses anyway.
- **DNS rebinding.** The local listener accepts only `Host` values that name the
  loopback interface (`127.0.0.1`, `localhost`, `[::1]`) on its port, the
  configured `AF_ADDR` host, or, when `AF_ADDR` binds every interface, an IP
  address literal (which cannot be rebound). A rebinding page still sends its own host name and
  gets 421.
- **WebSockets** (terminal and WebRTC signaling) always require a device token
  in the `token` query parameter, including from `127.0.0.1`; there is no
  loopback trust. The token is checked before the `Origin`, and the `Origin`
  must exactly match the public origin, `http://127.0.0.1:<port>`,
  `http://localhost:<port>`, `http://[::1]:<port>`, the configured `AF_ADDR`
  host, or `AF_DEV_CORS_ORIGIN`. A connection without an `Origin` header (a
  non-browser client) still needs the token. Only request paths are logged,
  never query strings.
- **No side effects before auth.** A WebSocket attach to an unknown run ID
  returns an error and never starts a process. Attaching to a run that was
  stopped on purpose does not restart it; the web UI has an explicit
  "Restart session" action for that.

### 3. Other users on the same computer

- **Files.** The database (and its `-wal`/`-shm` files) is created `0600`
  before SQLite opens it, and re-chmodded on every open. The hook secret,
  `remote.json`, `account.json`, `machine-id`, the lock file, the env file and
  uploaded files are `0600`. The
  admin socket's directory is `0700` and the socket `0600`. The Tailscale state
  directory, which holds the node's private keys, is forced to `0700`. The data
  directory is created `0700` by the daemon.
- **Admin socket.** It has no TCP exposure; the filesystem permissions above are
  its access control.
- **Loopback port.** Other local users can connect to `127.0.0.1:4344`. They
  get the same treatment as anyone else: every API call needs a device token,
  except `pair/complete` and `pair/request` (rate limited) and the local
  `/health`, which reports the machine name, the `claude` version and session
  counts. Approving an access request from the local listener also needs a
  device token.
- **Tunnel listener.** With the system Tailscale, any local process can also
  connect to the loopback listener `tailscaled` forwards to and
  put whatever it likes in the forwarding header. That gains it nothing: it is
  the public root, the less trusted of the two, with the same checks as from
  the internet; the header only decides which rate-limit bucket it lands in.
- **Child environment.** Every `AF_*` variable except `AF_CLAUDE`, and
  Tailscale's `TS_*` variables, are removed from the environment of spawned
  agents.

A non-loopback `AF_ADDR` (for example `0.0.0.0:4344`) makes the local listener,
which is plain HTTP, reachable from your network. The daemon prints a warning
at startup when you do this. Every API call still needs a device token, but
tokens then cross your network unencrypted. Prefer `AF_REMOTE_MODE=tailnet`.

### 4. A lost or stolen phone

A paired phone holds its device token in the browser's `localStorage`.

- **Revoke it**: `agentflow devices` lists devices, `agentflow revoke <id>`
  revokes one (the web UI's pairing page can also revoke devices). Revocation
  clears the stored token hash, so the next HTTP request fails, and closes that
  device's open terminal and signaling WebSockets immediately; the command
  reports how many it closed. Other devices are unaffected. If the daemon is
  not running, `revoke` updates the database directly.
- **Belt and braces**: every open WebSocket re-checks its token once a minute
  and closes if the token is no longer valid, whatever revoked it.
- **Idle expiry**: a device token that goes unused for 30 days stops working.
- Any paired device can list and revoke devices through the API, so a thief
  could revoke your other devices too; re-pair them with `agentflow pair`.

### 5. A prompt-injected or misbehaving agent

This is the threat agentflow does least about. An agent runs as you with
permission prompts skipped. It can read and change anything your user can,
including agentflow's own database, hook secret, env file and Tailscale node
keys, and it can use the admin socket to mint pairing links. An agent that has
been talked into it could therefore hand remote access to someone else.

What agentflow does:

- Loop mail typed into member terminals is sanitized first: control characters
  (except newline and tab), escape sequences and bracketed-paste markers are
  stripped, and a body over 16 KB is replaced by a pointer to read it through
  the mail API.
- The member mail API that agents use to talk to each other is local-only and
  uses member tokens, a separate credential from device tokens. A member's
  token is kept in an owner-only file under `<data dir>/member-tokens/` and is
  not passed on the command line. Every member runs as you with the same
  permissions, though, so the rule that workers only talk through the
  orchestrator keeps cooperating agents organized; it is not a security
  boundary between them.
- Attachments sent from a phone are accepted only as PNG, JPEG, WebP or GIF,
  identified from their content, not their name; file names are reduced to a
  safe leaf name inside the uploads directory; per-file (10 MB), per-session
  (200 MB, 100 files), global (2 GB) and rate limits apply; files are deleted
  after 7 days.

Review what you ask agents to work on, and do not point them at untrusted
content you would not run yourself.

### 6. Tailscale

Using Tailscale for remote access means trusting Tailscale:

- Tailscale's coordination server distributes your tailnet's keys and access
  rules. In `tailnet` mode, anything Tailscale (or anyone with admin access to
  your tailnet) lets onto the tailnet can reach the listener; they still need a
  device token.
- Funnel traffic enters through Tailscale's ingress servers, which forward the
  TLS connection without decrypting it. TLS terminates on this computer: in
  the agentflow process with the embedded node, in `tailscaled` with the
  system Tailscale (which then forwards plain HTTP over loopback to agentflow).
- Tailscale controls the `ts.net` DNS zone, so in principle it could obtain a
  certificate for your node's name and intercept Funnel traffic. Device tokens
  and pairing tokens would be exposed to such an attacker.
- With the embedded node, Tailscale client log upload is disabled: the daemon
  sets `TS_NO_LOGS_NO_SUPPORT=true` before the node starts, unless you set
  `AF_TS_LOGS=on`. The system Tailscale follows its own settings. Tailscale's
  control plane still sees node metadata as it does for any Tailscale device.
- With the system Tailscale, agentflow changes one thing in its configuration:
  it adds a serve entry for port 8443 (with Funnel allowed in `funnel` mode)
  and removes it on a clean shutdown. It leaves every other serve entry alone,
  refuses to take over port 8443 from anything else, and never signs the
  computer's Tailscale in or out. On Linux this needs
  `sudo tailscale set --operator=$USER` once, which lets your user change
  Tailscale's settings in general, not only agentflow's entry.
- `agentflow remote logout` logs the embedded node out; that machine stays
  listed in your Tailscale admin console until you remove it there.

### 7. The account service

If you sign in with an email, the account service learns your email address,
this machine's random id, host name, transport, current public address and
agentflow version, and when it last reported. Whoever runs it, or gets into
it, learns where your agentflow is reachable: the public address, which is
already discoverable (see threat 1) and still requires pairing. It can't pair
a device or reach a terminal: it never gets device or pairing tokens, and it
is not in the path between a phone and the computer. It does email you links;
treat an unexpected "Your agentflow link" email like any other. The session
token for it is stored in `<data dir>/account.json` (`0600`);
`agentflow account logout` revokes it on the service and deletes the file.

## Token lifecycle

| Token | Issued by | Stored | Lifetime |
|---|---|---|---|
| Pairing token | `agentflow pair` / `agentflow start` (through the admin socket, or straight into the database when the daemon is stopped) | SHA-256 hash | single use, and expires 15 minutes after it was minted |
| Device token | `POST /api/v1/agentd/pair/complete`, or the first poll of an approved access request, once per pairing | SHA-256 hash in the database; plaintext only in that browser's `localStorage` and, on the public listener, its `af_device` cookie | until revoked, or 30 days without use (each authenticated request extends it) |
| Access request poll secret | `POST /api/v1/agentd/pair/request` | SHA-256 hash in the daemon's memory; plaintext only in the requesting page's memory | until the request is collected or denied, or 5 minutes pass (forgotten 5 minutes later); gone when the daemon restarts |
| Account session token | the account service, after the emailed 6-digit code | `<data dir>/account.json`, `0600` | until `agentflow account logout` or the account is deleted |
| Hook secret | generated on first start (32 random bytes, hex) | `<data dir>/hook.secret`, `0600` | until the file is deleted |

The pairing link has the form
`<base>/pair/#token=…&expires=…`. The token rides in the URL fragment, which
browsers never send to the server, and the pairing page removes it from the
address bar as soon as it reads it. The pairing page refuses a link whose
`expires` time has passed, and the daemon refuses the token itself once its
15 minutes are up. Treat an unused pairing link as a secret until it has been
used or has expired.

Rotating the credentials the engines use (Anthropic, OpenCode providers) is
outside agentflow: agentflow never stores them. Update the engine's own login,
then `agentflow restart`.

## What is stored, and where

Defaults; `AF_DB`, `AF_DATA_DIR` and `AF_UPLOADS_DIR` move them.

| Path | Mode | Contents |
|---|---|---|
| `~/.config/agentflow/agentflow.env` | `0600` (directory `0700` when agentflow creates it) | settings |
| `~/.local/share/agentflow/agentflow.db` (+ `-wal`, `-shm`) | `0600` | session index and conversation events (prompts, responses, tool inputs and outputs, thinking content) for the sessions agentflow runs and the OpenCode sessions it indexes from OpenCode's own storage; loops and their mail; device and pairing token hashes; approvals |
| `~/.local/share/agentflow/agentflow.db.lock` | `0600` | single-instance lock (PID) |
| `~/.local/share/agentflow/hook.secret` | `0600` | approvals hook secret |
| `~/.local/share/agentflow/remote.json` | `0600` | last remote-access status |
| `~/.local/share/agentflow/run/agentflow.sock` | `0600` in a `0700` directory | admin socket |
| `~/.local/share/agentflow/tailscale/` | `0700` | embedded Tailscale node state, including its private keys, and the `hostname` file |
| `~/.local/share/agentflow/tailscale-serve.json` | `0600` | the serve entry agentflow added to the system Tailscale (host, port, proxy target) |
| `~/.local/share/agentflow/account.json` | `0600` | email, account session token, machine id, sign-in time |
| `~/.local/share/agentflow/machine-id` | `0600` | random id this machine is known by on the account service |
| `~/.local/share/agentflow/defaults/` | `0750`, files `0600` | read-only reference copy of the built-in loop plays and rules |
| `~/.local/share/agentflow/uploads/` | `0700`, files `0600` | image attachments, deleted after 7 days |
| `~/.config/systemd/user/agentflow.service` or `~/Library/LaunchAgents/com.arthurobo.agentflow.plist` | `0644` | service definition (binary path and `PATH`; no secrets) |

Session history in the database (events, including tool input/output and
thinking content) is pruned once a day for sessions untouched for
`AF_RETENTION_DAYS` days (90 by default; `0` keeps everything). Sessions with a
live run are kept. Remove the database (`agentflow uninstall --purge`) to delete
everything at once.

To list sessions, the daemon reads Claude Code transcripts under
`~/.claude/projects` (each file's first lines for its folder and first prompt,
and its title records) and OpenCode's
session storage. It never writes there for listing, so a paired device can see
the names, folders and times of every session on the machine, which it could
open anyway.

agentflow also writes outside its own directories:

- `~/.claude.json`: before starting a terminal in a directory, it marks that
  directory as trusted (`hasTrustDialogAccepted`,
  `hasClaudeMdExternalIncludesApproved`), so Claude's trust prompt doesn't
  block a phone. `agentflow uninstall` does not undo this.
- Claude transcripts: `agentflow make-resumable` (and the daemon, when a
  browser-started chat session is terminated) rewrites a session's launch
  markers so it appears in `claude -r`, after saving a
  `.agentflow-bak-<unix time>` backup next to the transcript.

## What agentflow does not protect against

- **A paired device, or anyone holding its token.** It can start terminals in
  any directory and run any command as you.
- **Agents.** They are not sandboxed; see threat 5.
- **Your user account being compromised.** Anything running as you can read
  agentflow's data and use its admin socket.
- **Tailscale, or your tailnet's admins, acting against you.** See threat 6.
- **Denial of service** beyond the per-client rate limits.

Thank you for helping keep agentflow and its users safe.
