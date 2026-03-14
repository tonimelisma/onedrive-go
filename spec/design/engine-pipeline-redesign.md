# Engine Pipeline Redesign: watchState, Bootstrap, Async Reconciliation

**Status**: Proposed
**Depends on**: `tracker-redesign.md` (Phases 1-5 must complete first)
**Scope**: `internal/sync/engine.go`, `internal/sync/engine_shortcuts.go`, `internal/sync/dep_graph.go`
**Related design docs**: `sync-engine.md`, `sync-observation.md`, `sync-planning.md`, `sync-execution.md`
**Related requirements**: R-2.1, R-2.6, R-2.8, R-6.4.2, R-6.4.3, R-6.6.7-12, R-6.8.9, R-6.8.15

---

## 1. Problem Statement

After the tracker redesign (tracker-redesign.md) completes, the engine's dispatch correctness issues (D-1 through D-11) are resolved. Five structural and two operational issues remain. These concern engine organization, not dispatch mechanics.

### 1.1 Structural Issues

**S-1: The Engine is a union of two state machines.** ~12 fields are nil/zero in one-shot mode, only initialized during `RunWatch`. ~15 nil guards are scattered across shared methods (`processWorkerResult`, `processTrialResult`, `feedScopeDetection`, etc.) to handle watch-only fields.

After the tracker redesign, the field situation improves (no `tracker`, no `onHeld`, no `trialCh`, no `retrier` field — retrier is a drain-loop timer). But new watch-only state is introduced: `trialPending map[string]trialEntry` (drain-goroutine-only), `retryBatchSize`, and the drain-loop retry timer. The nil-guard pattern persists.

**S-2: Three separate observe→buffer→plan→dispatch orchestrations.** The same logical pipeline is written inline three times:
- Pipeline 1: one-shot (`RunOnce` → `observeChanges` → `executePlan`)
- Pipeline 2: watch batch (`processBatch`, from `runWatchLoop`)
- Pipeline 3: 24-hour reconciliation (`runFullReconciliation`)

Each has its own delta commitment, shortcut handling, permission rechecking, safety config, and dispatch path.

**S-3: Bootstrap divergence.** `RunWatch` calls `RunOnce` literally for the initial sync (engine.go:955-958). The first sync in watch mode uses a different tracker (non-persistent), different worker pool (ephemeral), no scope detection, no failure retrier, no rolling delete counter. After the tracker redesign, this is even more problematic: the first sync uses a throwaway `DepGraph` and no `ScopeGate`, while every subsequent sync uses the persistent watch infrastructure.

**S-4: Duplicated safety config.** Two methods produce `SafetyConfig`: `resolveSafetyConfig` (one-shot) and `resolveWatchSafetyConfig` (watch). The watch variant hardcodes `forceSafetyMax`.

**S-5: Cross-mode naming confusion.** `setWatchShortcuts` is called during one-shot execution (engine.go:382).

### 1.2 Operational Issues

**O-1: 24-hour reconciliation blocks the watch loop.** `runFullReconciliation` runs synchronously inside the `runWatchLoop` select. For a drive with 100K+ items, full delta enumeration can take minutes. Normal batches, recheck ticks, observer errors — all queue up.

**O-3: Bootstrap failure is fatal.** If `RunOnce` fails during the initial sync in `RunWatch`, the daemon exits. Transient failures during startup should be recoverable.

---

## 2. Design

### 2.1 watchState Struct

Bundle all watch-only fields into a struct. One `e.watch != nil` check replaces ~15 scattered nil guards.

```go
type Engine struct {
    // Shared infrastructure (both modes)
    baseline        *SyncStore
    planner         *Planner
    execCfg         *ExecutorConfig
    fetcher         DeltaFetcher
    driveVerifier   DriveVerifier
    folderDelta     FolderDeltaFetcher
    recursiveLister RecursiveLister
    permChecker     PermissionChecker
    permCache       *permissionCache
    syncRoot        string
    driveID         driveid.ID
    logger          *slog.Logger
    sessionStore    *driveops.SessionStore
    transferWorkers int
    checkWorkers    int
    bigDeleteThreshold int

    // From tracker redesign
    depGraph  *DepGraph
    scopeGate *ScopeGate
    readyCh   chan *TrackedAction

    // Engine-owned result counters
    succeeded    atomic.Int32
    failed       atomic.Int32
    syncErrors   []error
    syncErrorsMu sync.Mutex

    // Test hooks
    nowFn               func() time.Time
    localWatcherFactory func() (FsWatcher, error)

    // Watch-mode state — nil in one-shot
    watch *watchState
}

type watchState struct {
    // Buffer — promoted from local variable per tracker-redesign.md Phase 4
    buf *Buffer

    // Big-delete protection
    deleteCounter   *deleteCounter
    lastDataVersion int64

    // Scope detection
    scopeState *ScopeState

    // Trial management (drain-goroutine-only state per tracker-redesign.md §3.9)
    trialPending map[string]trialEntry
    trialTimer   *time.Timer
    trialMu      sync.Mutex

    // Retry timer (replaces FailureRetrier per tracker-redesign.md §3.8)
    retryTimer     *time.Timer
    retryBatchSize int

    // Observer references
    remoteObs *RemoteObserver
    localObs  *LocalObserver

    // Shortcuts synchronized between goroutines
    shortcuts   []Shortcut
    shortcutsMu sync.RWMutex

    // Throttling and deduplication
    lastPermRecheck  time.Time
    lastSummaryTotal int

    // Async reconciliation guard
    reconcileRunning atomic.Bool
}
```

