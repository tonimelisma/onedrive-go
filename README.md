# onedrive-go

[![CI](https://github.com/tonimelisma/onedrive-go/actions/workflows/ci.yml/badge.svg)](https://github.com/tonimelisma/onedrive-go/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/tonimelisma/onedrive-go)](https://goreportcard.com/report/github.com/tonimelisma/onedrive-go)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A Go OneDrive CLI and sync client. It provides Unix-style file operations
(`ls`, `get`, `put`) plus bidirectional sync with conflict tracking.

## Status

**Active development** — capability claims use the repo requirement statuses:
`verified`, `implemented`, `designed`, `planned`, and `future`. A release claim
must not exceed the proof recorded in requirements, CI, live E2E artifacts, or
benchmark artifacts for that release SHA.

See the [requirements](spec/requirements/) for the source-of-truth capability
status and the full plan.

## Capability Status

| Capability | Status | Claim boundary |
| --- | --- | --- |
| CLI file operations (`ls`, `get`, `put`, `rm`, `mkdir`, `stat`, `mv`, `cp`) | `verified` | Covered by requirements and test evidence for normal command behavior. |
| Configuration, drive identity, and multi-drive selection | `verified` | Config/catalog/token ownership is requirements-backed; live account coverage remains credential-gated. |
| Bidirectional sync | `verified` | One-shot, watch, conflict preservation, retry/blocker state, and shortcut lifecycle are implemented with repo test evidence. |
| `sync --dry-run` | `verified` | No local sync-tree content mutation and no remote OneDrive content mutation; operational side effects such as logs, sockets, token refresh, config reconciliation, state open/checkpoint, and scratch planning DBs may still occur. |
| Microsoft Graph quirks | `verified` per documented quirk | Only quirks documented in `spec/reference/graph-api-quirks.md` with tests or live evidence are launch claims. Unknown provider behavior remains unclaimed. |
| OneDrive Business, SharePoint, and shared libraries | `implemented` / `verified` per requirement | Covered behavior is tracked in requirements and live suites; do not claim full support without a green live release lane on the release SHA. |
| Performance numbers | `planned` | Public "fast" claims require repo-owned benchmark artifacts with machine/date/methodology context. |
| Native packages and container images | `future` | Not a launch claim. |

## Platforms

- Linux (primary) — full CI: build, lint, unit tests, coverage gate, live E2E
- macOS (primary) — full CI: build, lint, unit tests
- FreeBSD (best-effort) — cross-compiled on every CI run; not otherwise tested

Every platform listed here is compiled on every CI run, so this list cannot
drift ahead of what actually builds.

## Building from Source

```bash
git clone https://github.com/tonimelisma/onedrive-go.git
cd onedrive-go
go build -ldflags "-X github.com/tonimelisma/onedrive-go/internal/cli.version=$(git rev-parse --short HEAD)" -o onedrive-go .
```

The version flag matters: a build without it reports version `dev` and, to
protect your real data during development, refuses to run unless
`XDG_DATA_HOME`, `XDG_CONFIG_HOME` and `XDG_CACHE_HOME` are all set
(`source scripts/dev-env.sh` sets them for a scratch tree).

Requires Go 1.24+.

## Running Under systemd

`sync --watch` supports `Type=notify` units: it reports `READY=1` once every
drive that could start is watching, keeps a one-line `STATUS=` ("Watching
~/OneDrive"), and answers `WatchdogSec=`. Under the journal its output carries
priorities, so `journalctl -p warning` works. An example user unit is in
[`contrib/systemd/onedrive-go.service`](contrib/systemd/onedrive-go.service):

```ini
[Service]
Type=notify
ExecStart=%h/.local/bin/onedrive-go sync --watch
Restart=on-failure
RestartSec=30s
RestartPreventExitStatus=77 78
WatchdogSec=2min
```

### Exit Status

| Status | Meaning |
| --- | --- |
| 0 | Success, or `sync --watch` stopped by a signal |
| 1 | Any other failure, including network, throttling and server errors; running again may succeed |
| 2 | Command-line mistake (unknown command or flag, wrong arguments) |
| 77 | Sign-in required: run `onedrive-go login` (EX_NOPERM) |
| 78 | Invalid configuration: fix `config.toml` or `--drive` (EX_CONFIG) |
| 130 | Stopped with Ctrl-C |

77 and 78 mean that running again unchanged fails the same way, which is why
the unit above sets `RestartPreventExitStatus=77 78`. `sync --watch` exits with
one of them only when every drive that could not start failed for that reason.

## Development

New contributors should start with the
[new developer onboarding guide](spec/reference/developer-onboarding.md) for a
repo-wide architecture tour and reading order.

Work is done in PR-backed increments from isolated worktrees. Fetch first, then
start from current `origin/main`:

```bash
git fetch origin
go run ./cmd/devtool worktree add --path <path> --branch <branch>
```

The canonical local verification entrypoint is:

```bash
go run ./cmd/devtool verify default
```

The Definition of Done is automated through staged verifier runs:

```bash
go run ./cmd/devtool verify --dod --stage start
go run ./cmd/devtool verify --dod --stage pre-pr
go run ./cmd/devtool verify --dod --stage pre-merge --pr <number>
go run ./cmd/devtool verify --dod --stage post-merge --pr <number> --worktree <path> --branch <branch>
```

`start` audits unresolved review threads from recent merged PRs and writes the
ignored `.dod-pr-comments.json` manifest. Agents must manually classify every
unresolved thread as `fixed`, `already_fixed`, or `non_actionable` before the
DoD can pass. `fixed` and `already_fixed` require `what_changed`, `how_fixed`,
and evidence entries; `non_actionable` requires a reason. There is no passing
deferred state.

`pre-pr` runs the local verification profile, checks the branch is based on
latest `origin/main`, and rejects undecided PR-comment carryover. `pre-merge`
waits for PR CI (`verify`, `integration`, and `e2e`; PR `stress` may be
skipped), posts the manifest's templated replies, resolves handled review
threads, and requires zero unresolved review threads. `post-merge` performs the
squash merge, handles already-completed multi-worktree merge/cleanup cases,
fast-forwards root `main`, removes the increment worktree and branch, validates
the configured post-merge CI-skip rule when applicable, and runs
`cleanup-audit`.

Direct package-level commands are still useful during short loops:

```bash
go build ./...                    # Build
go test -race ./...               # Test with race detector
golangci-lint run                 # Lint
```

See [AGENTS.md](AGENTS.md), [CLAUDE.md](CLAUDE.md), and the
[system architecture doc](spec/design/system.md) for repo workflow and
architecture details.

## Release Readiness

A release is not ready until these proof gates are current for the release SHA:

- `go run ./cmd/devtool verify default` passes cleanly.
- Manual live E2E is green for the release SHA; broader SharePoint/shared-library
  claims require matching live coverage.
- Any public performance claim has a repo-owned benchmark artifact with
  scenario, machine, date, method, and subject-under-test context.
- Side-effect expectations are current, especially dry-run and destructive
  command behavior.
- README and spec language use only `verified`, `implemented`, `designed`,
  `planned`, or `future` claims and contain no launch-blocking stale promises.

## License

[MIT](LICENSE)
