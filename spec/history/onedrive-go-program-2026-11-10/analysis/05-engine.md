# 05 — Breaking up the engine (`internal/sync`)

Repository `~/src/onedrive-go`, branch `main`, commit `70301c0e`. Read-only analysis; nothing was built, run or tested.

## 0. Conventions

- A bare `file.go:N` means `~/src/onedrive-go/internal/sync/file.go:N`. Other paths are given from the repository root (`internal/multisync/…`, `spec/design/…`). `end-state.md` is `~/agent/notes/onedrive-go-sync-split-2026-11-10/end-state.md`.
- **VERIFIED** = I read the code that proves it. **SUSPECTED** = not proven; the text says what would settle it.
- "Proposal" marks my design, as opposed to a finding about today's code.
- "Loop goroutine" = the goroutine that called `Engine.RunOnce` or `Engine.RunWatch`; both loops run synchronously on it (`engine_run_once.go:364`, `engine_watch.go:109`). VERIFIED.
- Section 11 lists what I did not read.

## 0.1 Findings at a glance

| # | Finding | Status |
|---|---------|--------|
| F1 | `engineFlow.engine` is still used 228 times (153 direct selectors, 9 alias sites `eng := flow.engine` carrying 75 more) in 24 `engine*.go` files; none outside them | VERIFIED |
| F2 | 35 of those uses bypass `runtimeDeps` for things `runtimeDeps` already carries (`baseline`, `logger`, `nowFunc`, `emitDebugEvent`) | VERIFIED |
| F3 | Seven kinds of goroutine run `*watchRuntime`/`*engineFlow`/`*Engine` method bodies beside the loop; nothing but convention keeps them off `sched`/`retries`/`scopes` | VERIFIED |
| F4 | A third off-loop writer of `Engine.protectedRoots` exists (the orchestrator's ack handle) that the two comments naming the writers omit | VERIFIED |
| F5 | `engine_housekeeping.go:26` reads `protectedRoots` without `protectedRootsMu` | VERIFIED (the read); overlap with a writer SUSPECTED |
| F6 | `startObservers` starts the remote observer goroutine before two fallible local set-up steps; on their error paths the goroutine is left running and unrecorded | VERIFIED |
| F7 | The observer `WaitGroup` (`obsWg`) is never waited | VERIFIED |
| F8 | Three vestigial locks: `scopeLedger.mu`, `watchTimerState.timerMu`, `depGraph.mu` (+ `trackedNode.depsLeft` atomic). Four vestigial atomics on `Engine` | VERIFIED |
| F9 | `retry_work` rows live in SQLite and in `retryLedger.rowsByKey`; nine hand-written mirror sites; hold decisions read the copy; no invariant compares the two; four concrete divergence paths | VERIFIED (mechanism); reachability of the worst path SUSPECTED |
| F10 | Watch lifecycle = a three-value enum plus a variadic-argument mode flag, booleans, and nil-ed channel fields; two transitions are unconditional setters | VERIFIED |
| F11 | `executorConfig` is documented "immutable" and has 9 setters, 3 test-only direct field writes, a mutex-guarded mutable member, and its pool has 2 post-construction field writes (14 mutation points) | VERIFIED |
| F12 | 10 test-only hooks on `Engine`/`engineInputs`, 6 on `executorConfig`/`SyncStore`/`workerPool`, 3 on observers; no package-level hook variables | VERIFIED |
| F13 | Dead code: `trackedAction.Cancel`, `depGraph.MarkTrial`, `Engine.diskAvailableFn`; stale `depGraph` capture in watch cleanup; four stale comments | VERIFIED |

---

## 1. `engineFlow` holds a back-pointer to `Engine`

### 1.1 Current structure

`engine_runtime_types.go:8-45` (VERIFIED):

```go
type engineFlow struct {
    engine *Engine              // :9  the back-pointer
    deps runtimeDeps            // :13
    signals runtimeSignals      // :18
    inspect watchInspector      // :22
    admitted admittedWorkOwner  // :28
    sched   actionScheduler     // :32
    retries retryLedger         // :33
    scopes  scopeLedger         // :34
    results runResults          // :35
    runID string                // :37
    supersedeStreaks map[string]int // :44
}
```

- Constructor: `newEngineFlow(engine *Engine) *engineFlow` (`engine_runtime_types.go:47-61`), which also takes `runID` from `engine.nextRuntimeRunID()` (`:54`) and builds `runtimeDeps` from the engine (`:50`). VERIFIED.
- `runtimeDeps{logger *slog.Logger; store *SyncStore; clock syncClock; events func(engineDebugEvent)}` (`engine_runtime_deps.go:19-24`), built by `newRuntimeDeps(engine *Engine)` (`:26-33`). Its comment (`:8-18`) records the earlier state: "That pointer carried 320 references, and four targets accounted for 191 of them". VERIFIED.
- Embedders: `oneShotRunner{*engineFlow; outbox}` (`engine_runtime_types.go:63-70`), `watchRuntime{*engineFlow; loop; resources; timers}` (`engine_watch_state.go:132-137`). VERIFIED.
- The comment at `engine_runtime_types.go:3-7` states the intent: "The Engine itself remains a dependency container whose fields are fixed after construction, apart from the few it guards itself". VERIFIED.

### 1.2 Every use, counted

Method: `grep -rnoE "\b(flow|rt|r|f|runner|e)\.engine\b|\beng := (flow|rt|r|f)\.engine\b|\beng\.[a-zA-Z]+"` over non-test `engine*.go`, plus `grep -rnE "\.engine\b"` over every other non-test file of the package (no match). VERIFIED.

- 153 direct `.engine` selectors, by file: `engine_current_plan.go` 31; `engine_watch.go` 27; `engine_primary_root.go` 11; `engine_primary_root_watch.go` 11; `engine_watch_state.go` 9; `engine_run_once.go` 9; `engine_housekeeping.go` 8; `engine_runtime_completion.go` 7; `engine_runtime_permissions.go` 6; `engine_watch_loop.go` 6; `engine_runtime_start.go` 4; `engine_watch_maintenance.go` 3; `engine_watch_local_observation.go` 3; `engine_results.go` 3; `engine_watch_replan.go` 2; `engine_runtime_lifecycle.go` 2; `engine_runtime_workset.go` 2; `engine_observation_postprocess.go` 2; `engine_mount_root.go` 2; `engine_log_fields.go` 1; `engine_runtime_admission.go` 1; `engine_runtime_release_ramp.go` 1; `engine_runtime_invariants.go` 1; `engine_watch_observation.go` 1.
- 9 alias sites `eng := flow.engine` (`engine_startup.go:13`; `engine_watch_observation.go:38`, `:64`, `:152`; `engine_run_once.go:540`; `engine_mount_root.go:41`, `:299`; `engine_current_plan.go:322`, `:363`) carrying 75 further uses (`engine_mount_root.go` 29, `engine_current_plan.go` 19, `engine_run_once.go` 13, `engine_watch_observation.go` 9, `engine_startup.go` 5).
- Total 228. (`engine_remote_path_lookup.go:11` names an `*Engine` receiver `eng`; those are not back-pointer uses.)

### 1.3 What is reached, grouped

**A. Reads of immutable configuration** (set once in the `Engine` literal, `engine.go:198-238`). VERIFIED.

| Field / method | Sites |
|---|---|
| `driveID` | `engine_log_fields.go:16`; `engine_runtime_completion.go:490`; `engine_current_plan.go:426, 434, 445, 460` and `:329` via alias; `engine_run_once.go:446, 455, 466, 478, 493` and `:541, :564, :574` via alias; `engine_watch_observation.go:17`; `engine_primary_root.go:142, 203, 233`; `engine_primary_root_watch.go:31, 98`; `engine_mount_root.go:395`; `engine_watch.go:514, 524, 548, 574` |
| `transferWorkers` | `engine_runtime_lifecycle.go:137, 187`; `engine_runtime_release_ramp.go:209`; `engine_watch_state.go:368, 374`; `engine_watch.go:196`; `engine_run_once.go:359` |
| `workerPoolSize()` (`engine_results.go:201-203`) | `engine_runtime_workset.go:20` |
| `contentFilter` | `engine_current_plan.go:269, 491, 543, 544, 571` and `:376` via alias; `engine_watch_loop.go:357`; `engine_observation_postprocess.go:242`; `engine_watch.go:370`; `engine_housekeeping.go:26-28` |
| `shortcutNamespaceID` | `engine_current_plan.go:238, 481, 489` and `:335` via alias; `engine_watch_loop.go:355`; `engine_primary_root_watch.go:34`; `engine_run_once.go:547` via alias |
| `syncRoot` | `engine_current_plan.go:268, 490`; `engine_watch_loop.go:356` |
| `syncTree` | `engine_watch.go:450`; `engine_housekeeping.go:22`; `engine_current_plan.go:386` via alias |
| `dataDir` | `engine_runtime_completion.go:448` |
| `remoteRootItemID` | `engine_primary_root_watch.go:99`; `engine_mount_root.go:396` |
| `enableWebsocket` | `engine_watch.go:509`; `engine_primary_root_watch.go:87` |
| `checkWorkers` | `engine_watch.go:364`; `engine_current_plan.go:368` via alias |
| `localRules` | `engine_watch.go:372`; `engine_current_plan.go:378` via alias |
| `hasRemoteMountRoot()` (`engine_mount_root.go:15-17`) | `engine_primary_root.go:111`; `engine_primary_root_watch.go:18` |
| `resolveDebounce` / `resolvePollInterval` (`engine_watch.go:717-732`, pure) | `engine_watch.go:203, 504` |
| `nowFunc` (`engine_results.go:186-192`) | `engine_runtime_workset.go:38`; `engine_watch.go:185`; `engine_watch_observation.go:161` via alias |

**B. Collaborators (immutable pointers; the collaborator has its own state).** VERIFIED.

| Collaborator | Sites |
|---|---|
| `execCfg` | `engine_runtime_start.go:92` (per-plan reset of the parent-check memo); `engine_watch.go:193`; `engine_run_once.go:354` |
| `permHandler` | `engine_runtime_completion.go:446, 449, 454`; `engine_runtime_permissions.go:55, 59, 82, 185, 188, 194` |
| `collector()` | `engine_watch_replan.go:34, 48`; `engine_watch_maintenance.go:174`; `engine_runtime_admission.go:71`; `engine_current_plan.go:28, 49, 78, 636`; `engine_watch_local_observation.go:36, 47, 67`; `engine_watch_state.go:291, 319, 328, 346`; `engine_watch.go:194`; `engine_run_once.go:355` |
| Graph clients `fetcher`, `itemsClient`, `socketIOFetcher`, `folderDelta`, `recursiveLister`, `remotePathLookup(ctx)` | `engine_primary_root_watch.go:31-33`; `engine_watch.go:509, 522-523`; via alias `engine_run_once.go:541-543`, `engine_current_plan.go:329-331`, `engine_mount_root.go:58, 69, 82, 102, 107` |
| `sessionStore` | `engine_housekeeping.go:23` |
| `shortcutChildWorkSink` | `engine_current_plan.go:263, 266`; `engine_watch_loop.go:347, 354` |
| `syncRootIdentity` | `engine_watch.go:373`; `engine_current_plan.go:379` via alias |
| `emitActivity` | `engine_runtime_completion.go:35` |
| `buildCurrentActionPlanFromInputs` | `engine_current_plan.go:604` |
| `confirmEnumerationOrphans`, `mountRootEvents`, `preferredMountRootObservationMode` | `engine_run_once.go:578`; `engine_mount_root.go:47, 74, 117` (all via alias) |
| start-up methods | `engine_startup.go:15, 20, 29, 36` via alias |
| the whole `*Engine` passed onward | `engine_primary_root.go:137, 147, 198, 208, 228, 243`; `engine_primary_root_watch.go:74` (to `buildRemoteObservationBatch`, which uses only `engine.logger`, `engine_primary_root.go:48`, and `buildPrimaryWatchBatch`, `:77-96`) |

**C. Ambient capabilities that `runtimeDeps` already carries but are still reached through the pointer (35 uses).** VERIFIED.

- `baseline` instead of `deps.store` (14): `engine_current_plan.go:610, 615, 631`; `engine_watch.go:365, 374`; via alias `engine_mount_root.go:50, 301`; `engine_run_once.go:570`; `engine_current_plan.go:323, 370, 380`; `engine_watch_observation.go:39, 161`; `engine_startup.go:38`.
- `logger` instead of `deps.logger` (17): `engine_results.go:160, 166, 174`; `engine_primary_root.go:48, 132`; via alias `engine_mount_root.go:64, 94, 97`; `engine_run_once.go:541, 590`; `engine_current_plan.go:329, 368`; `engine_watch_observation.go:41, 69, 86, 101, 106`.
- `nowFunc` instead of `deps.now` (3): listed in table A.
- `emitDebugEvent` instead of `deps.emit` (1): `engine_watch_observation.go:157`.

**D. Test hooks read through the pointer.** VERIFIED. `dispatchOrder` (`engine_runtime_start.go:55-56`), `assertInvariants` (`engine_runtime_invariants.go:11`), `localWatcherFactory` (`engine_watch.go:395-396`), `serializeWorkers` (`engine_watch_state.go:371`; `engine_watch.go:195`; `engine_run_once.go:356`), `socketIOWakeSourceFactory` (`engine_watch.go:522`).

**E. Mutable or self-guarded `Engine` state reached.** VERIFIED. This is the part that contradicts "fields are fixed after construction".

| What | Sites | Guard |
|---|---|---|
| `refreshProtectedRootsFromStore` (writes `protectedRoots`) | `engine_current_plan.go:256`, `:332` and `:364` via alias; `engine_watch.go:97`; `engine_run_once.go:544` via alias | `protectedRootsMu` (`protected_roots.go:33-37`) |
| `currentProtectedRoots()` | `engine_watch.go:371`; `engine_primary_root_watch.go:34`; `engine_run_once.go:547`; `engine_current_plan.go:335, 377` | mutex, returns a copy (`protected_roots.go:41-49`) |
| direct read `flow.engine.protectedRoots` | `engine_housekeeping.go:26` | **none** (F5) |
| `refreshProtectedRootsForHousekeeping` (`protectedRootRefreshFailing`) | `engine_housekeeping.go:20` | atomic |
| `purgeExpiredTrash` (`trashPurgedAt`, `trashPurgeFailing`) | `engine_watch_maintenance.go:14`; `engine_housekeeping.go:32` | atomics |
| `mountRootDeltaFallbackReported` | `engine_mount_root.go:73, 93` via alias | atomic |
| `nextRuntimeRunID` (`nextRunID`) | `engine_runtime_types.go:54` | atomic |
| `commitLocalSnapshot` (takes `identity.bindMu`, `engine.go:355-407`) | `engine_current_plan.go:398` | mutex in `syncRootIdentity` |
| `reconcileShortcutRootLocalState`, `confirmShortcutCompleteOmissions`, `publishShortcutChildWorkSnapshot` | `engine_current_plan.go:249, 240`; `engine_watch_loop.go:343`; `engine_watch.go:279` | store transactions + `protectedRootsMu` |
| `fullRemoteRefreshDelay`, `shouldRunFullRemoteRefresh` | `engine_watch_maintenance.go:95`; `engine_current_plan.go:64` | store read + clock |

**F. Nil-checks of the pointer** (tests build flows with no engine): `engine_runtime_start.go:89`; `engine_current_plan.go:232, 480`; `engine_watch_state.go:324, 368`; `engine_observation_postprocess.go:238`. VERIFIED.

### 1.4 Why it hurts

- Every method on `engineFlow` (159), `watchRuntime` (135) and `oneShotRunner` (14) can reach all 49 `Engine` fields and 75 `Engine` methods. Bug class: an off-loop method (section 2) touching engine state that only the loop may touch, or a loop method touching engine state that an off-loop goroutine writes. One instance exists: the unguarded read at `engine_housekeeping.go:26`. VERIFIED.
- The 35 group-C uses show the cost of leaving the pointer in place: the narrower capability exists and is bypassed, because nothing stops it. VERIFIED.
- Six production nil-checks exist only because tests cannot build a flow without the whole engine. VERIFIED (the checks); that tests are the only nil-engine constructors is SUSPECTED (settle: `grep -rn "engineFlow{" --include=*_test.go`).

### 1.5 Proposed end state

Proposal. `engineFlow` gets what it uses, by name; the `engine` field is deleted.

```go
// engine_runtime_types.go
type runConfig struct {            // plain values, copied once per run
    driveID             driveid.ID
    remoteRootItemID    string
    dataDir             string
    syncRoot            string
    syncTree            *synctree.Root
    transferWorkers     int
    poolSize            int          // poolWorkerCount(transferWorkers, serialize)
    contentFilter       ContentFilterConfig
    shortcutNamespaceID string
}

type flowInputs struct {           // every field named at the one construction site
    cfg       runConfig
    deps      runtimeDeps          // gains: perf *perf.Collector, activity func(*actionCompletion, …)
    runID     string
    planner   *planner
    exec      *executorConfig      // until item 7 lands; then the pool config
    parents   *remoteParentChecks  // item 7: per-plan memo, reset by the flow
    perms     *permissionHandler
    roots     *protectedRootSet    // owns protectedRootsMu + protectedRoots
    observe   *observation         // item 2
    shortcuts *shortcutRoots       // owns reconcile / omission confirmation / child-work publication
    upkeep    *housekeeper         // owns trash purge + stale-session cleanup state
    hooks     engineHooks          // item 8; zero value = production
}

func newEngineFlow(in flowInputs) *engineFlow
func (e *Engine) flowInputs() flowInputs   // engine.go; the only place that reads Engine fields for a run
```

New owners (all in package `sync`, each owns real state, so none is a pass-through):

| Type | File | Owns | Replaces |
|---|---|---|---|
| `protectedRootSet{mu stdsync.Mutex; roots []ProtectedRoot; store *SyncStore}` with `refresh(ctx) error`, `current() []ProtectedRoot` | `protected_roots.go` | the one piece of engine state with three off-loop writers (section 2.3) | `Engine.protectedRootsMu`, `Engine.protectedRoots`, `refreshProtectedRootsFromStore`, `setProtectedRoots`, `currentProtectedRoots` (`protected_roots.go:16-49`) |
| `housekeeper{syncTree; sessionStore; trash; contentFilter; roots *protectedRootSet; logger; clock; purgedAt time.Time; purgeFailing, refreshFailing bool}` with `afterSync(ctx)`, `purgeExpiredTrash(force bool)` | `engine_housekeeping.go` | the three housekeeping fields, as plain fields (they are loop-only, section 4) | `engineFlow.postSyncHousekeeping`, `Engine.refreshProtectedRootsForHousekeeping`, `Engine.purgeExpiredTrash`, `Engine.trashPurgedAt`, `trashPurgeFailing`, `protectedRootRefreshFailing` (`engine_housekeeping.go:19-97`, `engine.go:59, 132-133`) |
| `observation` | `engine_observation.go` (new) | section 2.4 | section 2.4 |
| `shortcutRoots` | `shortcut_root_lifecycle.go` | the shortcut-root rows and the child-work sink; called by the loop and by the orchestrator's ack handle | the 26 `Engine` methods in the shortcut-family files (`end-state.md:124-130` keeps them in `sync`). Field set SUSPECTED: I read only `shortcut_root_lifecycle.go:40-99` and `engine_shortcut_topology.go` in full; settle by reading the other 18 methods |

Migration of each group:

- Group A → `flow.cfg.<field>`; `nowFunc` → `flow.deps.now`; `resolveDebounce`/`resolvePollInterval` become free functions of `WatchOptions`.
- Group B → named `flowInputs` fields; `collector()` → `flow.deps.perf`; `emitActivity` → `flow.deps.activity`; `buildCurrentActionPlanFromInputs` → a flow method using `flow.planner` and `flow.cfg.driveID` (its body uses `e.driveID` at `engine_current_plan.go:748`; the rest of its field use is SUSPECTED — settle by reading `:732-752`).
- Group C → `flow.deps.*` (mechanical).
- Group D → `flow.hooks.*` (item 8).
- Group E → the owners in the table; `nextRuntimeRunID` → `flowInputs.runID`; `fullRemoteRefreshDelay`/`shouldRunFullRemoteRefresh` become flow methods over `deps.store` and `deps.now` (their bodies were read only at `engine_watch_maintenance.go:77-92`; the fields used at `:59-76` are SUSPECTED).
- `runStartupStage` (`engine_startup.go:10-44`) is inverted: it becomes `(e *Engine) runStartup(ctx, flow *engineFlow) (*Baseline, error)`, which calls `flow.normalizePersistedScopes(ctx)` (`:25`) between the engine's own identity-proof calls (`:15, :20, :29, :36`). Callers: `engine_watch.go:66`, `engine_run_once.go:302-307`.
- Group F: the six nil-checks are deleted; tests pass a `flowInputs` literal.

Deleted: `engineFlow.engine`; `newRuntimeDeps(engine)`; `Engine.nowFunc`, `since`, `workerPoolSize` if no caller remains; `buildRemoteObservationBatch`'s and `buildPrimaryWatchBatch`'s `*Engine` parameter.

Reads of immutable config vs mutable state, summarised: groups A, C and D are pure reads of values fixed at construction (about 120 of the 228 uses by the tables above; exact split not tallied). Group B is immutable pointers to collaborators with their own state. Group E is the mutable engine state; it is what needs owners rather than a config copy.

### 1.6 Tests that pin today's behaviour

- `engine_runtime_signals_test.go:30` `TestEngineFlow_OneShotDefaultsToNoRuntimeSignals`, `:55` `TestWatchRuntime_IsItsOwnSignalSinkAndInspector`, `:69` `TestWatchRuntime_MarkReplanNeededWithoutDirtyBufferIsSafe`.
- `engine_config_test.go:68` `TestNewMountEngine_PropagatesOrdinaryMountConfig`, `:101` `TestNewMountEngine_LoadsPersistedShortcutProtectedRoots`, `:174` `TestNewMountEngine_PropagatesRootedMountConfig`, `:207` `TestNewMountEngine_UsesMountConfigNotSessionResolvedFields`, `:276` `TestNewEngine_PropagatesPathConvergence`, `:325` `TestEngine_StoreAndExecutorTellTheEngineClocksTime`, `:342` `TestRefreshProtectedRootsFromStore_ConcurrentReadersAreRaceFree`.
- `engine_runtime_architecture_test.go:17, 100, 128, 167, 212` (`TestRuntimeArchitecture_*`; behavioural — the file imports neither `go/parser` nor `go/ast`).
- `engine_housekeeping_test.go:48` `TestPostSyncHousekeeping_SkipsProtectedShortcutRoots`, `:142` `TestPostSyncHousekeeping_SkipsTransferCleanupWhenProtectedRootsCannotBeRead`, `:199` `TestPurgeExpiredTrash_WarnsOncePerFailureStreak`.

Names VERIFIED by `grep "^func Test"`; bodies not read (SUSPECTED as to exactly what each asserts).

### 1.7 Tests to write first

1. `TestPostSyncHousekeeping_ReadsProtectedRootsUnderTheLock` (race tier): one goroutine loops `ShortcutChildAckHandle().AcknowledgeChildFinalDrain(...)`, another loops `postSyncHousekeeping`. Fails under `-race` today if F5 overlaps; passes after the read goes through `current()`. `// Validates: R-6.3.4`.
2. `TestEngine_EachRunGetsADistinctRunID` — pins what `nextRunID` provides before it becomes a constructor input.
3. `TestRunWatch_HousekeepingRunsBeforeTheFirstRefreshTimerIsArmed` — pins the order at `engine_watch.go:326-327` before `postSyncHousekeeping` moves to `housekeeper`.
4. No source-scanning guard is needed for the final state: once the field is gone the compiler enforces it.

### 1.8 Design-doc sections to update

`spec/design/sync-engine.md`: "Ownership Contract" (`:47-68`), "Construction" (`:131-217`), "Run State Is Split By Authority" (`:219-246`), "Run Capabilities Are Passed, Not Reached For" (`:261-279`, which still describes the 320/191 split), the "Verified By" table (`:70-107`), and the `GOVERNS:` line for the new `engine_observation.go`.

### 1.9 Risks; mechanical or not

- Groups A, C, D: purely mechanical.
- Group B: mechanical, but the construction site (`engine.go:198-249`) is two-phase today (`e.permHandler = newEnginePermissionHandler(e, …)` at `:244`, after the literal; `newEnginePermissionHandler` uses only `engine.baseline` `:261` and `engine.nowFunc` `:268`). Build the permission handler from the store and clock before the literal.
- Group E: not mechanical. `housekeeper` changes atomics to plain fields (depends on section 4's proof); `shortcutRoots` is entered from another goroutine (section 2.3), so its methods must touch only the store, the `protectedRootSet` and immutable config — which is what `acknowledgeChildFinalDrain` does today (`engine_shortcut_topology.go:39-77`). VERIFIED for that method only.
- The series touches nearly every `engine*.go` file; do not interleave it with the package split (section 10).

---

## 2. Observation methods that run off the loop

### 2.1 Goroutine starts and what runs on them

VERIFIED by reading each start site and the method bodies it reaches.

| # | Goroutine (start site) | Method bodies run on it | State read / written |
|---|---|---|---|
| G1 | full remote refresh, `go func()` `engine_watch_maintenance.go:131-136` | `(*watchRuntime).recoverFullRemoteRefreshPanic` `:147-163`, `performFullRemoteRefresh` `:165-208`, `finishFullRemoteRefresh` `:210-219`; through `:179` `(*engineFlow).executePrimaryRootObservation` (`engine_primary_root.go:106-116`) and, by root kind and the full flag, `executeDriveRootObservation` `:118-157`, `executeDriveRootFullObservation` `:190-218`, `executeMountRootObservation` `:220-253`; from those `observeRemoteWithShortcutTopology` (`engine_current_plan.go:318-348`), `observeRemoteFullWithShortcutTopology` (`engine_run_once.go:535-596`), `observeMountRootRemote` (`engine_mount_root.go:36-120`) | reads `rt.deps`, `rt.engine.collector()` (`:173-175`), `Engine` fetchers, `driveID`, `logger`, `baseline`, `shortcutNamespaceID`; **writes `Engine.protectedRoots`** through `refreshProtectedRootsFromStore` (`engine_run_once.go:544`, `engine_current_plan.go:332`); **writes `Engine.mountRootDeltaFallbackReported`** (`engine_mount_root.go:73, 93`); reads the loop-owned field `rt.resources.refreshResults` (`:212, :215`) |
| G2 | drive-root remote observer, `go func()` `engine_primary_root_watch.go:60-83` | `remoteObs.Watch` callback `:72-80` → `buildPrimaryWatchBatch(rt.engine, …)` `:74`; expired-delta recovery closure `:35-45` → `rt.executeDriveRootFullObservation` `:36` | as G1 without `refreshResults` |
| G3 | mount-root remote observer, `go func()` `engine_primary_root_watch.go:20-27` | `(*watchRuntime).watchMountRootRemote` (`engine_mount_root.go:308-339`), `handleMountRootObservationResult` `:341-370`, `handleMountRootPollError` `:383-400`, `dispatchMountRootBatch` `:402-418`, `sleepMountRootWatch` `:420-434`; through `:323` `executeMountRootObservation` → `observeMountRootRemote` | `rt.deps.logger`; `rt.engine.driveID` `:395`, `remoteRootItemID` `:396`; `mountRootDeltaFallbackReported` |
| G4 | local observer, `go func()` `engine_watch.go:426-434` | `(*watchRuntime).watchLocalWithFallback` `:449-494`, `runPeriodicFullScan` `:626-671`, `runFallbackScan` `:675-714` | `rt.engine.syncTree` `:450`; `rt.deps.now/since/emit/logger/clock` |
| G5 | Socket.IO wake source, `go func()` `engine_watch.go:532-552` and its inner `go func()` `:537-543` | `(*watchRuntime).runSocketIOWakeSource` `:563-583`; `emitSocketIOLifecycleEvent` `:585-620`, passed as `LifecycleHook` at `:528` | `rt.engine.driveID` `:548, :574`; `rt.deps`; reads the loop-owned field `rt.resources.socketIOWakeDone` to close it `:535` |
| G6 | timer goroutines (`AfterFunc`) | closures at `engine_watch_maintenance.go:104-109` (reads field `rt.resources.refreshCh`), `engine_results.go:38-41` (emit, `rt.kickRetryHeldReleaseNow` `:57-63` → send on `rt.timers.retryTimerCh`), `engine_results.go:252-258` (emit, send on `rt.timers.trialCh`), `engine_watch_state.go:478-483` (admit timer), `engine_watch.go:634-639` (local channel only), `engine_run_once_results.go:169` (closes a local channel) | channel fields only; no timer pointer, no ledger |
| G7 | the multisync orchestrator's event goroutine, through the ack handle returned by `Engine.ShortcutChildAckHandle()` (`engine_shortcut_topology.go:29-37`) | `(*Engine).acknowledgeChildFinalDrain` `:39-77` → `releaseShortcutRootProjectionAfterDrain` `:52`, `reconcileShortcutRootLocalState` `:55` (→ `baseline.replaceShortcutRoots` `shortcut_root_lifecycle.go:73`, `refreshProtectedRootsFromStore` `:76`); `acknowledgeChildArtifactsPurged` `:79-102` | `baseline`, `shortcutNamespaceID`, `syncRoot`, `contentFilter`, `shortcutChildWorkSink`; **writes `Engine.protectedRoots`** through the mutex |
| G8 | workers, `go wp.worker(ctx)` `worker.go:108`; closer `:111-114` | `workerPool` and `executor` methods | `executorConfig` (shared), `remoteParentChecks` under its mutex, the store; writes `ta.Cancel` (`worker.go:186`, a dead field, section 9.3). Not flow methods |

Other goroutines in the package that run no engine/flow method: `buffer.go:143` (`debounceLoop`), `scanner.go:268-274` (hash errgroup), `socketio_conn.go:107`. VERIFIED (start sites; bodies not all read).

### 2.2 G7 in detail (the writer the comments omit)

- The handle is held outside the engine: `parentAck: shortcutParentAckHandleForMount(mount, engine)` at `internal/multisync/orchestrator_watch.go:421` and `internal/multisync/orchestrator_run_once.go:239`. VERIFIED.
- In watch mode `(*Orchestrator).handleWatchRunnerEvent` (`internal/multisync/orchestrator_watch.go:647-695`) runs on the orchestrator's event goroutine; for a finished final-drain child it calls `handleFinalDrainWatchRunnerEvent` (`:743-783`), which passes `watchParentDrainAckers(runners)` (`:785-797`: the `parentAck` of every standalone runner still in the map, i.e. parents whose `RunWatch` is live on their own runner goroutines, `:427-468`) to the finalisation call at `:764-770`. The ack is invoked at `internal/multisync/orchestrator_run_once.go:413-417` inside `acknowledgeSuccessfulFinalDrains` (`:394`). VERIFIED, except the name of the intermediate function whose parameter list is at `orchestrator_run_once.go:282` (SUSPECTED to be `finalizeSuccessfulFinalDrainMounts`; settle by reading `:270-300`).
- So a parent engine's `protectedRoots` and shortcut-root rows are rewritten from the orchestrator goroutine while that engine's loop runs. The comments at `engine.go:63-67` and `protected_roots.go:12-15` name only "the full-refresh goroutine and … the remote observer's expired-delta recovery". VERIFIED.

### 2.3 Why it hurts

- G1–G5 hold `rt` (the whole `*watchRuntime`), so `rt.sched`, `rt.retries`, `rt.scopes`, `rt.loop`, `rt.timers` are one selector away on a goroutine that must not touch them. None of the bodies listed in 2.1 touches them today (VERIFIED by reading them), but section 4 wants to delete the locks that would currently hide such a mistake.
- Three loop-owned fields are read off-loop: `rt.resources.refreshResults`, `refreshCh`, `socketIOWakeDone`. Cleanup deliberately leaves the first usable (`engine_watch.go:246-252` nils seven fields, not `refreshResults`; pinned by `TestWatchPipelineCleanup_KeepsRefreshResultChannelUsable`, `engine_refresh_test.go:744`). The correctness rests on a field that must never be reassigned. VERIFIED (code); the test body was not read.
- F5: `engine_housekeeping.go:26` passes `flow.engine.protectedRoots` straight to `transferArtifactCleanupSkipDirs`, against the rule at `engine.go:63-67`. Its two callers are `engine_run_once.go:99` and `engine_watch.go:326` (bootstrap, before `startObservers` `:100` and before the refresh timer is armed `:327`), so G1–G3 do not exist yet; G7 does. The read is VERIFIED; a real overlap with a G7 write is SUSPECTED (settle: test 1 in section 1.7).
- F6: `startObservers` (`engine_watch.go:337-414`) starts the remote observer at `:358`, then does two fallible steps — `rt.engine.baseline.newLocalHashCache(ctx)` `:365` (`return err` `:366-368`) and `listMountBoundaries(ctx)` `:374` (`return err` `:375-377`) — and only at `:407-412` records `observerErrs`/`activeObservers`. On either error `RunWatch` returns it (`:100-105`); `pipe.cleanup` (`:222-255`) neither cancels the context nor waits. The remote observer keeps running until the caller's context ends (in production `mountCtx`, cancelled by `defer mountCancel()` at `internal/multisync/orchestrator_watch.go:429`). VERIFIED.
- F7: `obsWg` is created at `engine_watch.go:351`, `Add`ed at `:355, :401`, `Done`d at `engine_watch.go:427` and `engine_primary_root_watch.go:21, :61`, and never `Wait`ed (no `.Wait()` in either file). `RunWatch` therefore returns while observers may still be writing the store. VERIFIED.

### 2.4 Proposed end state

Proposal. One type per concern, holding only what observation may touch.

```go
// engine_observation.go (package sync; after the split it stays in sync — it wires
// syncobserve observers to the store, which syncobserve may not import, end-state.md:24-59)
type observation struct {
    logger *slog.Logger
    store  *SyncStore
    clock  syncClock
    events func(engineDebugEvent)
    perf   *perf.Collector

    driveID                driveid.ID
    remoteRootItemID       string
    remoteRootDeltaCapable bool
    shortcutNamespaceID    string
    enableWebsocket        bool
    contentFilter          ContentFilterConfig
    localRules             LocalObservationRules
    checkWorkers           int
    syncTree               *synctree.Root
    syncRootIdentity       *syncRootIdentity

    fetcher         deltaFetcher
    itemsClient     itemClient
    folderDelta     folderDeltaFetcher
    recursiveLister recursiveLister
    socketIOFetcher socketIOEndpointFetcher

    roots *protectedRootSet                        // section 1.5
    mountRootDeltaFallbackReported atomic.Bool     // moved from Engine (G1 and G3 both write it)
    hooks observationHooks                         // localWatcherFactory, socketIOWakeSourceFactory (item 8)
}

func newObservation(in observationInputs) *observation

// One-shot and loop-side use (called on the loop goroutine):
func (o *observation) primaryRoot(ctx context.Context, bl *Baseline, full bool) (remoteObservationBatch, error)
func (o *observation) local(ctx context.Context, bl *Baseline) (…)        // today's observeLocal
func (o *observation) commitObserved(ctx context.Context, items []observedItem, token string) error

// Watch-side:
type observerOutputs struct {          // send-only: an observer cannot read loop state through these
    remoteBatches       chan<- remoteObservationBatch
    localBatches        chan<- localObservationBatch
    protectedRootEvents chan<- protectedRootEvent
    skipped             chan<- …       // element type of watchResources.skippedItems
    errs                chan<- error
    refreshResults      chan<- remoteObservationBatch
}
type runningObservers struct { cancel context.CancelFunc; wg stdsync.WaitGroup; count int; remote *remoteObserver; local *localObserver }

func (o *observation) startWatch(ctx context.Context, bl *Baseline, out observerOutputs, opts WatchOptions) (*runningObservers, error)
func (r *runningObservers) stop()      // cancel, then wg.Wait()
func (o *observation) startFullRefresh(ctx context.Context, bl *Baseline, out chan<- remoteObservationBatch)
```

What moves onto `observation` (current receiver → new):

- `(*engineFlow)`: `executePrimaryRootObservation`, `executeDriveRootObservation`, `executeDriveRootFullObservation`, `executeMountRootObservation` (`engine_primary_root.go:106-253`); `observeRemoteWithShortcutTopology`, `observeLocal` (`engine_current_plan.go:318-392`); `observeRemoteFullWithShortcutTopology` (`engine_run_once.go:535-596`); `observeMountRootRemote`, `commitObservedItems` (`engine_mount_root.go:36-120, 294-306`).
- `(*Engine)`: `hasRemoteMountRoot`, `mountRootDeltaSupported`, `mountRootEvents`, `confirmMountRootOrphans`, `confirmEnumerationOrphans`, `liveItemLeftMount` (`engine_mount_root.go:15-292`), `preferredMountRootObservationMode` (`engine_primary_root.go:98-104`), `remotePathLookup` (`engine_remote_path_lookup.go:11`).
- `(*watchRuntime)`: `startPrimaryRootWatch`, `warnMountRootWebsocketFallback` (`engine_primary_root_watch.go:10-101`); `watchMountRootRemote` and its four helpers (`engine_mount_root.go:308-434`); `startLocalObserver`, `watchLocalWithFallback`, `startRemoteObserver`, `startSocketIOWakeSource`, `runSocketIOWakeSource`, `emitSocketIOLifecycleEvent`, `runPeriodicFullScan`, `runFallbackScan` (`engine_watch.go:418-714`); `performFullRemoteRefresh`, `finishFullRemoteRefresh`, `recoverFullRemoteRefreshPanic` (`engine_watch_maintenance.go:147-219`).
- Free functions `buildRemoteObservationBatch`, `buildPrimaryWatchBatch` (`engine_primary_root.go:40-96`) take a logger instead of `*Engine`.

What stays on the loop: the `refreshActive` gate (`engine_watch_maintenance.go:124-129`), `applyRemoteRefreshResult` `:221-231`, `armFullRefreshTimer` `:94-117`, `activeObservers` counting, the dirty buffer, all `handleWatch…Signal` methods.

`startWatch` fixes F6 and F7: it does every fallible local step first (hash cache, mount boundaries), starts goroutines only after they succeed, derives its own cancellable context, and `pipe.cleanup` calls `stop()`. The existing model for "a goroutine-side helper that holds only what it needs" is `recoverObserverPanic(logger, observer, errs)` (`engine_watch_panic_recovery.go:28-57`).

Deleted: the off-loop reads of `rt.resources.refreshResults`, `refreshCh` (the timer closure gets the channel by value), `socketIOWakeDone`; `Engine.mountRootDeltaFallbackReported`.

### 2.5 Tests that pin today's behaviour

`engine_refresh_test.go:88, 143, 293, 442, 510, 586, 650, 677, 711, 744, 767, 845, 914, 980, 1019, 1076` (observation and full-refresh hand-off; e.g. `:711` `TestRunFullRemoteRefreshAsync_SkipsIfRunning`, `:845` `TestWatchLoop_RefreshTick_RunsPeriodicFullRemoteRefreshThroughResultHandoff`, `:914` `TestRunFullRemoteRefreshAsync_ShutdownAfterCommit`); `engine_watch_panic_recovery_test.go:21, 43, 63, 87, 118, 158`; `engine_config_test.go:342`; `panic_stack_test.go:20` `TestPanicRecoverySitesCaptureTheStack` (scans directories for `recover()` sites — the two recover sites move file, `end-state.md:478-510`); `engine_watch_rescan_replan_test.go:24, 110`. Names VERIFIED; bodies not read.

### 2.6 Tests to write first

1. `TestStartObservers_LocalSetupFailureStartsNoObserver` — make `newLocalHashCache` (or `listMountBoundaries`) fail through the store fault seam (`store_fault_seam_test.go:246, 288, 321` show one exists; its API was not read) and assert no observer goroutine was started (fake fetcher records zero calls after the error). Fails today.
2. `TestRunWatch_ReturnsOnlyAfterObserversHaveExited` — a fake remote fetcher that blocks until its context ends and then records a timestamp; assert `RunWatch` returns after it. Fails today (F7).
3. `TestAckHandle_ConcurrentWithFullRefresh_IsRaceFree` (race tier) — pins that G1 and G7 may both rewrite protected roots.
4. `TestObservation_HoldsNoRunState` is not a test: the type has no such field; the compiler enforces it.

### 2.7 Design docs; risks; mechanical or not

- `spec/design/sync-engine.md`: "Watch Mode" (`:556-779`), "Observer Panics Fail One Mount" (`:902-948`), "Ownership Contract" (`:47-68`); `spec/design/sync-execution.md` is unaffected.
- Moving the methods is mechanical. Fixing F6/F7 is a behaviour change: `stop()` waits, so an observer that ignores its context would now hang shutdown where today it is abandoned. Whether every observer send is context-aware during drain was not read (section 11); settle before making `stop()` unconditional, or bound the wait and log (precedent: `outcomeCommitTimeout`, `worker.go:37`).

---

## 3. Scheduler, dependency graph and ledgers

### 3.1 What exists

All VERIFIED. Owner goroutine of every structure below: the loop goroutine (one-shot: everything between `newWorkerPool` and `pool.Stop()` at `engine_run_once.go:354-365` runs on the `RunOnce` goroutine; watch: section 0). The header comment says so: "each is written and read by the single runtime goroutine that owns the run" (`engine_runtime_owners.go:8-16`).

| Structure | Declaration | State | API (callers are all flow methods) |
|---|---|---|---|
| `depGraph` | `dep_graph.go:26-32`; `trackedNode` `:13-17` | `mu`, `actions map[int64]*trackedNode`, `logger`; per node `depsLeft atomic.Int32`, `dependents` | `Register` `:93-110` (caller `engine_runtime_start.go:99`), `WireDeps` `:116-154` (`engine_runtime_start.go:108`), `Complete` `:195-229` (only `engine_runtime_graph.go:13`), `Get` `:176-186` (only `engine_runtime_owners.go:110`), `InFlightCount` `:233-238` (`engine_runtime_start.go:41`, `engine_watch_loop.go:656`, `engine_watch_drain.go:16`, `engine_watch.go:235`); `Add` `:51-86` has no production caller (tests only); `MarkTrial` `:159-171` has no caller at all |
| `actionScheduler` | `engine_runtime_owners.go:20-35` (9 fields) | graph, `dispatchCh`, `nextActionID`, `queuedByID`, `runningByID`, `runningCount`, cold-start ramps | `trackedActionForCompletion`, `markQueued`, `markRunning`, `markFinished`, `allocatePlanActionIDs` (`:105-174`); `resetColdStart`, `coldStartRampFor`, `coldStartAdmits`, `noteColdStartSuccess`, `handOverColdStart` (`engine_runtime_cold_start.go:60-156`); `nextDispatchable`, `markDispatched` (`:206-238`) |
| `retryLedger` | `engine_runtime_owners.go:46-51` | `rowsByKey` (copy of `retry_work`, section 5), `heldByKey`, `heldByScope`, `nextHeldOrder` | `releaseHeldAction`, `earliestHeldRetryAt`, `applyPersistedRetryAdmission` (`:176-244`); flow methods `holdAction`, `hasDueHeldWork`, `dueRetryKeys`, `dueTrialKeys` (`engine_runtime_workset.go:42-131`), `heldScopeBatch` (`engine_runtime_release_ramp.go:148-160`) |
| `scopeLedger` | `engine_runtime_owners.go:62-70` | `mu`, `active []activeScope`, `state *scopeState` | eight methods `:246-327`, each a locked call of a pure helper in `active_scopes.go` (`findBlockingScope` `:17-44`, `upsertScope` `:67-83`, `removeScope` `:86-98`, `hasScope`, `lookupScope`, `extendScopeTrial` `:119-137`, `dueTrials`, `earliestTrialAt`, `scopeKeys`) |
| `scopeState` | `scope.go:45-49` | sliding windows per scope key | `UpdateScope` `:70-113`, `RecordSuccess` `:124-127`. Comment `:41-44`: "all calls come from applyRuntimeCompletionStage on one goroutine" |
| release ramp | `engine_runtime_release_ramp.go:21-78` | none (pure) | `nextReleaseAllowance(transferWorkers, lastAllowance, slowStartThreshold int) int` `:46-62`, `firstReleaseAllowance`, `slowStartThresholdAfterThrottle` |
| `runResults` | `engine_runtime_owners.go:73-87` | counters, errors, summaries | flow methods in `engine_results.go:75-224` |
| the in-flight "ledger" | not a separate type | `runningByID`/`runningCount` in the scheduler plus the graph's unresolved nodes | `InFlightCount`, `markRunning`, `markFinished` |

Workers never touch any of it: a worker sends an `actionCompletion` and the loop calls `Complete` (`worker.go:42-43, 173, 182, 234`; pinned by `TestWorker_NeverCallsComplete`, `worker_test.go:663`). VERIFIED.

### 3.2 Why it hurts

- The four "owners" are value fields of `engineFlow`; their maps are read and written directly from 22 files of flow methods (for example `flow.retries.heldByKey[key] = held` at `engine_runtime_workset.go:58`, `flow.retries.rowsByKey[…] = *row` at `engine_runtime_completion.go:638`). The split by authority is documentation. VERIFIED.
- Tests reach into the internals (`rt.sched.graph.Add(…)`, `rt.retries.heldByKey`; `engine_retry_trial_test.go:12-34`), so any representation change breaks tests that are not about the representation. VERIFIED for that file.
- The graph is replaced on every plan install (`engine_runtime_start.go:24`) while `pipe.cleanup` holds the first one (`depGraph := newDepGraph(…)` `engine_watch.go:179`, read at `:235`). Actions are registered only on the replacement (`engine_runtime_start.go:99`), so the "graceful shutdown: draining in-flight actions" log (`engine_watch.go:236-240`) reads a graph that never holds an action after the first install. VERIFIED. Effect: one log line that cannot fire; it shows what a shared mutable handle costs.
- The split plan puts `dep_graph.go`, `active_scopes.go` and `scope.go` in `syncexec` ("Executor, worker pool, dependency graph, scope state", `end-state.md:24-59, 577-579`), i.e. in the worker package, although no worker uses them. VERIFIED.

### 3.3 Proposed end state

Proposal. One package, `internal/sync/internal/syncsched`: pure decisions and bookkeeping for one run. It imports only `syncdomain`; it has no store, no timers, no channels, no goroutines, no locks. The engine loop applies the effects (store writes, timers, dispatch).

```go
package syncsched

type Scheduler struct { /* graph, queued/running sets, cold-start ramps, held actions, active scopes, scope windows */ }

func New(now func() time.Time, logger *slog.Logger) *Scheduler

// Plan install (replaces initializeRuntimeState + registerPlanActions + newDepGraph):
func (s *Scheduler) Install(actions []syncdomain.Action, deps [][]int, poolSize int, scopes []syncdomain.ActiveScope) (ready []*syncdomain.TrackedAction)

// Graph:
func (s *Scheduler) Tracked(id int64) (*syncdomain.TrackedAction, bool)
func (s *Scheduler) Complete(id int64) (ready []*syncdomain.TrackedAction, ok bool)
func (s *Scheduler) InFlight() int

// Dispatch bookkeeping and cold start:
func (s *Scheduler) Queued(ta *syncdomain.TrackedAction)
func (s *Scheduler) Running(ta *syncdomain.TrackedAction)
func (s *Scheduler) Finished(ta *syncdomain.TrackedAction) (wasRunning bool)
func (s *Scheduler) RunningCount() int
func (s *Scheduler) NextDispatchable(outbox []*syncdomain.TrackedAction, now time.Time) (index int, wakeAt time.Time)
func (s *Scheduler) NoteSuccess(ta *syncdomain.TrackedAction)
func (s *Scheduler) HandOverColdStart(key syncdomain.ScopeKey) []string

// Admission decision (pure; the caller supplies the durable row, section 5):
func (s *Scheduler) Admit(ta *syncdomain.TrackedAction, row *RetryRow, now time.Time) Admission

// Held work:
func (s *Scheduler) Hold(ta *syncdomain.TrackedAction, reason HeldReason, scope syncdomain.ScopeKey, retryAt time.Time)
func (s *Scheduler) Release(key syncdomain.RetryWorkKey) *syncdomain.TrackedAction
func (s *Scheduler) DueRetries(now time.Time) []syncdomain.RetryWorkKey
func (s *Scheduler) DueTrials(now time.Time) []syncdomain.RetryWorkKey
func (s *Scheduler) HeldBatch(scope syncdomain.ScopeKey, n int) []syncdomain.RetryWorkKey
func (s *Scheduler) NextWake() (retryAt, trialAt time.Time)

// Scopes:
func (s *Scheduler) Scope(key syncdomain.ScopeKey) (syncdomain.ActiveScope, bool)
func (s *Scheduler) Scopes() []syncdomain.ActiveScope
func (s *Scheduler) PutScope(scope syncdomain.ActiveScope)
func (s *Scheduler) DropScope(key syncdomain.ScopeKey)
func (s *Scheduler) BlockingScope(ta *syncdomain.TrackedAction) syncdomain.ScopeKey
func (s *Scheduler) ObserveFailure(r *syncdomain.ActionCompletion) syncdomain.ScopeUpdateResult
func (s *Scheduler) ObserveSuccess()

// Pure ramp arithmetic:
func NextReleaseAllowance(transferWorkers, lastAllowance, slowStartThreshold int) int
```

Files that move: `dep_graph.go`, `active_scopes.go`, `scope.go`; the scheduler, held-action and scope parts of `engine_runtime_owners.go`; `engine_runtime_cold_start.go`; the pure half of `engine_runtime_release_ramp.go` (`:21-78`); `holdAction`, `hasDueHeldWork`, `dueRetryKeys`, `dueTrialKeys` from `engine_runtime_workset.go`; `decideAdmission`'s pure part (`engine_runtime_admission.go:91-144`). What the loop still does itself: every store call, every timer, `dispatchCh`, `runResults`, invariants.

What the engine loop calls, per event: plan installed → `Install`; action ready → `Admit` then `Queued`/`Hold`; dispatched → `Running`; completion → `Tracked`, `Finished`, `ObserveFailure`/`ObserveSuccess`, `Complete`; timer fired → `DueRetries`/`DueTrials`, `Release`; scope change → `PutScope`/`DropScope`/`HeldBatch`; arming timers → `NextWake`; shutdown → `InFlight`.

Types it needs from `syncdomain` after the split: `trackedAction` (`tracked_action.go:12`), `activeScope`, `BlockScope`, `scopeUpdateResult` (`scope_block.go:23, 10, 84`), `ScopeKey` (`scope_key.go`), `RetryWorkKey` (`retry_work_key.go:10`), `actionCompletion` (`worker_result.go:13`) — all assigned to `syncdomain` by `end-state.md:544-551`. VERIFIED (assignments). Where `RetryWorkRow` (`store_types.go:101`) lands was not checked (SUSPECTED `syncstore`); `Admit` therefore takes its own three-field `RetryRow{Blocked bool; ScopeKey; NextRetryAt}` so the package does not import the store.

Deleted: `depGraph.mu`, `depsLeft` as an atomic, `scopeLedger.mu` (section 4); `depGraph.Add` (tests use `Install`), `depGraph.MarkTrial`; the stale capture at `engine_watch.go:179/235`; `rt.sched.graph = depGraph` at `engine_watch.go:180`.

### 3.4 Tests that pin today's behaviour

`dep_graph_test.go:20, 39, 64, 105, 139, 157, 268, 290, 360` (and three concurrency tests, section 4.4); `active_scopes_test.go:26-231` (eleven tests); `scope_test.go:15-546`; `engine_cold_start_test.go:233, 268, 308, 517, 536`; `engine_release_jitter_test.go:65, 139, 145`; `engine_runtime_release_ramp_test.go:19, 52, 148, 213, 315, 363, 413, 444`; `engine_runtime_start_test.go:12`; `engine_throttle_ramp_test.go:283`; `engine_supersede_streak_test.go:15, 44, 58, 91`; `worker_test.go:663, 870, 329`; `engine_run_once_results_test.go:16-351`. Names VERIFIED; bodies not read.

### 3.5 Tests to write first

1. Characterisation tests through the *flow's* public behaviour for every case the internal-reaching tests cover (hold → due → release → admit), so the internals can move: e.g. rewrite `engine_retry_trial_test.go:12` to assert the released action reaches the outbox instead of asserting `rt.retries.heldByKey` is empty.
2. `TestScheduler_InstallReplacesEveryPerPlanStructure` — pins what `initializeRuntimeState` resets (`engine_runtime_workset.go:8-40`).
3. A table test for `Admit` covering the three branches of `applyPersistedRetryAdmission` (`engine_runtime_owners.go:227-243`) and `applyActiveScopeAdmission` (`engine_runtime_admission.go:126-144`).

### 3.6 Design docs; risks; mechanical or not

- `spec/design/sync-execution.md`: its `GOVERNS:` line lists `active_scopes.go`, `dep_graph.go`, `scope.go`; "Worker And Dependency Model" (`:60-257`); "Scope Helpers" (`:659-669`, which already says `active_scopes.go` "is no longer a separate runtime subsystem"). `spec/design/sync-engine.md`: "Run State Is Split By Authority" (`:219-246`), "Outcome And Scope Lifecycle" (`:997-1017`), "Retry and trial release" (`:1115-1149`), "Release ramps" (`:1151-1230`), "Cold start" (`:1232-1294`). `end-state.md`: the `syncexec` file list (`:577-579`) and the `sync → syncexec` edge inventory (`:82`: "`depGraph`, `scopeState` … and their constructors").
- Not mechanical: the API replaces direct map access at roughly 60 sites, and tests that reach into internals must be rewritten first.
- Hard ordering constraint: the package cannot exist before `syncdomain` does, because `sync` would import `syncsched` while `syncsched` needs `trackedAction`, `ScopeKey` etc. that today live in package `sync` (import cycle). VERIFIED (type locations above).

---

## 4. Vestigial locks

### 4.1 Every lock and atomic in production code of `internal/sync`

VERIFIED by `grep` for `sync.Mutex|RWMutex|atomic\.|WaitGroup|Once` over non-test files, then reading the users of each engine-side one.

| Primitive | Guards | Goroutines that touch the state | Verdict |
|---|---|---|---|
| `scopeLedger.mu` (`engine_runtime_owners.go:65`, `RWMutex`) | `scopeLedger.active` | loop only. Callers of the eight methods: `engine_runtime_admission.go:134, 235`; `engine_runtime_completion.go:49, 312, 659`; `engine_runtime_lifecycle.go:30, 116, 133, 167, 205, 282, 313, 339, 409, 555, 558`; `engine_watch_state.go:409`; `engine_runtime_permissions.go:109`; `engine_runtime_release_ramp.go:168, 173, 192, 203, 236`; `engine_run_once.go:160`; `engine_runtime_invariants.go:106, 269, 297`; `engine_runtime_workset.go:30, 74, 109`. None is in a method listed in section 2.1 | **vestigial** |
| `watchTimerState.timerMu` (`engine_watch_state.go:63`, `RWMutex`) | per its comment `:61-62`, `trialTimer` and `retryTimer`; in fact also `admitTimer` (`:463-495`) and another group's `rt.resources.refreshTimer` (`:503-513`) | loop / `RunWatch` goroutine only: `engine_watch_state.go:422-513` accessors are called from flow signals (`engine_runtime_lifecycle.go:199, 223, 294, 410, 512`; `engine_runtime_permissions.go:70, 90, 173`; `engine_runtime_admission.go:177`; `engine_runtime_completion.go:395, 398, 782`; `engine_runtime_release_ramp.go:271`), loop methods (`engine_retry_trial.go:28, 50`; `engine_watch_loop.go:540, 553`), init and cleanup (`engine_watch.go:206, 211, 233, 242-244`), drain (`engine_watch_drain.go:13, 26-29`), `engine_watch_maintenance.go:104`, invariants (`engine_runtime_invariants.go:118, 121, 124`). The timer callbacks themselves touch only channels (section 2.1, G6) | **vestigial** |
| `depGraph.mu` (`dep_graph.go:29`) and `trackedNode.depsLeft atomic.Int32` (`:15`) | `depGraph.actions`; the per-node counter | loop only (callers in section 3.1). The comment `:27-28` says "The engine adds and completes nodes while workers report outcomes" — workers report through a channel and never call the graph | **vestigial** |
| `Engine.trashPurgedAt` (`engine.go:59`), `trashPurgeFailing` (`:133`) | purge schedule and warn-once flag | loop only: `purgeExpiredTrash` has two callers, `engine_watch_maintenance.go:14` and `engine_housekeeping.go:32` | **vestigial atomic** |
| `Engine.protectedRootRefreshFailing` (`engine.go:132`) | warn-once flag | loop only: `refreshProtectedRootsForHousekeeping`'s one caller is `engine_housekeeping.go:20` | **vestigial atomic** |
| `Engine.nextRunID` (`engine.go:118`) | run counter | one caller, `engine_runtime_types.go:54`, on the goroutine that starts the run | **vestigial atomic** |
| `Engine.protectedRootsMu` (`engine.go:68`) | `protectedRoots` | loop, G1, G2, G7 (section 2) | needed |
| `Engine.mountRootDeltaFallbackReported` (`engine.go:125`) | report-once flag | loop, G1, G3 | needed |
| `remoteParentChecks.mu` (`executor_parent_checks.go:20`) | `visible`, `folders` | workers (`executor.go:166, 224, 233`) and the loop's per-plan `reset` (`engine_runtime_start.go:92`) | needed |
| `workerPool.wg`, `closeOnce` (`worker.go:62-63`) | worker lifetime | pool | needed |
| `obsWg` (`engine_watch.go:351`) | nothing (never waited, F7) | — | dead as written; made real by section 2.4 |
| Outside the engine files: `Baseline.mu` (`core_types.go:125`), `SyncStore.baselineMu`/`contentDriveMu` (`store.go:68, 77`), `syncRootIdentity` (`observation_root_identity.go:78, 84, 87`), `dirtyBuffer.mu` (`buffer.go:62`), observers and scanner (`observer_remote.go:74, 79, 101-104`; `observer_local.go:506-508`; `scanner.go:136, 263, 405`; `scanner_hash_cache.go:56`), `permissionHandler.rootInaccessibleReported` (`permission_handler.go:33`), `debug_event_sink.go:18` | cross-goroutine state | several | not examined beyond their declarations; not claimed vestigial |

The one-engine-one-run fact the atomics rely on: `startWatchRunner` creates the engine (`internal/multisync/orchestrator_watch.go:403`) and its single runner goroutine calls either `engine.RunOnce` (`:504`) or `engine.RunWatch` (`:508`); `buildEngineWork` creates one engine per mount (`internal/multisync/orchestrator_run_once.go:218`) and calls `RunOnce` once (`:240-248`). G7 does not touch these four fields. VERIFIED.

The comment on `scopeLedger.mu` — "the one field here read outside the runtime goroutine, by admission checks and status snapshots" (`engine_runtime_owners.go:63-64`) — describes readers that do not exist: admission runs on the loop (`engine_runtime_admission.go:31-42`), and no caller outside the flow takes a snapshot (`snapshotActiveScopes` callers: `engine_watch_state.go:409`, `engine_runtime_workset.go:74, 109`, `engine_runtime_invariants.go:106`). VERIFIED.

### 4.2 Why it hurts

- A lock that guards nothing states a concurrency contract that is false; the next change is designed around readers that do not exist, and the lock hides an accidental off-loop access from the race detector (the one tool that would report it).
- `timerMu` already guards a field of a different state group (`engine_watch_state.go:503-513`) and a field its comment does not name (`admitTimer`), against the project rule that a mutex comment names the fields it guards. VERIFIED.
- `RWMutex` where the project says prefer `Mutex` (two of the three).

### 4.3 Proposed end state and how to prove it

Proposal.

1. Delete `scopeLedger.mu`, `timerMu`, `depGraph.mu`; make `depsLeft` an `int32`; make the four `Engine` atomics plain fields (they move to `housekeeper` and `flowInputs.runID` in section 1.5). Fix the three comments (`engine_runtime_owners.go:63-64`, `dep_graph.go:27-28`, `engine.go:127-131`).
2. Proof by the race detector: with the locks gone, any access from a second goroutine is a reportable data race. Run `-race` over the suites that drive these structures through real goroutines: `engine_watch_test.go`, `engine_watch_loop_owner_test.go`, `engine_watch_handlers_test.go`, `engine_refresh_test.go`, `engine_watch_panic_recovery_test.go`, `engine_runtime_lifecycle_test.go`, `engine_runtime_release_ramp_test.go`, `engine_cold_start_test.go`, `engine_throttle_ramp_test.go`, `engine_run_once*_test.go`, `worker_test.go`, `crash_points_test.go`, plus the stress lane over `./internal/sync/...` (`end-state.md:478-510`). The nightly full tier already runs the race detector.
3. A test-only ownership assertion, so a violation fails deterministically rather than when the scheduler happens to interleave: one hook `ownerCheck func()` in the hooks struct (item 8), nil in production, called at the top of each scheduler/ledger/timer accessor. The test helper installs a closure that captures the goroutine that started the run and fails the test when called from another. The goroutine-identity trick lives in `_test.go`; production code holds a nil func.
4. A devtool rule (optional, after item 3): no function literal passed to `go` or to `AfterFunc` may contain a selector on the scheduler value. After items 2 and 3 this is a two-type AST check; before them it is not worth writing, because `rt` is captured everywhere.

### 4.4 Tests

- Pin today: `dep_graph_test.go:173` `TestDepGraph_ConcurrentComplete`, `:215` `TestDepGraph_ConcurrentAddAndComplete`, `:323` `TestDepGraph_ConcurrentMultiAdd` exercise concurrency production does not have; they must be deleted in the same PR (they are the only thing the mutex serves). `worker_test.go:663` `TestWorker_NeverCallsComplete` is the test that makes the deletion safe. `engine_config_test.go:342` pins the lock that stays.
- Write first: `TestWatchRuntime_TimerCallbacksTouchOnlyChannels` (fire each timer with a manual clock while the loop is parked; under `-race` with `timerMu` removed it must stay clean); the `ownerCheck` helper and one test per structure that proves the helper fires (call a ledger method from a second goroutine, expect failure).

### 4.5 Design docs; risks; mechanical or not

- `spec/design/sync-engine.md` "Ownership Contract" (`:47-68`), "Run State Is Split By Authority" (`:219-246`), "Watch State Is Grouped And Qualified" (`:248-259`); `spec/design/sync-execution.md` "Worker And Dependency Model" (`:60-257`).
- Mechanical deletion. Risk: a test (not production) that reads these structures from the test goroutine while a loop goroutine runs would start failing under `-race`; `engine_observation_postprocess_test.go:442` and `engine_runtime_lifecycle_test.go:277` call `snapshotActiveScopes` — whether a loop is running at that moment is SUSPECTED (settle by reading the two tests).

---

## 5. The retry ledger: two sources of truth

### 5.1 Current structure

```go
// engine_runtime_owners.go:44-51
// retryLedger owns exact held work: the durable retry rows loaded for this run,
// the actions currently held, and their release ordering.
type retryLedger struct {
    rowsByKey     map[RetryWorkKey]RetryWorkRow
    heldByKey     map[RetryWorkKey]*heldAction
    heldByScope   map[ScopeKey][]RetryWorkKey
    nextHeldOrder uint64
}
```

- Durable truth: table `retry_work` (`store_retry_work.go:11-44`). VERIFIED.
- In-memory copy: `rowsByKey`, rebuilt on every plan install from `runtime.RetryRows` (`engine_runtime_workset.go:9-13`), which `reconcileRuntimeStateStage` fills from `store.ListRetryWork` (`engine_current_plan.go:674`, `:710`). VERIFIED.
- `heldByKey`/`heldByScope` are run-only state (actions waiting in this process); they are not a copy of anything. Two tests pin that a durable row without a held entry is not released: `TestReleaseDueHeldRetriesNow_DoesNotConsultDurableRetryRowsWithoutHeldRuntimeEntry` (`engine_retry_trial_test.go:35`, body read) and its trial twin (`:120`). VERIFIED.

**Writes to the copy — nine hand-written mirror sites** (VERIFIED):

| Site | After store call | Mirror |
|---|---|---|
| `engine_runtime_lifecycle.go:443` | `RecordBlockedRetryWork` `:431` | `rowsByKey[row.WorkKey()] = *row` |
| `engine_runtime_lifecycle.go:459-461` | `ClearBlockedRetryWork` `:456` | conditional `delete` |
| `engine_retry_trial.go:124` | `ResolveRetryWork` `:116` | `delete`, only if the store had the row |
| `engine_runtime_admission.go:193-195` | (via `clearBlockedRetryWorkForScope` `:190`) | the same conditional `delete` a second time |
| `engine_runtime_completion.go:638` | `RecordBlockedRetryWork` `:606` or `RecordRetryWorkFailure` `:615` | assign |
| `engine_runtime_completion.go:779-781` | `RecordRetryWorkFailure` `:772` | assign, guarded by `if flow.retries.rowsByKey != nil` |
| `engine_runtime_permissions.go:310` | `RecordRetryWorkFailure` `:299` | assign |
| `engine_runtime_permissions.go:340` | `RecordBlockedRetryWork` `:333` | assign |
| `engine_runtime_release_ramp.go:223-227` | `ReleaseScopeWork` `:212` | hand edit: `row.Blocked = false; row.NextRetryAt = now.UnixNano()` |

**Reads of the copy that drive decisions** (VERIFIED):

- Admission: `row, ok := l.rowsByKey[decision.RetryWorkKey]` then hold-under-scope or hold-until (`engine_runtime_owners.go:227-243`).
- Holding after a failure: `row, ok := flow.retries.rowsByKey[work]`; missing → error "missing retry_work row" (`engine_runtime_lifecycle.go:481-484`); `row.Blocked`/`row.NextRetryAt` decide the hold (`:485-494`).
- A store write skipped on the copy's word: `if ok && row.Blocked && row.ScopeKey == decision.ScopeKey { return nil }` (`engine_runtime_admission.go:209-212`); the "old scope" to discard is also taken from the cached row (`:229`).

**Store mutations of `retry_work` that return nothing to mirror from** (VERIFIED): `ReleaseScopeWork` returns only `scopeRemoved bool` (`store_scope_admin.go:17-54`); `DiscardScope` returns only `error` (`:62-92`); `ClearBlockedRetryWork` (`store_retry_work.go:310-339`); `PruneRetryWorkToCurrentActions` (`:413-438`, followed by a reload); `ReleaseOrphanBlockedRetryWork` (`:122-139`, returns a count).

### 5.2 Where the two can diverge (quoted paths)

**D1 — resolve finds no store row.** `engine_retry_trial.go:116-125`:

```go
row, found, err := flow.deps.store.ResolveRetryWork(ctx, work)
…
if !found || row == nil {
    return nil
}
delete(flow.retries.rowsByKey, work)
flow.retries.releaseHeldAction(work)
```

If the store has no row but the copy (or the held map) does, the copy survives the action's success. VERIFIED.

**D2 — scope release.** The store flips only rows still blocked under the scope (`store_retry_work.go:106-109`: `… WHERE path = ? AND old_path = ? AND action_type = ? AND scope_key = ? AND blocked = 1`; comment `:94-96` "A key that is not blocked under the scope any more is left alone"). The engine then edits the copy for every key that had a held action, unconditionally, and skips keys with no held action although the store just changed them (`engine_runtime_release_ramp.go:219-227`: `ta := flow.retries.releaseHeldAction(work); if ta == nil { continue }` then the hand edit). The store returns no rows to check against. VERIFIED.

**D3 — scope discard deletes rows the copy keeps.** `DiscardScope` runs `deleteRetryWorkByScopeTx` (`store_scope_admin.go:83`), which is `DELETE FROM retry_work WHERE scope_key = ?` (`store_retry_work.go:86-92`) — every row carrying the key, blocked or not. Its comment says "blocked retry work is deleted" (`store_scope_admin.go:56-61`), and so does the engine's (`engine_runtime_lifecycle.go:402-403`). Neither engine discard path removes anything from `rowsByKey`, `heldByKey` or `heldByScope`: `discardScope` (`engine_runtime_lifecycle.go:404-420`) and the inline second copy of its body in `transitionTrialScopeToPersistedBlock` (`:336-344`, which also omits `armTrialTimer` and `mustAssertDiscardedScope`). VERIFIED.

Non-blocked rows that carry a scope key exist by two routes (VERIFIED):

- released rows: `markRetryWorkReadyTx` sets `blocked = 0` and leaves `scope_key` (`store_retry_work.go:106-109`);
- scope-evidence rows: `recordRetryWork` takes `scopeKey := decision.retryWorkScope()` (`engine_runtime_completion.go:585`; `retryWorkScope` returns `decision.ScopeEvidence` for non-blocking classes, `:348-354`, "other work records its evidence, which a later activation of that scope may block" `:344-347`) and passes it as `RetryWorkFailure.ScopeKey` (`:608-615`), which the store writes (`store_retry_work.go:191`).

Every discard is gated only on the store having no *blocked* rows for the scope (`engine_runtime_lifecycle.go:248, 327`; `engine_runtime_admission.go:238`). So when a scope with no blocked rows left is discarded while a non-blocked row still carries its key, the store row is deleted and the copy and the held action remain. Consequences that follow from the code (VERIFIED): the action still fires from the held map; its next failure finds no existing row, so `AttemptCount` restarts at 1 and the backoff curve resets (`store_retry_work.go:187-198`); its success takes the early return in D1; a restart finds no retry row at all. Whether this sequence is reachable is SUSPECTED — it depends on which failures carry a `ScopeEvidence` equal to an active scope's key, which is decided in `engine_result_classify.go` (not read). Settle with the test in 5.5(2).

The invariant agrees with the SQL, not with the comments: `assertDiscardedScope` fails on any remaining row with the key, blocked or not (`engine_runtime_invariants.go:315-319`). VERIFIED.

**D4 — no check exists.** `assertPersistedInvariants` (`engine_runtime_invariants.go:168-204`) checks the store against itself; `assertReleasedScope` and `assertDiscardedScope` (`:268-322`) compare the scope ledger with the store. Nothing compares `rowsByKey` with `retry_work`. VERIFIED.

Two symptoms of hand-mirroring (VERIFIED): the conditional delete is written twice in a row (`engine_runtime_lifecycle.go:459-461` and again `engine_runtime_admission.go:193-195`); one mirror site alone tolerates a nil map (`engine_runtime_completion.go:779-781`).

Not a divergence: `ReleaseOrphanBlockedRetryWork` (`engine_runtime_lifecycle.go:61`) runs in `runStartupStage` (`engine_startup.go:25`) before any rows are loaded. VERIFIED.

The same two-authority shape exists for scopes and is stated in the code — "persistActiveScope writes a scope's timing to both authorities, the store row and the ledger" (`engine_runtime_lifecycle.go:106-107`) — but the scope ledger also carries run-only fields (`ReleaseAllowance`, `BatchOutstanding`, `Generation`, `SlowStartThreshold`; used at `engine_runtime_release_ramp.go:200-254`), so it is not a pure copy. VERIFIED.

### 5.3 Why it hurts

Bug class: a decision taken from stale in-memory rows — wrong hold (work that should retry now waits, or the reverse), a skipped durable write (`engine_runtime_admission.go:209-212`), or a reset backoff (D3). Each new store mutator of `retry_work` adds a mirror site that nothing checks. This is the case the project rule names: in-memory state "must be rebuildable and must never become a competing authority".

### 5.4 Proposed end state

Proposal, recommended form: the copy stays but has exactly one write path, and the store reports every change.

```go
// engine_retry_work.go (package sync) — the only engine code that calls a retry_work mutator.
type retryWork struct {
    store *SyncStore
    rows  map[RetryWorkKey]RetryWorkRow   // pure cache: never written except from a store result
}

func (w *retryWork) load(ctx context.Context) error                                   // plan install
func (w *retryWork) row(key RetryWorkKey) (RetryWorkRow, bool)
func (w *retryWork) recordFailure(ctx context.Context, f *RetryWorkFailure, delay func(int) time.Duration) (RetryWorkRow, error)
func (w *retryWork) recordBlocked(ctx context.Context, key RetryWorkKey, scope ScopeKey) (RetryWorkRow, error)
func (w *retryWork) resolve(ctx context.Context, key RetryWorkKey) (RetryWorkRow, bool, error)   // drops the cache entry whether or not the store had one
func (w *retryWork) clearBlocked(ctx context.Context, key RetryWorkKey, scope ScopeKey) error
func (w *retryWork) releaseScope(ctx context.Context, scope ScopeKey, keys []RetryWorkKey, now time.Time) (released []RetryWorkRow, scopeRemoved bool, err error)
func (w *retryWork) discardScope(ctx context.Context, scope ScopeKey) (deleted []RetryWorkKey, err error)
func (w *retryWork) prune(ctx context.Context, keep []RetryWorkKey) error
```

Store signature changes (three): `ReleaseScopeWork(…) (released []RetryWorkRow, scopeRemoved bool, err error)`; `DiscardScope(…) (deleted []RetryWorkKey, err error)`; `ClearBlockedRetryWork(…) (cleared bool, err error)`. `RecordRetryWorkFailure`, `RecordBlockedRetryWork`, `ResolveRetryWork` already return the row (`store_retry_work.go:156, 210, 283`).

Engine changes: the nine mirror sites and the duplicated delete disappear; `holdActionFromPersistedRetryState` takes the row the mutator just returned instead of re-reading the map (`engine_runtime_completion.go:278-281`); `discardScope` and its inline copy become one function, which receives the deleted keys and releases their held actions back to admission; `retryLedger` keeps only the held maps (and moves to `syncsched`, section 3). New invariant under `assertInvariants`: after every completion, `ListRetryWork` equals the cache.

Alternative: no cache; admission reads the store. Admission already does one store read per action (`evaluateActionFreshnessFromStore(ctx, flow.deps.store, &ta.Action)`, `engine_runtime_admission.go:60`). The cost is a second point read per admitted action, including the whole initial frontier of a large plan; the gain is that the divergence class cannot exist. I recommend the cache with one write path first (small, checkable by the invariant) and treat removal as a later measurement question.

One decision needed from the owner before the PR (D3): either `DiscardScope` deletes only `blocked = 1` rows, as both comments and the test name `TestSyncStore_DiscardScope_DeletesBlockedRetryWorkAndPreservesObservationIssues` (`store_scope_admin_test.go:360`) say — then `assertDiscardedScope` gains `&& Blocked` and non-blocked rows keep their backoff — or it deletes every row with the key, as the SQL and the invariant say — then the comments change and the engine must drop the cache rows and held actions it gets back. The code and its documentation disagree today; I did not find which is intended.

### 5.5 Tests

Pin today: `engine_retry_trial_test.go:12, 35, 59, 100, 120, 146, 174`; `engine_runtime_lifecycle_test.go:17, 50, 172, 198, 241, 260, 283, 317, 336, 376, 417, 447, 482`; `store_scope_admin_test.go:52, 251, 319, 337, 360, 393`; `engine_runtime_invariants_test.go:12, 30, 49, 67, 85, 103`; `engine_scope_window_test.go:41, 73, 111`; `engine_runtime_retry_curve_test.go:18`. Names VERIFIED; only `engine_retry_trial_test.go:1-60` was read.

Write first:

1. `TestEngineFlow_RetryCacheMatchesStoreAfterEveryCompletion` — the invariant, enabled in the model-based and crash-point suites (`engine_model_based_test.go`, `crash_points_test.go:390`). It should pass today except where D1–D3 bite; a failure is the reachability proof.
2. `TestEngineFlow_DiscardScope_LeavesNoCachedRowTheStoreDeleted` — scope K active with one blocked row; a second action B holds a timed-retry row whose `ScopeKey` is K (written directly with `UpsertRetryWork`); resolve the blocked row and drive `rearmOrDiscardScope` (`engine_runtime_lifecycle.go:243-261`); assert `ListRetryWork` and the flow's rows agree and that B's attempt count survives its next failure. Expected to fail today (D3).
3. `TestEngineFlow_ReleaseScopeBatch_CacheEqualsStore` — one batch member whose store row is no longer blocked; one key with no held action (D2).
4. `TestEngineFlow_ResolveWithoutStoreRow_ClearsTheCachedRow` (D1).
5. Store tests for the three new return values.

### 5.6 Design docs; risks; mechanical or not

- `spec/design/sync-engine.md` "Retry and trial release" (`:1115-1149`; it says "Scope release updates both authorities in one logical step") and "Outcome And Scope Lifecycle" (`:997-1017`); the store design doc that governs `store_retry_work.go` and `store_scope_admin.go` (not identified; find its `GOVERNS:` line).
- Not mechanical: three store signatures change, one behaviour decision (D3), one new invariant that may expose existing divergence in the model-based suite.
- Cheaper before the package split: afterwards the three signatures are `syncstore` exports and `syncexec.Store`/engine interfaces must follow.

---

## 6. Watch-mode phases

### 6.1 Current structure

```go
// engine_watch_state.go:139-145
type watchRuntimePhase string
const (
    watchRuntimePhaseBootstrap watchRuntimePhase = "bootstrap"
    watchRuntimePhaseRunning   watchRuntimePhase = "running"
    watchRuntimePhaseDraining  watchRuntimePhase = "draining"
)
```

Transitions today (VERIFIED):

| Where | Transition | Guard |
|---|---|---|
| `newWatchRuntime` `engine_watch_state.go:151` | initial phase = `running` | — (before start-up, infra or bootstrap have happened) |
| `runWatchUntilQuiescent` `engine_watch_loop.go:23` → `enterBootstrap` `engine_watch_state.go:182-184` | any → `bootstrap` | none (would overwrite `draining`) |
| `runBootstrapLoopStep` `engine_watch_loop.go:145-147` → `enterRunning` `engine_watch_state.go:186-188` | `bootstrap` → `running` on `isBootstrapQuiescent()` (`:672-676`) | none in the setter |
| `startObservers` `engine_watch.go:342` → `beginObserverBackedRunning` `engine_watch_state.go:190-199` | → `running` | refuses when draining (`errWatchObserversDuringDrain`) or `activeObservers != 0` |
| `beginWatchDrain` `engine_watch_drain.go:8-24` → `enterDraining` `engine_watch_state.go:201-209` | any → `draining`, once | idempotent |
| `pipe.cleanup` `engine_watch.go:222-255` | implicit "stopped": timers stopped, pool stopped, seven fields nil-ed | no phase value |

Lifecycle state that is *not* in the enum (VERIFIED):

- The loop's mode is inferred from a variadic argument: `isBootstrapLoop := len(bootstrapLogC) > 0` (`engine_watch_loop.go:44`), which also decides whether the outbox is cleared (`:45-47`) and whether a non-drained completion is an error (`:64-72`).
- The pre-loop stages (start-up, infra, bootstrap plan, observers) are a straight-line sequence in `RunWatch` (`engine_watch.go:58-116`) with no phase value; "bootstrapped but observers not started" is `running` + `activeObservers == 0`.
- A cancellation during bootstrap: the bootstrap loop drains and returns nil (the check at `:64` is skipped for bootstrap loops), `RunWatch` carries on to `startObservers`, which refuses because the phase is `draining`, and `isWatchObserverStartupStopped` (`engine_watch.go:128-134`) turns that refusal into a clean stop. The fact "bootstrap ended because we drained" is not returned; it is rediscovered two calls later. Pinned by `TestStartObservers_RefusalDuringDrainClassifiesAsShutdown` (`engine_watch_loop_completion_test.go:50`).
- Booleans and counters: `loop.hasPendingReplan` (`engine_watch_state.go:16`, with `canPrepareNow()` `:224-226`), `loop.postReplanOutbox` (`:20`), `resources.refreshActive` (`:119`), `resources.activeObservers`.
- Inputs are enabled and disabled by assigning nil to channel fields: on close (`engine_watch_loop.go:317, 330, 368, 381, 413, 427`), on drain (`engine_watch_drain.go:42-48`: `replanReady`, `localBatches`, `remoteBatches`, `skippedItems`, `maintenanceC`; `:51-53` `observerErrs`), on cleanup (`engine_watch.go:246-252`). `protectedRootEvents` is neither nil-ed nor selected during drain (`engine_watch_drain.go:67-74`).
- The step dispatcher is a `switch` with a default "unknown watch runtime phase" error (`engine_watch_loop.go:115-124`).
- The inspector used by invariants reports `running` for a one-shot run (`engine_runtime_signals.go:59-66`).

So: yes, an implicit state machine — one enum of three values plus a mode flag, an implicit start and end, and input enablement spread over nil assignments.

### 6.2 Why it hurts

- `enterBootstrap` and `enterRunning` are unconditional; a call in the wrong place silently leaves `draining`, re-opening admission during shutdown. The existing defences are after-the-fact: the completion check (`engine_watch_loop.go:64-72`) and the invariants `assertDispatchAdmissionSealed`, `assertHeldReleaseAllowed`, `assertObserverExitPhase` (`engine_runtime_invariants.go:132-166`), which are off in production (`engine.go:78-80`).
- `drainAfterCanceledStepError` (`engine_watch_loop.go:79-106`) exists because a cancellation inside a step left through a path that never drained; its comment describes timers left armed after a "clean" stop. That is a past bug of exactly this class. VERIFIED (comment).
- Which inputs are live in which phase can only be learned by tracing nil assignments in three files.

### 6.3 Proposed end state

Proposal. An enumerated phase, an enumerated event, one transition function, one table of enabled inputs.

```go
// engine_watch_phase.go (new)
type watchPhase uint8
const (
    watchPhaseStarting  watchPhase = iota // constructed; start-up, infra and bootstrap plan
    watchPhaseBootstrap                   // bootstrap actions draining to quiescence
    watchPhaseReady                       // bootstrap done, observers not yet started
    watchPhaseRunning                     // observers live
    watchPhaseDraining                    // admission sealed
    watchPhaseStopped                     // cleanup done
)

type watchEvent uint8
const (
    watchEventBootstrapPlanned   watchEvent = iota // plan installed with actions
    watchEventBootstrapSettled                     // quiescent, or no actions
    watchEventObserversStarted
    watchEventShutdownRequested                    // ctx done, or a step failed because ctx is done
    watchEventDrained                              // completions closed, no refresh, no observers
)

func (rt *watchRuntime) transition(ev watchEvent) error   // table lookup; an absent pair is an error naming both
```

| From \ Event | BootstrapPlanned | BootstrapSettled | ObserversStarted | ShutdownRequested | Drained |
|---|---|---|---|---|---|
| Starting | Bootstrap | Ready | error | Draining | error |
| Bootstrap | error | Ready | error | Draining | error |
| Ready | error | error | Running | Draining | error |
| Running | error | error | error | Draining | error |
| Draining | error | error | error (the refusal `RunWatch` treats as a clean stop) | Draining (no-op) | Stopped |
| Stopped | error | error | error | Stopped (no-op) | error |

Enabled inputs per phase, replacing nil assignment (the `select` reads `rt.input(x)`, which returns nil when the table says off or the source has closed):

| Input (today's select case) | Bootstrap | Running | Draining |
|---|---|---|---|
| dispatch (`engine_watch_loop.go:228`) | on | on | off |
| `replanReady` (`:206, :231`) | on | on | off |
| `completions` (`:220, :236`) | on | on | on |
| `localBatches`, `protectedRootEvents`, `remoteBatches`, `skippedItems` (`:238-245`) | off (no observers yet) | on | off |
| `refreshCh` (`:246`) | off (timer first armed at `engine_watch.go:327`) | on | off |
| `refreshResults` (`:249`) | on | on | on |
| `maintenanceC` (`:251`) | on | on | off |
| `observerErrs` (`:254`) | off | on | on while observers remain |
| trial / retry / admit timers (`:256-260`) | on | on | off |
| bootstrap log ticker (`:263`) | on | off | off |
| `ctx.Done()` (`:266`) | on | on | off |

The "Bootstrap" column records today's effective behaviour (the cases exist in the shared select, `engine_watch_loop.go:150`, but the sources are nil or unarmed until later). VERIFIED for each row from the cited lines.

What changes in code: `runWatchLoop` loses the variadic parameter and reads the phase; the completion check at `:64-72` becomes "the loop may return only in `Ready` (bootstrap call) or `Stopped`"; `enterBootstrap`, `enterRunning`, `beginObserverBackedRunning`, `enterDraining` become calls to `transition`; the constructor starts in `Starting`; `bootstrapSync` returns whether it ended by settling or by draining, so `RunWatch` no longer discovers a shutdown through `startObservers`' refusal; `disableDrainInputs` and the cleanup nil-assignments go. `hasPendingReplan` and `refreshActive` stay as they are: they are orthogonal to the lifecycle (a pending replan surviving into `draining` is harmless because only `runRunningLoopStep` consumes it, `engine_watch_loop.go:115-121, 153-166`. VERIFIED).

### 6.4 Tests

Pin today: `engine_watch_loop_owner_test.go:19, 48, 62, 76, 93, 117, 133, 156, 178, 200, 220, 253, 292, 334, 375, 426, 494, 516` (among them `:48` `TestWatchRuntime_RunWatchLoop_BootstrapPhaseQuiescesAndReturnsToRunning`, `:62` `TestWatchRuntime_BeginObserverBackedRunningNormalizesBootstrapPhase`, `:76` `…RejectsDrainingOrDuplicateObservers`, `:178` `…DrainsFromBootstrapPhase`); `engine_watch_loop_completion_test.go:18, 35, 50`; `engine_watch_drain_error_test.go:25, 44, 60, 77`; `engine_watch_observer_exit_test.go:15, 29, 43, 58`; `engine_watch_bootstrap_test.go:12, 65`; `engine_one_shot_quiescence_test.go:94, 132`. Names VERIFIED; bodies not read. The test at `:62` suggests observers may be started while the phase is still `bootstrap`; if so the table needs `Bootstrap × ObserversStarted → Running` (SUSPECTED; settle by reading that test).

Write first:

1. `TestWatchPhase_TransitionTableIsTotal` — every (phase, event) pair is either a listed transition or returns an error naming both; no pair panics.
2. `TestWatchPhase_NothingLeavesDrainingExceptDrained` — property over random event sequences.
3. `TestWatchLoop_ServicesOnlyThePhasesEnabledInputs` — for each phase, make every input ready and assert only the enabled ones are consumed.
4. `TestRunWatch_CancelDuringBootstrapStopsWithoutStartingObservers` — asserts no observer factory call; today the path reaches `startObservers` and relies on its refusal.

### 6.5 Design docs; risks; mechanical or not

- `spec/design/sync-engine.md` "Watch Mode" (`:556-779`), "Loop Completion Is Phase-Checked, Not Assumed" (`:950-971`), "Watch State Is Grouped And Qualified" (`:248-259`), "Asking For Future Work" (`:281-308`, the `watchInspector` contract).
- Not mechanical. Risk: tests that construct a `watchRuntime` and call loop steps directly rely on the constructor's `running` default (helper `testWatchRuntime`, used at `engine_retry_trial_test.go:14`); they need a helper that puts the runtime in a named phase. Debug-event payloads carry the phase as a string (`emitRuntimeDebugEvent`, `engine_watch_state.go:385-403`); keep a `String()` with today's three values so recorded event streams compare.

---

## 7. Executor configuration through setters

### 7.1 Current structure

`executorConfig` (`executor.go:33-78`, 19 fields) is introduced as "the immutable configuration" (`:30-32`) and "an immutable executor configuration" (`:98-99`). VERIFIED. Constructor:

```go
func newExecutorConfig(
    items itemClient, downloads driveops.Downloader, uploads driveops.Uploader,
    syncTree *synctree.Root, driveID driveid.ID, logger *slog.Logger, pathConvergence driveops.PathConvergence,
) *executorConfig        // executor.go:100-119
```

Mutation points after construction — 14 (VERIFIED):

| # | Mutation | Declared | Production caller | Test callers |
|---|---|---|---|---|
| 1 | `SetTrash` | `executor.go:81-83` | `engine.go:312` | `executor_local_trash_test.go:46, 77, 170, 223` and others in the list below |
| 2 | `SetPendingCreates` | `:86-88` | `engine.go:310` | |
| 3 | `SetTransferMgr` | `:124-126` ("Must be called before any Executor is created from this config", `:121-123`) | `engine.go:193-196` | |
| 4 | `SetRemoteRootItemID` | `:130-132` | `engine.go:309` | |
| 5 | `SetContentFilter` | `:138-141` | `engine.go:313` | |
| 6 | `SetSyncRootIdentity` | `:146-148` | `engine.go:314` | |
| 7 | `SetNowFunc` | `:240-242` ("Used in tests", `:238-239`) | `engine.go:249` | |
| 8 | `SetDownloads` | `:252-254` | none | tests only |
| 9 | `SetUploads` | `:264-266` | none | tests only |
| 10 | `pool.perfCollector = …` | field `worker.go:44-64` | `engine_watch.go:194`, `engine_run_once.go:355` | `action_freshness_test.go:36, 74, 107, 142` |
| 11 | `pool.serialize = …` | `worker.go:59` | `engine_watch.go:195`, `engine_run_once.go:356` | |
| 12 | `cfg.items = …` | — | none | `worker_test.go:984` |
| 13 | `cfg.hashReader = …` | — | none | `executor_local_trash_test.go:80` |
| 14 | `execCfg.crashAt = …` | `executor.go:57` | none | `engine_model_based_test.go:379`, `crash_points_test.go:342` |

Setter call sites in tests (all nine setters together): `executor_verify_before_destroy_test.go:148, 160, 249, 291`; `engine_unavailable_not_absent_test.go:110`; `worker_test.go:89, 90, 403, 414, 485, 490, 547, 554, 613, 618, 776, 785, 878, 887, 1028`; `executor_delete_test.go:103, 104, 105, 129, 193`; `executor_local_trash_test.go:46, 77, 170, 223`; `executor_test.go:165, 166, 402, 883, 964, 1624, 1661, 2840, 3802`; `name_length_test.go:75`. VERIFIED (grep). Two of them (`executor_verify_before_destroy_test.go:249, 291`) mutate `eng.execCfg` after the engine was built.

Production wiring order (VERIFIED): `newExecutorConfig` `engine.go:172-180` → `configureEngineExecutor` `:182` (five setters, `:303-316`) → `SetTransferMgr` `:193-196` → `Engine` literal `:198-238` → `SetNowFunc(e.nowFunc)` `:249`. The comment at `engine.go:184-187` praises the *TransferManager* for having "no post-hoc field mutation" two lines before a setter installs it.

Further mutable state inside the "immutable" config: `parentChecks *remoteParentChecks` (`executor.go:75-77`), a mutex-guarded memo written by workers (`executor.go:166, 224, 233`) and reset per plan by the loop through `(cfg *executorConfig) resetRemoteParentChecks()` (`executor_parent_checks.go:92-97`, called at `engine_runtime_start.go:92`). VERIFIED.

Stale comment: `executor.go:45-46` documents `expectedSyncRoot`; the field is `syncRootIdentity` (`:47`). VERIFIED.

### 7.2 Why it hurts

- A half-configured executor is constructible and is the normal state for six statements of `newEngine`. Fields whose absence changes behaviour silently: `trash` nil → "removal is permanent" (`executor.go:67-69`); `pendingCreates` nil (`:71-73`); `transferMgr` nil. Both nil cases are documented as test-only, i.e. production branches that only tests reach. VERIFIED (comments); the branches themselves are in `executor*.go` bodies I did not read (SUSPECTED where; settle with `grep -n "\.trash == nil\|pendingCreates == nil"`).
- `executorConfig` is shared by every worker goroutine (`worker.go:208` `newExecution(wp.cfg, bl)`); a setter call after `pool.Start` is a data race. Two tests already mutate an engine's config after construction.
- The executor's clock is late-bound through the engine (`SetNowFunc(e.nowFunc)`), which forces the two-phase `newEngine` and is why tests can swap `eng.clock` after construction (9 sites, section 8).

### 7.3 Proposed end state

Proposal (lands in `syncexec` after the split; names shown unexported as today).

```go
type executorInputs struct {
    Items            itemClient
    Downloads        driveops.Downloader
    Uploads          driveops.Uploader
    Transfers        *driveops.TransferManager
    SyncTree         *synctree.Root
    DriveID          driveid.ID
    RemoteRootItemID string                 // empty = drive root
    Logger           *slog.Logger
    ContentFilter    ContentFilterConfig
    SyncRootIdentity *syncRootIdentity
    PathConvergence  driveops.PathConvergence
    Trash            *localtrash.Trash
    PendingCreates   pendingCreateStore
    Now              func() time.Time
    Hooks            executorHooks          // CrashAt, HashFile, HashReader; zero value = production (item 8)
}

func newExecutorConfig(in executorInputs) (*executorConfig, error)   // error names the first missing required field

type workerPoolInputs struct {
    Exec             *executorConfig
    Parents          *remoteParentChecks    // run-scoped memo; the flow holds the same value and resets it per plan
    Dispatch         <-chan *trackedAction
    Store            workerStore            // syncexec.Store after the split (end-state.md:190-207)
    Logger           *slog.Logger
    CompletionBuffer int
    Perf             *perf.Collector
    Serialize        bool                   // D-090; set only by tests through the engine hooks
}

func newWorkerPool(in workerPoolInputs) *workerPool
```

- Required (constructor error if zero): `Items`, `Downloads`, `Uploads`, `Transfers`, `SyncTree`, `Logger`, `SyncRootIdentity`, `Trash`, `PendingCreates`, `Now`.
- Deleted: all nine `Set…` methods; the getters `Items()`, `Downloads()`, `Uploads()` (`executor.go:152-154, 246-248, 258-260`) if only tests call them (SUSPECTED; settle with grep); `configureEngineExecutor` (`engine.go:303-316`); `executorConfig.parentChecks` and `resetRemoteParentChecks`; the two pool field writes; the nil-`trash`/nil-`pendingCreates` branches (tests get a temp-dir trash and a fake store from one helper).
- `newEngine` builds the clock first, then trash, session store and transfer manager, then one `executorInputs` literal, then the `Engine` literal — single-phase.
- This supersedes `end-state.md:220-240`, which plans a new setter `SetRuntimeOptions(collector *perf.Collector, serialize bool)` for the pool and `…ForTest` setters on `executorConfig` (`end-state.md:403-428`).

### 7.4 Tests

Pin today: `engine_config_test.go:276` `TestNewEngine_PropagatesPathConvergence`, `:325` `TestEngine_StoreAndExecutorTellTheEngineClocksTime`; `worker_test.go:200, 214, 229, 250, 282, 329, 377, 441, 478, 538, 606, 977, 1021`; `executor_parent_checks_test.go:45` `TestExecutor_Upload_SiblingCreatesReadTheirParentOncePerPlan`, `:83, :123`; `crash_points_test.go:390, 413`. Names VERIFIED; bodies not read.

Write first:

1. `TestNewExecutorConfig_RejectsEachMissingRequiredField` — table over the required fields.
2. `TestWorkerPool_ParentChecksAreResetPerPlanAndSharedByWorkers` — pins the memo's lifetime before it moves out of the config (extends `executor_parent_checks_test.go:45`).
3. A test helper `newTestExecutorInputs(t) executorInputs` returning a complete literal; migrate the roughly 45 setter call sites to "copy, change one field, construct". This migration is the bulk of the PR and is mechanical.

### 7.5 Design docs; risks; mechanical or not

- `spec/design/sync-execution.md` "Ownership Contract" (`:24-33`), "Executor Live Preconditions" (`:259-396`), "Worker And Dependency Model" (`:60-257`); `spec/design/sync-engine.md` "Construction" (`:131-217`) and "The Engine Clock Is One Capability" (`:109-129`); `end-state.md` section 5 and the `sync → syncexec` edge (`:82`).
- Mostly mechanical. Non-mechanical parts: the clock must be fixed at construction (depends on item 8's hooks, because nine tests replace `eng.clock` afterwards); removing the nil-trash and nil-pending-create branches changes what "executor-only" tests exercise.

---

## 8. Test hooks in production code

### 8.1 Inventory

VERIFIED by reading each declaration and grepping its assignment sites. There is **no package-level func-typed variable** in production code of `internal/sync`: package `var`s are sentinel errors only (for example `worker.go:15`, `engine_watch.go:121`, `errors.go:19-156`), and `timeSleep` is a function (`observer_remote.go:716-719`).

**On `Engine`**

| Hook | Declaration | Read at | Set by tests at |
|---|---|---|---|
| `assertInvariants bool` | `engine.go:80` | `engine_runtime_invariants.go:11` | `symlink_upload_test.go:52`; `engine_root_identity_test.go:72`; `engine_test_helpers_test.go:396, 462` |
| `dispatchOrder func([]*trackedAction) []*trackedAction` | `engine.go:88` | `engine_runtime_start.go:55-56` | `dispatch_order_test.go:129`; `engine_model_based_test.go:357` |
| `serializeWorkers bool` | `engine.go:91` | `engine_run_once.go:356`; `engine_watch.go:195`; `engine_watch_state.go:371`; `engine_results.go:202` | `executor_verify_before_destroy_test.go:379`; `dispatch_order_test.go:130`; `engine_model_based_test.go:356`; `crash_points_test.go:289` |
| `oneShotPassLimit int` | `engine.go:95` | `engine_run_once.go:227-228` | `engine_move_composition_test.go:498` |
| `waitFn func(ctx context.Context, d time.Duration) error` | `engine.go:99` | `engine_run_once.go:212-213` | `engine_test_helpers_test.go:855`; `engine_run_once_scope_wait_test.go:36, 74` |
| `localWatcherFactory func() (fsWatcher, error)` | `engine.go:104` | `engine_watch.go:395-396` | `engine_watch_panic_recovery_test.go:90`; `engine_phase0_test.go:75`; `observer_local_watch_health_test.go:117, 582, 612`; `engine_watch_test.go:567, 740, 841` |
| `socketIOWakeSourceFactory func(socketIOEndpointFetcher, driveid.ID, socketIOWakeSourceOptions) socketIOWakeSourceRunner` | `engine.go:108-112` (default `:231-237`) | `engine_watch.go:522` | `engine_watch_panic_recovery_test.go:162`; `engine_watch_test.go:118, 187, 236` |
| `clock syncClock` (production value, replaced by tests) | `engine.go:117` | `engine_results.go:186-192` | `engine_test_helpers_test.go:32, 839, 1273`; `engine_absence_confirmed_test.go:279, 327`; `engine_model_based_test.go:351`; `fs_shim_scenario_test.go:69, 149`; `nfd_local_spelling_test.go:251` |
| `perfCollector` (production value, overwritten by tests) | `engine.go:54` | — | `engine_watch_loop_owner_test.go:379, 430`; `engine_watch_local_observation_test.go:21, 87, 117, 150, 185`; `action_freshness_test.go:364` |
| `diskAvailableFn func(string) (uint64, error)` | `engine.go:76`, assigned `:229` | nowhere | nowhere — dead |

**On `engineInputs`**: `rootLockFlock func(fd int, how int) error` (`api_types.go:96-98`; set at `engine_root_lock_test.go:109, 127`; used `engine.go:155`); `interposeSyncTree func(synctree.Handle) synctree.Handle` (`api_types.go:99-102`; set at `engine_test_helpers_test.go:352`; used `engine.go:325-327`).

**On `executorConfig`**: `crashAt` (`executor.go:57`, read `:1049-1055`); `nowFunc` (`:60`); `hashFunc` (`:61`); `hashReader` (`:64`); plus test-only `SetDownloads`/`SetUploads` and direct writes (section 7.1).

**On `SyncStore`**: `crashAt` (`store.go:71-74`, read `:235-239`; set at `engine_model_based_test.go:380`, `crash_points_test.go:343`); `setNowFunc` (`store.go:190`; production `engine.go:248`; tests `engine_test_helpers_test.go:850`, `store_test_seams_test.go:12`, `pending_creates_test.go:491, 493`, and about 22 sites in `baseline_test.go:371-1407`).

**On `workerPool`**: `serialize` (`worker.go:59`).

**On observers** (move to `syncobserve`): `localObserver.watcherFactory` (`observer_local.go:495`, default `:580`, setter `:690-694`, used `:929`); `localObserver.hashFunc`; observer `clock` fields — each set directly by many tests (for example `observer_local_test.go:381, 478, 479`; `observer_remote_test.go:1413`; `watch_test_helpers_test.go:41, 43, 60`).

**Exported from a production file for tests**: `NewBaselineForTest` (`core_types.go:328-330`), used by this package's tests and by `internal/syncverify/verify_test.go:258, 302, 335, 370, 398, 429`, which is why it cannot be a `_test.go` helper.

**Production hooks, not test hooks** (stay out): `debugEventHook` (`engine.go:81`; `SetDebugEventHook` `engine_debug_events.go:113-115`; called by `internal/multisync/orchestrator.go:162`), `activityHook` (`engine.go:82`; `SetActivityHook` `engine_activity.go:45-47`; `internal/multisync/orchestrator.go:165`), `socketIOWakeSourceOptions.LifecycleHook` (used `engine_watch.go:528`). `multisync/orchestrator.go` is the only production file outside the package that calls either setter. VERIFIED. Both are set after construction and read unguarded from several goroutines (`emitDebugEvent` reads `e.debugEventHook` at `engine_debug_events.go:108` and is called from the loop, observers and timer callbacks via `deps.emit`); they belong in `EngineMountConfig` (`engine_config.go:15-34`) so the engine is single-phase.

### 8.2 Why it hurts

- Ten test seams are interleaved with 39 production fields, so "what can differ in a test" has no single answer, and each seam is read through the back-pointer (section 1.3 D).
- Tests assign them after construction; for `clock`, `serializeWorkers` and `perfCollector` that is after other components captured the old value or a closure over the engine.
- The split plan answers the cross-package version of this with six `…ForTest` functions and a devtool check on the identifier suffix (`end-state.md:403-428`); that is six more mutators on types this report wants construct-once.

### 8.3 Proposed end state

Proposal.

```go
// engine_hooks.go (package sync). The zero value is production behaviour.
type engineHooks struct {
    assertInvariants bool
    ownerCheck       func()                                            // section 4.3
    dispatchOrder    func([]*trackedAction) []*trackedAction
    serializeWorkers bool
    oneShotPassLimit int
    wait             func(ctx context.Context, d time.Duration) error
    clock            syncClock                                         // nil = the real clock
    rootLockFlock    func(fd int, how int) error
    interposeSyncTree func(synctree.Handle) synctree.Handle
    observe          observationHooks                                  // localWatcherFactory, socketIOWakeSourceFactory
}
```

- One field on `engineInputs` (`hooks engineHooks`) and one on `Engine`; the ten scattered fields and the default assignment at `engine.go:231-237` go (the nil case is resolved where the factory is used).
- After the split, one hooks struct per package that has seams, passed in that package's constructor inputs: `syncexec.Hooks{CrashAt, HashFile, HashReader}` (section 7.3), `syncstore.Hooks{CrashAt, Now}`, `syncobserve.Hooks{WatcherFactory, HashFile, Clock}`. These replace the six planned `…ForTest` functions.
- Check (devtool, replacing the planned `ForTest`-suffix check): no non-test file contains a composite literal of, or an assignment to a field of, a type named `…Hooks`/`engineHooks`, other than passing the value through. One AST rule keyed on type names.
- Stays as is: `NewBaselineForTest`; the three production hooks, which move to `EngineMountConfig`.
- Deleted: `Engine.diskAvailableFn`.

### 8.4 Tests

Pin today: `dispatch_order_test.go:294` `TestReverseDispatch_RunOnceScenariosPass`, `:307` `TestReverseDispatchTable_ListsEveryRunOnceScenario` (the one source-scanning test in the package; `end-state.md:478-510` notes it must follow file moves); `crash_points_test.go:390, 413`; `engine_run_once_scope_wait_test.go:22, 63`; `store_fault_seam_test.go:246, 288, 321`; `engine_one_shot_quiescence_test.go:254`; `engine_root_lock_test.go` (seam sites `:109, :127`).

Write first: `TestEngineHooks_ZeroValueIsProduction` — build an engine with `engineHooks{}` and assert the real clock, the fsnotify factory, the real wake-source factory, pool size from `transferWorkers`, invariants off. Then migrate assignment sites (about 44: 4 + 2 + 4 + 1 + 3 + 8 + 4 + 9 + 9 by the table) to the test helper that builds the engine.

### 8.5 Design docs; risks; mechanical or not

- `spec/design/sync-engine.md` "Construction" (`:131-217`), "The Engine Clock Is One Capability" (`:109-129`), "Activity Hook" (`:1326-1338`); `end-state.md:403-428`.
- Stage 1 (move the fields into the struct, tests write `eng.hooks.x`) is purely mechanical. Stage 2 (hooks fixed at construction) is not: tests that flip a hook mid-test must be restructured; I did not check whether any does (SUSPECTED; settle by reading the 44 sites).

---

## 9. Size and shape

### 9.1 Line counts (`wc -l`, commit `70301c0e`)

VERIFIED. 46 `engine*.go` production files, 11,812 lines in total (sum of the figures below):

`engine.go` 545; `engine_activity.go` 133; `engine_config.go` 104; `engine_current_plan.go` 784; `engine_debug_events.go` 115; `engine_housekeeping.go` 108; `engine_log_fields.go` 65; `engine_mount_root.go` 438; `engine_observation_postprocess.go` 260; `engine_primary_root.go` 253; `engine_primary_root_watch.go` 101; `engine_remote_path_lookup.go` 31; `engine_result_classify.go` 378; `engine_results.go` 271; `engine_retry_trial.go` 136; `engine_root_lock.go` 206; `engine_run_once.go` 605; `engine_run_once_report.go` 94; `engine_run_once_results.go` 249; `engine_runtime_admission.go` 244; `engine_runtime_cold_start.go` 238; `engine_runtime_completion.go` 870; `engine_runtime_completion_trial.go` 154; `engine_runtime_deps.go` 56; `engine_runtime_graph.go` 63; `engine_runtime_invariants.go` 322; `engine_runtime_lifecycle.go` 575; `engine_runtime_owners.go` 327; `engine_runtime_permissions.go` 351; `engine_runtime_release_ramp.go` 279; `engine_runtime_shutdown.go` 65; `engine_runtime_signals.go` 66; `engine_runtime_start.go` 118; `engine_runtime_types.go` 91; `engine_runtime_workset.go` 131; `engine_shortcut_topology.go` 174; `engine_startup.go` 44; `engine_watch.go` 732; `engine_watch_drain.go` 139; `engine_watch_local_observation.go` 74; `engine_watch_loop.go` 676; `engine_watch_maintenance.go` 236; `engine_watch_observation.go` 167; `engine_watch_panic_recovery.go` 57; `engine_watch_replan.go` 113; `engine_watch_state.go` 574.

Executor: `executor.go` 1055; `executor_conflict.go` 325; `executor_delete.go` 646; `executor_parent_checks.go` 97; `executor_preconditions.go` 1034; `executor_transfer.go` 630 (3,787 in total).

Related: `worker.go` 365; `worker_result.go` 138; `dep_graph.go` 238; `active_scopes.go` 182; `scope.go` 208; `scope_block.go` 89; `scope_key.go` 323; `scope_lifecycle_policy.go` 123; `scope_semantics.go` 297; `store_retry_work.go` 495; `store_scope_admin.go` 160; `protected_roots.go` 127; `tracked_action.go` 37; `crash_points.go` 41.

Largest test files touched by this work: `executor_test.go` 3819; `engine_test_helpers_test.go` 1276; `engine_runtime_completion_test.go` 1086; `worker_test.go` 1068; `engine_watch_test.go` 964; `engine_phase0_test.go` 793; `scope_test.go` 579; `engine_watch_loop_owner_test.go` 573; `engine_runtime_lifecycle_test.go` 511; `engine_runtime_release_ramp_test.go` 485.

### 9.2 Fields and methods

VERIFIED (fields by reading the declarations; methods by listing every `func (recv *T)` in non-test files — 535 method declarations — and tallying by hand).

| Type | Fields | Methods | Notes |
|---|---|---|---|
| `Engine` (`engine.go:30-134`) | 49 | 75 | 1 dead field; 10 test seams; 6 self-guarded mutable fields. Methods by file: `shortcut_root_lifecycle.go` 20; `engine.go` 8; `engine_run_once.go` 8; `engine_mount_root.go` 6; `engine_shortcut_topology.go` 6; three each in `shortcut_alias_mutation.go`, `engine_watch.go`, `protected_roots.go`, `engine_activity.go`, `engine_run_once_report.go`, `engine_results.go`; two each in `engine_watch_maintenance.go`, `engine_debug_events.go`, `engine_housekeeping.go`; one each in `engine_remote_path_lookup.go`, `engine_current_plan.go`, `engine_primary_root.go` |
| `engineFlow` (`engine_runtime_types.go:8-45`) | 11 | 159 in 22 files | `engine_runtime_completion.go` 26; `engine_runtime_lifecycle.go` 21; `engine_current_plan.go` 19; `engine_runtime_invariants.go` 17; `engine_runtime_permissions.go` 11; `engine_runtime_admission.go` 9; five each in `engine_runtime_completion_trial.go`, `engine_results.go`, `engine_runtime_workset.go`; four each in `engine_retry_trial.go`, `engine_runtime_release_ramp.go`, `engine_runtime_shutdown.go`, `engine_runtime_cold_start.go`, `engine_watch_observation.go`, `engine_runtime_graph.go`, `engine_runtime_start.go`, `engine_primary_root.go`; `engine_run_once.go` 3; `engine_log_fields.go` 2; `engine_mount_root.go` 2; `engine_startup.go` 1; `engine_housekeeping.go` 1 |
| `watchRuntime` (`engine_watch_state.go:132-137`) | 4 groups | 135 own in 16 files, plus the 159 promoted | `engine_watch_state.go` 39; `engine_watch_loop.go` 32; `engine_watch.go` 13; `engine_watch_drain.go` 10; `engine_watch_maintenance.go` 9; `engine_results.go` 8; `engine_observation_postprocess.go` 5; `engine_mount_root.go` 5; `engine_watch_replan.go` 3; two each in `engine_retry_trial.go`, `engine_watch_local_observation.go`, `engine_current_plan.go`, `watch_condition_log.go`, `engine_primary_root_watch.go`; `engine_runtime_lifecycle.go` 1 |
| `oneShotRunner` | 2 | 14 own, plus promoted | `engine_run_once_results.go` 8; `engine_run_once.go` 3; `engine_current_plan.go` 2; `engine_runtime_types.go` 1 |
| `executorConfig` (`executor.go:33-78`) | 19 | 14 | 9 setters, 3 getters, `reachCrashPoint`, `resetRemoteParentChecks` |
| `executor` (`executor.go:93-96`) | 2 | 85 | `executor_preconditions.go` 28; `executor.go` 22; `executor_delete.go` 18; `executor_transfer.go` 12; `executor_conflict.go` 5 |
| `workerPool` (`worker.go:44-64`) | 10 | 14 | |
| `actionScheduler` | 9 | 10 | |
| `retryLedger` | 4 | 3 | |
| `scopeLedger` | 3 | 8 | |
| `runResults` | 6 | 0 | |
| `runtimeDeps` | 4 | 3 | |
| `depGraph` | 3 | 7 | 1 dead (`MarkTrial`), 1 test-only (`Add`) |
| `scopeState` | 3 | 3 | |
| `remoteParentChecks` | 3 | 5 | |
| `trackedAction` (`tracked_action.go:12-37`) | 7 | — | 1 dead (`Cancel`) |
| `EngineMountConfig` (`engine_config.go:15-34`) | 18 | — | |

A method on `watchRuntime` can therefore reach 294 methods and, through `rt.engine`, 75 more.

### 9.3 Dead code and stale comments found on the way

All VERIFIED.

- `trackedAction.Cancel` (`tracked_action.go:15`): written at `worker.go:186` (`ta.Cancel = cancel`), read nowhere. A worker goroutine writes a shared struct for nothing.
- `depGraph.MarkTrial` (`dep_graph.go:159-171`): no caller in production or tests.
- `Engine.diskAvailableFn` (`engine.go:76`, assigned `:229`): never read, never set by a test.
- Stale `depGraph` capture in watch cleanup (`engine_watch.go:179, 235-240`; section 3.2).
- `tracked_action.go:22-23` says the field is "Set by DispatchTrial"; no such function exists.
- `executor.go:45-46` documents `expectedSyncRoot`; the field is `syncRootIdentity`.
- `engine_runtime_owners.go:63-64`, `dep_graph.go:27-28`, `engine.go:127-131`: describe concurrent readers that do not exist (section 4).
- `engine.go:63-67`, `protected_roots.go:12-15`: omit the third writer (section 2.2).
- `store_scope_admin.go:56-61` and `engine_runtime_lifecycle.go:402-403`: say "blocked retry work"; the SQL deletes every row with the scope key (section 5.2).
- `transitionTrialScopeToPersistedBlock` contains a second, incomplete copy of `discardScope` (`engine_runtime_lifecycle.go:336-344` vs `:404-420`).
- Receiver names are inconsistent: `f` for `engineFlow` in `engine_results.go` and `engine_runtime_shutdown.go`, `flow` elsewhere; `eng` for `Engine` in `engine_remote_path_lookup.go:11`, `e` elsewhere.

---

## 10. Dependency-ordered PR breakdown

Each PR is independently green. "Split" = the package split of `end-state.md`.

### 10.1 Before the split (engine-side; no package boundary needed)

| PR | Content | Depends on | Mechanical? | Tests first |
|---|---|---|---|---|
| E0 | Dead code and stale comments (section 9.3): delete `trackedAction.Cancel`, `depGraph.MarkTrial`, `Engine.diskAvailableFn`; cleanup reads `rt.sched.graph` (or drops the redundant log — `beginWatchDrain` already logs `in_flight` from the live graph, `engine_watch_drain.go:15-17`); fix the comments | — | yes | none needed |
| E1 | Guarded read at `engine_housekeeping.go:26` (`currentProtectedRoots()`); name the third writer in both comments | — | yes | 1.7(1) |
| E2 | Observer start order and lifetime (F6, F7): fallible local set-up before any goroutine; own cancel; cleanup waits | — | no | 2.6(1), 2.6(2) |
| E3 | Vestigial locks and atomics (section 4), `ownerCheck` hook, delete the three concurrent `depGraph` tests | E0 | yes (deletion) | 4.4 |
| E4 | Retry-work single write path (section 5): three store return values, `retryWork` type, one `discardScope`, cache-equals-store invariant. Needs the owner's D3 decision first | — | no | 5.5(1)–(5) |
| E5 | Watch phase table (section 6) | E2 (uses the stop handle for `Stopped`) | no | 6.4(1)–(4) |
| E6 | `engineHooks` struct, stage 1 (fields move; section 8) | — | yes | 8.4 |
| E7 | Back-pointer, group C: 35 ambient uses → `runtimeDeps` (which gains `perf` and `activity`) | — | yes | existing suite |
| E8 | Back-pointer, groups A and D: `runConfig` value and `flow.hooks` | E6, E7 | yes | existing suite |
| E9 | `protectedRootSet` and `housekeeper` | E1, E3, E8 | mostly | 1.7(3), 2.6(3) |
| E10 | `observation` type (section 2.4); moves about 30 methods | E2, E9 | yes for the move | section 2.5 suite under `-race` |
| E11 | `shortcutRoots` type; start-up inversion; planner, `exec`, `perms` as named inputs; delete `engineFlow.engine`, the six nil-checks, `newRuntimeDeps(engine)` | E8–E10 | mostly | 1.7(2) |
| E12 | `engineHooks` stage 2: hooks and clock fixed at construction; production hooks into `EngineMountConfig` | E6, E11 | no | 8.4 |

E0–E6 are independent of each other except as marked and are small. E7–E12 touch nearly every `engine*.go` file; run them as one uninterrupted series, either entirely before the split starts or entirely after it ends, never interleaved with the split's own PRs. Every PR here changes the declaration and line inventories `end-state.md` quotes (for example "754 decls / 12,503 lines" for `sync`, `end-state.md:24-59`); regenerate them before the split starts.

### 10.2 Must wait for the split

| PR | Content | Why it must wait |
|---|---|---|
| X1 | `syncsched` package (section 3) | Hard constraint: it needs `trackedAction`, `ScopeKey`, `activeScope`, `RetryWorkKey`, `actionCompletion` from a package that `sync` also imports; today they are in `sync` itself (import cycle). Also needs E3 (no locks) and E4 (no row cache in the ledger). The split plan must be amended so `dep_graph.go`, `active_scopes.go`, `scope.go` go to `syncsched`, not `syncexec` (`end-state.md:577-579`) — otherwise they are moved twice |
| X2 | Per-package hooks structs for `syncexec`, `syncstore`, `syncobserve`, replacing the six planned `…ForTest` functions (`end-state.md:403-428`) and the planned identifier check | The packages do not exist yet |

### 10.3 Either side, with a preference

| PR | Content | Preference |
|---|---|---|
| X3 | Executor and worker-pool constructors (section 7); `remoteParentChecks` out of the config | After the split, with the split plan amended to drop `SetRuntimeOptions` and the `executorConfig` `…ForTest` setters (`end-state.md:220-240, 403-428`). Before the split is possible (all files are in one package today) but changes the 44-symbol `sync → syncexec` edge the plan enumerates (`end-state.md:82`). Depends on E12 for the clock |

### 10.4 Item → PR map

Item 1 → E7, E8, E9, E11. Item 2 → E1, E2, E10. Item 3 → X1. Item 4 → E3. Item 5 → E4. Item 6 → E5. Item 7 → X3. Item 8 → E6, E12, X2. Item 9 → no PR (numbers); section 9.3 → E0.

---

## 11. Not read

Claims that depend on these are marked SUSPECTED in the text.

- `engine_log_fields.go`, `engine_result_classify.go` (decides `ScopeEvidence`; reachability of D3 depends on it), `watch_condition_log.go`.
- `executor.go:280-1048`, `executor_delete.go`, `executor_preconditions.go`, `executor_transfer.go`, `executor_conflict.go` bodies — including the nil-`trash` and nil-`pendingCreates` branches.
- `engine.go:424-545` (the start-up methods called from `engine_startup.go:15-36`) and `Engine.collector`'s declaration.
- `shortcut_root_lifecycle.go` outside `:40-99` and `shortcut_alias_mutation.go` (18 of the 20 + 3 shortcut `Engine` methods); the field set of the proposed `shortcutRoots` type.
- `engine_current_plan.go:732-752` (re-read needed for the planner's full field use); `engine_watch_maintenance.go:59-76`.
- `engine_watch_state.go:84-123` was read earlier but the element type of `skippedItems` was not recorded; `engine_watch.go:379-389` (the protected-root event sink) and whether every observer send is context-aware during drain.
- `engine_watch_loop.go:272-676` beyond the lines cited (in particular `isBootstrapQuiescent` `:672-676` and `handleWatchDispatch`).
- The bodies of the pinning tests: only `engine_retry_trial_test.go:1-60` was read; every other test is cited by name and line from `grep "^func Test"`.
- `internal/multisync/orchestrator_run_once.go:260-420` bodies (the function at `:282` is unnamed here), `orchestrator_watch.go:814-990`, `shortcut_child_artifacts.go` bodies.
- The store fault seam's API (`store_fault_seam_test.go`).
- Which design doc governs `store_retry_work.go` and `store_scope_admin.go`.
- Where `RetryWorkRow` and `RetryWorkFailure` (`store_types.go:101, 43`) land in the split.
- Locks outside the engine files (`Baseline.mu`, `SyncStore` mutexes, observers, scanner, `dirtyBuffer`) were located, not analysed.
- No test, build, vet or devtool command was run; every "fails today" in the tests-first lists is a prediction from reading the code, not an observed failure.
