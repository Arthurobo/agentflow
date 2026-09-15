# Release checklist

Manual checks for a maintainer before tagging a release. CI covers everything
that doesn't need a real Tailscale account, the account service, a phone or a
service manager; this list covers the rest. Use a spare user account (or VM) where a step installs,
purges or logs out, so your own setup and sessions are never at risk.

Throughout, `$PORT` is `4344` and `$DATA` is `~/.local/share/agentflow`
unless you changed `AF_ADDR` or `AF_DB`.

## 1. Before tagging

- [ ] CI is green on the commit to tag: `go` (gofmt, vet, `make test`,
      `make darwin-check`), `golangci-lint`, `govulncheck`, `web` (lint,
      typecheck, test, build).
- [ ] `CHANGELOG.md` has the version and date; README and `docs/` match any
      changed commands, settings or paths; `.env.example` matches
      `defaultEnvFile` (`make test` fails otherwise).
- [ ] The pinned `cloudflared` is current: compare `Version` in
      `internal/remote/cloudflared/install.go` with the latest
      [release](https://github.com/cloudflare/cloudflared/releases); to move,
      follow the steps in that file's comment (asset and extracted-binary
      SHA-256 for all four platforms, `/ready`, the log lines and
      `TUNNEL_TOKEN` still work).
- [ ] `make release-snapshot` succeeds. In `dist/` there are four archives named
      `agentflow_<version>_{linux,darwin}_{amd64,arm64}.tar.gz` and a
      `checksums.txt`. Each archive contains `agentflow`, `README.md`,
      `LICENSE`, `CHANGELOG.md` and `SECURITY.md`.
- [ ] The snapshot binary serves the real web UI: run it with
      `AF_REMOTE=off AF_DB=/tmp/af-rc/agentflow.db AF_ADDR=127.0.0.1:4355`
      and open `http://127.0.0.1:4355/`. The page must not say "Web UI not
      included in this build".

After the release workflow has published the tag:

- [ ] The release has the four archives, `checksums.txt`, `checksums.txt.sig`
      and `checksums.txt.pem`.
- [ ] The signature verifies:

      ```sh
      cosign verify-blob --certificate checksums.txt.pem --signature checksums.txt.sig \
        --certificate-identity-regexp '^https://github\.com/arthurobo/agentflow/\.github/workflows/release\.yml@refs/tags/v.*$' \
        --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
      sha256sum --check --ignore-missing checksums.txt
      ```

## 2. Install and first start (Linux and macOS)

On a clean user account with a Tailscale account that has never used Funnel:

- [ ] `curl -fsSL https://raw.githubusercontent.com/arthurobo/agentflow/main/install.sh | sh`
      with `cosign` installed prints "verifying the signature on checksums.txt
      with cosign" and "checksum verified"; without `cosign` it prints "cosign
      not found; skipping signature verification (checksums are still
      verified)".
- [ ] A tampered download is refused: serve a copy of the release with one byte
      of `checksums.txt` changed, run with `AGENTFLOW_RELEASE_BASE` pointing at
      it, and expect "signature verification failed; not installing" (with
      cosign) or "checksum mismatch for … ; not installing" (edit the hash, no
      cosign).
- [ ] **Email.** With no account, `agentflow start` shows "Email for your
      agentflow link and dashboard (optional, press Enter to skip):" and never
      asks how the phone should reach the computer. Enter skips it; entering an
      email and the emailed code signs in, and an email "Your agentflow link"
      arrives once the address is up.
- [ ] **System Tailscale.** On a machine with Tailscale installed and signed in,
      `agentflow start` shows (Linux, non-operator user)
      `sudo tailscale set --operator=$USER`; after running it, possibly the
      Funnel approval link, then "Remote URL:
      https://<machine>.<tailnet>.ts.net:8443 (Tailscale funnel)" and the
      pairing QR code. `tailscale serve status` shows the 8443 entry next to any
      entries you had, which are unchanged; `agentflow stop` removes only the
      8443 entry.
- [ ] **No Tailscale installed.** `agentflow start` shows the download link and the `AF_TAILSCALE=embedded` hint; with
      `AF_TAILSCALE=embedded` it shows the embedded node's sign-in link, "One
      click to allow public HTTPS (Tailscale Funnel) for this machine." and
      "Remote URL: https://agentflow-xxxxxx.<tailnet>.ts.net (Tailscale funnel)".
- [ ] While remote access is still waiting (sign-in, permission, approval),
      `agentflow pair` in another terminal shows what it waits for and no
      `127.0.0.1` link.
- [ ] Non-interactive: `agentflow start </dev/null` asks nothing and completes;
      `agentflow start --email you@example.com` still reads the code from
      stdin.
- [ ] On Linux without linger, `start` prints the `loginctl enable-linger $USER`
      note.
