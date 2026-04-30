# onedrive-go

[![CI](https://github.com/tonimelisma/onedrive-go/actions/workflows/ci.yml/badge.svg)](https://github.com/tonimelisma/onedrive-go/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/tonimelisma/onedrive-go)](https://goreportcard.com/report/github.com/tonimelisma/onedrive-go)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A fast, safe, and well-tested OneDrive CLI and sync client written in Go. Unix-style file operations (`ls`, `get`, `put`) plus robust bidirectional sync with conflict tracking.

## Status

**Active development** — Phases 1-3.5 complete. Working CLI with auth, file ops, and config integration. Phase 4 v2: event-driven sync engine rewrite in progress.

See the [requirements](spec/requirements/) for the full plan.

## Planned Features

- **CLI file operations**: `ls`, `get`, `put`, `rm`, `mkdir` — familiar Unix-style commands
- **Bidirectional sync**: Delta-based with three-way merge and conflict detection
- **Multi-account**: Single config, multiple drives
- **Safety first**: Big-delete protection, dry-run mode, recycle bin support
- **Graph API quirk handling**: All 12+ known Microsoft Graph API quirks handled
- **SharePoint/OneDrive Business**: Full support including shared libraries

## Platforms

- Linux (primary)
- macOS (primary)
- FreeBSD (best-effort)

## Building from Source

```bash
git clone https://github.com/tonimelisma/onedrive-go.git
cd onedrive-go
go build ./...
```

Requires Go 1.24+.

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

## License

[MIT](LICENSE)
