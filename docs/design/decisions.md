# Key Decisions

Architectural and design decisions for onedrive-go. Referenced from [CLAUDE.md](../../CLAUDE.md).

- **"Pragmatic Flat" architecture**: 5 packages (`cmd/onedrive-go/`, `internal/graph/`, `internal/sync/`, `internal/config/`, `pkg/quickxorhash/`), consumer-defined interfaces
- **CLI-first development order**: Build a working CLI tool (Phase 1) before building the sync engine (Phase 4)
- **graph/ is internal/**: `internal/graph/` not `pkg/graph/` (SDK carveout deferred)
- **Binary name**: `onedrive-go` (discoverable in apt, doesn't conflict with abraunegg)
- **License**: MIT
- **Platforms**: Linux (primary) + macOS (primary), FreeBSD best-effort
- **CLI design**: Unix-style verbs (`ls`, `get`, `put`, `sync`) — see PRD
- **Config format**: TOML (via BurntSushi/toml)
- **Sync database**: SQLite with WAL mode
- **Conflict handling**: Keep both + conflict ledger with resolution tracking
- **Multi-account**: Single config file, single daemon, multiple profiles
- **Transfers**: Parallel (default 8 each for uploads/downloads/checkers), with bandwidth scheduling
- **Real-time**: WebSocket for remote changes, inotify/FSEvents for local
- **Safety**: Conservative defaults (big-delete protection, dry-run, recycle bin)
- **API quirks**: All 12+ known Graph API quirks handled from day one
- **SharePoint enrichment**: Per-side hash baselines, not download-after-upload — see [sharepoint-enrichment.md](sharepoint-enrichment.md)
- **Non-goals**: Multi-cloud, GUI, encryption, mobile, Windows
- Go 1.23+, Cobra CLI, golangci-lint v2, 140 char line length, fieldalignment disabled
