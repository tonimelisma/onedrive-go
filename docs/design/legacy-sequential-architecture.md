# Legacy Sequential Architecture Reference

**Status**: Migration reference document (not a spec)
**Describes**: Phase 4v2 sequential execution model
**Replaced by**: [concurrent-execution.md](concurrent-execution.md) (Phase 5)

---

## 1. Purpose and Scope

This document describes the Phase 4v2 sequential execution architecture that
Phase 5 replaces with a DAG-based concurrent execution model. It exists for
three purposes:

1. **During migration**: Agents understand what they are replacing and why.
   Each section maps old patterns to their replacements in
   [concurrent-execution.md](concurrent-execution.md).
2. **After migration**: The pattern detection reference (§9) provides grep
   commands to verify the clean-slate invariant — zero vestiges of the old
   architecture remain in non-test production code.
3. **Historical**: Explains *why* the old architecture worked the way it did,
   and *why* it is insufficient for watch mode.

This document does NOT describe the Phase 4 v1 batch-pipeline architecture
(deleted in increment 4v2.0). It describes only the Phase 4v2 event-driven
architecture's execution model — the code that exists today in
`internal/sync/`.

---

## 2. The 9-Phase Execution Model

The `Execute()` method in `executor.go` dispatches actions in 9 sequential
phases. Each phase must complete before the next begins. Context cancellation
is checked between phases.

```
Phase 1: Folder creates    (sequential — parent before child)
Phase 2: Moves             (sequential — order matters for renames)
Phase 3: Downloads         (parallel — 8-worker errgroup pool)
Phase 4: Uploads           (parallel — 8-worker errgroup pool)
Phase 5: Local deletes     (sequential — depth-first from planner)
Phase 6: Remote deletes    (sequential)
Phase 7: Conflicts         (sequential)
Phase 8: Synced updates    (no I/O — baseline bookkeeping)
Phase 9: Cleanups          (no I/O — baseline bookkeeping)
```

**Implementation** (`executor.go:104-187`):

```go
func (e *Executor) Execute(ctx context.Context, plan *ActionPlan) ([]Outcome, error) {
    var outcomes []Outcome
    // Phase 1: Folder creates
    for i := range plan.FolderCreates { ... }
    // Phase 2: Moves
    for i := range plan.Moves { ... }
    // Phase 3: Downloads (parallel)
    e.executeParallel(ctx, plan.Downloads, e.executeDownload, &outcomes)
    // Phase 4: Uploads (parallel)
    e.executeParallel(ctx, plan.Uploads, e.executeUpload, &outcomes)
    // Phase 5-9: Sequential loops over remaining slices
    ...
    return outcomes, nil
}
```

**Parallel execution** (`executor.go:436-476`): `executeParallel()` uses
`golang.org/x/sync/errgroup` with `SetLimit(workerPoolSize)` where
`workerPoolSize = 8`. Results are collected in a pre-allocated slice indexed by
action position, then appended to outcomes in order. Fatal errors cancel the
group.

**Why this ordering**: Folder creates must precede downloads/uploads (parent
directory must exist). Deletes must follow transfers (don't delete a file that
is being synced). Moves precede transfers (move the file first, then update
content). Conflicts/synced/cleanups are bookkeeping with no I/O.

---

## 3. The 9-Slice ActionPlan

The `ActionPlan` struct in `types.go` contains 9 named slices, one per
execution phase:

```go
type ActionPlan struct {
    FolderCreates []Action
    Moves         []Action
    Downloads     []Action
    Uploads       []Action
    LocalDeletes  []Action
    RemoteDeletes []Action
    Conflicts     []Action
    SyncedUpdates []Action
    Cleanups      []Action
}
```

**Routing** (`planner.go:665-690`): `appendActions()` examines each action's
`Type` field and routes it to the correct slice via a switch statement:

```go
func appendActions(plan *ActionPlan, actions []Action) {
    for i := range actions {
        switch a.Type {
        case ActionFolderCreate: plan.FolderCreates = append(...)
        case ActionDownload:     plan.Downloads = append(...)
        case ActionUpload:       plan.Uploads = append(...)
        // ... 6 more cases
        }
    }
}
```

**Sorting** (`planner.go:694-727`): `orderPlan()` sorts three slices:
- **Folder creates**: shallowest-first (parent dirs exist before children)
- **Local deletes**: deepest-first, files before folders at same depth
- **Remote deletes**: deepest-first, files before folders at same depth