**Impact on tracker-redesign.md**: Phase 4 item 2 says "promote `buf` to engine field." With watchState, `buf` goes on `e.watch.buf` instead — `buf` is watch-mode only (one-shot uses `FlushImmediate` on a local buffer). The tracker redesign's drain-loop retrier code (`e.buf.Add(ev)`) becomes `e.watch.buf.Add(ev)`. Similarly, `trialPending` is already drain-goroutine-only (tracker-redesign.md §3.9) — it moves to `e.watch.trialPending`.

### 2.2 Unified Bootstrap

Replace the `RunOnce()` call in `RunWatch` with `bootstrapSync` that uses the watch pipeline.

```go
func (e *Engine) RunWatch(ctx context.Context, mode SyncMode, opts WatchOpts) error {
    e.watch = e.newWatchState(opts)

    pipe, err := e.initWatchInfra(ctx, mode, opts) // tracker, pool, drain — NOT observers
    if err != nil {
        return err
    }
    defer pipe.cleanup()

    if err := e.bootstrapSync(ctx, mode, pipe); err != nil {
        return fmt.Errorf("sync: initial sync failed: %w", err)
    }

    // Start observers AFTER bootstrap — they see the post-bootstrap baseline
    e.startObservers(ctx, pipe.bl, mode, e.watch.buf, opts)

    return e.runWatchLoop(ctx, pipe)
}
```

`bootstrapSync` does the same work as `RunOnce` (verify drive, crash recovery, observe, plan) but dispatches through the watch pipeline:

```go
func (e *Engine) bootstrapSync(ctx context.Context, mode SyncMode, pipe *watchPipeline) error {
    if err := e.verifyDriveIdentity(ctx); err != nil {
        return err
    }

    if err := e.baseline.ResetInProgressStates(ctx, e.syncRoot, retry.Reconcile.Delay); err != nil {
        e.logger.Warn("failed to reset in-progress states", slog.String("error", err.Error()))
    }

    // Load persisted scope blocks from tracker redesign
    if err := e.scopeGate.LoadFromStore(ctx); err != nil {
        return err
    }

    changes, err := e.observeChanges(ctx, pipe.bl, mode, false, false)
    if err != nil {
        return err
    }
    if len(changes) == 0 {
        return nil
    }

    // Dispatch through watch pipeline — same admitAndDispatch as steady-state
    e.processBatch(ctx, changes, pipe.bl, mode, pipe.safety, pipe.depGraph)

    return e.waitForQuiescence(ctx)
}
```

`waitForQuiescence` blocks until all in-flight actions complete. Requires `DepGraph.WaitForEmpty()`:

```go
// WaitForEmpty returns a channel closed when total == completed.
// In persistent mode, re-creates the channel when new actions are added.
func (g *DepGraph) WaitForEmpty() <-chan struct{}
```

### 2.3 Async Reconciliation

Replace synchronous `runFullReconciliation` with async:

```go
case <-p.reconcileC:
    e.runFullReconciliationAsync(ctx, p.bl)

func (e *Engine) runFullReconciliationAsync(ctx context.Context, bl *Baseline) {
    if !e.watch.reconcileRunning.CompareAndSwap(false, true) {
        return // previous still running
    }
    go func() {
        defer e.watch.reconcileRunning.Store(false)

        events, deltaToken, err := e.observeRemoteFull(ctx, bl)
        if err != nil {
            if ctx.Err() == nil {
                e.logger.Error("full reconciliation failed", slog.String("error", err.Error()))
            }
            return
        }

        observed := changeEventsToObservedItems(events)
        if err := e.baseline.CommitObservation(ctx, observed, deltaToken, e.driveID); err != nil {
            e.logger.Error("failed to commit reconciliation", slog.String("error", err.Error()))
            return
        }

        events = filterOutShortcuts(events)
        scEvents, _ := e.reconcileShortcutScopes(ctx, bl)
        events = append(events, scEvents...)

        for i := range events {
            e.watch.buf.Add(&events[i])
        }

        if refreshed, err := e.baseline.ListShortcuts(ctx); err == nil {
            e.watch.setShortcuts(refreshed)
        }
    }()
}
```