- [ ] `agentflow doctor` passes every check; `agentflow status` shows the
      service running, the daemon version, the transport, the remote URL and
      the email sign-in (or that it is optional and not set).
- [ ] macOS: `~/Library/LaunchAgents/com.arthurobo.agentflow.plist` exists and
      logs go to `~/Library/Logs/agentflow/agentflow.log`.

## 3. Phones and networks

On iOS Safari and Android Chrome, on Wi-Fi and on cellular:

- [ ] Scanning the pairing QR code pairs the phone and lands on the sessions
      list; the address bar no longer contains the token.
- [ ] Opening the address on an unpaired phone shows the pairing page; opening
      `/` on it is a blank page. **Request access** shows a 4-digit code; the
      same code appears at the `agentflow start` prompt and in
      `agentflow devices`; `agentflow approve <code>` pairs the phone within a
      few seconds and it lands on the sessions list. A denied request says so;
      one left alone for 5 minutes says it expired.
- [ ] The sessions list shows Claude Code sessions started in a terminal
      without agentflow and OpenCode sessions, newest first; a running one has
      a live dot; search and **Load more** work. Opening a session that isn't
      running resumes it and the URL changes to `/session/?id=<run id>`.
- [ ] A paired phone opening the address from a link in a mail app gets the
      app, not a blank page.
- [ ] **New session** starts a terminal; typing works with the on-screen keyboard.
- [ ] A terminal keeps streaming for 30+ minutes, and reconnects after the phone
      locks and unlocks.
- [ ] Attaching an image works on Wi-Fi; on cellular, where a direct connection
      may be impossible, the UI reports the failure instead of hanging.
- [ ] Adding the page to the home screen opens the app standalone at the
      sessions list.

## 4. Pairing and revocation

