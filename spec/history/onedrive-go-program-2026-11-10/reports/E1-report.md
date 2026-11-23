# E1 report: guarded protected-roots read, dead code, stale comments

Worktree `~/src/onedrive-go-wt/engine-guarded-read`, branch `fix/engine-guarded-read`, base `83fe1f82`, one commit `9c871682` (not pushed).

## What changed
- `engine_housekeeping.go`: `cleanTransferArtifacts` reads the protected roots through `currentProtectedRoots()` (P1); its comment and `postSyncHousekeeping`'s say what they do.
- `engine_housekeeping_test.go`: new race test and its two helpers (`signalingHandler`, `seedRemovedSharedFolders`).
- `tracked_action.go`: `Cancel` field gone (P2); struct comment and `TrialScopeKey` comment ("Set by DispatchTrial") rewritten (P3).
- `worker.go`: the write `ta.Cancel = cancel` gone (P2); "pure executors" and `TrackedAction` comments fixed.
- `dep_graph.go`: `MarkTrial` gone (P2); `mu` comment says who calls the graph (P3); stale `TrackedAction` names, the duplicated `Add` doc, "pure" and the "concurrent Add/Register callers" comment in `Complete` fixed. Lock and atomic untouched.
- `engine.go`: `diskAvailableFn` field and assignment gone (P2); `protectedRootsMu` comment names the third writer (P3); housekeeping flags' comment says who reads and writes them (P3); "Build DepGraph" in the RunOnce doc.
- `protected_roots.go`: `refreshProtectedRootsFromStore` comment names the third writer (P3).
- `engine_runtime_owners.go`: `scopeLedger.mu` comment says who reads and writes `active` (P3). Lock untouched.
- `engine_watch.go` (two hunks only): stale `depGraph` capture and the cleanup log line it fed gone (P2).
- `executor.go`: `syncRootIdentity` field comment (P3); `executorConfig`/`executor` docs ("immutable", "no shared mutation", `NewExecution`).
- `spec/design/sync-engine.md`: ownership contract names all three off-loop writers and that housekeeping reads through the accessor; verification row for the new test.