Concurrency is safe: SQLite WAL mode handles concurrent writers (CommitObservation + CommitOutcome serialize). Buffer is mutex-protected. Planner is idempotent on duplicates.

### 2.4 Mode-Conditional Result Processing

After tracker redesign + watchState:

```go
case resultRequeue:
    e.recordFailure(ctx, r, retry.Reconcile.Delay)
    e.recordError(r)
    ready := e.depGraph.Complete(r.ActionID)
    if r.Success {
        e.routeReadyActions(ctx, ready)  // drain goroutine
    } else {
        // Failure-aware dispatch (tracker-redesign.md §3.5)
        for _, dep := range ready {
            e.recordFailure(ctx, depToWorkerResult(dep, r), retry.Reconcile.Delay)
            e.depGraph.Complete(dep.ID)
        }
    }
    if e.watch != nil {
        e.watch.scopeState.UpdateScope(r)
    }
```

One `e.watch != nil` guard at a clear boundary. `routeReadyActions` is drain-goroutine-only (tracker-redesign.md §3.9).

### 2.5 Safety Config Unification

```go
func (e *Engine) resolveSafetyConfig(force bool) *SafetyConfig {
    if force || e.watch != nil {
        return &SafetyConfig{BigDeleteThreshold: forceSafetyMax}
    }
    return &SafetyConfig{BigDeleteThreshold: e.bigDeleteThreshold}
}
```

---

## 3. Migration Plan

All phases in this document execute AFTER tracker-redesign.md Phases 1-5. The complete roadmap across both documents is in section 4.

### Phase 8: Extract watchState

1. Create `watchState` struct with all watch-only fields
2. Add `watch *watchState` field to Engine
3. Move watch-only fields from Engine to watchState
4. Replace all nil guards (`e.scopeState != nil`, `e.retrier != nil`, etc.) with `e.watch != nil`
5. Move `buf` from Engine field (tracker-redesign Phase 4 item 2) to `e.watch.buf`
6. Move `trialPending` from Engine to `e.watch.trialPending`
7. Move `setWatchShortcuts`/`getWatchShortcuts` to watchState methods
8. Update all engine_test.go helpers

**Code**: `engine.go` (field reorganization, nil guard replacement), `engine_shortcuts.go` (shortcut access), `engine_test.go`
**Design docs**: `sync-engine.md` (Engine struct documentation)
**Requirements**: None

### Phase 9: Unified Bootstrap

1. Create `bootstrapSync` method
2. Create `waitForQuiescence` method
3. Add `WaitForEmpty()` to `DepGraph` (dep_graph.go)
4. Split `initWatchPipeline` → `initWatchInfra` (no observers) + `startObservers` (already exists, just decouple)
5. Rewrite `RunWatch` to call `bootstrapSync` instead of `RunOnce`
6. Add `scopeGate.LoadFromStore(ctx)` call in `bootstrapSync` (was in tracker-redesign Phase 4 item 16 — moves here since bootstrap runs before the watch loop)

**Code**: `engine.go` (RunWatch, bootstrapSync, waitForQuiescence, initWatchInfra), `dep_graph.go` (WaitForEmpty), `dep_graph_test.go`
**Design docs**: `sync-engine.md` (RunWatch behavior)
**Requirements**: None

### Phase 10: Async Reconciliation

1. Replace `runFullReconciliation` with `runFullReconciliationAsync`
2. Add `reconcileRunning atomic.Bool` to `watchState`
3. Update `runWatchLoop` select case

**Code**: `engine.go` (reconciliation methods, watch loop), `engine_test.go` (non-blocking test)
**Design docs**: `sync-engine.md` (reconciliation behavior)
**Requirements**: None

### Phase 11: Safety Config Unification

1. Merge `resolveSafetyConfig` + `resolveWatchSafetyConfig` → one method
2. Update callers

**Code**: `engine.go` (two methods → one)
**Design docs**: None
**Requirements**: None

---

## 4. Complete Roadmap Across Both Documents

This is the authoritative execution order. Each increment is a PR that leaves the repo green (builds, tests pass, lint clean).