Other slices are unsorted (order does not matter within a parallel phase).

**Why 9 slices**: The executor processes phases sequentially. Pre-sorting
actions into typed slices eliminates runtime dispatch. The planner encodes
execution order into the data structure itself.

---

## 4. Batch Commit Model

`BaselineManager.Commit()` in `baseline.go` applies all outcomes and the delta
token in a single SQLite transaction:

```go
func (m *BaselineManager) Commit(ctx context.Context,
    outcomes []Outcome, deltaToken, driveID string) error {
    tx, err := m.db.BeginTx(ctx, nil)
    // Apply ALL outcomes in a single transaction
    m.applyOutcomes(ctx, tx, outcomes, syncedAt)
    // Save delta token in the same transaction
    m.saveDeltaToken(ctx, tx, driveID, deltaToken, syncedAt)
    tx.Commit()
    // Invalidate and reload in-memory cache
    m.baseline = nil
    m.Load(ctx)
}
```

**Transaction scope**: One transaction per sync cycle. All-or-nothing: either
every outcome + delta token is persisted, or none are. After commit, the
in-memory baseline cache is invalidated and reloaded from the database.

**Why batch**: The sequential executor produces all outcomes before any are
committed. Batch commit is the natural fit — it ensures the delta token
advances only when all outcomes from that delta are durable.

**Limitation**: If the process crashes after executing 90% of actions but
before commit, all progress is lost. The next cycle re-fetches the same delta
and re-executes everything. For a one-shot sync, this is acceptable. For watch
mode with long-running transfers, it means a crash can lose hours of transfer
progress.

---

## 5. Engine Pipeline

`Engine.RunOnce()` in `engine.go` orchestrates a 9-step sequential pipeline:

```
Step 1: Load baseline (BaselineManager.Load)
Step 2: Observe remote (RemoteObserver.FullDelta) — skip if upload-only
Step 3: Observe local (LocalObserver.FullScan) — skip if download-only
Step 4: Buffer and flush (Buffer.AddAll + FlushImmediate)
Step 5: Early return if no changes
Step 6: Plan actions (Planner.Plan)
Step 7: Return early if dry-run (build report, no execution)
Step 8: Execute plan (Executor.Execute)
Step 9: Commit outcomes + delta token (BaselineManager.Commit)
```

**Steps 8-9 glue** (`engine.go:221-243`): `executeAndCommit()` chains
executor and commit:

```go
func (e *Engine) executeAndCommit(ctx context.Context,
    plan *ActionPlan, bl *Baseline, deltaToken string, report *SyncReport) error {
    exec := NewExecution(e.execCfg, bl)
    outcomes, execErr := exec.Execute(ctx, plan)
    report.Succeeded, report.Failed, report.Errors = classifyOutcomes(outcomes)
    if len(outcomes) > 0 {
        e.baseline.Commit(ctx, outcomes, deltaToken, e.driveID.String())
    }
    return execErr
}
```

**No watch mode**: `--watch` returns "not implemented." The engine has no
`RunWatch()` method. The pipeline is strictly one-shot: observe everything,
plan everything, execute everything, commit everything.

---

## 6. Executor State Management

The executor uses an **immutable config + ephemeral instance** pattern:

**Immutable** (`ExecutorConfig`): Holds clients, sync root, drive ID, logger,
and injectable test functions (nowFunc, hashFunc, sleepFunc). Created once per
engine lifetime via `NewExecutorConfig()`.

**Ephemeral** (`Executor`): Created fresh per `Execute()` call via
`NewExecution(cfg, baseline)`. Contains two mutable fields:

1. `baseline *Baseline` — read-only reference for parent ID resolution
2. `createdFolders map[string]string` — relative path → remote item ID

The `createdFolders` map is populated in Phase 1 (folder creates) and read in
Phases 3-4 (downloads/uploads need parent IDs for newly-created folders). This
cross-phase dependency is the reason folder creates must execute before
transfers.

**Why ephemeral**: Fresh executor per call eliminates temporal coupling. No
stale state from previous cycles. Both mutable fields are always initialized
by `NewExecution()`, preventing nil-map panics.

**Thread safety note**: The `createdFolders` map is NOT mutex-protected because
Phase 1 (writes) runs sequentially before Phases 3-4 (reads). In the
concurrent architecture, workers may write to this map concurrently, requiring
a mutex.

