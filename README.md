# onedrive-go

[![CI](https://github.com/tonimelisma/onedrive-go/actions/workflows/ci.yml/badge.svg)](https://github.com/tonimelisma/onedrive-go/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/tonimelisma/onedrive-go)](https://goreportcard.com/report/github.com/tonimelisma/onedrive-go)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A fast, safe, and well-tested OneDrive CLI and sync client written in Go. Unix-style file operations (`ls`, `get`, `put`) plus robust bidirectional sync with conflict tracking.

## Status

**Active development** — Phases 1-3.5 complete. Working CLI with auth, file ops, and config integration. Phase 4 v2: event-driven sync engine rewrite in progress.

See the [roadmap](docs/roadmap.md) for the full plan.

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

```bash
go build ./...                    # Build
go test -race ./...               # Test with race detector
golangci-lint run                 # Lint
```

See [CLAUDE.md](CLAUDE.md) for architecture, conventions, and contribution guidelines.

## License

[MIT](LICENSE)