| Increment | Source doc | What changes | Code files | Design docs | Requirements |
|---|---|---|---|---|---|
| **1. Backoff Timing** | tracker-redesign Phase 1 | Unified trial intervals, honor Retry-After | `scope.go`, `engine.go`, `scope_test.go`, `engine_test.go` | `sync-execution.md`, `sync-engine.md` | R-2.10.6, R-2.10.7, R-2.10.8, R-2.10.14 |
| **2. Extract DepGraph** | tracker-redesign Phase 2 | Pure dependency graph, `Complete` returns dependents, D-10 fix | `dep_graph.go` (new), `dep_graph_test.go` (new) | `sync-execution.md` (Tracker → DepGraph) | None |
| **3. Extract ScopeGate + Persist** | tracker-redesign Phase 3 | Scope blocks persisted, no held queue, `Admit`/`SetScopeBlock`/`ClearScopeBlock` | `scope_gate.go` (new), `scope_gate_test.go` (new), `migrations.go` | `sync-execution.md` (scope blocks + trials section) | R-2.10.5, R-2.10.11 |
| **4. Rewire Engine** | tracker-redesign Phase 4 | DepGraph + ScopeGate + readyCh. `admitAndDispatch`, `routeReadyActions`, `cascadeRecordAndComplete`, `onScopeClear`, `reobserve`, `createEventFromDB`, `isFailureResolved`. Retrier in drain loop. Trial interception. Failure-aware dispatch. | `engine.go`, `engine_test.go`, `engine_shortcuts.go`, `worker.go`, `worker_test.go`, `store_failures.go`, `store_admin.go` | `sync-execution.md`, `sync-engine.md` (state machine, scope clear, retrier, trials, failure-aware dispatch) | R-2.10.5-8, R-2.10.11, R-2.10.14 |
| **5. Delete Old Code** | tracker-redesign Phase 5 | Remove tracker.go, FailureRetrier, synthesizeFailureEvent | `tracker.go` (deleted), `tracker_test.go` (deleted), `reconciler.go`, `reconciler_test.go` | None | None |
| **6. Shortcut Enrichment** | tracker-redesign Phase 6 | Populate `targetShortcutKey` and `targetDriveID` in planner | `types.go`, `planner.go`, `planner_test.go` | `sync-execution.md` | R-2.10.1, R-2.10.16-31, R-6.8.12, R-6.8.13 |
| **7. sync_failures Ownership** | tracker-redesign Phase 7 | Engine owns failure lifecycle, store owns baseline | `store_baseline.go`, `engine.go` | `sync-engine.md` | None |
| **8. Extract watchState** | pipeline-redesign Phase 8 | Bundle watch-only fields, 1 nil guard replaces 15 | `engine.go`, `engine_shortcuts.go`, `engine_test.go` | `sync-engine.md` | None |
| **9. Unified Bootstrap** | pipeline-redesign Phase 9 | `bootstrapSync` replaces `RunOnce` in `RunWatch`, `WaitForEmpty` | `engine.go`, `dep_graph.go`, `dep_graph_test.go` | `sync-engine.md` | None |
| **10. Async Reconciliation** | pipeline-redesign Phase 10 | Non-blocking reconciliation goroutine | `engine.go`, `engine_test.go` | `sync-engine.md` | None |
| **11. Safety Config** | pipeline-redesign Phase 11 | Merge two methods into one | `engine.go` | None | None |

**Parallelization**: Increments 1-5 (tracker redesign) are strictly sequential — each builds on the previous. Increments 6, 7 (tracker cleanup) can run in parallel with each other but require increment 5 to be complete. Increments 8-11 (pipeline redesign) are strictly sequential and require increment 5 (or at minimum increment 4) to be complete. Increments 6-7 can run in parallel with increments 8-11 since they touch different files.

---

## 5. What This Design Does NOT Change

- **`RunOnce`** remains a standalone one-shot entry point. No watchState.
- **The planner** is untouched (pure function).
- **Workers** are untouched (same pool, same execution, same CommitOutcome).
- **Result classification** is untouched (`classifyResult` is pure).
- **Observer implementations** are untouched.
- **The Buffer type** gains no new methods from this design.
- **O-2 (polling vs push)** is not addressed — Microsoft API limitation.

---

## 6. Risks and Mitigations

**Risk**: `bootstrapSync` + `waitForQuiescence` — `WaitForEmpty()` never fires if a scope block holds actions during bootstrap.
**Mitigation**: Bootstrap has empty scope detection windows — no historical failures to trigger blocks. Rare case: multiple 429s during first pass. Trial timer eventually clears. 30-minute timeout on `waitForQuiescence` prevents hanging.

**Risk**: Observer startup after bootstrap may miss events.
**Mitigation**: Same window as current design. Delta token from bootstrap is committed; observer starts from that token. Next poll catches the gap.

**Risk**: Async reconciliation overlaps with normal observation.
**Mitigation**: Buffer deduplicates by path. Planner is idempotent. `HasInFlight` + `CancelByPath` prevent duplicate dispatch. `reconcileRunning` atomic prevents concurrent reconciliations.

**Risk**: Phase 8 (watchState extraction) is a large mechanical rename.
**Mitigation**: Go compiler catches all missing renames. Run with `-race`.