---

## 7. SyncReport Population

`buildReport()` in `engine.go` populates the `SyncReport` with plan counts
using direct `len()` on the 9 slices:

```go
func buildReport(plan *ActionPlan, mode SyncMode, opts RunOpts) *SyncReport {
    return &SyncReport{
        Mode:          mode,
        DryRun:        opts.DryRun,
        FolderCreates: len(plan.FolderCreates),
        Moves:         len(plan.Moves),
        Downloads:     len(plan.Downloads),
        Uploads:       len(plan.Uploads),
        LocalDeletes:  len(plan.LocalDeletes),
        RemoteDeletes: len(plan.RemoteDeletes),
        Conflicts:     len(plan.Conflicts),
        SyncedUpdates: len(plan.SyncedUpdates),
        Cleanups:      len(plan.Cleanups),
    }
}
```

Execution results (`Succeeded`, `Failed`, `Errors`) are populated from
`classifyOutcomes(outcomes)` after execution completes. The SyncReport struct
itself has fields matching the 9 slice names.

---

## 8. Why This Architecture Exists

Each pattern in the sequential architecture enforces a correctness constraint.
The table below explains what each pattern does, why it was correct for
one-shot sync, and why it is insufficient for watch mode.

| Pattern | Constraint Enforced | Correct for One-Shot | Insufficient for Watch Mode |
|---------|-------------------|---------------------|---------------------------|
| 9-phase sequential execution | Parents before children, transfers before deletes | Simple, deterministic ordering | Downloads block uploads. Large transfers block change detection. No new changes processed until entire plan completes. |
| `executeParallel()` with `workerPoolSize=8` | Bounded parallelism within a single phase | Prevents resource exhaustion | All 8 workers run the same action type. No interleaving of downloads and uploads. No interactive/bulk lane fairness. |
| Batch `Commit()` | Delta token advances atomically with outcomes | All-or-nothing consistency | Ctrl-C loses ALL progress since last commit. A 4-hour upload session crash means 4 hours re-downloaded. |
| `executeAndCommit()` glue | Execute-then-commit ordering | Clean separation | Cannot commit individual actions as they complete. No incremental progress. |
| 9-slice `ActionPlan` | Execution order encoded in data structure | Planner controls ordering via slice membership | Cannot express cross-type dependencies (e.g., "download A before uploading B"). No DAG edges. |
| `appendActions()` routing | Type-safe slice assignment | Correct routing | Unnecessary indirection — flat append is simpler when ordering is expressed via dependency edges. |
| `orderPlan()` sorting | Depth-based ordering within slices | Correct for folder hierarchy | `buildDependencies()` expresses the same constraints more precisely via DAG edges, enabling concurrent execution of independent subtrees. |
| `createdFolders` map (no mutex) | Phase 1 writes, Phases 3-4 read | Safe — sequential phases | Concurrent workers need mutex protection. |

**Root cause**: The architecture was designed for one-shot sync where all
actions are known upfront and execution is a single pass. Watch mode requires:
- Continuous observation while transfers are in-flight
- Per-action commits for crash resilience
- Mixed-type concurrent execution (downloads and uploads simultaneously)
- Action cancellation when newer changes supersede in-flight actions

None of these are possible with sequential phase execution and batch commits.

---

## 9. Pattern Detection Reference

Use these grep commands to verify that every old-architecture pattern has been
removed from production code. **Expected result after Phase 5 is complete: 0
hits in non-test `.go` files.**

```bash
# ActionPlan 9-slice fields
grep -rn "plan\.FolderCreates\|plan\.Moves\|plan\.Downloads\|plan\.Uploads\|plan\.LocalDeletes\|plan\.RemoteDeletes\|plan\.Conflicts\|plan\.SyncedUpdates\|plan\.Cleanups" internal/sync/

# 9-phase executor dispatch
grep -rn "func (e \*Executor) Execute\|executeParallel\|workerPoolSize" internal/sync/

# Batch commit and its internals
grep -rn "\.Commit(ctx.*\[\]Outcome\|executeAndCommit\|classifyOutcomes\|applyOutcomes" internal/sync/

# Planner routing and sorting helpers
grep -rn "appendActions\|orderPlan" internal/sync/

# SyncReport from plan slices
grep -rn "len(plan\." internal/sync/

# Per-executor createdFolders map (replaced by incremental baseline updates)
grep -rn "createdFolders" internal/sync/

# Batch report builder (replaced by countByType)
grep -rn "buildReport" internal/sync/

# errgroup import (only used by deleted executeParallel)
grep -rn "sync/errgroup" internal/sync/
```

