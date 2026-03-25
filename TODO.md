# TODO — Post-Split Architecture Improvements

Improvements identified during the sync-package-split review (2026-03-20). All are refinements — the split is structurally complete and all prior review findings are resolved.

---

## 1. synctypes Contains Non-Trivial Business Logic

**Severity:** Medium
**Spec claim:** synctypes is a "zero-logic package defining data contracts."
**Reality:** ~400 lines of real logic across two areas.

**Scope logic** in `scope_key.go` (212 lines):
- `BlocksAction()` — scope-blocking decision matrix (6 scope kinds, each with different blocking semantics)
- `ScopeKeyForStatus()` — HTTP status → scope key classification (429→throttle, 503→service, 507→quota)
- `Humanize()` — user-facing descriptions with shortcut lookup
- `IssueType()` — scope kind → issue constant mapping

**Baseline logic** in `types.go`:
- `Put()` — thread-safe insert with stale-entry cleanup and triple-index maintenance
- `Delete()` — thread-safe removal from 3 maps
- `DescendantsOf()` — O(n) prefix scan (called by syncplan)
- `FindOrphans()` — synthesizes `ChangeEvent` values (called by sync/engine and syncobserve)

**Callers:**
| Method | syncdispatch | sync/engine | syncobserve | syncplan | CLI |
|--------|-------------|-------------|-------------|----------|-----|
| `BlocksAction` | 2 | 2 | — | — | — |
| `ScopeKeyForStatus` | 2 | 2 | — | — | — |
| `Humanize` | — | — | — | — | 2 |
| `FindOrphans` | — | 1 | 1 | — | — |
| `DescendantsOf` | — | — | — | 1 | — |

### Options

**A. Update the spec** — Change "zero-logic" to "types + data structure accessors + type-associated behavior." Zero code change, zero risk. Doesn't prevent future logic accumulation.

**B. Move scope logic to syncdispatch** — Move `BlocksAction`, `ScopeKeyForStatus`, `IssueType`, `Humanize` to `syncdispatch/scope_key_logic.go`. Keep `ScopeKey` struct + constructors + serialization in synctypes. All callers already import syncdispatch or would naturally. _Pro:_ scope-blocking logic lives with scope infrastructure. _Con:_ `BlocksAction` becomes a free function instead of a method.

**C. Move FindOrphans/DescendantsOf to domain packages** — `FindOrphans` → `syncobserve` (orphan detection is observation), `DescendantsOf` → `syncplan` (cascade expansion is planning). Both use `Baseline.ForEachPath()` which already exists. _Pro:_ Baseline becomes pure thread-safe map. _Con:_ minor indirection.

**D. Combined B + C (recommended)** — Apply both. After this, synctypes contains only: type definitions, enums, interfaces, `ScopeKey` struct + serialization + predicate queries, `Baseline` CRUD. The Put/Delete/Get methods are legitimate type-associated logic.

### Recommendation: Option D

---

## 2. Stale GOVERNS Reference

**Severity:** Low
**File:** `spec/design/sync-planning.md` line 3

```
GOVERNS: internal/syncplan/planner.go, internal/synctypes/*.go, internal/sync/types.go
```

`internal/sync/types.go` was deleted as part of H-3 (types.go re-export shim removal). This GOVERNS reference is now dangling.

### Options

**A. Delete the stale entry (recommended)** — Remove `, internal/sync/types.go` from the GOVERNS line. Trivial fix.

**B. Audit all GOVERNS lines** — Systematically verify every GOVERNS line in every design doc matches files on disk. Could be scripted: extract GOVERNS paths, check `stat` each one. More thorough but the only known stale entry is this one.

### Recommendation: Option A, with a scripted audit (Option B) as a one-time follow-up

---

## 4. synctypes Imports `graph` and `driveops`

**Severity:** Low-Medium
**Spec claim:** synctypes imports "only `internal/driveid` and stdlib."
**Reality:** Also imports `graph` (via `consumer_interfaces.go`) and `driveops` (via `config.go`).

