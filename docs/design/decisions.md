# Key Decisions

Architectural and design decisions for onedrive-go. Referenced from [CLAUDE.md](../../CLAUDE.md).

- **"Pragmatic Flat" architecture**: 5 packages (`cmd/onedrive-go/`, `internal/graph/`, `internal/sync/`, `internal/config/`, `pkg/quickxorhash/`), consumer-defined interfaces
- **CLI-first development order**: Build a working CLI tool (Phase 1) before building the sync engine (Phase 4)
- **graph/ is internal/**: `internal/graph/` not `pkg/graph/` (SDK carveout deferred)
- **Binary name**: `onedrive-go` (discoverable in apt, doesn't conflict with abraunegg)
- **License**: MIT
- **Platforms**: Linux (primary) + macOS (primary), FreeBSD best-effort
- **CLI design**: Unix-style verbs (`ls`, `get`, `put`, `sync`) — see PRD
- **Config format**: TOML (via BurntSushi/toml), flat layout with quoted drive sections
- **Sync database**: SQLite with WAL mode, one DB per drive
- **Conflict handling**: Keep both + conflict ledger with resolution tracking
- **Multi-account**: Accounts (auth) and drives (sync) are separate concepts. `--account` for auth commands, `--drive` for everything else. Single config file, single daemon, multiple drives. See [accounts.md](accounts.md) for the full design.
- **Canonical drive identifiers**: `type:email[:site:library]` format derived from real data (e.g., `personal:toni@outlook.com`, `sharepoint:alice@contoso.com:marketing:Documents`). No arbitrary names. `:` replaced with `_` in filenames.
- **Transfers**: Parallel (default 8 each for uploads/downloads/checkers), with bandwidth scheduling
- **Real-time**: WebSocket for remote changes, inotify/FSEvents for local
- **Safety**: Conservative defaults (big-delete protection, dry-run, recycle bin)
- **API quirks**: All 12+ known Graph API quirks handled from day one
- **SharePoint enrichment**: Per-side hash baselines, not download-after-upload — see [sharepoint-enrichment.md](sharepoint-enrichment.md)
- **SharePoint token sharing**: SharePoint drives share OAuth token with the business account (same user, same session). Token per-user, state DB per-drive.
- **Fuzzy drive matching**: `--drive` resolves via exact canonical ID, alias, type prefix, email, or partial match. Shortest unique match wins. Ambiguous -> error with suggestions.
- **Login auto-adds primary drive**: `login` auto-creates config section for the primary drive (personal or business). SharePoint libraries added via `drive add`.
- **Text-level config manipulation**: Read via TOML parser, write via surgical line-based text edits to preserve comments. No round-trip serialization.
- **No config show command**: Users read config file directly. `status` shows runtime state. `--debug` shows config resolution.
- **Setup wizard for interactive config**: `setup` command replaces `config init`. One guided wizard for all configuration. No `config set` / `exclude add` CLI sprawl.
- **Service management**: `service install/uninstall/status` for systemd/launchd. Never auto-enables.
- **Email change detection**: Stable user GUID from Graph API. Auto-rename token/state/config on email change. No re-sync needed.
- **Non-goals**: Multi-cloud, GUI, encryption, mobile, Windows
- Go 1.23+, Cobra CLI, golangci-lint v2, 140 char line length, fieldalignment disabled

For the complete account/drive system design, see [accounts.md](accounts.md).
