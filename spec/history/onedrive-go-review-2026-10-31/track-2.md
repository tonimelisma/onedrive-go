# Track 2 — Concurrency, runtime lifecycle, long-running stability

Repository: ~/src/onedrive-go at 26b3bc8b (Go 1.26.10). Read-only review; `go build ./...` passes; `go vet -copylocks ./internal/sync/` is clean. No tests were run.

Target deployment: `onedrive-go sync --watch`, months-long, 2 cores, 6.7 GB, ~129,000 files, under a wrapper in a terminal session (no NOTIFY_SOCKET).

Overall verdict for this track: **GO WITH CAVEATS**. No critical finding (no data-loss, corruption, or silent-stop hang was found by tracing). Three medium findings: an unrecovered panic boundary around the per-mount watch loop (contradicts the documented isolation promise), content hashing that ignores context (shutdown and the local event loop block for a whole-file read), and an unsynchronized shared slice (`Engine.protectedRoots`) written from two background goroutines and read by the watch loop. Plus low findings and operational caveats below.

---

## 1. Goroutine inventory (every `go` statement in non-test code under internal/)

| # | file:line | What it runs | Owner / lifetime | Termination | Panic handling | Verdict |
|---|-----------|--------------|------------------|-------------|----------------|---------|
| 1 | internal/cli/signal.go:27 | signal handler (first signal cancels, second `os.Exit(1)`) | `shutdownContext`, one per command; stopped by returned `cancel` | `ctx.Done()` before a signal; after the first signal it waits on `parent.Done()` (never fires for `context.Background()`) | none (no risk) | HOLDS WITH CAVEAT — see F2-4 |
| 2 | internal/cli/sync_notify.go:173 (`startSyncWatchdog`) | systemd WATCHDOG=1 keepalive | `runSyncDaemonWithFactory`; `stop()` cancels and waits `done` | watchdogCtx cancel | n/a (nil notifier → not started) | HOLDS. No-op without NOTIFY_SOCKET (sdnotify.Open returns nil,nil) |
| 3 | internal/multisync/control_socket.go:128 | `http.Server.Serve` on unix socket | controlSocketServer; `Close` → Shutdown, waits `done` | listener closed | net/http recovers handler panics | HOLDS |
| 4 | internal/multisync/control_socket.go:138 | `<-ctx.Done()` then `Close` (1 s timeout) | same server | ctx cancel | n/a | HOLDS (Close is idempotent: ownerLock.release guards with mu/nil) |
| 5 | internal/multisync/watch_config_file.go:55 | config-file poll + SIGHUP | `watchConfigReloadTriggers`; stop fn cancels, `signal.Stop`, waits `done` | ctx cancel | n/a | HOLDS |
| 6 | internal/multisync/orchestrator_watch.go:334 | **per-mount `engine.RunWatch` (or final-drain RunOnce)** | `startWatchRunner`; `watchRunner.cancel` + `<-done` | RunWatch returns on mountCtx cancel or error; `defer mountCancel()` cleans observers | **none** | **GAP — F2-1** |
| 7 | internal/multisync/orchestrator_run_once.go:95, 114 | one-shot per-mount work | `runIndexedMountWork`, WaitGroup | fn returns | `MountRunner.run` recovers and logs stack (mount_runner.go:38) | HOLDS (one-shot only) |
| 8 | internal/multisync/orchestrator_run_once_children.go:135 | one-shot child runs for a parent | `oneShotChildRuns.childRunGroup` | fn returns | via MountRunner | HOLDS (one-shot only) |
| 9 | internal/sync/worker.go:102 | worker loop (×max(4,transfer_workers)) | workerPool; `Stop()` cancels + `wg.Wait()` | pool ctx cancel or dispatchCh closed | `safeExecuteAction` recovers, logs Error+stack, reports failed completion | HOLDS (worker.go:151-172; `TestWorkerPool_PanicRecovery`) |
| 10 | internal/sync/worker.go:105 | `wg.Wait()` then close(completions) | workerPool | when all workers exit | n/a | HOLDS (`closeOnce` guards double close with `Stop`) |
| 11 | internal/sync/buffer.go:143 | dirty-buffer debounce loop | `FlushDebounced`; owner = watch runtime | ctx cancel → `finalDrain`, close(out) | none (pure timer logic) | HOLDS. `armedTimer.stop` drains a fired tick; Go ≥1.23 semantics make this safe anyway |
| 12 | internal/sync/engine_watch.go:419 | local observer (`watchLocalWithFallback`) | `startObservers`; obsWg (never waited — see note) | ctx cancel; sends exactly one error on `errs` (cap 2) | `recoverObserverPanic` → fatal observer error, logs stack (engine_watch_panic_recovery.go) | HOLDS (`TestRunWatch_LocalObserverPanicFailsMountInsteadOfCrashing`) |
| 13 | internal/sync/engine_watch.go:525 | socket.io wake source `Run` | `startSocketIOWakeSource`; `pipe.cleanup` closes `socketIOWakeStop` and waits `socketIOWakeDone` | wakeCtx cancel (ctx or stopCh) | **none** — parses external websocket frames (socketio_protocol.go) | HOLDS WITH CAVEAT (part of F2-1) |
| 14 | internal/sync/engine_watch.go:530 | stopCh → wakeCancel bridge | child of #13 | `wakeCtx.Done()` | n/a | HOLDS |
| 15 | internal/sync/engine_primary_root_watch.go:20 | mount-root remote poller | `startPrimaryRootWatch` | ctx cancel | `recoverObserverPanic` | HOLDS |
| 16 | internal/sync/engine_primary_root_watch.go:59 | drive-root remote observer `Watch` | `startPrimaryRootWatch` | ctx cancel; `emitWatchBatch` selects on ctx; `waitApplied(ctx)` | `recoverObserverPanic` | HOLDS, but it executes `rt.executeDriveRootFullObservation` in its goroutine (delta-expired recovery) → **writes `Engine.protectedRoots` (F2-3)** |
| 17 | internal/sync/engine_watch_maintenance.go:130 (`runFullRemoteRefreshAsync`) | periodic full remote refresh (24 h cadence) | watch runtime; `refreshActive` flag prevents overlap; result via `refreshResults` (cap 1) | ctx cancel; `finishFullRemoteRefresh` non-blocking fallback | **none** | **GAP — F2-1 / F2-3** |
| 18 | internal/sync/socketio_conn.go:86 | websocket `readPackets` | `runConnection`; readCtx canceled on return | `conn.Read(ctx)` error or ctx | none (inside #13) | HOLDS |
| 19 | internal/sync/scanner.go (errgroup `g.Go` inside hashPhase, line ~277) | bounded hash workers (`checkWorkers`) | FullScan; `g.Wait()` | per-job `gCtx.Err()` check; the hash itself has no ctx | recovers per job, logs Error+stack, converts to skipped item | HOLDS (bounded by `SetLimit`; `TestHashPhase_PanicRecovery`) |
| 20 | internal/perf/logging.go:47 | periodic perf log | `perf.Session`; `Complete` closes stopCh, waits doneCh | stopCh | n/a | HOLDS |
| 21 | internal/graph/auth_browser.go:108 | OAuth callback server | login flow; server shutdown | listener closed | net/http | HOLDS (login only) |
| 22 | internal/cli/get.go:317, get_shared.go:173/181 | per-file download goroutines | `state.wg`; semaphore `state.sem` acquired with ctx | ctx/sem | none | HOLDS WITH CAVEAT: goroutines are spawned per item (fan-out scales with directory size; only the *running* count is bounded by the semaphore). CLI `get` only, not watch |
| 23 | internal/cli/shared_discovery.go:296 | shared-target enrichment | wg + `sema` | ctx | none | HOLDS (bounded by sema before spawn) |
| 24 | internal/cli/drive_catalog.go:173 | per-token drive discovery | wg | returns | none | HOLDS (bounded by token count) |

Notes on the table:
- `obsWg` in `startObservers` (engine_watch.go:349) is `Add`ed and `Done`d but never `Wait`ed. The drain loop instead waits for `activeObservers == 0`, which is decremented when each observer's single error send is received (engine_watch_loop.go:457-475). Functionally equivalent; the WaitGroup is dead code.
- The orchestrator's `RunWatch` deferred cleanup (`wr.cancel(); <-wr.done`, orchestrator_watch.go:61-69) waits **unbounded** for each engine. The only bound on a stuck drain is the second signal (`os.Exit(1)`) or systemd's `TimeoutStopSec`. Everything in the drain path respects ctx except content hashing (F2-2).

---

## 2. Per-area trace and verdict

### 2.1 Goroutine ownership — HOLDS WITH CAVEAT
Every goroutine has an owner and a termination path tied to a context (table above). No fire-and-forget goroutine was found in watch mode. Per-item fan-out exists only in CLI `get`/`get-shared` (bounded by a semaphore at run time, not at spawn time). Channel sends after a receiver has gone: all background senders (`refreshCh`, `refreshResults`, `trialCh`, `retryTimerCh`, `recoveryScanRequests`, `recoveryScanDue`, `HashRequests`, `skippedCh`, `protectedRootEvents`, `localBatches`) are either non-blocking `select{default}` or select on ctx. The remote observer's `emitWatchBatch` blocks on `batches` but also on ctx. Panic recovery exists at worker and observer boundaries but **not** around the per-mount watch loop or the full-refresh goroutine (F2-1).

### 2.2 Channels and select loops — HOLDS
Directions are declared on all function signatures I read (`chan<- actionCompletion`, `<-chan dirtyBatch`, etc.). Senders close: `localBatches`/`protectedRootEvents` closed by the local observer goroutine; `batches` by the remote observer; `completions` by the pool; `out` by the debounce loop. The watch loop's main select (engine_watch_loop.go:216-255) has a `ctx.Done()` case; the drain select (engine_watch_drain.go:66-73) deliberately does not — drain only starts after ctx is done and its three sources are all guaranteed to close/settle once ctx is canceled (workers exit → completions closes; refresh goroutine returns; observers send their exit error). No unbuffered send under a lock found: `dirtyBuffer.signalNewLocked` sends under `mu` but non-blocking into a cap-1 channel.

### 2.3 Mutexes — HOLDS WITH CAVEAT
Documented guards: `dirtyBuffer.mu` (dirty, fullRefresh, notify), `depGraph.mu` (actions), `SyncStore.baselineMu` (baseline cache), `SyncStore.contentDriveMu`, `Baseline.mu` (three indexes), `watchTimerState.timerMu` (timer pointers), `tokenBridge.mu`/`refreshMu`, `ownerLock.mu`, `perf.Session.mu`, `Orchestrator.statusMu`. Nesting observed: `baselineMu → contentDriveMu` (Load) and `baselineMu → Baseline.mu` (reloadBaselineCache); `Baseline.ForEachPath` holds `Baseline.mu` while calling back and documents that callbacks must not touch the Baseline — no reverse order found. No I/O under a lock except `SyncStore.Load` doing the SQL read under `baselineMu` (intentional: it is the cache fill and callers are workers that would otherwise race to fill it). `timerMu` is an RWMutex used for pointer swaps — style deviation from "prefer sync.Mutex", harmless. `go vet -copylocks` clean. Caveat: `Engine.protectedRoots` is a shared mutable field with **no** guard and multiple writer goroutines (F2-3).

### 2.4 Watch mode event pipeline — HOLDS WITH CAVEAT
Traced inotify → `fsnotifyWrapper` → `watchLoop` (observer_local_handlers.go:45-100) → `TrySendLocalObservationBatch` (non-blocking, cap 256) → watch loop `applyLocalObservationBatch` → SQLite `local_state` patch → `dirtyBuffer.MarkDirty` → debounce (5 s quiet, 30 s max wait) → `runSteadyStateReplan` → **full local scan** + plan from committed truth.
- IN_Q_OVERFLOW: `fsnotify.ErrEventOverflow` → suspect + armed recovery scan with backoff (handleWatcherError:208-250). Dropped batch/hash request → `requestRecoveryScan`. Recovery scan paced at max(10 s, 2×last scan duration), doubling under a storm, capped at the safety interval. Good.
- Watch-add failure at the inotify limit: ENOSPC on a new directory is reported, the directory stays unwatched, and `rewatchUnwatchedDirectories` re-adds up to 256 per full scan; a limit at watcher creation/initial walk falls back to periodic scans and retries the watcher at 10 min doubling to 1 h. Capacity warning at 80 % of `max_user_watches` (inotify_linux.go:50). For 129k files the directory count matters; the operator should confirm `max_user_watches` ≥ 2× directory count.
- Directory create race: watch is added **before** the new directory is scanned (handleDirectoryCreate:452-470), and the directory row is emitted before its contents. Correct.
- Rename pairs: delete half is sent immediately, create half reuses the synced hash on identity match or defers the hash (D-080). A pair split by the 30 s deadline timer is theoretically possible but the window is microseconds; the planner composes moves by identity within one snapshot. Accepted.
- Poll timer/wake: remote `sleepWatch` uses `time.NewTimer` + `Stop`; backoff capped at the poll interval; socket.io wake coalesced into a cap-1 channel and drained before each poll.
- Hours of no events: the loop blocks in `select` with a 10 s maintenance ticker; nothing accumulates. Timers: one retry timer, one trial timer, one refresh timer, each replaced via `reset*Timer` (stop old first). No `time.After` in loops.
- After failures: observer exit → `handleObserverExit` → when both observers are gone the mount fails → orchestrator restarts it with 5 s→5 min doubling backoff, reset after a 5-min healthy run (watch_reporting.go:11-23). Full-refresh failure → `markFullRefreshIfIdle` → dirty buffer `MarkFullRefresh` → next replan; timer is re-armed from persisted `NextFullRemoteRefreshAt` when a batch applies.
- Caveat 1 (F2-5): every steady-state replan runs a full 129k-entry local scan on the loop goroutine; a second, independent full scan runs in the observer every 5 min.
- Caveat 2 (F2-2): file hashing for a created or written file runs synchronously on the observer's event loop; a multi-GB file stalls event consumption for the read's duration (fsnotify's Events channel is unbuffered), which is exactly the condition that produces IN_Q_OVERFLOW under concurrent churn. Self-healing via recovery scan, but costly.
- Memory growth checked: `writeChurn` pruned at safety scans; `DirNameCache`, `CollisionPeers`, `RecentLocalDeletes` rebuilt each safety scan; `PendingTimers` deleted when fired/canceled; `watchedDirs` ∝ directories; `supersedeStreaks` pruned per plan (engine_runtime_completion.go:763-782); `lastRemoteBlocked` replaced wholesale; `heldByKey`/`heldByScope` released on dispatch; `rowsByKey` mirrors `retry_work` rows; perf collectors removed with the mount. One unbounded-in-principle set: `excludedSymlinkPaths` (F2-8, low).

### 2.5 Worker pool and executor — HOLDS WITH CAVEAT
Concurrency = max(4, transfer_workers) per mount, flat pool on an unbuffered dispatch channel (watch mode), so undispatched work stays engine-owned. A panicking worker is recovered and reported as a failed completion; the engine owns `depGraph.Complete` (R-6.8.9). Dependency graph: `Complete` copies dependents under the lock, decrements atomically outside; a node is deleted on completion so late `Add`s treat it as satisfied; `Register` sets `depsLeft=1` sentinel until `WireDeps`. Held/blocked actions are released by due timers (`drainDueHeldWorkNow`), and a pending replan retires the outbox and released frontier rather than wedging them. Retry fairness: durable `retry_work` with `retry.Reconcile` 1 s→1 h ±25 % jitter; one failing item cannot starve others because failed work leaves the worker immediately (R-6.8.7) and re-enters only when due. Shutdown: a worker mid-transfer sees ctx cancel through the HTTP request; the detached outcome commit is bounded (30 s, `outcomeCommitTimeout`). Caveat: the pre-upload hash (`transfer_upload.go:259,350`) ignores ctx (F2-2).

### 2.6 Shutdown — HOLDS WITH CAVEAT
`shutdownContext` (one per command, root.go:664) cancels on SIGINT/SIGTERM; second signal → `os.Exit(1)`. `RunWatch` → `beginWatchDrain` seals admission, completes the outbox as shutdown, keeps consuming worker completions (recording finished transfers through `WithoutCancel` commits), and ends when completions close, refresh finishes, and both observers have exited. In-flight state: download partials are kept on cancel (transfer_download.go:628-681); upload sessions are persisted before fragments (transfer_session.go:186) and deleted only on success (transfer_upload.go:454). Control socket and `.lock` are removed by `controlSocketServer.Close` (also triggered by the ctx-watch goroutine); the root lock is released in `Engine.Close` after the store checkpoint. Exit codes: clean stop → 0 (matches docs/running-as-a-service.md), interrupted-with-error → 130, 77/78 reserved for no-restart. Wrapper in a terminal session: `sdnotify.Open` returns a nil notifier without `NOTIFY_SOCKET`; every method is nil-safe; watchdog goroutine is not started. Caveats: (a) the process has no internal drain deadline — a goroutine that ignores ctx delays exit until the second signal (F2-2 is the one such path found); (b) the signal goroutine is not released after a first signal (F2-4).

### 2.7 SQLite — HOLDS WITH CAVEAT
`modernc.org/sqlite`, DSN pragmas on every connection: WAL, synchronous=FULL, foreign_keys, busy_timeout 5000 ms, journal_size_limit 64 MB; `SetMaxOpenConns(1)` (sole-writer). `status` opens its own `mode=ro&query_only(1)&busy_timeout(1000)` connection (store_inspect.go:189-197), so R-6.3.2 holds under WAL (readers never block the writer). Checkpointing: `PRAGMA wal_checkpoint(TRUNCATE)` only in `Close` (with `WithoutCancel`); otherwise SQLite's passive auto-checkpoint. With one in-process connection there is no in-process reader to starve it; a cross-process `status` holds a read snapshot only for the duration of a few queries. Transactions are short and local (no network inside a tx); the largest is `replaceLocalStateTx`, which reads all `local_state` rows and writes only the diff — O(129k) row reads per safety scan and per replan, no network. A ctx cancel mid-transaction is rolled back by database/sql and surfaced as an error; finished-transfer commits are detached from cancellation. Single-connection hazard (a nested query while a `*sql.Rows` is open deadlocks) — spot check of every store file shows matching `QueryContext`/`Close` counts; no nested pattern found in the files read. Caveat: on a months-long run, WAL growth between auto-checkpoints is bounded by `journal_size_limit` only after a checkpoint completes; if `status` were polled continuously (e.g. a dashboard every second) a passive checkpoint could repeatedly fail to reset the WAL. Not the stated deployment.

### 2.8 Token refresh under concurrency — HOLDS WITH CAVEAT
In-process: `tokenBridge.ForceRefresh` serializes on `refreshMu` and shares one forced refresh per 30 s (auth_token.go:174-213); `oauth2.ReuseTokenSource` serializes silent refreshes; `OnTokenChange` persists atomically via `tokenfile.Save` (write-temp + rename). Cross-process (`sync --watch` plus `status`/`ls`): no file lock; two processes can refresh with the same refresh token and both write the file; last writer wins. Both access tokens remain valid for their lifetime, so the running watch is unaffected until its next refresh, which uses its own in-memory refresh token. Whether Microsoft invalidates the older refresh token on rotation is a provider question I cannot settle from the code (F2-9, speculative, low).

### 2.9 Timers and clocks — HOLDS
No `time.After` in loops. `armedTimer.restart` stops and drains before `Reset` (buffer.go:155-169). Engine timers go through the injected clock; callbacks only signal buffered channels and never touch loop state. Due instants (`NextRetryAt`, `NextTrialAt`, `NextFullRemoteRefreshAt`, trash `TrashedAt`) are wall-clock UnixNano in SQLite, compared against `now()`; armed durations are monotonic. A large NTP step shifts "due" by the step for already-persisted instants (bounded by the 1 h retry cap, 24 h refresh cadence, 30-day trash retention); nothing spins or fires early in a harmful way. System suspend is not a concern for this server.

### 2.10 Panics — HOLDS WITH CAVEAT
All 24 `panic(` sites in non-test code are internal-invariant panics (unknown enum values, accessor misuse, nil clock, dep-graph bookkeeping, `http.DefaultTransport` type) or gated behind `engine.assertInvariants` (off in production, engine_runtime_invariants.go:10-12). None is reachable from API payloads, filesystem state, or config by the paths I traced: external input goes through `json.Unmarshal` and explicit validation (e.g. socketio_protocol.go checks the `42` prefix before slicing). Recovers: worker (logs Error + Debug stack), observers (Error + stack, fatal observer error), hash workers (Error + stack, skipped item), one-shot MountRunner (Info + stack). The gap is the **absence** of a recover around the watch-loop goroutine and the full-refresh goroutine (F2-1).

### 2.11 Memory/CPU on a long run — HOLDS WITH CAVEAT
Retained between passes: the `Baseline` (three maps, ~129k entries, tens of MB), `watchedDirs` registry, retry ledgers (∝ failing items), perf counters (fixed set). Per pass but transient: scan results and `local_state` rows (129k), planner inputs (local + remote rows), `replaceLocalStateTx`'s map of current rows. No O(n²) over the tree in the hot path: `watchRegistry` keeps a parent→children index precisely to avoid a linear scan per event; collision checks are per-directory maps; `replaceLocalStateTx` is a hash diff. The CPU caveat is frequency, not complexity (F2-5): on this host each replan is a full lstat walk of 129k entries plus a 129k-row read, serialized on the loop goroutine while workers sit idle.

---

## 3. Findings

### F2-1 — MEDIUM — Per-mount watch loop and full-refresh goroutine have no panic boundary
- **Where**: internal/multisync/orchestrator_watch.go:334-379 (`go func() { ... engine.RunWatch(mountCtx, mode, opts) ... }()`), internal/sync/engine_watch_maintenance.go:130-133 (`go func() { result := rt.performFullRemoteRefresh(ctx, bl); ... }()`), internal/sync/engine_watch.go:525-545 (socket.io wake source).
- **Rule**: CLAUDE.md "Goroutine ownership" / Error Handling; spec/design/sync-control-plane.md:54 ("per-mount panic/error isolation"), :138 ("If a parent runner exits, panics, ... multisync immediately cancels all child runners"); spec/design/sync-engine.md:855 ("Observer Panics Fail One Mount, Not The Process").
- **Scenario**: an internal invariant panic in the watch loop (e.g. engine_runtime_graph.go:15 `complete unknown action ID`, engine_runtime_completion.go:444, or a nil dereference in a handler) or in the full-refresh goroutine (which converts Graph delta pages — external input — into events) terminates the **whole process**, taking every other mount down without a drain, leaving the control socket file and no final STATUS. The MountRunner recover (mount_runner.go:38) is applied only to one-shot `RunOnce`; watch mode calls `engine.RunWatch` directly. The doc text promises isolation that the code only provides for observers.
- **Evidence**:
  ```go
  // orchestrator_watch.go:361
  if watchErr := engine.RunWatch(mountCtx, mode, opts); watchErr != nil {
  ```
  no `recover()` anywhere in that closure; `grep -rn "recover()" internal/multisync` → only mount_runner.go:38.
- **Consequence on this host**: crash → the wrapper in a terminal session restarts the binary; under systemd `Restart=on-failure` after 30 s. Not silent, not data loss (WAL + partials + sessions survive), but a single-mount bug becomes a process-wide restart loop, and the crash happens without the drain the design promises.
- **Existing tests**: `TestRunWatch_LocalObserverPanicFailsMountInsteadOfCrashing` covers observers only; none for the loop or refresh goroutine.
- **Proposed regression test**: inject an `engineRunner` whose `RunWatch` panics into `startWatchRunner`; assert `RunWatch` of the orchestrator keeps running, the mount is reported failed via `runnerEvents`, and sibling mounts stay up. Second test: a `socketIOFetcher`/delta fetcher that panics during `performFullRemoteRefresh`; assert the mount fails with a logged stack instead of crashing the test binary.
- **Confidence**: CONFIRMED by tracing.

### F2-2 — MEDIUM — Content hashing ignores context: shutdown and the local event loop block for a whole-file read
- **Where**: internal/driveops/hash.go:35 `func ComputeQuickXorHash(fsPath string) (string, error)` (no ctx; `io.Copy` to EOF). Callers on blocking paths: internal/driveops/transfer_upload.go:259 and :350 (`tm.hashFunc(localPath)` in a worker before every upload), internal/sync/observer_local_handlers.go:532 (`stableHashOrEmpty`, synchronous in `handleCreate`/`scanNewDirectory`) and :1007 (`HashAndEmit` on the observer event loop), internal/sync/scanner.go:1176 (`computeStableHashWith`, up to 3 full re-reads on `errFileChangedDuringHash`).
- **Rule**: CLAUDE.md "In-flight operations are interruptible. Every long operation checks context cancellation"; "Timeouts on everything external ... every file operation that could hang (NFS, FUSE)"; R-6.3.6 cooperative shutdown.
- **Scenario A (shutdown)**: SIGTERM arrives while a worker is hashing a 20 GB file before upload on the dev host's storage (~100–150 MB/s) → `pool.Stop()` (engine_watch.go:243) and the drain's `p.completions == nil` condition wait 2–3 minutes; systemd's default `TimeoutStopSec=90s` would SIGKILL; in a terminal session the user needs the second Ctrl-C. The same holds for the observer goroutine: `drainLoopDone` requires `activeObservers == 0`, and the observer cannot exit while `HashAndEmit` is reading.
- **Scenario B (event loop stall)**: a large file is created or written in the sync folder; `watchLoop` is the only reader of `watcher.Events()` (unbuffered in fsnotify by default) and it is inside `computeStableHashWith` for the duration of the read; concurrent activity (a `git checkout`, a photo import) fills the kernel queue (`max_queued_events`, default 16384) → IN_Q_OVERFLOW → recovery full scan of 129k entries. Self-healing but turns one big file into a storm of work. The D-080 fix only covers the *rename* case.
- **Evidence**:
  ```go
  // observer_local_handlers.go:1007 (runs on the watchLoop goroutine)
  hash, err := computeStableHashWith(req.FsPath, o.hashFunc)
  // transfer_upload.go:350 (worker goroutine, no ctx reaches the read)
  localHash, err := tm.hashFunc(localPath)
  ```
- **Existing tests**: none exercise cancellation during a hash; `TestWatch_EventOverflowRunsRecoveryScan` covers the recovery path, not the cause.
- **Proposed regression test**: inject a `hashFunc` that blocks on a channel; cancel the pool/observer ctx; assert `workerPool.Stop()` and the local observer return within a bounded time (today they do not). Second: a slow hash in `HashAndEmit` while N create events arrive; assert they are consumed (would require moving hashing off the loop or a ctx-aware reader).
- **Confidence**: CONFIRMED (blocking without ctx); PLAUSIBLE (magnitude on this host).

### F2-3 — MEDIUM — Data race on `Engine.protectedRoots` between the watch loop and background observation goroutines
- **Where**: write: internal/sync/protected_roots.go:17 and :24 (`e.protectedRoots = ...`, no lock). Writers off the loop goroutine: internal/sync/engine_run_once.go:440 (`observeRemoteFullWithShortcutTopology` → `eng.refreshProtectedRootsFromStore`), reached from (a) the remote observer goroutine via the `SetDeltaExpiredRecovery` callback (engine_primary_root_watch.go:34-44 → `rt.executeDriveRootFullObservation`) and (b) the full-refresh goroutine (engine_watch_maintenance.go:130 → `performFullRemoteRefresh` → `executePrimaryRootObservation(ctx, bl, true)` → `executeDriveRootFullObservation`). Readers/writers on the loop goroutine: engine_current_plan.go:358-364 (`observeLocal`, every replan), engine_housekeeping.go:15-25, engine_watch.go:96, engine_current_plan.go:256 and :330, engine_watch_loop.go:330.
- **Rule**: CLAUDE.md "Mutex scope: A mutex protects data ... Comment what fields it guards"; R-6.3.4 (single documented owner of mutable state). The `Engine` struct comment calls it "an immutable dependency container" (engine_runtime_types.go:3-5), which this field violates.
- **Scenario**: the 24-hourly full refresh of a 129k-item drive takes minutes; a local change during that window triggers a replan on the loop goroutine, which reads `eng.protectedRoots` while the refresh goroutine assigns it. A slice header is three words; a torn read yields a wrong length/pointer → wrong protected roots for that scan (shortcut boundaries not protected for one pass) or an index panic in the loop goroutine, which has no recover (F2-1). With `shortcutNamespaceID == ""` (no shared-folder shortcuts) the write is `nil`, still a race by the memory model and still torn-readable.
- **Evidence**:
  ```go
  // protected_roots.go:24 (called from three goroutines)
  e.protectedRoots = protectedRootsForShortcutRoots(records, e.shortcutNamespaceID)
  // engine_current_plan.go:364 (loop goroutine, concurrently)
  obs.SetProtectedRoots(eng.protectedRoots)
  ```
- **Existing tests**: none run a full refresh concurrently with a replan under `-race`.
- **Proposed regression test**: under `-race`, start `RunWatch` with a fake fetcher whose full enumeration blocks until a local change has triggered a replan; assert no race report. Unit variant: call `observeLocal` and `performFullRemoteRefresh` concurrently against one Engine.
- **Confidence**: CONFIRMED by tracing (race exists); PLAUSIBLE (observable failure frequency).

### F2-4 — LOW — Signal handler goroutine is not released after a first signal
- **Where**: internal/cli/signal.go:38-45.
- **Rule**: R-6.3.6 "The signal handler shall be released when the command completes."
- **Scenario**: first SIGINT → `cancel()`; the goroutine then waits on `sigCh` or `parent.Done()`. `parent` is `context.Background()` (root.go:664), so after the command returns and the caller's `cancel` runs, the goroutine and the `signal.Notify` registration persist until process exit. Harmless for the binary (main exits right after), a leak for in-process callers.
- **Evidence**:
  ```go
  select {
  case <-sigCh:
      logger.Warn("Forcing exit"); os.Exit(1)
  case <-parent.Done():   // never fires for Background()
      return
  }
  ```
- **Existing tests**: `TestShutdownContext_CancelReleasesSignalGoroutine` only tests the no-signal path; `TestShutdownContext_SecondSignalForcesExit` tests the exit.
- **Proposed test**: send one signal, call `cancel`, assert the goroutine count returns to baseline (would fail today).
- **Confidence**: CONFIRMED.

### F2-5 — LOW (operational caveat, by design) — Each steady-state replan is a full local scan of the tree, serialized on the watch loop
- **Where**: internal/sync/engine_watch_replan.go:277 → engine_current_plan.go:302-315 → :353-378 (`newLocalObserver(...).FullScan(ctx, eng.syncTree)`), then `commitLocalSnapshot` → `replaceLocalStateTx` (store_local_state.go:281-318: reads every `local_state` row, writes the diff). Independently, the observer's safety scan does the same every 5 min (observer_local_handlers.go:1341).
- **Rule**: none violated; spec/design/sync-engine.md's snapshot-first model requires it. Listed because on a 2-core host with 129k files it is the dominant steady-state cost: with the 5 s debounce and 30 s max wait, sustained local churn means up to two 129k-entry lstat walks per minute plus two 129k-row SQLite reads, during which no completions are consumed and no dispatch happens (`perf` counters `replan_idle_local_refresh_ms`, `replan_idle_planning_ms` measure exactly this).
- **Proposed check rather than test**: after a week, read `onedrive-go perf` (`GET /v1/perf`) for `observe_runs`, `observe_time_ms`, `replan_idle_local_refresh_ms`; if observe time is a large fraction of wall time, raise the debounce.
- **Confidence**: CONFIRMED (mechanism); magnitude PLAUSIBLE.

### F2-6 — LOW — systemd watchdog keepalive does not reflect watch-loop health
- **Where**: internal/cli/sync_notify.go:96-110, :161-183. The keepalive goroutine ticks independently of the engine, so a wedged loop (e.g. stuck in F2-2 or a hung SQLite I/O) never trips `WatchdogSec`. docs/running-as-a-service.md:64 says "if it hangs, systemd restarts it", which is only true for a hang of the Go scheduler. Not relevant in a terminal session; relevant if the unit is adopted later.
- **Confidence**: CONFIRMED.

### F2-7 — LOW — No internal drain deadline
- **Where**: internal/multisync/orchestrator_watch.go:61-69 (`wr.cancel(); <-wr.done`), internal/sync/engine_watch_drain.go:55-74.
- **Rule**: CLAUDE.md "exit when the runtime settles. Second signal = immediate exit" — the code matches the rule; the caveat is that "settles" has no upper bound other than the second signal. Combined with F2-2 this is the only route to a long-stuck stop found. Acceptable per the project's own design; noted so the operator knows a stop can legitimately take minutes.
- **Confidence**: CONFIRMED.

### F2-8 — LOW — `excludedSymlinkPaths` grows for the life of a watch session
- **Where**: internal/sync/observer_local.go (field in `localWatchState`, comment: "Paths stay recorded until the same alias is observed as a real file/dir again or the observer is recreated").
- **Scenario**: a tool that creates uniquely named symlinks that are later deleted (build systems, package managers inside the sync root) adds one entry per unique path that is never removed (HandleDelete only forgets it when the path is not in the baseline *and* has an excluded ancestor). Growth is one string per unique symlink path, so megabytes over months at worst.
- **Confidence**: PLAUSIBLE / SPECULATIVE (depends on workload).

### F2-9 — LOW — Token file has no cross-process lock
- **Where**: internal/graph/auth_token.go:98-120 (`OnTokenChange` → `tokenfile.Save`), internal/tokenfile/tokenfile.go:76-96 (atomic write, no lock).
- **Scenario**: `sync --watch` and a concurrent `status`/`ls` both find the access token expired and both redeem the same refresh token; both writes succeed atomically; the file ends with whichever finished last. Each process keeps a valid in-memory token. Whether the provider revokes the earlier refresh token on rotation is not determinable from the code; if it does, the watch process would hit `invalid_grant` on its *next* silent refresh (about an hour later) and exit 77 (sign-in required).
- **Confidence**: SPECULATIVE.

---

## 4. Checked and found sound (brief)

- Worker pool: bounded, unbuffered dispatch in watch mode, panic-recovered, completions channel closed exactly once (`closeOnce`), detached outcome commit bounded at 30 s.
- Dirty buffer: single owner, non-blocking notify, correct timer stop/drain, deadline bounds the trailing debounce (30 s or 6× debounce).
- Observer error channel: cap 2 for exactly two single-shot senders; panic path and normal path are mutually exclusive per goroutine.
- Full-refresh overlap guarded by `refreshActive`; result channel cap 1 plus non-blocking fallback on shutdown; `refreshActive` cleared on apply, on drain drop, and on channel close.
- Remote observer: exponential backoff capped at the poll interval, wake coalescing, cursor-rejection tracker, delta-expired recovery through the full refresh with orphan detection; zero-event polls do not advance the token.
- Local observer fallback when inotify is unavailable: scans at once, then on the safety interval, retries the watcher 10 min → 1 h, warns once per cause, resets after a healthy hour.
- Orchestrator: single owner lock via flock with inode re-check; stale socket replaced only under the lock; control socket removed and lock released on every exit path (plus a ctx-driven close); runner restart backoff 5 s → 5 min with healthy-run reset; config-file poll with settle detection and SIGHUP reload; `runnerEvents` sends select on ctx.
- Shutdown bookkeeping: `drainAfterCanceledStepError` converts an in-step cancellation into a drain so timers are stopped; completion errors during drain are logged at Debug (cancellation) or Warn (anything else) and the next run rediscovers from truth; partial downloads and upload sessions survive.
- SQLite: WAL + synchronous=FULL + busy_timeout on every pooled connection; read-only `status` path; transactions short and network-free; `Close` checkpoints with `WithoutCancel`.
- Token refresh in-process: forced refresh serialized and rate-limited (one per 30 s) so a burst of 401s from parallel workers makes one token call.
- HTTP transports: dial 30 s, TLS 15 s, response-header 2 min, HTTP/2 ping 30 s/15 s, transfer stall watchdog 2 min — no request can hang indefinitely outside the body read, and the body read is watched.
- Timers: no `time.After` in loops; all `AfterFunc` callbacks only signal buffered channels; previous timers stopped before re-arming.
- `sdnotify`: nil-safe no-op outside systemd; `Open` returns `(nil, nil)` without `NOTIFY_SOCKET`.
- `go vet -copylocks ./internal/sync/` clean; `activeScope` values copied by design contain no locks.
