# Sync Package Modularization

**Status**: Complete — review findings resolved
**Depends on**: Tracker & engine pipeline redesign (complete)
**Scope**: All files previously in `internal/sync/` — split into 7 focused packages

GOVERNS: internal/synctypes/*.go
**Related design docs**: `sync-observation.md`, `sync-planning.md`, `sync-execution.md`, `sync-engine.md`, `sync-store.md`, `data-model.md`

---

## 1. Problem Statement

`internal/sync` is a monolithic package containing 41 production files and 15,495 lines of code (plus 32,568 lines of tests). It is 4× larger than the next largest package (`internal/graph` at 3,578 lines). The package contains five architecturally distinct subsystems — observation, planning, execution/dispatch, persistence, and orchestration — that communicate through well-defined data contracts but share a single package namespace.

### 1.1 Why This Is a Problem

**Coupling surface.** Any type or function in the package can reference any other. The compiler cannot enforce the architectural boundaries documented in the five governing design docs. Engine methods appear in observation-governed files (`permissions.go`), store methods appear in observation-governed files (`shortcuts.go`), and store code calls functions defined in observation-governed files (`compute_status.go`). The design docs say where things *should* live; the package structure permits anything.

**Cognitive load.** A developer modifying the planner (pure reconciliation logic, zero I/O) must navigate a package containing filesystem watchers, SQLite transactions, Graph API calls, goroutine pools, and scope detection state machines. The planner has exactly one structural dependency (shared types) but shares a namespace with 40 other files.

**Test isolation.** All 86 files (production + test) compile together. A test helper defined for store tests is visible to observer tests. Test build times scale with the entire package, not the subsystem under test.

**Accidental dependencies.** Nothing prevents a future change from having the planner import store functions, the observer call executor methods, or the worker pool directly access the dependency graph. These would be architectural violations invisible to tooling.

### 1.2 Why Now (After Engine Pipeline Redesign)

The tracker redesign (Phases 1-5) replaced `DepTracker` with `DepGraph` + `ScopeGate`, established clean boundaries between dependency resolution and admission control, and integrated the failure retrier into the drain loop. The engine pipeline redesign (Phases 8-11) will extract `watchState`, unify the bootstrap path, and make the one-shot/watch distinction structural rather than scattered across nil guards.

After both redesigns complete:
- The Engine struct will be cleaner, with watch-mode state bundled
- The dispatch infrastructure (DepGraph, ScopeGate) will have stable APIs
- The drain loop structure will be finalized
- No further structural changes to Engine internals are planned

Splitting the package *during* those redesigns would create constant merge conflicts and force the redesign to work across package boundaries simultaneously. Splitting *after* means every subsystem has reached its intended API surface, and the extraction is mechanical.

### 1.3 What We Are Not Doing

This is a package reorganization, not a redesign. No behavior changes. No new features. No API surface changes to the Engine, Orchestrator, or SyncStore. The sync pipeline (observe → buffer → plan → dispatch → execute → commit) is unchanged. The only user-visible difference is import paths.

---

## 2. Analysis

### 2.1 Current Structure by Subsystem

The five design docs already partition the 41 files into coherent groups. The table below shows the *actual* code ownership (what the code does), which in three cases diverges from the design doc GOVERNS mapping.

| Subsystem | Design doc | Files | Prod lines | True owner |
|-----------|-----------|-------|------------|------------|
| **Observation** | sync-observation.md | observer_remote, observer_local, observer_local_handlers, observer_local_collisions, item_converter, scanner, buffer, inotify_linux, inotify_other | ~3,000 | Observation |
| **Observation (misattributed)** | sync-observation.md | permissions.go | 591 | **Engine** (8 Engine methods) |
| **Observation (misattributed)** | sync-observation.md | shortcuts.go | 88 | **Store** (4 SyncStore methods) |
| **Planning** | sync-planning.md | planner.go | 943 | Planning |
| **Shared types** | sync-planning.md | types.go | 732 | Shared (all subsystems) |
| **Execution/Dispatch** | sync-execution.md | executor, executor_conflict, executor_delete, executor_transfer, worker, dep_graph, scope_gate, scope, delete_counter | ~2,430 | Execution/Dispatch |
| **Execution (misattributed)** | sync-execution.md | compute_status.go | 96 | **Store** (called by store_observation) |
| **Execution (shared)** | sync-execution.md | issue_types.go | 34 | **Shared** (used by store, engine, scanner) |
| **Store** | sync-store.md | store, store_baseline, store_observation, store_conflicts, store_failures, store_admin, store_interfaces, store_scope_blocks, migrations, verify, trash | ~2,690 | Store |
| **Engine** | sync-engine.md | engine, engine_shortcuts, orchestrator, drive_runner | ~4,310 | Engine |
| **Shared** | — | errors.go, failure_messages.go | ~154 | Shared |

Three files are governed by the wrong design doc — `permissions.go`, `shortcuts.go`, and `compute_status.go`. These misattributions aren't documentation bugs; they reflect real cross-cutting concerns that a monolithic package permits but a modular structure must resolve.

### 2.2 Dependency Analysis (Types)

`types.go` (732 lines) defines the shared vocabulary used by every subsystem. A type-level dependency analysis:

| Type | Defined in | Used by |
|------|-----------|---------|
| ChangeEvent | types.go | observer, buffer, scanner, planner (via PathChanges), engine |
| BaselineEntry, Baseline | types.go | observer (path materialization), planner (three-way view), executor (parent ID), store (persistence), engine |
| PathChanges, PathView | types.go | buffer (output), planner (input), engine (pipeline) |
| RemoteState, LocalState | types.go | planner (input) |
| Action, ActionPlan | types.go | planner (output), dep_graph (TrackedAction), executor (input), engine (dispatch) |
| Outcome | types.go | executor (output), store (CommitOutcome), engine |
| SyncReport | types.go → engine.go | engine (output), store (WriteSyncMetadata) |
| ConflictRecord | types.go | planner (output), executor_conflict, store_conflicts |
| Shortcut | types.go | item_converter, engine_shortcuts, store (shortcuts.go) |
| ScopeKey, ScopeBlock | scope.go, scope_gate.go | scope_gate, store_failures, store_scope_blocks, store_admin, engine, worker, permissions |
| WorkerResult | worker.go | worker (output), engine (drain loop), scope (UpdateScope) |
| SyncFailureParams, SyncFailureRow | store_interfaces.go | store, engine, permissions |
| TrackedAction | dep_graph.go | dep_graph, worker, engine, scope_gate |
| All enums (ChangeSource, ActionType, ItemType, SyncMode, etc.) | types.go | everywhere |
| Consumer interfaces (DeltaFetcher, ItemClient, etc.) | types.go | observer, executor, engine |
| Store interfaces (ObservationWriter, OutcomeWriter, etc.) | store_interfaces.go | store (implements), engine (programs to) |
| IssueMessage, Issue* constants | failure_messages.go, issue_types.go | store, engine, scanner |
| PermissionChecker | permissions.go | engine |
| FsWatcher | observer_local.go | observer |

**Key insight:** ScopeKey and ScopeBlock are the most cross-cutting non-types.go definitions. They're defined in `scope.go` and `scope_gate.go` (execution/dispatch) but used by store (3 files), engine, worker, and permissions. Any split must promote these to shared types.

### 2.3 Method Boundary Problems

In Go, all methods on a struct must be in the same package. Three files have methods on structs that belong to a different subsystem:

**permissions.go** — 8 methods on `*Engine`:
- `handle403`, `handlePermissionCheckError`, `walkPermissionBoundary` — 403 response handling
- `recheckPermissions` — per-pass permission recheck
- `handleLocalPermission` — os.ErrPermission handling
- `recheckLocalPermissions` — per-pass local permission recheck
- `clearScannerResolvedPermissions` — scanner-driven permission clearing

These methods access Engine fields: `e.baseline` (store), `e.permCache`, `e.permChecker` (graph API), `e.scopeGate`, `e.syncRoot`, `e.driveID`, `e.logger`, `e.nowFunc`. They cannot be extracted to another package without refactoring.

**Resolution:** Extract a `PermissionHandler` struct with explicit dependencies. Engine creates one and delegates.

**shortcuts.go** — 4 methods on `*SyncStore`:
- `UpsertShortcut`, `GetShortcut`, `ListShortcuts`, `DeleteShortcut`

**Resolution:** Move to store package. These are SQL persistence methods.

**compute_status.go** — Package-level function `computeNewStatus()`:
- Called by `store_observation.go` in `processObservedItem()`
- Pure function implementing the remote_state state machine

**Resolution:** Move to store package. It's store domain logic (state machine for a store-owned table).

### 2.4 Cross-Cutting Constants

`issue_types.go` defines 13 issue type constants used by:
- Store (recording failures, classifying actionable issues)
- Engine (result classification, permission handling)
- Scanner (skipping items)
- CLI (display)

`failure_messages.go` maps issue types to user-facing messages, used by CLI display code.

Both must be in the shared types package.

---

## 3. Target Architecture

Seven packages. One for each architectural layer plus shared vocabulary.

```
internal/
  synctypes/       shared vocabulary: types, enums, interfaces, errors, constants
  syncstore/       persistence: SQLite state, migrations, verification, trash
  syncobserve/     change discovery: remote/local observers, scanner, buffer
  syncplan/        reconciliation: planner (pure function)
  syncdispatch/    dependency resolution + admission control: dep_graph, scope
  syncexec/        action execution: executor, worker pool
  sync/            orchestration: engine, orchestrator, permissions, shortcuts
```

### 3.1 Package Descriptions

#### `synctypes` — Shared Vocabulary (~1,200 lines)

Zero-logic package defining the data contracts between all other packages. No dependencies beyond `internal/driveid` and standard library. Never imports any sibling sync package.

**Contents:**
- All enums: ChangeSource, ChangeType, ItemType, SyncMode, ActionType, FolderCreateSide
- Observation types: ChangeEvent, SkippedItem, ScanResult
- State types: BaselineEntry, Baseline (with thread-safe accessors), RemoteState, LocalState, dirLowerKey
- Planning types: PathChanges, PathView, ConflictRecord
- Execution types: Action, ActionPlan, Outcome, TrackedAction
- Report types: SyncReport, RunOpts, SafetyConfig, VerifyResult, VerifyReport
- Shortcut type: Shortcut (with observation strategy constants)
- Scope types: ScopeKey, ScopeKeyKind, ScopeBlock, ScopeUpdateResult, all SK* constructors, ParseScopeKey
- Worker types: WorkerResult
- Store contract types: SyncFailureParams, SyncFailureRow, ActionableFailure, ObservedItem, RemoteStateRow, PendingRetryGroup, IssueMessage
- Store interfaces: ObservationWriter, OutcomeWriter, StateReader, StateAdmin, SyncFailureRecorder, ScopeBlockStore
- Consumer interfaces: DeltaFetcher, ItemClient, DriveVerifier, FolderDeltaFetcher, RecursiveLister, PermissionChecker, FsWatcher
- Issue constants: all Issue* constants, MessageForIssueType
- Sentinel errors: all Err* values
- Engine config types: EngineConfig, OrchestratorConfig (struct definitions only)

**Why this size is acceptable:** ~1,200 lines of pure type definitions, enums, and interfaces. No logic, no I/O, no goroutines. This is the standard Go pattern for breaking shared vocabulary out of a monolithic package. Compare: `net/http` defines types used by both client and server. The package is a leaf — it imports nothing from sibling packages and can only grow by adding new shared types, never new logic.

#### `syncstore` — Persistence Layer (~2,800 lines)

SQLite state management. Only external dependency is `driveid` (via `synctypes`). No knowledge of observers, planners, executors, or the engine. Implements the store interfaces defined in `synctypes`.

**Contents:**
- `store.go` — SyncStore struct, NewSyncStore, Close, Checkpoint, DataVersion
- `store_baseline.go` — Load, CommitOutcome, delta tokens
- `store_observation.go` — CommitObservation, computeNewStatus (absorbed from compute_status.go)
- `store_conflicts.go` — conflict CRUD
- `store_failures.go` — failure recording, retry queries, trial candidates
- `store_admin.go` — reset, metadata, state queries
- `store_scope_blocks.go` — scope block persistence
- `shortcuts.go` — shortcut CRUD (moved from observation)
- `migrations.go` — goose runner + embedded SQL
- `verify.go` — VerifyBaseline (imports driveops for hash computation)
- `trash.go` — OS trash integration

**Compile-time interface checks:** `var _ synctypes.ObservationWriter = (*SyncStore)(nil)` etc.

#### `syncobserve` — Change Discovery (~3,000 lines)

Produces `[]ChangeEvent` from Graph API delta responses and local filesystem walks. Stateless across passes (counters are informational). No knowledge of planning, execution, or the engine.

**Contents:**
- `observer_remote.go` — RemoteObserver (FullDelta, Watch)
- `observer_local.go` — LocalObserver (FullScan, Watch)
- `observer_local_handlers.go` — fsnotify event routing, write coalescing
- `observer_local_collisions.go` — case collision cache
- `item_converter.go` — graph.Item → ChangeEvent two-pass pipeline
- `scanner.go` — filesystem walk + parallel hash + filename validation
- `buffer.go` — event grouping + debounce → []PathChanges
- `inotify_linux.go`, `inotify_other.go` — platform watch limit detection

**External dependencies:** `internal/graph` (delta API types), `internal/driveops` (hash computation), `fsnotify` (filesystem events), `golang.org/x/text/unicode/norm` (NFC normalization).

#### `syncplan` — Reconciliation Logic (~950 lines)

Pure function. Takes `[]PathChanges` + `*Baseline` + `SyncMode` + `SafetyConfig` + denied prefixes, returns `*ActionPlan` or error. Zero I/O, zero goroutines, zero side effects. Deterministic given the same inputs.

**Contents:**
- `planner.go` — Planner struct, Plan(), classifyPathView, detectMoves, buildPathView, all decision matrix logic

**External dependencies:** `internal/driveid` (via synctypes). Nothing else.

#### `syncdispatch` — Dependency Resolution + Admission Control (~1,100 lines)

Pure data structures and stateful admission control. No knowledge of OneDrive, Graph API, or filesystem. Reusable infrastructure that could serve any pipeline with dependency ordering and scope-based throttling.

**Contents:**
- `dep_graph.go` — DepGraph (pure DAG, sequential IDs, Complete() returns dependents)
- `scope.go` — ScopeState (sliding window detection, UpdateScope, RecordSuccess)
- `scope_gate.go` — ScopeGate (admission control, trial management, persists via ScopeBlockStore interface)
- `delete_counter.go` — rolling-window big-delete protection for watch mode

**External dependencies:** None beyond synctypes and standard library. The ScopeGate persists via the `ScopeBlockStore` interface (defined in synctypes, implemented by syncstore). It never imports syncstore directly.

#### `syncexec` — Action Execution (~1,000 lines)

Translates `Action` → `Outcome` via Graph API calls and filesystem I/O. Thin wrappers with no retry logic, no error classification, no scope awareness. Workers are flat goroutines that read from a channel and write results to a channel.

**Contents:**
- `executor.go` — Executor, ExecutorConfig, dispatch by ActionType
- `executor_conflict.go` — conflict resolution (download conflict copy, rename)
- `executor_delete.go` — local delete (hash verify + trash) and remote delete (recycle bin)
- `executor_transfer.go` — download (.partial + verify + rename) and upload (via TransferManager)
- `worker.go` — WorkerPool (goroutine pool, reads TrackedAction from channel, returns WorkerResult)

**External dependencies:** `internal/graph` (CRUD operations), `internal/driveops` (TransferManager, hash computation).

#### `sync` — Orchestration (~4,400 lines)

The glue. Creates all subsystems, wires them together, owns the drain loop, classifies results, manages scope transitions, coordinates multi-drive sync. The only package that imports all siblings.

**Contents:**
- `engine.go` — Engine struct, NewEngine, RunOnce, RunWatch, drain loop, result classification, retry sweep, trial dispatch
- `engine_shortcuts.go` — shortcut lifecycle orchestration (concurrent observation, collision detection, scope block management)
- `permissions.go` — PermissionHandler struct (refactored), handle403, recheckPermissions, handleLocalPermission, recheckLocalPermissions, clearScannerResolvedPermissions
- `orchestrator.go` — Orchestrator, multi-drive coordination (RunOnce, RunWatch)
- `drive_runner.go` — DriveRunner, per-drive panic recovery

**After engine pipeline redesign completes**, this package will also contain:
- watchState struct (Phase 8)
- bootstrapSync (Phase 9)
- Async reconciliation goroutine (Phase 10)

### 3.2 Dependency Graph

```
                    synctypes
                   ↗  ↑  ↑  ↑  ↖
                  /   |  |  |   \
          syncstore   |  |  |   syncobserve
                      |  |  |
               syncplan  |  syncdispatch
                         |
                      syncexec

                      sync
                  (imports all)
```

Rules:
1. `synctypes` is a leaf — imports only `internal/driveid` and stdlib
2. Every other package imports `synctypes` and external dependencies
3. No package imports a sibling (except `sync` which imports all). Note: syncexec no longer imports syncstore — it uses `synctypes.OutcomeWriter` interface instead (fixed in review).
4. `sync` is the root — nothing imports it except the CLI layer

### 3.3 Interface Boundaries

Each non-engine package exposes its functionality through concrete types that implement interfaces defined in `synctypes`. The engine creates concrete instances and programs to interfaces where appropriate.

| Package | Exposes | Engine uses via |
|---------|---------|----------------|
| syncstore | `*SyncStore` | `synctypes.ObservationWriter`, `synctypes.OutcomeWriter`, `synctypes.StateReader`, `synctypes.StateAdmin`, `synctypes.SyncFailureRecorder`, `synctypes.ScopeBlockStore` |
| syncobserve | `*RemoteObserver`, `*LocalObserver`, `*Buffer` | Direct struct usage (created per-pass) |
| syncplan | `*Planner` | Direct struct usage (pure function) |
| syncdispatch | `*DepGraph`, `*ScopeGate`, `*ScopeState` | Direct struct usage (created per-pass or per-engine) |
| syncexec | `*Executor`, `*WorkerPool` | Direct struct usage (created per-pass) |

---

## 4. Refactoring Prerequisites

Three preparatory refactorings must happen as part of the split. Each is mechanical — no behavior changes.

### 4.1 Extract PermissionHandler from Engine — COMPLETE

**Status:** Completed in review fix Step 5.

`PermissionHandler` struct extracted to `internal/sync/permission_handler.go` with explicit dependencies (baseline, permChecker, permCache, logger, syncRoot, driveID, nowFn) plus callbacks for scope management (setScopeBlockFn, onScopeClearFn, isWatchModeFn). Engine creates one in `NewEngine` and delegates via `e.permHandler`. All 7 permission methods are now methods on `*PermissionHandler` instead of `*Engine`.

### 4.2 Promote Cross-Cutting Types to synctypes

Move from their current locations to `synctypes`:
- `ScopeKey`, `ScopeKeyKind`, all SK* constructors, `ParseScopeKey` (from `scope.go`)
- `ScopeBlock` (from `scope_gate.go`)
- `ScopeUpdateResult` (from `scope.go`)
- `WorkerResult` (from `worker.go`)
- `TrackedAction` (from `dep_graph.go`)
- Issue constants and `MessageForIssueType` (from `issue_types.go`, `failure_messages.go`)
- Store sub-interfaces and DTOs (from `store_interfaces.go`)
- `PermissionChecker` interface (from `permissions.go`)
- Consumer interfaces: DeltaFetcher, ItemClient, etc. (from `types.go`)

### 4.3 Absorb compute_status.go into store_observation.go — COMPLETE

**Status:** Completed in review fix Step 3.

`computeNewStatus()` and its helper functions (`computeDeleted`, `computeSameHash`, `computeDifferentHash`) plus status constants absorbed into `store_observation.go`. Functions unexported as specified. Both `compute_status.go` and `compute_status_test.go` deleted; tests moved to `store_observation_test.go`.

---

## 5. Execution Plan

### 5.1 Prerequisites (Complete)

1. **Tracker redesign Phases 1-5** — Done (DepGraph + ScopeGate replace DepTracker)
2. **Engine pipeline redesign Phases 8-11** — Done (watchState extraction, unified bootstrap, async reconciliation, safety config unification)

### 5.2 Execution Sequence

All in one PR. The philosophy is "prefer large, long-term solutions" and "ensure after refactoring the code doesn't show any signs of the old architecture." An incremental approach leaves the codebase in a half-split state.

**Step 1: Create package directories and synctypes**
- Create `internal/synctypes/`, `internal/syncstore/`, `internal/syncobserve/`, `internal/syncplan/`, `internal/syncdispatch/`, `internal/syncexec/`
- Move types, enums, interfaces, constants, errors to synctypes (§4.2)
- Split into logical files within synctypes (types.go, interfaces.go, scope_key.go, enums.go, errors.go, etc.)
- All existing files: update `package` declaration to `sync`, add synctypes imports

**Step 2: Extract syncstore**
- Move store_*.go, migrations.go, verify.go, trash.go, shortcuts.go to `internal/syncstore/`
- Absorb compute_status.go into store_observation.go (§4.3)
- Update package declarations, imports
- Add compile-time interface satisfaction checks

**Step 3: Extract syncobserve**
- Move observer_*.go, item_converter.go, scanner.go, buffer.go, inotify_*.go to `internal/syncobserve/`
- Update package declarations, imports

**Step 4: Extract syncplan**
- Move planner.go to `internal/syncplan/`
- Update package declarations, imports

**Step 5: Extract syncdispatch**
- Move dep_graph.go, scope.go, scope_gate.go, delete_counter.go to `internal/syncdispatch/`
- Update package declarations, imports

**Step 6: Extract syncexec**
- Move executor_*.go, worker.go to `internal/syncexec/`
- Update package declarations, imports

**Step 7: Refactor permissions.go (§4.1)**
- Extract PermissionHandler struct
- Update engine.go to create and delegate to PermissionHandler

**Step 8: Fix remaining compilation**
- Resolve all remaining import cycles and compilation errors
- This is expected to surface any hidden coupling not identified in the analysis

**Step 9: Migrate tests**
- Move test files to their corresponding packages
- Extract shared test helpers to `internal/synctypes/testutil_test.go` or a shared test helper package
- Verify all tests pass with `-race`

**Step 10: Update documentation**
- Update GOVERNS lines in all 5 sync design docs + data-model.md
- Update CLAUDE.md routing table
- Update this document's status to "Complete"

### 5.3 Verification

After the split:
- `gofumpt -w . && goimports -local github.com/tonimelisma/onedrive-go -w .`
- `golangci-lint run`
- `go build ./...`
- `go test -race ./...`
- `go test -tags=e2e -race -v -parallel 5 -timeout=10m ./e2e/...`
- Verify no import cycles: `go vet ./...`
- Verify GOVERNS coverage: every production .go file appears in exactly one design doc

---

## 6. Risk Assessment

### 6.1 Low Risk
- **syncplan extraction** — Planner is a pure function with zero cross-cutting concerns. One file, one struct, one method.
- **syncstore extraction** — Already interface-segregated. Only cross-cutting issue is ScopeKey (resolved by promoting to synctypes).
- **synctypes creation** — Mechanical move of type definitions. No logic changes.

### 6.2 Medium Risk
- **syncobserve extraction** — 9 files with moderate coupling. `item_converter.go` uses Baseline heavily (read-only). Buffer produces PathChanges consumed by planner. No method boundary issues.
- **syncdispatch extraction** — ScopeGate persists via interface (clean), but ScopeState.UpdateScope takes WorkerResult (cross-cutting type, resolved by synctypes).
- **syncexec extraction** — Worker pool reads TrackedAction (synctypes) and calls Executor. Clean boundary once types are shared.

### 6.3 Higher Risk
- **permissions.go refactoring** — Converting 8 Engine methods to PermissionHandler methods requires careful dependency threading. Each method currently accesses Engine fields freely; the refactored version must pass all dependencies explicitly. Risk is omitting a dependency and introducing a nil dereference.
- **Test migration** — 32,568 lines of tests across 45 files. Some tests may have implicit dependencies on same-package access to unexported functions. These will surface as compilation errors and must be resolved by either exporting the function or restructuring the test.
- **Hidden coupling** — The analysis identifies all *known* cross-cutting concerns. There may be unexported helper functions used across subsystem boundaries that only surface during compilation. The mitigation is that step 8 (fix remaining compilation) is explicitly budgeted.

---

## 7. Post-Split State

### 7.1 Package Sizes (Approximate)

| Package | Prod files | Prod lines | Test files | Purpose |
|---------|-----------|------------|------------|---------|
| synctypes | ~8 | ~1,200 | ~2 | Shared vocabulary |
| syncstore | 11 | ~2,800 | ~13 | Persistence |
| syncobserve | 9 | ~3,000 | ~10 | Change discovery |
| syncplan | 1 | ~950 | ~2 | Reconciliation |
| syncdispatch | 4 | ~1,100 | ~5 | Dep resolution + admission |
| syncexec | 5 | ~1,000 | ~5 | Action execution |
| sync | 5 | ~4,400 | ~8 | Orchestration |

### 7.2 Design Doc Updates Required

| Design doc | Current GOVERNS | New GOVERNS |
|-----------|----------------|-------------|
| sync-observation.md | 11 files in internal/sync/ | 9 files in internal/syncobserve/ |
| sync-planning.md | 2 files in internal/sync/ | 1 file in internal/syncplan/ (types.go moves to synctypes) |
| sync-execution.md | 10 files in internal/sync/ | 4 files in internal/syncdispatch/ + 5 files in internal/syncexec/ |
| sync-engine.md | 6 files in internal/sync/ | 5 files in internal/sync/ |
| sync-store.md | 11 files in internal/sync/ | 11 files in internal/syncstore/ |
| (new) this document | — | ~8 files in internal/synctypes/ |

### 7.3 CLAUDE.md Routing Table Updates

The routing table will expand to reference the new package paths. Each row's "Read first" and "Also consult" columns remain the same — the design docs don't change, only the file paths in their GOVERNS lines.

---

## 8. Design Decisions and Alternatives Considered

### 8.1 Why 7 Packages, Not Fewer

**"Just extract the store" (2 packages)** — Gets the biggest single win but leaves 12,700 lines in `sync/`. Settles for "good enough for now." The observation, planning, dispatch, and execution subsystems remain coupled.

**"Pipeline + Infrastructure" (3 packages: synctypes, syncstore, sync)** — Fewer boundaries to maintain, but `sync/` at ~12,000 lines is still a monolith. The compiler still can't enforce the observation/planning/execution boundaries. This is the "pragmatic" option that the engineering philosophy explicitly rejects.

**"Core vs. Shell" (4 packages by I/O boundary)** — Separates pure logic from I/O code. But observers and executors have nothing in common besides doing I/O. Grouping them creates an incoherent package.

### 8.2 Why 7 Packages, Not More

**Splitting syncexec further (executor vs. workers)** — Workers and executors are tightly coupled: workers call executors. Splitting them creates a package with 1 file. No value.

**Splitting syncobserve (remote vs. local)** — Remote and local observers share the same output type (ChangeEvent), the same buffer, and the item_converter. Two 4-file packages with shared dependencies is worse than one 9-file package.

### 8.3 Why synctypes Instead of Interface-Per-Consumer

Go's "accept interfaces, return structs" idiom suggests each consumer define its own interface. We could have `syncobserve` define `DeltaFetcher` and `syncexec` define `ItemClient` separately. However:

- 15+ types are used by 3+ packages. Defining them in each consumer creates duplication and divergence risk.
- `Baseline` is the central data structure — it can't be defined in three places.
- `ScopeKey` is used by store, dispatch, engine, and workers — no single consumer owns it.
- A shared types package is the standard Go solution for this pattern.

Consumer-specific interfaces (e.g., `FsWatcher` used only by `syncobserve`) could stay in their consuming package. The guideline: if a type is used by exactly one non-engine package, it can live there. If used by 2+, it goes to synctypes.

### 8.4 Why All-at-Once Instead of Incremental

The engineering philosophy says "prefer large, long-term solutions over quick fixes" and "ensure after refactoring the code doesn't show any signs of the old architecture." An incremental approach:
- Leaves the codebase in a half-split state between steps
- Each intermediate state must compile and test
- Developers must understand which packages have been split and which haven't
- Creates N PRs instead of 1, each requiring review
- Risks stalling midway, leaving permanent architectural inconsistency

The all-at-once approach is a larger single PR but produces a clean final state with no transitional artifacts.