**Import sources:**
- `graph` — 6 consumer interfaces reference `graph.DeltaPage`, `graph.Item`, `graph.Drive`, `graph.Permission`
- `driveops` — `EngineConfig.Downloads` is `driveops.Downloader`, `.Uploads` is `driveops.Uploader`

### Options

**A. Accept and document** — Update spec. Zero risk. _Con:_ `driveops` change → synctypes recompile → all 7 packages recompile.

**B. Move consumer interfaces to consuming packages** — Each package defines its own narrow interface. _Problem:_ `EngineConfig` references these interfaces as fields, so it would need to move too → cascading change. Some interfaces used by 2+ packages → duplication.

**C. Move EngineConfig to `internal/sync/config.go` (recommended)** — Removes the `driveops` import. `EngineConfig` is engine configuration — it belongs in the engine package. Consumer interfaces stay in synctypes (the `graph` import is defensible: `graph.Item` is as fundamental as `driveid.ID`). CLI already imports `internal/sync/` so no new dependency.

**D. Intermediate types** — Define synctypes-native equivalents for `graph.Item` etc. _Con:_ massive duplication, maintenance burden, violates "preserve what you don't understand."

### Recommendation: Option C

---

## 5. `NewBaselineForTest` Exported in Production Code

**Severity:** Low
**Rule:** CLAUDE.md: "Unexported by default. Export only what other packages need."
**Location:** `synctypes/types.go:301`

**Callers** (all test files): `synctest/helpers.go` (2), `syncplan/*_test.go` (9), `sync/engine_test.go` (4), `syncobserve/*_test.go` (4), `synctypes/types_test.go` (3).

### Options

**A. Move to `synctest/helpers.go` (recommended)** — Rename to `NewBaseline`. For `synctypes/types_test.go` (3 calls), use inline construction or a local unexported helper since it can't import synctest (circular). _Pro:_ test utility removed from production API. _Con:_ 20+ callers to update (mechanical).

**B. Keep exported, add comment** — Zero change. _Con:_ violates "unexported by default."

**C. Use `Baseline.Put()` everywhere** — Remove helper entirely. _Con:_ boilerplate in every test file; 3 map initializations are implementation details tests shouldn't know.

### Recommendation: Option A

---

## 6. `worker_test.go` Imports `syncstore`

**Severity:** Low
**Location:** `syncexec/worker_test.go`
**Issue:** Production `worker.go` correctly accepts `synctypes.OutcomeWriter` interface. But tests bypass the interface and use `*syncstore.SyncStore` directly.

12 test functions pass a real SyncStore. `newWorkerTestSetup` returns `*syncstore.SyncStore`. If SQLite schema changes, worker tests break — even though workers don't logically depend on storage.

### Options

**A. Mock `OutcomeWriter` (recommended)** — Create a test-local `mockOutcomeWriter` with an in-memory `*Baseline`. Remove the `syncstore` import. _Pro:_ true unit tests, faster (no SQLite), enforces interface boundary. _Con:_ loses worker→store integration coverage, but that's already in `sync/engine_test.go`.

**B. Move integration tests to `sync/`** — Keep mock-based unit tests in `syncexec/`, move store-dependent tests to `sync/worker_integration_test.go`. _Pro:_ right test type in right package. _Con:_ `ExecutorConfig` is internal to syncexec, harder to construct from outside.

**C. Accept as integration tests** — Document and leave. _Con:_ doesn't fix the dependency leak.

### Recommendation: Option A

---

## Execution Order

1. **#5** — `NewBaselineForTest` → synctest (smallest, no dependencies)
2. **#2** — Stale GOVERNS fix (trivial)
3. **#4** — Move `EngineConfig` to `internal/sync/` (enables #1)
4. **#1** — Move scope logic + FindOrphans/DescendantsOf (core architectural)
5. **#6** — Mock `OutcomeWriter` in worker tests (independent)
