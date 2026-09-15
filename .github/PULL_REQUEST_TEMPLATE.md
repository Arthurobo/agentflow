<!-- Thanks for contributing to agentflow! -->

## What does this change?

<!-- The change and the problem it solves. -->

## Related issue

<!-- e.g. Closes #123 -->

## Checklist

- [ ] `make vet` and `make test` pass (tests run in a sandboxed `HOME`)
- [ ] `gofmt -s -l .` prints nothing
- [ ] `make lint` and `make darwin-check` pass
- [ ] If `web/` changed: `npm run lint`, `npm run typecheck`, `npm test` and `npm run build` pass
- [ ] Tests added or updated for the change; no test touches the real `HOME`, spawns a real engine outside a sandbox, starts a real Tailscale node or `cloudflared`, calls the real provision endpoint or calls the real service manager
- [ ] README, `docs/`, SECURITY.md and `.env.example` updated if commands, settings, paths or security behavior changed
- [ ] No secrets, tokens, real transcripts or personal paths added
