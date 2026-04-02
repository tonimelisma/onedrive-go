# System Architecture

Implements: R-6.2.1 [verified], R-6.2.2 [verified], R-6.3.1 [verified]

## Package Structure

Implements: R-6.1.4 [verified], R-6.1.5 [verified], R-6.9.1 [verified]

```
.                             Root package (Cobra CLI commands)
internal/
  config/                     TOML config, drive sections, XDG paths, override chain
  driveid/                    Type-safe drive identity: ID, CanonicalID, ItemKey (leaf, stdlib-only)
  driveops/                   Authenticated drive access: sessions, transfers, hashing
  fsroot/                     Root-bound managed-state file capabilities
  graph/                      Graph API client: auth, retry, items CRUD, delta, transfers
  localpath/                  Explicit arbitrary-local-path boundary helpers
  logfile/                    Log file creation, rotation, retention
  multisync/                  Multi-drive sync control plane and watch reload
  retry/                      Retry policies, exponential backoff with jitter (leaf, stdlib-only)
  sync/                       Single-drive sync engine (see pipeline below)
  synctree/                   Root-bound sync runtime filesystem capability
  tokenfile/                  Pure OAuth token file I/O (leaf, stdlib + oauth2 only)
pkg/
  quickxorhash/               QuickXorHash algorithm (vendored from rclone, BSD-0)
e2e/                          E2E test suite (not production code)
testutil/                     Shared test helpers (not production code)
```

## Dependency Rules

```
root pkg (CLI) → internal/driveops/ → internal/graph/ → pkg/*
                 internal/multisync/ → internal/sync/ → internal/driveops/
                 internal/config/  → internal/driveid/
```

- No cycles. `driveops` does NOT import `sync`. `graph` does NOT import `config`.
- `multisync` owns multi-drive lifecycle; `sync` owns the single-drive runtime.
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
| Sync control plane | [sync-control-plane.md](sync-control-plane.md) |
| Sync store | [sync-store.md](sync-store.md) |
| Data model (schema) | [data-model.md](data-model.md) |
| CLI commands | [cli.md](cli.md) |

## Verification Policy

Static verification is a first-class architectural constraint, not a best-effort hygiene step.

- `golangci-lint` runs with `default: none`; every enabled linter is an explicit policy choice.
- `nolintlint` requires both a specific linter name and a short justification. Unused exclusions are surfaced with `linters.exclusions.warn-unused`.
- Inline suppressions are reserved for the small documented exception set where the code is correct and the linter cannot express that shape cleanly: interface-mandated receiver shapes, validated subprocess/request dispatch that the linter cannot follow across helper boundaries, non-cryptographic jitter, `driver.Valuer` SQL `NULL` semantics, fixed placeholder SQL, and intentional test fixtures/mocks.
- `scripts/verify.sh` is the single repo-owned verification entry point. It exposes explicit profiles: `default` (default local run: lint, build, race+coverage, coverage gate, stale-doc checks, fast E2E), `public`, `e2e`, `e2e-full`, and `integration`.
- Fast E2E is mandatory in the default local `default` profile. The harness loads `.env` and `.testdata` itself; verification does not silently skip fast E2E based on exported shell variables.
- The nightly/manual full E2E suite is layered on top of the fast suite. Its files use `//go:build e2e && e2e_full`, so the canonical invocation is the verifier's `e2e-full` profile, which sets both tags and preserves the fast-then-full ordering.
- Managed repo-state files use `internal/fsroot` root capabilities.
- Sync-runtime filesystem operations under one configured sync root use `internal/synctree`.
- Arbitrary local file paths outside those rooted domains use `internal/localpath` as the explicit trust boundary.

## Planned Improvements

- Threat model document covering all attack surfaces and guards. [planned]
- Resource consumption guarantees documented per component. [planned]
- Lock ordering contract: every mutex documented in a hierarchy. `tracker.go` has nested `mu` → `cyclesMu`. [planned]
- Audit all `_ = ...` patterns in production code for silently ignored errors. [planned]
- Add value assertions after bare `assert.NoError` test calls (42+ instances could mask incorrect results). [planned]
- Audit deferred `Close` on write paths — errors universally ignored. [planned]
- Degraded-mode behavior guarantees documented (what happens when components fail). [planned]
- Evaluate `sync` → `graph` error coupling — decouple via interface if warranted. [planned]
- Evaluate deeper `internal/sync/` runtime package splitting after the control-plane split. [planned]
- Evaluate CLI structure scaling — 21 files / 4k+ lines in root package, consider domain grouping. [planned]