- [ ] Opening a pairing link a second time fails ("pairing token invalid or
      expired").
- [ ] A pairing link older than 15 minutes shows "This pairing link has expired.
      Run `agentflow pair` on your computer to get a new one."
- [ ] With a terminal open on the phone, `agentflow revoke <id>` prints
      "Revoked <id> and closed N live connection(s). Other devices are
      unaffected.", the phone's terminal closes within seconds, and the phone is
      sent back to the pairing page.
- [ ] Another paired device keeps working.
- [ ] A browser on this computer that is paired, on `http://127.0.0.1:$PORT/pair/`,
      lists a waiting access request with Approve and Deny, and Approve pairs
      the phone. The same page on the public address shows no such list.
- [ ] With the daemon stopped (`agentflow stop`), `agentflow revoke <id>` prints
      "agentflow isn't running, so no live sessions were open."; after
      `agentflow start` that device gets 401.

## 5. Restart, reboot and a second instance

- [ ] `agentflow restart`: running terminals stop; opening one offers
      **Restart session**, which brings it back. Opening it without choosing
      restart does not start it.
- [ ] Reboot (with linger on Linux): the service comes back, the Tailscale URL
      is unchanged, and the phone is still paired.
- [ ] With the service running, `agentflow serve` exits 1 with
      `agentflow: agentflow is already running (pid N; lock $DATA/agentflow.db.lock)`
      and running terminals are unaffected.

## 6. Exposure checks

Get a device token for curl: run `agentflow pair`, take the `token=` value
from the link, and

```sh
curl -s -X POST http://127.0.0.1:$PORT/api/v1/agentd/pair/complete \
  -H 'Content-Type: application/json' -d '{"token":"<pairing token>","name":"curl"}'
```

Use the returned `deviceToken` as `$TOK`, and revoke the `curl` device when
done.

Local listener:

- [ ] `curl -si -H 'Host: evil.example' http://127.0.0.1:$PORT/api/v1/agentd/health` → 421 `bad_host`
- [ ] `curl -si http://127.0.0.1:$PORT/api/v1/agentd/sessions` → 401
- [ ] `curl -si -H "Authorization: Bearer $TOK" http://127.0.0.1:$PORT/api/v1/agentd/sessions` → 200
- [ ] `curl -si -X POST http://127.0.0.1:$PORT/api/v1/agentd/pair/complete -d '{}'` → 400;
      with `-d '{"token":"wrong"}'` → 401; the sixth attempt within a minute → 429 `rate_limited`
- [ ] A WebSocket upgrade without a token → 401:
      `curl -si -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' "http://127.0.0.1:$PORT/api/v1/agentd/sessions/x/tty/ws"`
- [ ] The same with `?token=$TOK` and `-H 'Origin: https://evil.example'` → 403 `bad_origin`
- [ ] The same with `?token=$TOK` and no Origin, for a made-up run ID → 404, and
      no process is started
- [ ] `curl -si -X POST http://127.0.0.1:$PORT/api/v1/agentd/approvals/request -d x` → 401;
      with `-H "X-AgentFlow-Hook-Secret: $(cat $DATA/hook.secret)"` → 200 with
      `"reason":"bad_payload"`
- [ ] A body over 1 MB to any `/api/` route → 413 `body_too_large`

Public URL (`$URL` from `agentflow remote status`), from a machine outside the
tailnet, with the system Tailscale and with the embedded node:

- [ ] `curl -s $URL/api/v1/agentd/health` returns only `status`
- [ ] Without a cookie, `curl -si $URL/`, `$URL/session/` and
      `$URL/api/v1/agentd/sessions` → 404 with an empty body; `curl -si $URL/pair/`
      → 200
- [ ] `curl -si -H "Authorization: Bearer $TOK" -b "af_device=$TOK" $URL/api/v1/agentd/sessions`
      → 200; with only the cookie → 401
- [ ] Six `curl -si -X POST $URL/api/v1/agentd/pair/request -d '{}'` in a minute:
      the third from the same address → 429; the sixth → 429
- [ ] System Tailscale, from this computer: the loopback listener without
      `X-Forwarded-For` → 421 (`curl -si http://127.0.0.1:<port>/`, the port
      from `tailscale serve status`)
- [ ] `curl -si $URL/api/v1/agent/whoami` → 404;
      `curl -si -X POST $URL/api/v1/agentd/approvals/request -d x` → 404
- [ ] Responses carry `Strict-Transport-Security`; `curl -sI $URL/` carries
      `Content-Security-Policy`
- [ ] 21 requests with a wrong bearer token in a row → the client is answered
      429 `locked_out`

## 7. Remote modes

- [ ] Cloudflare default, fresh spare account (no `AF_REMOTE`, no Tailscale
      state): `agentflow start` needs no sign-in step, prints
      `Remote URL: https://<adjective>-<noun>-<4 digits>.useagentflow.xyz (Cloudflare)`,
      and a phone pairs and opens a terminal through it. `ps` shows no token
      on `cloudflared`'s command line; `cloudflare-tunnel.json` is mode `0600`;
      without `CF-Connecting-IP`, `curl -si http://127.0.0.1:4345/` → 421.
      `agentflow restart` keeps the same URL and pairing; with `AF_CLOUD_URL`
      pointed at an unreachable address, a restart still comes up on the saved
      tunnel.

- [ ] `AF_REMOTE_MODE=tailnet` in the env file, `agentflow restart`: the URL does
      not load from a device outside the tailnet, and loads from a phone with the
      Tailscale app in the same tailnet.
- [ ] `agentflow remote off`: `agentflow remote status` prints `off`;
      `agentflow pair` prints an `http://127.0.0.1:$PORT/pair/#token=…` link.
      `agentflow remote on` and `agentflow start` bring the same URL back.
- [ ] `agentflow remote logout` with `AF_TAILSCALE=embedded`: status goes to
      `needs_login` with a new sign-in link. With the system Tailscale it refuses
      and nothing is signed out.
- [ ] A machine set up with the embedded node before this release, with no
      system Tailscale, keeps its `agentflow-xxxxxx` URL and paired phones after
      `agentflow update`; `agentflow status` shows `tailscale embedded`.
- [ ] `agentflow account login` while agentflow runs prints "agentflow is running
      and now reports this machine's link." and the dashboard lists the
      machine; `agentflow account logout` stops the reporting.
- [ ] With `AF_TS_LOGS` unset, no connections to `log.tailscale.com` show up
      (resolver log or `ss -tnp`) while the daemon runs.

## 8. Update

- [ ] From the previous release, `agentflow update` prints "Downloading …",
      "Signature verified." (with cosign), "Checksum verified.", "Installed
      agentflow vX.Y.Z at …" and "Restarted the service."; `agentflow version`
      shows the new version; the phone is still paired.
- [ ] Running it again prints "agentflow vX.Y.Z is already the latest release."

## 9. Uninstall

In the spare account:

- [ ] Record `find ~ -maxdepth 4 | sort > /tmp/before`.
- [ ] `agentflow uninstall` without `--yes` lists what it will do and asks;
      answering no prints "Nothing was removed."
- [ ] `agentflow uninstall --purge --yes` prints "Logged out of Tailscale." only
      with the embedded node, removes the service and `~/.local/bin/agentflow`,
      deletes the files it
      listed, and ends with the "Not touched:" list (`~/.claude.json` trust
      entries and `.agentflow-bak-*` backups).
- [ ] `find ~ -maxdepth 4 | sort | diff /tmp/before -` shows only the service
      definition, the binary, `~/.config/agentflow`, and paths under `$DATA`
      removed. With the system Tailscale, `tailscale serve status` no longer
      lists port 8443. `~/.claude.json` is unchanged.
- [ ] A binary outside `~/.local/bin` is left in place with "not installed by
      agentflow; remove it yourself".
