# System Architecture

Implements: R-6.2.1 [verified], R-6.2.2 [verified], R-6.3.1 [verified], R-6.10.5 [verified], R-6.10.7 [verified]

## Package Structure

Implements: R-6.1.4 [verified], R-6.1.5 [verified], R-6.9.1 [verified]

```
.                             Root package (thin process entrypoint)
cmd/
  devtool/                    Repo-owned verification and worktree bootstrap tooling
internal/
  cli/                        Cobra command tree, CLI bootstrap, output formatting, command handlers
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
root pkg → internal/cli/ → internal/driveops/ → internal/graph/ → pkg/*
                         → internal/multisync/ → internal/sync/ → internal/driveops/
                         → internal/config/  → internal/driveid/
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
6. **Boundary-owned error translation.** Each boundary normalizes the failures it understands once, and downstream layers consume that shared contract. See [error-model.md](error-model.md).

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
| Error model | [error-model.md](error-model.md) |
| Threat model | [threat-model.md](threat-model.md) |
| Degraded mode | [degraded-mode.md](degraded-mode.md) |
| CLI commands | [cli.md](cli.md) |

## Verification Policy

Static verification is a first-class architectural constraint, not a best-effort hygiene step.

- `golangci-lint` runs with `default: none`; every enabled linter is an explicit policy choice.
- `depguard` enforces architectural import boundaries, and `forbidigo` bans raw filesystem `os.*` calls outside the explicit boundary packages (`fsroot`, `synctree`, `localpath`).
- `nolintlint` requires both a specific linter name and a short justification. Unused exclusions are surfaced with `linters.exclusions.warn-unused`.
- Inline suppressions are reserved for the small documented exception set where the code is correct and the linter cannot express that shape cleanly: interface-mandated receiver shapes, validated subprocess/request dispatch that the linter cannot follow across helper boundaries, non-cryptographic jitter, `driver.Valuer` SQL `NULL` semantics, fixed placeholder SQL, and intentional test fixtures/mocks.
- `cmd/devtool` is the single repo-owned verification and worktree-bootstrap entry point. `go run ./cmd/devtool verify` exposes explicit profiles: `default` (default local run: format, lint, build, race+coverage, coverage gate, repo-consistency checks, fast E2E), `public`, `e2e`, `e2e-full`, and `integration`.
- Repo-consistency checks in `cmd/devtool verify` enforce the repo-level architecture constraints linters do not express cleanly: governed design docs must carry ownership contracts, required cross-cutting docs must exist and be linked from this document, and `internal/graph/client_preauth.go` remains the only raw production `http.Client.Do` boundary.
- `go run ./cmd/devtool worktree add --path <path> --branch <branch>` is the canonical way to create new worktrees from `origin/main`. It applies `.worktreeinclude` immediately so the new worktree is ready for fast E2E and local development.
- Fast E2E is mandatory in the default local `default` profile. The harness loads `.env` and `.testdata` itself; verification does not silently skip fast E2E based on exported shell variables.
- The nightly/manual full E2E suite is layered on top of the fast suite. Its files use `//go:build e2e && e2e_full`, so the canonical invocation is the verifier's `e2e-full` profile, which sets both tags and preserves the fast-then-full ordering.
- Managed repo-state files use `internal/fsroot` root capabilities.
- Sync-runtime filesystem operations under one configured sync root use `internal/synctree`.
- Arbitrary local file paths outside those rooted domains use `internal/localpath` as the explicit trust boundary.
- `fsroot.Root` and `synctree.Root` carry unexported injectable ops so managed-state and sync-runtime I/O failure paths can be tested deterministically without package-level test hooks.

## Planned Improvements

- Resource consumption guarantees documented per component. [planned]
- Lock ordering contract: every mutex documented in a hierarchy. `tracker.go` has nested `mu` → `cyclesMu`. [planned]
- Audit all `_ = ...` patterns in production code for silently ignored errors. [planned]
- Add value assertions after bare `assert.NoError` test calls (42+ instances could mask incorrect results). [planned]
- Audit deferred `Close` on write paths — errors universally ignored. [planned]
- Evaluate `sync` → `graph` error coupling — decouple via interface if warranted. [planned]
- Evaluate deeper `internal/sync/` runtime package splitting after the control-plane split. [planned]
- Continue expanding direct handler/service coverage over the new `internal/cli/` service split so the package reaches its 60%+ coverage target. [planned]
