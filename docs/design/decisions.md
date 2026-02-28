# Key Decisions

Architectural and design decisions for onedrive-go. Referenced from [CLAUDE.md](../../CLAUDE.md).

---

## Architecture Pivot (Option E)

- **Event-driven sync architecture**: Observers -> ChangeBuffer -> Planner -> Executor -> BaselineManager. Event-driven coordination through immutable events. See [event-driven-rationale.md](event-driven-rationale.md) for the full analysis.
- **Baseline-only persistence**: 11-column `baseline` table for confirmed synced state. Deletions remove the baseline row. See [data-model.md](data-model.md).
- **Pure-function Planner**: No I/O, no DB access. Takes (events + baseline) -> ActionPlan. Deterministic, exhaustively testable with table-driven tests. Every decision matrix cell (EF1-EF14, ED1-ED8) is independently verifiable.
- **Baseline-only move detection**: Moves detected via frozen baseline snapshot during observation. The baseline provides the "before" view for both remote and local move detection.
- **Path-keyed local observations**: Local observations keyed by path. Remote observations have server IDs. The baseline maps between them.
- **Sole DB writer**: BaselineManager is the only component that writes to the database. Outcomes + delta token committed atomically in a single transaction.
- **Watch-primary design**: `sync --watch` is the primary runtime mode. One-shot is "observe everything, process as one batch." Same planner, same executor, same baseline manager for both modes.
- **Change buffer with debounce**: Prevents processing the same file multiple times during rapid edits. Groups events by path. Move events dual-keyed (new path + synthetic delete at old path).
- **Symmetric filter application**: Filter applied in Planner to both remote-only AND local-only items. Applied symmetrically to both sides.
- **Per-side hash baselines**: `local_hash` and `remote_hash` columns handle SharePoint enrichment natively without special code paths. See [sharepoint-enrichment.md](sharepoint-enrichment.md).
- **Executor produces Outcomes, not DB writes**: Decouples execution from persistence. Each Outcome is self-contained with everything needed for baseline update.
- **Retries inside executor**: Exponential backoff with jitter happens inside the executor before producing the final Outcome. A failed Outcome means all retries were exhausted.

---

## Original Decisions (still valid)

- **"Pragmatic Flat" architecture**: 6 packages (`cmd/onedrive-go/`, `internal/driveid/`, `internal/graph/`, `internal/sync/`, `internal/config/`, `pkg/quickxorhash/`), consumer-defined interfaces
- **CLI-first development order**: Build a working CLI tool (Phase 1) before building the sync engine (Phase 4)
- **graph/ is internal/**: `internal/graph/` not `pkg/graph/` (SDK carveout deferred)
- **Binary name**: `onedrive-go` (discoverable in apt, doesn't conflict with abraunegg)
- **License**: MIT
- **Platforms**: Linux (primary) + macOS (primary), FreeBSD best-effort
- **CLI design**: Unix-style verbs (`ls`, `get`, `put`, `sync`) — see PRD
- **Config format**: TOML (via BurntSushi/toml), flat layout with quoted drive sections
- **Sync database**: SQLite baseline DB with WAL mode, one DB per drive
- **Conflict handling**: Keep both + conflict tracking with resolution tracking
- **Multi-account**: Accounts (auth) and drives (sync) are separate concepts. `--account` for auth commands, `--drive` for everything else. Single config file, single daemon, multiple drives. See [accounts.md](accounts.md) for the full design.
- **Canonical drive identifiers**: `type:email[:site:library]` format derived from real data (e.g., `personal:toni@outlook.com`, `sharepoint:alice@contoso.com:marketing:Documents`). No arbitrary names. `:` replaced with `_` in filenames.
- **Transfers**: Parallel (default 8 each for uploads/downloads/checkers), with bandwidth scheduling
- **Real-time**: WebSocket for remote changes, inotify/FSEvents for local
- **Safety**: Conservative defaults (big-delete protection, dry-run, recycle bin). S1-S7 invariants implemented as pure functions in the Planner.
- **API quirks**: All 12+ known Graph API quirks handled at the observer boundary (invisible to downstream)
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
