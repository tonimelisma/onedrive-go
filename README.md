# onedrive-go

[![CI](https://github.com/tonimelisma/onedrive-go/actions/workflows/ci.yml/badge.svg)](https://github.com/tonimelisma/onedrive-go/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/tonimelisma/onedrive-go)](https://goreportcard.com/report/github.com/tonimelisma/onedrive-go)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A fast, safe, and well-tested OneDrive CLI and sync client written in Go. Unix-style file operations (`ls`, `get`, `put`) plus robust bidirectional sync with conflict tracking.

## Status

**Early development** — building the foundation. The config package and design docs are complete. Currently implementing the Graph API client (Phase 1).

See the [roadmap](docs/roadmap.md) for the full plan.

## Planned Features

- **CLI file operations**: `ls`, `get`, `put`, `rm`, `mkdir` — familiar Unix-style commands
- **Bidirectional sync**: Delta-based with three-way merge and conflict detection
- **Multi-account**: Single config, multiple profiles
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
