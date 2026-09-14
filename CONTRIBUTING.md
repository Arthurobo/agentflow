# Contributing to agentflow

Thanks for taking the time to contribute.

## Ways to help

- **Report a bug**: open an issue with the bug template. `agentflow version`
  and `agentflow doctor` output help a lot.
- **Suggest a feature**: open an issue describing the problem you want solved,
  not only the solution.
- **Send a pull request**: fixes, engines, docs, tests. Small, focused PRs get
  reviewed fastest.

Security issues go through private reporting, not public issues; see
[SECURITY.md](SECURITY.md).

## Development setup

You need:

- [Go](https://go.dev/dl/) 1.26 (see `go.mod`);
- [Node.js](https://nodejs.org) and npm to build the web UI (CI uses Node 22);
- `claude` and/or `opencode` on your `PATH` if you want to run agents by hand.

```sh
git clone https://github.com/arthurobo/agentflow
cd agentflow
make build              # web UI + Go binary: ./bin/agentflow
./bin/agentflow version
```

Make targets:

| Target | What it does |
|---|---|
| `make build` | `make web`, then build `./bin/agentflow` with the web UI embedded |
| `make build-go` | build only the Go binary; it serves whatever is in `internal/webui/dist` (a placeholder page if empty) |
| `make web` | `npm ci && npm run build` in `web/`, then copy `web/out` into `internal/webui/dist` |
| `make test` | `go test -race -count=1` over every Go package, in a sandboxed `HOME` |
| `make vet` | `go vet` |
| `make fmt` | `gofmt -s -w` on every tracked Go file |
| `make lint` | `golangci-lint` (v2.13.2, installed with `go install` if missing) |
| `make vuln` | `govulncheck` |
| `make darwin-check` | build and vet for macOS (catches Linux-only code) |
| `make release-snapshot` | `goreleaser release --snapshot --clean` into `dist/` (needs goreleaser) |
| `make install` | build and install to `~/.local/bin/agentflow` |
| `make service` | `make install`, then `agentflow start` |

### Running a development daemon

The installed service already holds the default database and port. Run a
second daemon with its own database, port, and no Tailscale:

```sh
AF_DB=/tmp/af-dev/agentflow.db AF_ADDR=127.0.0.1:4355 AF_REMOTE=off ./bin/agentflow serve
AF_DB=/tmp/af-dev/agentflow.db AF_ADDR=127.0.0.1:4355 ./bin/agentflow pair
```

A development daemon is a real daemon: the terminals it starts run real
`claude`/`opencode` processes in your real home directory.

To work on the web UI with hot reload, allow the dev server's origin on the
daemon and point the dev server at it:

```sh
AF_DEV_CORS_ORIGIN=http://localhost:3000 AF_DB=/tmp/af-dev/agentflow.db \
  AF_ADDR=127.0.0.1:4355 AF_REMOTE=off ./bin/agentflow serve
cd web && NEXT_PUBLIC_AGENTD_URL=http://127.0.0.1:4355 npm run dev
```

## Tests must never touch your real home directory

Run Go tests with **`make test`**, not a bare `go test ./...`.

`make test` creates a temporary directory and sets `HOME`, `XDG_CONFIG_HOME`
and `XDG_DATA_HOME` to it, and sets `AF_REMOTE=off`. agentflow reads and writes
real user state: `~/.claude.json`, Claude and OpenCode transcripts,
`~/.config/agentflow`, `~/.local/share/agentflow`. A test that escapes its
sandbox can rename or rewrite your live Claude sessions; that has happened
before, and it is worse than whatever bug the test was chasing.

Rules for tests:

- Never depend on the real `HOME`. Tests that need a home directory use
  `t.TempDir()` and `t.Setenv("HOME", …)`.
- Never spawn a real engine through the real spawner without a sandboxed
  `HOME` and a sandboxed transcript root.
- Never start a real Tailscale node. `internal/remote` has a `Fake` remote,
  and the tsnet run loop is tested through a fake node.
- Never call the real service manager. `cmd/agentflow` routes every
  `systemctl`, `launchctl` and `loginctl` call through a runner that tests
  replace.

## Before you open a PR

Run what CI runs (`.github/workflows/ci.yml`):

```sh
gofmt -s -l .        # must print nothing (or run make fmt)
make vet
make test
make darwin-check
make lint
make vuln
```

If you touched `web/`:

```sh
cd web
npm ci
npm run lint
npm run typecheck
npm test
npm run build
```

Then `make build` to embed the new export and try it in a browser.

Expectations:

- **Tests** for behavior you add or change.
- **Docs** updated when commands, settings, paths or security behavior change
  (README, `docs/`, SECURITY.md, `.env.example`). The default env file in
  `cmd/agentflow/envfile.go` and `.env.example` must stay identical; a test
  checks it.
- **Comments** explain why, not what, and stay accurate.
- **Commits** have clear messages; Conventional Commit prefixes (`fix:`,
  `feat:`, `docs:`) are welcome but not required.
- **No secrets, tokens or personal paths** in code, fixtures or logs. Transcript
  fixtures under `internal/claudelog/testdata` must be synthetic (see
  `internal/claudelog/testdata/fixtures/README.md`).

## Pull request process

1. Fork and branch off `main`.
2. Make the change, with tests and docs.
3. Run the checks above.
4. Open the PR against `main`, describe the change and link any issue.
5. Address review feedback with more commits.

## License

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE) that covers this project.
