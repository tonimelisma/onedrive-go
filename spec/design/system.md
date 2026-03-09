# System Architecture

Implements: R-6.2.1 [implemented], R-6.2.2 [implemented], R-6.3.1 [implemented]

## Package Structure

```
.                             Root package (Cobra CLI commands)
internal/
  config/                     TOML config, drive sections, XDG paths, override chain
  driveid/                    Type-safe drive identity: ID, CanonicalID, ItemKey (leaf, stdlib-only)
  driveops/                   Authenticated drive access: sessions, transfers, hashing
  graph/                      Graph API client: auth, retry, items CRUD, delta, transfers
  logfile/                    Log file creation, rotation, retention
  retry/                      Retry policies, exponential backoff with jitter (leaf, stdlib-only)
  sync/                       Event-driven sync engine (see pipeline below)
  tokenfile/                  Pure OAuth token file I/O (leaf, stdlib + oauth2 only)
pkg/
  quickxorhash/               QuickXorHash algorithm (vendored from rclone, BSD-0)
e2e/                          E2E test suite (not production code)
testutil/                     Shared test helpers (not production code)
```

## Dependency Rules

```
root pkg (CLI) → internal/driveops/ → internal/graph/ → pkg/*
                 internal/sync/    → internal/driveops/
                 internal/config/  → internal/driveid/
```

- No cycles. `driveops` does NOT import `sync`. `graph` does NOT import `config`.
- `driveid`, `tokenfile`, and `retry` are leaf packages (no internal imports).
- Both `graph` and `config` import `tokenfile` for token file I/O.
- Callers pass token paths to `graph` — no config coupling.

## Event-Driven Sync Pipeline

The sync engine uses an event-driven pipeline. One-shot sync is "collect all events, then process as one batch." Watch mode is the same pipeline triggered incrementally.

```
Remote Observer ──┐
                  ├──→ Change Buffer ──→ Planner ──→ Executor ──→ Baseline
Local Observer  ──┘         │                │            │           │
                      debounce/dedup    pure function   workers    per-action
                                        (no I/O)      (parallel)   commit
```

### Design Principles

1. **No database coordination.** Observers produce ephemeral events. The planner reads baseline (cached in memory). The executor writes outcomes per-action. The database is never the coordination mechanism between stages.
2. **Remote state separation.** Three-table model: `remote_state` (observed), `baseline` (confirmed synced), `sync_failures` (issues). Remote observations persist at observation time, decoupling the delta token from sync success. See [data-model.md](data-model.md).
3. **Pure-function planning.** The planner has no I/O. `Plan(changes, baseline, mode, safety) → ActionPlan`. Every decision is deterministic and reproducible.
4. **Watch-primary.** `sync --watch` is the primary runtime mode. All components serve both one-shot and watch modes.
5. **Interface-driven testability.** All I/O (filesystem, network, database) is behind interfaces.

### Rationale

Event-driven processing was chosen over four alternatives (shared mutable DB, multi-table split, deferred persistence, pure snapshot pipeline) because it eliminates database coordination anti-patterns: tombstone split, synthetic ID lifecycle, SQLITE_BUSY during execution, incomplete lifecycle predicates, pipeline phase ordering, and asymmetric filter application. The event-driven model generalizes correctly — watch mode is the natural case, one-shot is the degenerate batch case.

## Module Design Docs

For detailed module design, see:

| Module | Design Doc |
|--------|-----------|
| Graph API client | [graph-client.md](graph-client.md) |
| Configuration | [config.md](config.md) |
| Drive identity | [drive-identity.md](drive-identity.md) |
| Drive transfers | [drive-transfers.md](drive-transfers.md) |
| Retry infrastructure | [retry.md](retry.md) |
| Sync observation | [sync-observation.md](sync-observation.md) |
| Sync planning | [sync-planning.md](sync-planning.md) |
| Sync execution | [sync-execution.md](sync-execution.md) |
| Sync engine | [sync-engine.md](sync-engine.md) |
| Sync store | [sync-store.md](sync-store.md) |
| Data model (schema) | [data-model.md](data-model.md) |
| CLI commands | [cli.md](cli.md) |

## Planned Improvements

- Threat model document covering all attack surfaces and guards. [planned]
- Resource consumption guarantees documented per component. [planned]
- Lock ordering contract: every mutex documented in a hierarchy. `tracker.go` has nested `mu` → `cyclesMu`. [planned]
- Audit all `_ = ...` patterns in production code for silently ignored errors. [planned]
- Add value assertions after bare `assert.NoError` test calls (42+ instances could mask incorrect results). [planned]
- Audit deferred `Close` on write paths — errors universally ignored. [planned]
- Degraded-mode behavior guarantees documented (what happens when components fail). [planned]
- Evaluate `sync` → `graph` error coupling — decouple via interface if warranted. [planned]
- Evaluate `internal/sync/` package splitting — 8k+ lines in one package. [planned]
- Evaluate CLI structure scaling — 21 files / 4k+ lines in root package, consider domain grouping. [planned]