## Test added
- `TestPostSyncHousekeeping_ReadsProtectedRootsUnderTheLock` (`// Validates: R-6.3.4`, the requirement the field comment and the sibling race test cite; it fits: the field's documented guard is what the test enforces): housekeeping loops on the test goroutine while a second goroutine acknowledges a final drain through `ShortcutChildAckHandle()` whose shortcut-root reconcile rewrites the protected roots. The overlap is built: the end of a refresh failure streak logs between housekeeping's refresh and its read, a lock-free slog handler hands that record to the acknowledging goroutine, and neither goroutine syncs with the other until the read is done. It also asserts the acknowledgement's write landed (the list is empty afterwards).

## P1 on the base
- The analysis's plain two-loop form (one root, ack + housekeeping loops) did not fail in `-race -count=20`: with one root the ack never writes the list (its release persists the state itself and the reconcile then changes nothing, so `refreshProtectedRootsFromStore` is not reached). A second root whose release is pending makes the reconcile write; with the overlap built as above, the base fails: 5 of 20 runs report the race (the detector reports one pair of stacks once per process).
- Write: `(*Engine).setProtectedRoots`, `protected_roots.go:36` (via `refreshProtectedRootsFromStore` <- `reconcileShortcutRootLocalState` `shortcut_root_lifecycle.go:75` <- `acknowledgeChildFinalDrain` `engine_shortcut_topology.go:55`).
- Previous read: `(*Engine).cleanTransferArtifacts`, `engine_housekeeping.go:96` (<- `postSyncHousekeeping`).
- After the fix: `-race -count=20` passes.

## P2 decisions
- `trackedAction.Cancel`, `depGraph.MarkTrial`, `Engine.diskAvailableFn`: no reader in any file of the module (file-name-only greps), deleted.
- Watch cleanup capture: the cleanup's in-flight line ("graceful shutdown: canceling in-flight actions ...") is what the drain start already logs from the live graph (`beginWatchDrain`, "graceful shutdown: sealing new work admission", `in_flight`), so the line went with the capture. The captured graph never held an action (`startRuntimeStage` replaces `sched.graph` for each plan and is the only registrar), so the line never printed.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...`: pass.
- `go test -count=1 ./internal/sync/...` (behind the lock): pass. `go test -count=1 ./internal/multisync/...`: pass.
- `-race -count=1`, 54 tests (housekeeping, `TestEngineClose_...`, `TestPurgeExpiredTrash_...`, worker pool and `worker_test.go`, dependency graph, `TestWatchPipelineCleanup_...`, `TestRefreshProtectedRootsFromStore_...`, the final-drain ack tests): pass.
- New test `-race -count=20` after the fix: pass.
- Lint `./internal/sync/...` (behind the lock): `0 issues.` on the second run; the first found one gocritic `hugeParam` on the test handler's by-value `slog.Record`, which `slog.Handler` fixes, answered with the same `//nolint:gocritic` `internal/synctest/logcapture.go:104` carries.
- Spec by hand: no spec file names `MarkTrial`, `diskAvailableFn`, `DispatchTrial`, `expectedSyncRoot`, the cancel field or the deleted log line; the new test exists; R-6.3.4 exists.

## Deviations
- The test needed a second shortcut root to make the ack write the list at all (see P1); the analysis's one-root loop does not reach the write.

## Also fixed
- `dep_graph.go`: `TrackedAction` names, duplicated `Add` doc, "pure", the `Complete` concurrency comment.
- `worker.go:45-48, 187-188, 329`: "pure executors", `TrackedActions`/`ActionCompletion` names, the per-action context comment.
- `executor.go:26-28, 87-89, 95`: "immutable"/"no shared mutation" claims, `NewExecution`.
- `engine_housekeeping.go:12-15`: `postSyncHousekeeping` doc named only two of its three cleanups.
- `engine.go`: RunOnce doc "Build DepGraph".

## Not fixed (seen)
1. `internal/sync/engine.go:79` `minFreeSpace` has no production reader (only `engine_config_test.go:96, :204` assert it) and its comment names a "startup disk-scope revalidation" no code does. Fix: delete the field, its assignment and the two assertions (the disk check reads `cfg.MinFreeSpace` through `WithDiskCheck`). Left because a test reads it.
2. `internal/sync/executor.go:249, :261` `SetDownloads`/`SetUploads` are called only by `worker_test.go`. Fix: set the fields in the test or delete the setters.
3. `internal/sync/shortcut_root_lifecycle.go:29, :72` `reconcileShortcutRootLocalState` lists the shortcut roots and replaces them in two separate store calls; it runs on the parent's loop (`engine_watch_loop.go:343`, `engine_current_plan.go:252`) and on the orchestrator's goroutine through the ack (`engine_shortcut_topology.go:55`), so two cycles can interleave. Fix: hand the ack's reconcile to the parent's loop, or serialize the reconcile.
4. `internal/sync/dep_graph_test.go`: `TestDepGraph_ConcurrentComplete`, `TestDepGraph_ConcurrentAddAndComplete`, `TestDepGraph_ConcurrentMultiAdd` exercise concurrent graph use production never has. Fix: the change that deletes `depGraph.mu` deletes or rewrites them.
5. `internal/sync/engine_watch.go:55-57, :171, :265`: "DepGraph"/"WorkerPool" names and a history comment ("Unlike the old approach"). Fix: name `depGraph`/`workerPool`, drop the history. Left because another branch is in this file.

## Unfinished
- Nothing.

## For the reviewer to test
- None beyond `internal/sync` and `internal/multisync` (run here).


## Second pass (done in review, 2026-11-14)

The review of the first hand-back (`R-E1-findings.md` in the scratch area) was applied by the reviewer of record, not by a second writer, because every item was a few lines.

- `Engine.minFreeSpace` is deleted: nothing read it. The threshold reaches the transfer manager through `driveops.WithDiskCheck(cfg.MinFreeSpace, driveops.DiskAvailable)` in `engine.go`.
- Comments corrected in `engine.go`, `engine_watch.go`, `worker.go` and `dep_graph.go`: who reads the protected roots, who writes them off the loop (three writers), which goroutine uses the dependency graph, and that only tests call `depGraph.Add`.
- The recovery line of the housekeeping refresh is one constant, `protectedRootRefreshRecoveredMessage`, in `engine_housekeeping.go`; `engine_housekeeping_test.go` waits for the constant instead of a copied string.
- `spec/design/sync-engine.md` names the readers and the third writer and cites the test.
- The commit message was rewritten to describe the commit as it stands.

Checks on the final commit before the merge pipeline: gofumpt clean; `go build ./...`, `go vet ./...` and the tagged vet clean; a targeted `-race` run over the housekeeping, engine construction, protected-root, dependency-graph and worker-pool tests passed; the test convention check passed; lint of `./internal/sync/...` reported 0 issues (`E1-lint.log`).

Not known: why the race detector catches the unguarded read in only some runs of the new test on the old code (5 of 20 when measured). The test is still a true negative on the fixed code; it is not a deterministic positive on the old code, and the PR says so.



## Merged as #908 (`fa601d30`, 2026-11-14 about 12:45 PST)

- One commit `87c7a111` on main at `2e465121`. The pre-pr gate passed on the first run; pre-merge passed with no review threads; post-merge passed and removed the `engine-guarded-read` worktree and its branch.
- Installed `fa601d30`; the watch client reports "Up to date, watching for changes", 19 files.
- No live test applies: the change is a lock around one read, removed code that nothing read, and comments. Its proof is the race test of the housekeeping read, which the nightly's race tier runs.
- Unknown and left so: why the race detector catches the unguarded read in only 5 of 20 runs on this host.
- No schema change and no store-generation bump.
- Next on this branch line: the vestigial locks (E3), whose worktree is rebased onto this main before it starts.