**Interpreting results**: After increment 5.0 (the pivot), ALL patterns should
show 0 hits in non-test `.go` files. Test files may reference old patterns in
comments or test helpers — this is acceptable as long as no test *calls* a
deleted function.

**Full sweep command** (single command for CI/automation):

```bash
grep -rn \
  "plan\.FolderCreates\|plan\.Moves\|plan\.Downloads\|plan\.Uploads\|plan\.LocalDeletes\|plan\.RemoteDeletes\|plan\.Conflicts\|plan\.SyncedUpdates\|plan\.Cleanups\|executeParallel\|workerPoolSize\|executeAndCommit\|appendActions\|orderPlan\|len(plan\.\|createdFolders\|classifyOutcomes\|applyOutcomes\|buildReport\|sync/errgroup" \
  internal/sync/ \
  --include="*.go" \
  --exclude="*_test.go" \
  | grep -v "legacy" \
  && echo "LEGACY PATTERNS FOUND — clean-slate invariant violated" \
  || echo "CLEAN: no legacy patterns in production code"
```

---

## 10. What Replaces Each Pattern

Mapping from old architecture → new architecture. References are to sections
in [concurrent-execution.md](concurrent-execution.md).

| Old Pattern | Location | New Pattern | Reference |
|-------------|----------|-------------|-----------|
| 9-slice `ActionPlan` (`FolderCreates`, `Downloads`, etc.) | `types.go` | Flat `Actions []Action` + `Deps [][]int` DAG | §2 Action Plan |
| `appendActions()` — route by type to 9 slices | `planner.go` | Direct `append(plan.Actions, action)` | §2 Action Plan |
| `orderPlan()` — sort slices by depth | `planner.go` | `buildDependencies()` produces DAG edges | §5 Dependency Model |
| `pathDepth()` — count path separators for sorting | `planner.go` | Eliminated — depth ordering expressed as dependency edges | §5 Dependency Model |
| `Execute()` — 9-phase sequential dispatch | `executor.go` | Workers pull from `DepTracker` channels | §4 Dependency Tracker, §6 Workers |
| `executeParallel()` + `workerPoolSize=8` | `executor.go` | Lane-based `WorkerPool` (interactive + bulk + overflow) | §6 Workers |
| `errgroup` import | `executor.go` | Eliminated — `WorkerPool` uses native goroutines + channels | §6 Workers |
| Batch `Commit(ctx, []Outcome, deltaToken, driveID)` | `baseline.go` | `CommitOutcome(ctx, outcome)` per action + `CommitDeltaToken(ctx, token, driveID)` | §12 Commit Model |
| `applyOutcomes()` — iterate `[]Outcome` in batch tx | `baseline.go` | `CommitOutcome()` — single outcome per tx | §12 Commit Model |
| `executeAndCommit()` — execute-then-commit glue | `engine.go` | Workers commit individually after each action | §6 Workers |
| `classifyOutcomes()` — count successes/failures in batch | `engine.go` | `WorkerPool` atomic counters incremented per action | §11 Progress Reporting |
| `buildReport()` with `len(plan.FolderCreates)` etc. | `engine.go` | `countByType(plan)` from flat `plan.Actions` | §2 Action Plan |
| `createdFolders` map (no mutex) | `executor.go` | Eliminated — `CommitOutcome()` updates `Baseline` incrementally (under `RWMutex`), so `resolveParentID()` finds newly-created folders in the baseline | §12 Commit Model, §6 Workers |
| `e.createdFolders[action.Path] = item.ID` in `createRemoteFolder` | `executor.go` | Eliminated — worker's `CommitOutcome()` calls `baseline.Put()` instead | §12 Commit Model |
| Direct `baseline.ByPath[x]` map access (no synchronization) | multiple files | Locked `baseline.GetByPath()` / `baseline.GetByID()` accessors (RWMutex) | §12 Commit Model |
| `upload_sessions` table | `00001_initial_schema.sql` | File-based `SessionStore` | §14 Crash Recovery |
| No watch mode (`--watch` returns "not implemented") | `engine.go` / CLI `sync.go` | `RunWatch()` with continuous observers + persistent workers | §15 Execution Modes |
