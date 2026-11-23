# 02 — Remove the ways the daemon hangs or fails silently

Repository `~/src/onedrive-go`, branch `main`, commit `70301c0e`. Read-only analysis; nothing was built, run or tested.

Conventions in this report:

- Paths are relative to the repository root unless they start with `/`. `FORK/` means the module-cache directory of the forked oauth2 module at version `v0.0.0-20260209071456-249baba404ab` (the `replace` target in `go.mod:42`).
- **[VERIFIED]** = I read the code or text that proves the statement. **[SUSPECTED]** = not proven; the line says what would settle it.
- "Proposed" values (timeouts, names) are proposals, not facts about the code.
- Everything I did not read is listed in section 10.

Rules from the repository `CLAUDE.md` that the proposals are held to: no magic numbers (`CLAUDE.md:120`), recover only at goroutine boundaries and log the panic as Error (`:137`), every goroutine has an owner (`:142`), "Timeouts on everything external ... every channel send that could block. No operation waits forever." (`:180`), `// Validates: R-X.Y.Z` before each test (`:209`), `time.Sleep` banned in tests (`:217`), every production file governed by one design doc (`:46`).

---

## 1. Watch runner events and the five unbounded `<-wr.done` waits

### 1.1 Current behaviour

**[VERIFIED]** A runner event names its sender only by mount ID. `internal/multisync/orchestrator_watch.go:19-41`: `watchRunnerEvent{mountID, report, err, parentSnapshot, parentSnapshotChanged}`; `watchRunner{mount, engine, parentAck, cancel, done, startedAt}`.

**[VERIFIED]** The handler resolves the event to whichever runner currently holds that ID, then joins it without cancelling it (`orchestrator_watch.go:667-680`):

```go
wr := runners[event.mountID]            // :667
if wr == nil { ... return }             // :668-676
<-wr.done                               // :678
o.closeEngine(ctx, wr.mount, wr.engine) // :679
delete(runners, event.mountID)          // :680
```

**[VERIFIED]** The five waits. All run on the `RunWatch` goroutine (the single owner of `runners`, `spec/design/sync-control-plane.md:58`).

| # | Line | Function | Reached from | Cancelled first |
|---|------|----------|--------------|-----------------|
| 1 | `:66` | deferred drain in `RunWatch` (`:63-71`) | `ctx.Done()` (`:107-108`), stop command (`:92-94`), a panic unwinding the loop | yes (`:65`) |
| 2 | `:678` | `handleWatchRunnerEvent` | runner event (`:101`) | **no** |
| 3 | `:1032` | `stopInactiveWatchRunners` | `applyWatchMountSet` (`:853`) from `reload` (`:576`) and `reconcileWatchRunners` (`:613`) | yes (`:1031`) |
| 4 | `:1137` | `stopChildWatchRunnersForParent` | handler when a parent exited (`:683`); `reconcileWatchRunnersForParent` (`:910`) | yes (`:1136`) |
| 5 | `:1173` | `stopInactiveChildWatchRunnersForParent` | `applyWatchMountSetForParent` (`:974`) from a parent-snapshot event (`:664`) | yes (`:1172`) |

**[VERIFIED]** None has a timeout. After each, `closeEngine` calls `engine.Close(ctx)` with no timeout (`orchestrator_watch.go:372`); `Engine.Close` cleans stale upload sessions and closes the store (`internal/sync/engine.go:427`, `:450`) and only then releases the sync-root lock (`:438-444`).

**[VERIFIED]** While the loop is blocked in any of these, the select at `orchestrator_watch.go:87-110` is not serviced: control commands block on the unbuffered `commands` channel (`internal/multisync/control_socket.go:334-338`) until the client's 2 s timeout (`internal/cli/control_client.go:22`); no reload, reconcile, restart or pause expiry runs; the systemd watchdog keeps being pinged by an independent goroutine (`internal/cli/sync_notify.go:96-110`).

**[VERIFIED]** Shutdown is one of these waits. `sync-control-plane.md:597-602`: "No timer escalates the first signal; forced exit is owned only by the second signal." The shipped unit has no `TimeoutStopSec` (`contrib/systemd/onedrive-go.service`, 41 lines), so a runner that does not return holds `systemctl stop` until systemd's default stop timeout (90 s, `/usr/share/man/man5/systemd-system.conf.5.gz` roff lines 626-627) and then SIGKILL.

**[SUSPECTED]** That an engine can actually fail to return after cancellation. I did not read the engine's watch shutdown path beyond `internal/sync/engine_watch.go:58`, `:167`, `:667`. Settle by reading `internal/sync/engine_watch*.go` for blocking file or network calls that do not take `ctx` (the class `CLAUDE.md:180` names: NFS, FUSE).

### 1.2 Can an event be attributed to the wrong runner? Yes

**[VERIFIED by trace]** Standalone mount M, runner A:

1. A fails on its own. Its goroutine queues `watchRunnerEvent{mountID: M, err}` and closes `done` (`orchestrator_watch.go:455-466`, `:428`). Call the event E1. The channel is buffered (`watchRunnerEventBuffer = 64`, `:19-41`).
2. Before the loop reads E1, a reload becomes ready (config file poll every 5 s, `internal/multisync/watch_config_file.go:16`, or SIGHUP, or a control command). Go's `select` picks among ready cases at random (`:90-109`).
3. The reload carries a spec for M that is not restart-equivalent (for example a changed `sync_dir`; `mountSpecRuntimeEquivalent`, `:1279-1286`). `stopInactiveWatchRunners` cancels, joins and deletes A (`:1031-1035`); `releaseRunnerRestartWaits()` clears the backoff (`:575`); `startReloadWatchRunners` starts runner B under the same ID (`:1207`, `:1225`).
4. The loop reads E1. `runners[M]` is B (`:667`). `<-wr.done` (`:678`) blocks until B exits by itself, because nothing cancels B.
5. When B finally exits, the handler closes B's engine, deletes B (`:679-680`) and reports A's old error against B's `startedAt` (`:690-692`, `:700-701`).

This contradicts the design text: "An event from a runner the orchestrator already stopped is ignored" (`sync-control-plane.md:415-417`). That is only true when no replacement was started (`:668-676`).

A plausible real trigger: the user moves the sync folder (the runner fails on the missing root) and edits `sync_dir` in the config within the same few seconds.

**[VERIFIED]** The same shape exists for shortcut child mounts. A child's mount ID is `config.ChildMountID(namespaceID, root.BindingItemID)` in both run modes (`internal/sync/shortcut_root_publication.go:44`, `:53`; `internal/multisync/mount_spec.go:484`, `:499`), so a final-drain child replaces a normal child under the same ID. If normal child A queued a failure event and a parent snapshot replaces it with final-drain runner B (`stopInactiveChildWatchRunnersForParent`, `:1160-1175`), E1 is attributed to B: the loop waits for B's drain, then calls `handleFinalDrainWatchRunnerEvent` with A's event (`:686-688`), whose nil report is classified "final-drain child did not produce a mount report" → failed (`internal/multisync/shortcut_final_drain.go:57-59`). B's own event is later dropped as "already stopped" (`:668-676`).

**[VERIFIED]** Parent-snapshot events are checked by content, not by sender: `parentSnapshotEventIsCurrent` (`:727-741`) accepts an event when the ID maps to a standalone runner and the snapshot equals the cached one.

**[VERIFIED]** A second, rarer hang: the runner's event sends select only on the parent `ctx.Done()`, not on its own `mountCtx.Done()` (`:447-450`, `:462-465`). A final drain that completes with `runErr == nil` just as the loop cancels it still sends (`:436-452`). If the 64-slot buffer is full while the loop sits in `wr.cancel(); <-wr.done` for that runner, the two wait on each other until the parent context ends.

### 1.3 Related defect found on the way: run mode is not part of runner equivalence

**[VERIFIED]** `mountSpecsEquivalentForWatchRestart` and its helpers (`orchestrator_watch.go:1250-1309`) never read the child's run mode. `child.mode` is read only at `mount_spec.go:738` and `:763`. `ShortcutChildEngineSpecsEqual` compares `LocalRoot`, `RemoteDriveID`, `RemoteItemID`, `ContentFilter`, `LocalRootIdentity` (`internal/sync/shortcut_topology.go:135-144`).

**[VERIFIED]** The publisher derives the engine spec from root-record fields and derives only `Mode` from the lifecycle state (`shortcut_root_publication.go:43-55`; `runMode: ShortcutChildRunModeFinalDrain` for the removed-final-drain and same-path-replacement states, `internal/sync/shortcut_root_status.go:323`, `:400`).

**[SUSPECTED]** Consequence: when a shortcut is removed on OneDrive while watching, the parent republishes the same child with `Mode` changed to `final_drain`; the specs compare equal, so the loop keeps the normal `RunWatch` runner, only swaps `wr.mount = next` (`:1162`, `:1021`), and `startReloadWatchRunners` skips the ID because a runner exists (`:1196-1198`). The final drain would then never start in watch mode, and a later self-exit of that runner would be routed into `handleFinalDrainWatchRunnerEvent` (`:686-688`) with a nil report. Settle by (a) reading whether the transition into a final-drain state also rewrites `RelativeLocalPath`, `RemoteItemID` or `LocalRootIdentity` on the root record (not read), and (b) the test in 1.5. No existing test publishes one child first normal, then final-drain, in watch mode: every final-drain test sets the mode before the runner starts (`internal/multisync/orchestrator_test.go:684`, `:823`, `:1166`, `:1238`, `:1652`, `:1740`; `watch_reporting_test.go:376`; `shortcut_child_work_test.go:68`).

### 1.4 Proposed end state

All in `internal/multisync/orchestrator_watch.go` unless stated.

**Runner identity.** Each started runner gets a generation number from a counter owned by the loop goroutine.

```go
type watchRunner struct {
    id         mountID   // immutable; wr.mount may be swapped for an equivalent spec
    generation uint64
    stuck      bool      // set when a bounded stop timed out
    // existing fields: mount, engine, parentAck, cancel, done, startedAt
}

type watchRunnerEvent struct {
    mountID    mountID
    generation uint64    // the runner that sent the event
    // existing fields: report, err, parentSnapshot, parentSnapshotChanged
}

func (o *Orchestrator) nextRunnerGeneration() uint64          // loop goroutine only
func eventIsFromCurrentRunner(runners map[mountID]*watchRunner, ev watchRunnerEvent) (*watchRunner, bool)
```

- `startWatchRunner` (`:388-476`) takes the generation as a parameter; both callers (`:307` initial start, `:1206-1207` reload) allocate it before `attachParentChildWorkSink`, and `parentChildWorkSinkForMount` (`internal/multisync/shortcut_child_work.go:26-57`) stamps it on parent-snapshot events. A generation is used instead of a pointer because the sink is attached to the mount spec before the runner value exists (`:1206` precedes `:1207`).
- `handleWatchRunnerEvent` starts with `eventIsFromCurrentRunner`; a mismatch is the existing Debug "ignoring event from a runner that was already stopped". `parentSnapshotEventIsCurrent` keeps its content check and adds the generation check.
- Runner sends also select on `mountCtx.Done()` (`:447-450`, `:462-465`), so a cancelled runner never blocks on the event channel. A send that still wins the race is dropped by the generation check.
- `childEngineSpecsEquivalent` (`:1288-1293`) also compares `child.mode`, so a normal → final-drain republication restarts the child.

**One bounded join.** The five inline `cancel / <-done / closeEngine / delete` sequences are deleted and replaced by one helper.

```go
const (
    // watchRunnerStopTimeout bounds how long the loop waits for one cancelled
    // runner to return before the watch gives up and exits.
    watchRunnerStopTimeout = 30 * time.Second   // proposed; see risks
    // engineCloseTimeout bounds Engine.Close (upload-session cleanup, store close).
    engineCloseTimeout = 15 * time.Second       // proposed
)

// ErrWatchRunnerStuck: a drive's sync did not stop when told to.
var ErrWatchRunnerStuck = errors.New("multisync: a drive's sync did not stop")

// retireWatchRunner cancels wr, waits for its goroutine up to
// watchRunnerStopTimeout, closes its engine and removes it from runners.
// On timeout it marks wr stuck, leaves it in runners, does not close its
// engine, logs at Error with a dump of all goroutine stacks, and returns an
// error wrapping ErrWatchRunnerStuck.
func (o *Orchestrator) retireWatchRunner(ctx context.Context, runners map[mountID]*watchRunner, wr *watchRunner) error
```

- The timer comes from the existing injectable seam `restartTimer func(time.Duration) (<-chan time.Time, func())` (`internal/multisync/orchestrator.go:121-123`), renamed `newTimer` since it is no longer restart-only.
- `closeEngine` (`:365-386`) wraps `engine.Close` in `context.WithTimeout(context.WithoutCancel(ctx), engineCloseTimeout)` (the same form `closeControlSocket` uses at `:356`). A close that times out is an Error, not the present Debug for deadline errors (`:376-381`).
- Wait #2 (`:678`) also goes through the helper. For the true sender only deferred functions remain, so it returns at once.

**What the orchestrator does when a runner will not stop: fail closed, exit.**

- The stuck runner stays in `runners`, so `startReloadWatchRunners` cannot start anything under its ID (`:1196-1198`). Its engine is never closed while its goroutine may still be running: closing would release the sync-root lock (`internal/sync/engine.go:438-444`) and close a store the engine may still write.
- `RunWatch` returns the `ErrWatchRunnerStuck` error. The deferred drain (`:63-71`) retires the remaining runners with the same bound and skips runners already marked `stuck`.
- The CLI maps the error to exit 1 (`internal/cli/failure_class.go:54-74`: anything untyped is 1), so `Restart=on-failure` starts a fresh process after `RestartSec=30s` (`contrib/systemd/onedrive-go.service:32-33`); 1 is not in `RestartPreventExitStatus=77 78` (`:36`). Process exit is the only way to end a goroutine that ignores cancellation, and it releases every flock.
- Reason for exiting instead of running on without that drive: a second engine for the same sync root is refused only where the root lock works (`ErrSyncRootInUse`, `internal/sync/engine_root_lock.go:113-119`); where the filesystem cannot lock, the engine continues unlocked with a warning (`:120-129`, R-2.9.5). On those filesystems only the orchestrator's refusal to start a replacement keeps the store single-owner, and a drive that silently never syncs again is the failure this item exists to remove.
- On a user-requested stop where a runner is stuck, the command also exits 1, not 130: "A timeout or internal cancellation is reported as an error" (R-6.6.27, `spec/requirements/non-functional.md:93`).

### 1.5 Tests to write first

All in `internal/multisync`, no sleeps; use the existing fakes.

1. `TestHandleWatchRunnerEvent_EventFromReplacedRunnerIsIgnored` in `orchestrator_test.go`, built like `TestApplyWatchMountSet_StopsStaleChildBeforeStartingReplacement` (`:2202`) with hand-made `watchRunner` values: put runner B (generation 2, `done` never closed) in the map, deliver an event with generation 1, run the handler in a goroutine that closes a channel when it returns, assert it returns and B is still in `runners`. It fails today by blocking at `:678`.
2. Same for a child replaced by a final-drain runner: assert `handleFinalDrainWatchRunnerEvent` is not reached with the stale event (no "Couldn't finish removing shortcut" warning in `logCapture`, `test_helpers_test.go:118-159`).
3. `TestHandleWatchRunnerEvent_IgnoresStaleParentSnapshotEvents` (`:2129`): add a table case "replacement parent runner, equal snapshot, older generation".
4. `TestApplyWatchMountSetForParent_NormalToFinalDrainRestartsChild`: old child normal, next child identical except `Mode = ShortcutChildRunModeFinalDrain`; assert one stop, one start, and `mockEngine.runOnceFn` called (`orchestrator_test.go:2666-2702`). This also settles 1.3.
5. `TestRetireWatchRunner_TimeoutFailsClosed`: `mockEngine.runWatchFn` that ignores `ctx` until a test channel closes; inject the timer seam and fire it by hand; assert the returned error `errors.Is(..., ErrWatchRunnerStuck)`, the runner is still in the map, `mockEngine.closed` is false, one Error record. Extend `backoffHarness` (`watch_reporting_test.go:125-180`), whose `exit()` must now pass the runner's generation.
6. `TestRunWatch_StuckRunnerOnReloadEndsTheWatchWithAnError` through `RunWatch` with the readiness-channel pattern the existing RunWatch tests use.
7. `TestStartWatchRunner_CancelledRunnerDoesNotBlockOnAFullEventChannel`: unbuffered or pre-filled `runnerEvents`, final-drain engine returning nil after cancel; assert `done` closes.
8. `internal/cli/failure_class_test.go` `TestCommandExitCode` (`:34`): add `ErrWatchRunnerStuck` → 1, also with `stopRequested = true`.

### 1.6 Spec and docs

- `spec/design/sync-control-plane.md`: "### RunWatch" `:415-417` (ignored events are identified by generation), "## Ownership Contract" `:58`, "## Runtime Ownership" `:540-553`, "## CLI Contract" `:597-602` (the "No timer escalates the first signal" sentence becomes false), "## Verified By" `:61-80`.
- New requirement, next free ID **R-6.3.9** (R-6.3.8 is the highest, `spec/requirements/non-functional.md:46`): "A watch shall not wait without bound for a drive to stop. A drive that does not stop within the stop timeout is logged with all goroutine stacks, is never replaced or closed under itself, and ends the watch with exit status 1."
- `docs/running-as-a-service.md:99-103` (Stopping).

### 1.7 Risks

- **Timeout value.** `engine_watch.go:167` says "the watch runtime owns settle/drain time". `watchRunnerStopTimeout` must exceed the engine's own shutdown drain; I did not read that bound (section 10). Too small a value turns a slow clean stop into a restart.
- Exiting with a goroutine still running leaves the store as a kill would. The repository already tests kill-mid-transfer recovery (e2e `TestE2E_Sync_KillMid*`, named in the host notes; I did not read those tests).
- A deterministic stuck engine becomes a restart every 30 s under systemd. That is loud, which is the intent, but see the restart-limit note in 2.6.

---

## 2. Panic handling on the multisync loop and at goroutine boundaries

### 2.1 What the existing `recover()` covers

**[VERIFIED]** `orchestrator_watch.go:491-498` is inside `runWatchRunnerEngine` (`:484-509`). It covers only `engine.RunOnce` / `engine.RunWatch` for one runner, and turns a panic into that mount's error through `recoveredSyncPanic` (`internal/multisync/mount_runner.go:73-85`, Error log with `debug.Stack()`). The comment at `:478-483` states the intent. The rest of the runner goroutine body (`:427-468`) is outside it.

**[VERIFIED]** All production `recover()` sites: `internal/multisync/mount_runner.go:38`, `internal/multisync/orchestrator_watch.go:492`, `internal/sync/engine_watch.go:569`, `internal/sync/engine_watch_maintenance.go:148`, `internal/sync/scanner.go:279`, `internal/sync/engine_watch_panic_recovery.go:29`, `internal/sync/worker.go:159`. None in `internal/cli`, `internal/synccontrol`, `internal/sdnotify`, `internal/driveops`, `internal/graph`, `main.go`.

### 2.2 Goroutines with no recover at their boundary

**[VERIFIED]** by reading each launch site:

| Goroutine | Site | Effect of a panic today |
|-----------|------|-------------------------|
| The `RunWatch` loop itself (runs on the command goroutine: `internal/cli/sync_runtime.go:229`, `internal/cli/root.go:704-742` has no recover) | `orchestrator_watch.go:47-111` | process dies; see 2.3 |
| Control socket `Serve` | `control_socket.go:128-136` | process dies |
| Control socket closer on ctx cancel | `control_socket.go:138-150` | process dies |
| Config file / SIGHUP watcher | `watch_config_file.go:55-77` | process dies |
| systemd watchdog pinger | `internal/cli/sync_notify.go:184-187` | process dies |
| Signal handler | `internal/cli/signal.go:41-60` | process dies |
| One-shot child run group | `orchestrator_run_once_children.go:135` (body `:174-214`) | process dies |
| One-shot `markParentDone` after the runner's own recover | `orchestrator_run_once.go:117-119` (launches `:95`, `:114`) | process dies |
| Drive discovery per token | `internal/cli/drive_catalog.go:173` | process dies |
| Shared-target enrichment | `internal/cli/shared_discovery.go:296` | process dies |
| `get` workers | `internal/cli/get.go:317`, `get_shared.go:173`, `:181` | process dies |
| Browser-login callback server | `internal/graph/auth_browser.go:108-112` | process dies |

`internal/synccontrol`, `internal/sdnotify` and `internal/driveops` start no goroutines.

**[VERIFIED]** Control handlers are a special case: `net/http` recovers a handler panic per connection and logs it through `Server.ErrorLog`, falling back to the standard `log` package (`/usr/lib/go/src/net/http/server.go:1936-1940`, `:3784-3790`). No `ErrorLog` is set anywhere under `internal/`, so such a panic bypasses slog and the log file.

### 2.3 What a loop panic does today

- **[VERIFIED]** No recover between the loop and `main`; `serviceNotifier.failed(err)` (`sync_runtime.go:173`) runs during unwinding with a nil `err`, so no failure `STATUS=` is sent; the Error log at `sync_runtime.go:234` is never reached, so the log file gets nothing, against the intent of R-6.6.35 (`non-functional.md:100`).
- **[VERIFIED]** The deferred drain (`:63-71`) runs during unwinding and can block on `<-wr.done` (item 1), turning a crash into a hang.
- **[SUSPECTED]** The process exits with status 2, which collides with `exitUsage` (`failure_class.go:17-26`), so scripts read a crash as a usage error. Settle by reading `/usr/lib/go/src/runtime/panic.go` (the exit call in `fatalpanic`); I did not open it.
- **[VERIFIED]** No requirement covers it. R-6.3.8 covers only panics in goroutines "the mount's watch runtime owns" (`non-functional.md:46`).

### 2.4 `TestPanicRecoverySitesCaptureTheStack`

**[VERIFIED]** `internal/sync/panic_stack_test.go` (82 lines): scans `../sync` and `../multisync` (`:23-26`) for `recover()`, takes a 900-byte window (`:61-77`), **skips** windows that contain no `.Error(` (`:44-46`) and asserts the rest contain `debug.Stack()` (`:50-51`). So: (a) a site that delegates to a helper is skipped, not checked (the site at `orchestrator_watch.go:492` is one); (b) `internal/cli` is not scanned; (c) it cannot detect a goroutine that has no recover at all. Its `// Validates:` line names R-6.7.1 (`:13`), which is the delta-reordering requirement (`non-functional.md:107`); the panic requirement is R-6.3.8.

### 2.5 Proposed end state

**One helper.** New leaf package `internal/panicguard` (one file, added to the `GOVERNS:` line of `spec/design/system.md`, which already lists the leaf utility packages):

```go
// ErrPanic marks an error produced from a recovered panic.
var ErrPanic = errors.New("internal error")

// Recovered logs a recovered panic value at Error with the stack of the
// panicking goroutine and returns an error wrapping ErrPanic. Call it only
// from a deferred function, with the value recover() returned.
func Recovered(logger *slog.Logger, where string, r any) error
```

`recoveredSyncPanic` (`mount_runner.go:73-85`) keeps its user wording and calls `panicguard.Recovered`. `internal/graph` and `internal/cli` can import the leaf without importing `multisync`.

**Where recovery belongs and what it does.**

1. **`RunWatch` boundary.** `RunWatch` gets a named error result and a deferred recover registered first, so it runs after the (now bounded, item 1) runner drain: `err = fmt.Errorf("sync --watch: %w", panicguard.Recovered(o.logger, "watch loop", r))`. The CLI then logs "sync --watch stopped with an error" to the log file (`sync_runtime.go:234`), sends the failure `STATUS=` (`:173`), and exits 1. The daemon is not kept alive: after a panic the loop's maps and report state are of unknown consistency, and exit 1 lets systemd restart it.
2. **Daemon-critical helper goroutines** (control `Serve`, closer, config watcher): recover, then report on a `fatal chan error` (buffered 1) that the `RunWatch` select reads and returns. A recovered goroutine that just ends would leave a daemon with no control channel or no config reloads, which is a silent failure. The same channel carries an unexpected `Serve` return (item 7).
3. **CLI goroutines** (watchdog pinger, signal handler): recover, then cancel the command context with the error (`context.WithCancelCause`), so the command returns it and exits 1.
4. **One-shot helper goroutines** (`runChildrenForParent`, `markParentDone`, drive discovery, shared enrichment, `get` workers, browser callback server): recover into that unit's existing error result, as `MountRunner.run` does (`mount_runner.go:37-50`).
5. **`mainWithWriters`** (`root.go:704-742`): a deferred recover on the command goroutine as the last line of defence: log through `panicguard.Recovered`, exit `exitFailure`. This removes the status-2 collision for panics on the main goroutine.
6. **Control server**: set `http.Server.ErrorLog` to `slog.NewLogLogger(logger.Handler(), slog.LevelError)` at `control_socket.go:115-118`, so handler panics reach the log file.

### 2.6 Tests to write first

- Rewrite `TestPanicRecoverySitesCaptureTheStack` (`internal/sync/panic_stack_test.go`): scan `../cli`, `../driveops`, `../graph`, `../config`, `../multisync`, `../sync` and `../../main.go`; a `recover()` window must contain either `panicguard.Recovered(` / `recoveredSyncPanic(` or inline `.Error(` plus `debug.Stack()`; a window with neither **fails** instead of being skipped. Fix its ID to `// Validates: R-6.3.8`.
- Add `TestEveryGoroutineHasAPanicBoundary` in the same file using `go/parser`: every `go` statement in non-test files of those packages must start a function literal whose first statement is a `defer` that reaches the helper, or call a named function on an explicit allowlist in the test.
- `internal/multisync`: `TestRunWatch_LoopPanicIsLoggedWithStackAndReturnsAnError` (make `WatchLifecycle` panic; it runs on the loop, `orchestrator.go:96-98`); assert an Error record with a `stack` attribute via `logCapture` and `errors.Is(err, panicguard.ErrPanic)`. Model: `TestRunWatch_RunnerPanicFailsOnlyThatMountAndRestartsIt` and `TestMountRunner_Run_PanicIsLoggedAtErrorWithStack` (both listed in `sync-control-plane.md:65-80`).
- `internal/multisync/orchestrator_run_once_test.go`: a panic in a child run (`mockEngine.shouldPanic`) reaches the report as that child's error and `wait()` returns.
- `internal/cli/failure_class_test.go`: `TestCommandExitCode` case `panicguard.ErrPanic` → 1; a `TestMainWithWriters_PanicExitsOne` next to `TestMainWithWriters_ConfigErrorExits78` (`:101`).
- `internal/cli/sync_notify_test.go`: with `lifecycleOrchestrator` (`:81-97`) returning an `ErrPanic` error, assert the failure `STATUS=` and exit 1 (pattern of `TestRunSyncDaemon_SignInFailureSetsStatusAndExitCode`, `:162`).

### 2.7 Spec and docs

- New requirement **R-6.3.10**: "A panic outside a mount's own runtime (the watch control loop, the control socket, the config watcher, the watchdog keepalive, the signal handler) shall be logged with its stack to the log file and shall end the process with exit status 1, never 2."
- `sync-control-plane.md` "### RunWatch" `:419-425` and "## Verified By"; `spec/design/cli.md` "## Exit Status" `:123-153`; `spec/design/system.md` GOVERNS line.
- Fix the wrong ID in the comment at `internal/cli/sync_runtime.go:230-233` (it cites R-6.6.33; the requirement about writing the watch's stop reason to the log file is R-6.6.35).

### 2.8 Risks

- A panic that repeats on every start becomes a restart every 30 s. **[SUSPECTED]** that systemd's default start limit does not stop that loop with `RestartSec=30s`; settle by reading `StartLimitIntervalSec=` / `StartLimitBurst=` in `systemd.unit(5)` and decide whether the unit should set them.
- The AST test needs an allowlist discipline; without one it will be loosened the first time it is inconvenient.

---

## 3. systemd watchdog tied to loop progress

### 3.1 Current behaviour

**[VERIFIED]** `internal/cli/sync_notify.go:96-110`:

```go
// runWatchdog sends WATCHDOG=1 at the interval systemd asked for until ctx
// ends. The keepalive proves the process is scheduling goroutines; the watch
// loop's own health is reported through STATUS= and the exit status.
func (s *syncNotifier) runWatchdog(ctx context.Context, ticks <-chan time.Time) {
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticks:
            if err := s.notifier.Watchdog(); err != nil { ... Debug ... }
```

The design doc gives the same reasoning: "The keepalive proves the process is alive and scheduling, not that every drive is healthy; drive health is in `STATUS=` and `status`." (`spec/design/cli.md:179-183`). R-6.6.32 only requires "`WATCHDOG=1` at half of `$WATCHDOG_USEC`" (`non-functional.md:98`).

**[VERIFIED]** The reasoning fails for a hung loop. `STATUS=` is sent from the `WatchLifecycle` hook, which "runs on the watch loop" (`internal/multisync/orchestrator.go:96-98`), and an exit status needs the loop to return. A loop blocked in any wait of item 1, in the config lock of item 6, or in a network call during a reload sends neither, while the pinger (`startSyncWatchdog`, `sync_notify.go:172-194`, started at `sync_runtime.go:174` before the orchestrator exists) keeps systemd satisfied. The user-facing doc promises the opposite: "if it hangs, systemd restarts it" (`docs/running-as-a-service.md:64-65`).

**[VERIFIED]** Mechanics: interval = `WATCHDOG_USEC / watchdogPingDivisor` with divisor 2 (`internal/sdnotify/sdnotify.go:31`, `:83-105`, `:118-124`); each send has a 2 s write timeout (`:35`). The unit sets `WatchdogSec=2min` (`contrib/systemd/onedrive-go.service:38`), so one ping a minute. systemd activates the watchdog "when the start-up is completed" and on timeout aborts the service; `Restart=on-failure` restarts "when the configured watchdog timeout is triggered" (`/usr/share/man/man5/systemd.service.5.gz` roff lines 1067, 1072-1074, 1156).

**[VERIFIED]** No test calls `runWatchdog` or `startSyncWatchdog` directly (grep of `internal/cli`).

**[VERIFIED]** Doc drift in the same section: `cli.md:183-185` says "there is no SIGHUP reload", but the watcher handles SIGHUP (`watch_config_file.go:55-77`), the control-plane doc lists it (`sync-control-plane.md:497-502`), R-4.4.3 requires it (`spec/requirements/configuration.md:27`) and the unit has `ExecReload=/bin/kill -HUP $MAINPID` (`onedrive-go.service:29`).

### 3.2 Legitimately long steps on the loop

**[VERIFIED]** that each of these runs on the loop goroutine; durations are as stated.

| Step | Site | Bounded today |
|------|------|---------------|
| Runner joins and `closeEngine` | `orchestrator_watch.go:66`, `:678`, `:1032`, `:1137`, `:1173`, `:372` | no (item 1 bounds them) |
| Reload: config load, `ClearExpiredPauses` (config writes under the lock) | `:540`, `:822-827` | no (item 6 bounds the lock) |
| Starting a runner: `SyncSession` plus `engineFactory` with `VerifyDrive: true` (network) | `:395`, `:403-409` | only by transport timeouts; `ResponseHeaderTimeout` is 2 min (`internal/graphtransport/profiles.go:13-26`) |
| `buildRuntimeWorkSet` | `:561`, `:605`, `:750` | not read |
| Final-drain finalisation and parent acks | `:743-783` | not read |
| Shortcut artifact purge | `:855`, `:976` | not read |
| `WatchLifecycle` hook → notify socket write | `orchestrator.go:96-98` | 2 s per message (`sdnotify.go:35`) |
| Status reply built on the loop | `control_socket.go:566` | not measured |

An idle loop has no periodic case shorter than the reconcile tick (default 5 min, `orchestrator_watch.go:1320-1331`), which is longer than the ping interval.

### 3.3 Proposed end state

**Heartbeat owned by the orchestrator** (new file `internal/multisync/loop_heartbeat.go`, governed by `sync-control-plane.md`):

```go
const (
    // loopHeartbeatInterval is how often an idle loop beats.
    loopHeartbeatInterval = 15 * time.Second        // proposed
    // loopSilenceLimit is how long the loop may go without a beat outside a
    // declared long step before it counts as hung.
    loopSilenceLimit = 4 * loopHeartbeatInterval    // proposed
)

type loopHeartbeat struct {
    now   func() time.Time
    last  atomic.Int64 // UnixNano of the last beat
    limit atomic.Int64 // nanoseconds the loop may stay silent after that beat
    step  atomic.Pointer[string]
}

func (h *loopHeartbeat) beat()                                     // limit = loopSilenceLimit
func (h *loopHeartbeat) beatFor(step string, limit time.Duration)  // entering a step that may take up to limit
func (h *loopHeartbeat) alive() (ok bool, step string, silent time.Duration)

// LoopAlive reports whether the watch loop made progress recently.
func (o *Orchestrator) LoopAlive() (ok bool, step string, silent time.Duration)
```

- The loop beats at the top of every select iteration (`orchestrator_watch.go:87`) and on a new `case <-heartbeatTicks:` driven by an injectable ticker like `reconcileTicks` (`orchestrator.go:120`).
- Each long step declares itself with `beatFor` using **the same named constant that bounds it**: `watchRunnerStopTimeout` and `engineCloseTimeout` (item 1), the config lock timeout (item 6), a new `watchRunnerStartTimeout` for the start step. Start/stop loops beat once per mount, so the bound is per mount, not per reload.
- `RunWatch` calls `beatFor("starting", ...)` on entry; before `READY=1` systemd does not enforce the watchdog, and `TimeoutStartSec=5min` covers start-up (`onedrive-go.service:31`).

**Pinger** (`internal/cli/sync_notify.go`):

```go
func (s *syncNotifier) runWatchdog(ctx context.Context, ticks <-chan time.Time, loopAlive func() (bool, string, time.Duration))
```

It sends `WATCHDOG=1` only when `loopAlive` reports true. On the first withheld ping it logs one Error ("the sync loop has made no progress for <d> (in <step>); not sending the systemd keepalive, so systemd will restart sync") and sends that text as `STATUS=`. `startSyncWatchdog` moves below the orchestrator construction in `runSyncDaemonWithFactory` (between `sync_runtime.go:225` and `:229`), and the orchestrator interface used by `cc.seams.syncDaemonOrchestrator` gains `LoopAlive`.

Detection time is at most `loopSilenceLimit` (or the declared step limit) plus `WatchdogSec`.

**Roll-out in two PRs to avoid false restarts.** First ship the heartbeat and the Error log with pings unconditional; after it has run through a long watch soak without a false "no progress" line, make the ping conditional.

### 3.4 Tests to write first

- `internal/cli/sync_notify_test.go`: `TestRunWatchdog_PingsOnlyWhileTheLoopIsAlive` drives `runWatchdog` with a hand-fed `ticks` channel and `fakeNotifySocket` (`:30-78`, `watchdogMessage` at `:27`): alive → `WATCHDOG=1` arrives; not alive → the next datagram is the `STATUS=` line and one Error is logged; alive again → pings resume. `// Validates: R-6.6.32`.
- `TestRunSyncDaemon_NotifiesSystemdOfLifecycle` (`:113`): `lifecycleOrchestrator` (`:81-97`) gains `LoopAlive`.
- `internal/multisync`: `TestRunWatch_BeatsOnEveryIterationAndWhileIdle` with `testClock` (`test_helpers_test.go:88-107`) and an injected heartbeat tick channel; `TestLoopHeartbeat_DeclaredStepExtendsTheLimit` as a pure unit test.
- Extend the stuck-runner test of 1.5 to assert `LoopAlive` is true during the bounded join and false only past the declared limit.

### 3.5 Spec and docs

- R-6.6.32 (`non-functional.md:98`): add "only while the watch loop is making progress; a loop silent beyond its limit stops the keepalive so the service manager restarts it".
- `cli.md` "## systemd Notify" `:179-185` (rewrite the keepalive paragraph; fix the SIGHUP sentence), verification row `:51`.
- `sync-control-plane.md` "### RunWatch" `:368-425`.
- `docs/running-as-a-service.md:64-65`; unit comment `contrib/systemd/onedrive-go.service:37`.

### 3.6 Risks

- A false restart is worse than the present false comfort. Every step in 3.2 marked "not read" or "no" must have a real bound before the ping becomes conditional; that is why this is the last PR.
- The start step's worst case under retries is not known to me (section 10). Bounding `engineFactory` with `context.WithTimeout` is only safe if the engine does not keep that context after construction; not read.
- **[SUSPECTED]** On the watchdog abort Go prints every goroutine's stack to stderr (journal), which would show where the loop was stuck. Settle by reading `/usr/lib/go/src/os/signal/doc.go` on SIGABRT.

---

## 4. Token refresh: HTTP client, the oauth2 fork, save failures

### 4.1 Current behaviour

**[VERIFIED]** Which package: `go.mod:20` requires `golang.org/x/oauth2 v0.30.0`; `go.mod:41-42` replace it with a personal fork ("Replace until upstream proposal golang/go#77502 lands"). The design doc records this as an accepted risk: "the build depends on a personal repository remaining available, which is a single point of failure no amount of pinning removes" (`spec/design/graph-client.md:599-601`).

**[VERIFIED]** What the fork adds: `Config.OnTokenChange func(newToken *Token)` (`FORK/oauth2.go:61-70`), called by `reuseTokenSource.Token()` after a refresh, outside its mutex (`:321-341`). Nothing else in the files I read differs in a way this code uses.

**[SUSPECTED]** That upstream v0.30.0 has everything else the repository uses (`DeviceAuth`, `DeviceAccessToken`, `Exchange`, `VerifierOption`, the `HTTPClient` context key). Upstream is not in the module cache and network access was not allowed. Settle by reading upstream `oauth2.go`, `deviceauth.go`, `pkce.go` at v0.30.0, or by a build once the `replace` is deleted.

**[VERIFIED]** No HTTP client is supplied. Refresh, device-code and auth-code requests take their client from `internal.ContextClient(ctx)`, which returns the `oauth2.HTTPClient` context value or else `http.DefaultClient` (`FORK/internal/transport.go:21-28`; used at `FORK/internal/token.go:259`, `FORK/deviceauth.go:110`). Neither `oauth2.HTTPClient` nor `oauth2.NewClient` appears anywhere in the repository (grep). `http.DefaultClient` is `&Client{}` with no timeout (`/usr/lib/go/src/net/http/client.go:107-111`), and `DefaultTransport` has dial and TLS timeouts but no `ResponseHeaderTimeout` (`/usr/lib/go/src/net/http/transport.go:47-57`).

**[VERIFIED]** How a stalled refresh spreads:

- `reuseTokenSource.Token()` holds its mutex across the refresh request (`FORK/oauth2.go:321-341`), so every request goroutine sharing the source waits behind it.
- `tokenBridge.Token()` takes no context (`internal/graph/auth_token.go:215-227`), so a Graph request's own deadline does not bound the refresh. `ForceRefresh` holds `refreshMu` across it (`:174-213`).
- Only the context bound at construction ends it: "ctx must outlive the TokenSource" (`auth_token.go:21-23`); for the daemon that is the whole watch context (`orchestrator_watch.go:395`).
- Failure scenario: the token endpoint accepts the connection and never answers. All Graph traffic for that account stops with no log line and no error until the daemon is stopped.

**[VERIFIED]** Save failure after a successful refresh is only a warning (`auth_token.go:104-118`):

```go
OnTokenChange: func(tok *oauth2.Token) {
    if err := tokenfile.Save(tokenPath, tok); err != nil {
        logger.Warn("couldn't save the refreshed sign-in; the next run may ask you to sign in again. Check the permissions on this file", ...)
        return
    }
```

Both login flows already treat the same failure as an error (`internal/graph/auth_device.go:96-98`, `auth_browser.go:242-244`).

**[VERIFIED]** Device-code and browser login use the same package and the same missing client: `cfg.DeviceAuth(ctx)` and `cfg.DeviceAccessToken(ctx, auth)` (`auth_device.go:28`, `:41`), `cfg.Exchange(...)` (`auth_browser.go:228`), with `cfg := oauthConfig(tokenPath, logger)` (`internal/graph/auth.go:41-50`, `:82-91`). Device polling issues one token request per tick (`FORK/deviceauth.go:196-203`), so a per-request timeout does not cut the polling loop short.

**[VERIFIED]** `internal/graph/client_construction.go:28-30`: `if httpClient == nil { httpClient = http.DefaultClient }` — a Graph client built without a client silently gets the timeout-free default.

### 4.2 Proposed end state

**(a) Auth requests use a tuned client.**

- `internal/graphtransport/profiles.go`: new exported constructor reusing `buildTransport` (`:108-136`):

```go
const (
    authResponseHeaderTimeout = 30 * time.Second  // proposed
    authRequestTimeout        = 60 * time.Second  // proposed; whole request including body
)

// AuthClient returns the HTTP client for the identity platform's token and
// device-code endpoints. Responses are small, so unlike the Graph profiles
// it sets an overall request timeout.
func AuthClient() *http.Client
```

  `graph-client.md:604` says no Graph-facing client uses `http.Client.Timeout`; the line needs one sentence stating that the token endpoint is not Graph-facing and why the overall timeout is right there (the response body is capped at 1 MiB, `FORK/internal/token.go:263`).
- `internal/graph`: the client is injected, not defaulted. `TokenSourceFromPath(ctx, tokenPath, authHTTP *http.Client, logger)`, and `Login` / `LoginWithBrowser` gain the same parameter; each does `ctx = context.WithValue(ctx, oauth2.HTTPClient, authHTTP)` before using the config. A nil `authHTTP` is an error. `driveops.SessionRuntime` passes `graphtransport.AuthClient()` (it already imports the package, `internal/driveops/session.go:16`); the login commands pass it from `internal/cli` (which also imports it, `internal/cli/root.go:21`). I checked only the direct imports of `graphtransport`, `retry`, `perf` and `tokenfile` for a cycle with `graph`; injection avoids the question.
- Delete the `http.DefaultClient` fallback at `client_construction.go:28-30`; nil becomes an error from `NewClient`.

**(b) Replace the fork with upstream plus a small persisting wrapper.** Delete `go.mod:41-42` and the `OnTokenChange` field at `auth_token.go:103-118`. New file `internal/graph/auth_persist.go` (covered by the `internal/graph/auth*.go` GOVERNS entry of `graph-client.md`):

```go
// ErrTokenNotSaved: a refreshed sign-in could not be written to the token file.
var ErrTokenNotSaved = errors.New("graph: refreshed sign-in not saved")

type tokenStamp struct {
    access, refresh string
    expiry          time.Time
}

// tokenSaver writes the token file when it is shown a token newer than the
// one last written, and remembers the last failure.
type tokenSaver struct {
    path  string
    write func(path string, tok *oauth2.Token) error // tokenfile.Save
    mu    sync.Mutex
    saved tokenStamp // what the file holds
    err   error      // nil when the file is current
}

func newTokenSaver(path string, loaded *oauth2.Token) *tokenSaver // saved = stamp of the loaded token
func (s *tokenSaver) observe(tok *oauth2.Token)
func (s *tokenSaver) saveError() error // wraps ErrTokenNotSaved, names the path

// persistingTokenSource saves every token its inner source refreshes.
type persistingTokenSource struct {
    src   oauth2.TokenSource // cfg.TokenSource(ctx, tok)
    saver *tokenSaver
}

func (p persistingTokenSource) Token() (*oauth2.Token, error) {
    tok, err := p.src.Token()
    if err != nil {
        return nil, err
    }
    p.saver.observe(tok)
    return tok, nil
}
```

`observe` rules: (1) same stamp as `saved` and no pending error → return; (2) a token whose expiry is not later than `saved.expiry` is never written — two goroutines can receive an old and a new token in either order, and the fork's callback (which ran only in the refreshing goroutine) did not have this hazard; (3) otherwise `tokenfile.Save` (atomic write, `internal/tokenfile/tokenfile.go:76-96`); on failure keep `err` and retry on the next call that shows the same or a newer token; on success clear it and log the existing Info "refreshed sign-in".

`newTokenBridge` (`auth_token.go:147-156`) wraps both `src` and the `restart` function in `persistingTokenSource` sharing one `tokenSaver`, so a forced refresh (`:174-213`) is saved by the same path.

**(c) A failed save becomes an error the owner must handle.** Facts that shape it:

- **[VERIFIED]** The process can keep working: the refreshed access token is in memory, and the refresher keeps the rotated refresh token in memory (`FORK/oauth2.go:299-301`).
- **[VERIFIED]** What is at risk is only the next process start, which reads the old refresh token from disk. The repository's own comment assumes rotation can invalidate old refresh tokens (`internal/driveops/session.go:98-102`). **[SUSPECTED]** whether the identity platform actually rejects the previous refresh token; this cannot be settled offline.
- If it does, the next start fails with sign-in required and exits 77, which systemd does not restart (`onedrive-go.service:36`): a daemon that worked for weeks dies at its next restart, and the only trace was one Warn.

Recommended behaviour (design B): requests do not fail; the failure is delivered as a typed error to the token source's owner.

- `tokenBridge` gains `SaveError() error` (returns `tokenSaver.saveError()`), exposed through an optional interface in `internal/graph`, so fakes need no change.
- `driveops.SessionRuntime` gains `TokenSaveErrors() []error` over its token cache (`session.go:119-127`). This depends on item 5: after it, every token source in a process is created by the runtime.
- One-shot commands, including one-shot `sync`: after the command's work, a non-nil save error is returned as the command error → exit 1, message "couldn't save the refreshed sign-in to <path>: <cause>; the next run may ask you to sign in again". The work itself was done; the durable state was not, and a cron user has to hear it.
- Daemon: the loop checks `TokenSaveErrors()` on its heartbeat tick (item 3) or reconcile tick. It logs Error once per distinct failure (the once-per-change mechanism already exists as `warnOnChange`, `orchestrator_watch.go:607-609`), sets a `STATUS=` line, keeps syncing, and each later `Token()` call retries the save. A daemon stopped while the error is still set exits 1 instead of 0/130.

Alternative (design A): `Token()` returns the `ErrTokenNotSaved` error while the file cannot be written, so all Graph calls for the account fail until it can. It matches the literal wording "an error surfaced to the caller" and stops further rotations piling up in memory, but it stops sync for a condition that does not prevent syncing, and how the engine would classify the error is not read (section 10). **This choice is the owner's**; I recommend B.

### 4.3 Tests to write first

- `internal/graph/auth_test.go`: replace `TestOAuthConfig_OnTokenChange` (`:551`) and `TestOAuthConfig_OnTokenChangeError` (`:691`) with `TestPersistingTokenSource_SavesARefreshedToken`, `..._SaveFailureIsReportedAndRetried`, `..._NeverWritesAnOlderToken` (fake inner `oauth2.TokenSource`, injected `write`), and `TestTokenBridge_ForceRefreshIsSaved` beside `TestTokenBridge_ForceRefreshRedeemsRefreshTokenOncePerBurst` (`internal/graph/client_auth_refresh_test.go:172`).
- `TestTokenSourceFromPath_RefreshUsesTheInjectedClient`: an `httptest` token endpoint, an expired token on disk, and an auth client whose `RoundTripper` records the call; assert the refresh went through it. `TestTokenSourceFromPath_StalledTokenEndpointTimesOut`: handler blocks on a channel the test closes at cleanup; the injected client has a short timeout; assert `Token()` returns an error (readiness channel, no sleep). `TestTokenSourceFromPath_NilAuthClientIsAnError`.
- The same injected-client assertion for the device and browser flows through the existing flow injection (`graph-client.md:99-103`).
- `internal/graph`: `TestNewClient_NilHTTPClientIsAnError`.
- `internal/driveops/session_test.go`: `TestSessionRuntime_TokenSaveErrors` using `TokenSourceFn` (`session.go:109-111`), next to `TestSessionRuntime_TokenSourceError` (`:419`).
- `internal/cli`: a one-shot command with a token source reporting a save error exits 1 (`TestCommandExitCode`, `failure_class_test.go:34`).

### 4.4 Spec and docs

- `graph-client.md`: "## Authentication" `:94-98`, "## 401 Token Refresh" `:541-547`, "## Runtime Ownership" `:590`, "## Design Constraints" `:598-601` (delete the fork lines and the accepted-risk paragraph) and `:604`.
- R-6.8.14 (`non-functional.md:157`) stays; add **R-6.8.26** (R-6.8.25 is the highest): "Every request to the sign-in service has a deadline. A refreshed sign-in that cannot be saved is an error: one-shot commands exit non-zero, and a watch reports it once and keeps retrying the save."

### 4.5 Risks

- The upstream API surface is unverified (4.1). The build after deleting the `replace` settles it.
- Whether any caller passes a nil HTTP client to `NewClient` today is not enumerated; tests may.
- `authRequestTimeout` also applies to each device-code poll; it must stay above the polling interval's request time, which it does by a wide margin (polls are single small requests, `FORK/deviceauth.go:196-203`).

---

## 5. The seven direct `graph.TokenSourceFromPath` calls in `internal/cli`

### 5.1 What the "session runtime" is and adds

**[VERIFIED]** `driveops.SessionRuntime` (`internal/driveops/session.go:103-128`), created per command at `internal/cli/root.go:486`, `:530`, `internal/cli/sync_flow.go:186`, `internal/cli/sync_runtime.go:187`. It adds:

- one cached `graph.TokenSource` per token path — "preventing OAuth2 refresh token rotation races (two independent refreshes can invalidate each other's refresh tokens)" (`:98-102`; `getOrCreateTokenSource`, `:255-271`);
- cached HTTP client profiles (`BootstrapMeta`, `:326-335`; interactive and sync sets);
- one Graph base URL (`GraphBaseURL`, `:107`, applied at `:283-286`);
- shared retry notices, `FlushTokenCache` (`:304-310`), and the injectable `TokenSourceFn` (`:109-111`, default `graph.TokenSourceFromPath`, `:140`).

**[VERIFIED]** It does not coordinate between processes. The token file is last-writer-wins through an atomic rename (`internal/tokenfile/tokenfile.go:91`). A CLI command and the daemon each hold their own source.

### 5.2 The seven sites

**[VERIFIED]** grep finds exactly these seven in `internal/cli`. Each builds its own token source and then `newGraphClientWithHTTP(<base URL>, runtime.BootstrapMeta(), ts, logger)` (`internal/cli/root.go:419-435`): they share the runtime's HTTP client but bypass its token cache.

| Site | Function | Notes |
|------|----------|-------|
| `drive_search.go:111` | `searchAccountSharePoint` | client at `:118-123`; error → `tokenDiscoveryAuthRequirement` (`:115`); base URL is a parameter |
| `email_reconcile.go:33` | `probeAccountIdentity` | client at `:38-43`; callers `sync_flow.go:253`, `root.go:360`, `shared_target.go:146`, `:178` |
| `drive_catalog.go:221` | `discoverDrivesForToken` (`:205-260`) | called from one goroutine per token (`:171-193`) |
| `shared_target.go:157` | `sharedBootstrapMetaClient` (`:138-171`) | also calls `probeAccountIdentity` at `:146`: two sources for one file in one call |
| `status_live.go:47` | `loadStatusLiveOverlay` (`:27-67`) | error → `statusLiveAuthOverlay` checks `errors.Is(err, graph.ErrNotLoggedIn)` (`:49`, `:69-79`); base URL from `cc.GraphBaseURL` (`:53`) |
| `drive_add.go:507` | `resolveSharedDisplayName` (`:499-533`) | passes `""` as base URL (`:512`), ignoring any configured base URL |
| `shared_discovery.go:136` | `discoverSharedTargetsForAccount` (`:105-192`) | error → `tokenDiscoveryAuthRequirement` (`:142`); base URL from `cc.graphBaseURL()` (`:146-151`) |

**[VERIFIED]** Consequences today: `cc.Session` creates two independent sources for one token file in sequence (`root.go:360` uncached probe, then `:377` cached session); four different ways of choosing the base URL; none of the seven files derives a shorter-lived context (grep for `context.With*` in them is empty), so the context bound into each source is the command context.

**[SUSPECTED]** That two sources in one process ever both refresh and invalidate each other. They are used in sequence, and a refresh by the first is saved before the second loads the file only if the save succeeded. Settle with a test that expires the token and runs `cc.Session` against an `httptest` token endpoint counting refresh calls.

### 5.3 Proposed end state

One exported method in `internal/driveops/session.go`, built on `getOrCreateTokenSource`, `BootstrapMeta` and the runtime's base URL, mirroring `clientsForTokenPath` (`:273-299`):

```go
// BootstrapClient returns a Graph client for account-level calls (identity,
// drive and shared-item discovery) made before a drive session exists. It
// uses the runtime's one token source for tokenPath.
func (r *SessionRuntime) BootstrapClient(ctx context.Context, tokenPath string) (*graph.Client, error)
```

- Errors from the token source are wrapped with `%w`, as `SharedTargetClients` already does (`:239-241`), because callers branch on `graph.ErrNotLoggedIn`.
- All seven sites become `cc.runtime().BootstrapClient(ctx, tokenPath)`. The base-URL parameters and the `""` at `drive_add.go:512` are deleted. `sharedBootstrapMetaClient` ends up with one source.
- **None of the seven needs to stay outside the runtime.** The one legitimate direct construction is the login flow, which holds the token source the login just produced (`internal/cli/auth_login.go:215`); it is not one of the seven.
- After a login or logout inside a process that already used the runtime, the cache must be flushed (`FlushTokenCache`); `cc.Session` already does this when the probe changed something (`root.go:364-365`). **[SUSPECTED]** whether any login path uses the runtime before and after the login in one process; `auth_login.go:1-150` and `auth_flow.go` were not read.
- `getOrCreateTokenSource` holds the runtime mutex across `TokenSourceFn` (`:255-271`); that call only reads a local file, so the parallel discovery goroutines of `drive_catalog.go:171-193` just serialise briefly.

### 5.4 Tests to write first

- `internal/driveops/session_test.go`: `TestSessionRuntime_BootstrapClientReusesOneTokenSourcePerPath` (a counting `TokenSourceFn`; call `BootstrapClient` twice and `InteractiveSession` once; assert one call) and `TestSessionRuntime_BootstrapClientKeepsNotLoggedIn` (`errors.Is(err, graph.ErrNotLoggedIn)`), beside `TestSessionRuntime_TokenSourceError` (`:419`).
- A source-scan guard in `internal/cli` (same technique as `internal/sync/panic_stack_test.go`): `graph.TokenSourceFromPath(` may not appear in any non-test file of `internal/cli`.
- Existing command tests for `status`, `drive list`, `drive add`, `drive search` and shared discovery must stay green with `SessionRuntime.TokenSourceFn` as their only token seam.

### 5.5 Spec and docs

- `sync-control-plane.md:621-623` already states the rule ("Session creation goes through `driveops.SessionRuntime` ..., keeping token-source caching owned in one place"); extend it to account-level bootstrap clients.
- `graph-client.md` "## Runtime Ownership" `:584-592`; `spec/design/drive-transfers.md` (governs `session.go`) and `spec/design/drive-identity.md` (governs `drive_*.go`).
- No requirement covers it (grep). Add **R-6.8.27**: "Within one process, one sign-in file has one token source."

### 5.6 Risks

- The cached source binds the first caller's context. Today every caller passes the command context; a future caller passing a short-lived context would poison the cache for the rest of the process. `BootstrapClient`'s doc comment must say so.

---

## 6. Config file lock

### 6.1 Current behaviour

**[VERIFIED]** `internal/config/write.go:770-806`, `withConfigMutationLock(path, createDir, fn)`: opens the config **directory** (`:781`), then

```go
if flockErr := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); flockErr != nil {
    return fmt.Errorf("locking config file: %w", flockErr)
}
```

(`:795`) — a blocking exclusive lock with no timeout. If the directory does not exist and is not to be created, it runs unlocked (`:783-785`). The design doc describes it as "an advisory lock on the config directory" (`spec/design/config.md:448`).

**[VERIFIED]** Who takes it:

- Every config mutation: `AppendDriveSection` (`:100-104`), `SetDriveKey` (`:210-214`), `setDriveStringKeyIfMissing` (`:247-265`), `DeleteDriveKey` (`:326-330`), `DeleteDriveKeyIfDriveEquals` (`:351-368`), `DeleteDriveSection` (`:404-408`), `DeleteDriveSectionIfDriveEquals` (`:428-444`), `RenameDriveSections` (`:480-488`).
- CLI callers: `internal/cli/sync_pause_resume.go:41`, `:49`; `resume.go:43`, `:47`; `auth_flow.go:69`, `:226`, `:230`; `drive_cleanup.go:16`, `:77`, `:117`; `drive_add.go:182`, `:270`, `:290`, `:426`, `:432`, `:473`.
- The daemon, at exactly one place: `config.ClearExpiredPauses` during a reload (`orchestrator_watch.go:827` → `internal/config/resolver.go:161-192`, `DeleteDriveKey` at `:178-179`), on the `RunWatch` goroutine.

**[VERIFIED]** Can a stuck holder block forever? Yes. A holder that is stopped (SIGSTOP, a debugger), blocked on a hung filesystem inside `fn`, or any foreign program holding a flock on that directory blocks every other taker in an uninterruptible `flock` call:

- a CLI command hangs; the first Ctrl-C only cancels a context the syscall does not see, the second reaches `os.Exit(1)` (`internal/cli/signal.go:53-56`);
- the daemon's loop hangs inside `reload` (`:540` → `:827`), with the effects listed in 1.1, while the watchdog keeps pinging.

**[VERIFIED]** The daemon already tolerates failing to clear the keys: a `DeleteDriveKey` error is only a Warn and the in-memory pause is cleared regardless (`resolver.go:183-190`).

**[VERIFIED]** Adjacent gap: catalog updates take no lock at all. `UpdateCatalogForDataDir` is load → update → save (`internal/config/catalog.go:153-162`), with daemon-side writers (`internal/multisync/shortcut_child_artifacts.go:487`, `internal/sync/engine_runtime_completion.go:447`, `internal/sync/engine.go:525`) and CLI writers (`internal/cli/status_live.go:96`, `:118`). **[SUSPECTED]** that a concurrent CLI and daemon update can lose one of the two writes; settle with a cross-process test like `TestAppendDriveSection_CrossProcessCreatesPreserveAllSections`.

### 6.2 Proposed end state

In `internal/config/write.go`:

```go
const (
    // configLockTimeout bounds how long a config change waits for another
    // onedrive-go process to finish its own change.
    configLockTimeout = 10 * time.Second            // proposed
    // configLockPollInterval is the gap between non-blocking lock attempts.
    configLockPollInterval = 50 * time.Millisecond  // proposed
)

// ErrConfigLocked: the config lock was held by another process for the whole wait.
var ErrConfigLocked = errors.New("config: locked by another process")

// configLocker carries the seams of the lock wait.
type configLocker struct {
    flock func(fd int, how int) error   // syscall.Flock
    now   func() time.Time
    wait  func(time.Duration)           // timer wait in production, a fake in tests
}

func (l configLocker) with(path string, createDir bool, fn func() error) error
```

- `with` loops `flock(fd, LOCK_EX|LOCK_NB)`: nil → run `fn`; `EWOULDBLOCK` → `wait(configLockPollInterval)` until `configLockTimeout` has passed; then return `fmt.Errorf("%w: %s has been locked for %s; if no other onedrive-go command is running, try again", ErrConfigLocked, dir, configLockTimeout)`. Any other errno is returned as today.
- `withConfigMutationLock` keeps its signature and delegates to a package default `configLocker`, so no caller changes.
- Non-blocking flock with `EWOULDBLOCK` is already used on both platforms the CI builds: `internal/multisync/owner_lock.go:52`, `internal/sync/engine_root_lock.go:113-119`.
- Daemon: `ClearExpiredPauses` keeps its Warn path for `ErrConfigLocked`; the pause still ends in memory and the keys are cleared on the next reload.
- CLI: `ErrConfigLocked` is an ordinary failure, exit 1 ("may work if run again", R-6.6.31).
- Lock target: keep the directory descriptor unless the question below is settled the other way. `engine_root_lock.go:30-38` records that flock on a directory fails with `EBADF` on Linux NFS and `ENOSYS` on some FUSE filesystems. **[SUSPECTED]** that the config directory can hit this; if a config directory on NFS matters, switch to a dedicated lock file beside the config and treat unsupported errnos as the root lock does (`isUnsupportedLockErrno`, `:77-87`).

### 6.3 Tests to write first

In `internal/config/write_test.go`:

- `TestWithConfigMutationLock_HeldLockTimesOutWithTheDirectoryNamed`: a `configLocker` whose `flock` always returns `EWOULDBLOCK`, a fake `now`/`wait` pair advancing a counter; assert `errors.Is(err, ErrConfigLocked)`, the message contains the directory, `fn` never ran.
- `TestWithConfigMutationLock_AcquiresWhenTheHolderReleases`: `flock` returns `EWOULDBLOCK` twice, then nil; assert `fn` ran once and `wait` was called twice.
- `TestSetDriveKey_CrossProcessHolderIsNotWaitedForForever`: reuse the worker-process pattern of `TestAppendDriveSection_CrossProcessCreatesPreserveAllSections` (`:160`, worker at `:215`): the worker takes the real flock and signals readiness on a pipe; the parent calls with a locker whose timeout is zero and asserts `ErrConfigLocked`.
- `internal/config` resolver tests: `ClearExpiredPauses` with a locker that times out still clears the in-memory pause (beside `TestDeleteDriveKey_PausedUntilRoundTrip`, `:563`).
- `internal/multisync/orchestrator_test.go` `TestOrchestrator_Reload_TimedPauseExpiry` (`:3484`) must stay green.

### 6.4 Spec and docs

- `spec/design/config.md:448` and verification row `:50`.
- No requirement mentions the lock (grep). Add **R-4.4.4** (R-4.4.3 is the highest, `spec/requirements/configuration.md:27`): "A config change waits a bounded time for another onedrive-go process's change; when the wait runs out the command fails and names the locked directory, and a running watch continues."

### 6.5 Risks

- A legitimately slow holder (the config on a very slow disk) would make another command fail after 10 s instead of waiting. Every holder I read only edits and atomically rewrites one small file (`write.go:812-827`).
- Polling adds up to one poll interval of latency under contention.

---

## 7. Control socket: server and client

### 7.1 Current behaviour

**[VERIFIED]** Server (`internal/multisync/control_socket.go:115-118`):

```go
server := &http.Server{
    Handler:           handler,
    ReadHeaderTimeout: controlHeaderTimeout,
}
```

`controlHeaderTimeout = 5 * time.Second` (`:24-37`). No `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, `MaxHeaderBytes` or `ErrorLog`. With `IdleTimeout` and `ReadTimeout` both zero, idle keep-alive connections never time out (`/usr/lib/go/src/net/http/server.go:3087-3091`).

**[VERIFIED]** Can a stuck client block the accept loop or the orchestrator loop? No. Each connection has its own goroutine; replies to the loop go through a buffered(1) channel (`:333`), so a handler that has left never blocks the loop. A stuck client can pin a handler goroutine and a connection: the perf-capture body is read with a size cap but no deadline (`:387`, `maxControlRequestBytes` at `:24-37`), and responses have no write deadline.

**[VERIFIED]** Handlers wait for the loop with no deadline of their own (`:333-345`): both selects end only on the loop's answer or the request context.

**[VERIFIED]** Can a hung daemon make `pause` / `resume` / `status` / `reload` hang forever? No: every client call has a context timeout — `controlClientTimeout = 2 * time.Second` (`internal/cli/control_client.go:22`, applied at `:72` and in `reload`, `:176-193`), capture = duration + 10 s (`:23`, `:159-174`). But the outcomes are silent or wrong:

- `pause` / `resume`: any error that is not "socket gone" or `reload_failed` prints "Note: could not tell the running 'sync --watch' about the change (...); it applies when sync next starts." and exits 0 (`internal/cli/pause.go:82-89`). That sentence is false while the daemon is alive: the daemon picks up config file changes by itself within its 5 s poll (`watch_config_file.go:16`, `:48-84`), and a reload that takes longer than 2 s still completes after the client gave up, because the command was already handed to the loop (`control_socket.go:334-345`, `:568`).
- `status` drops the live overlay on any probe error without saying so (`internal/cli/status_runtime.go:19-42`, `:21-23`).
- `drive reset-sync-state` refuses only when the probe itself failed or found an owner (`internal/cli/drive_reset_sync_state.go:133-157`); a daemon whose control server has stopped looks like "no owner".

**[VERIFIED]** If `Serve` returns unexpectedly, the daemon logs one Warn ("The control channel stopped; status, pause and resume can't reach this sync until it restarts") and carries on with no control channel (`control_socket.go:128-136`); nothing restarts the listener.

**[VERIFIED]** Perf capture has no maximum duration: the server rejects only `duration <= 0` (`:524-527`) and `internal/perf/capture.go:168-175` waits for whatever it is given, while the design says "The request carries bounded duration" (`sync-control-plane.md:469`).

**[VERIFIED]** Wrong requirement IDs: the four control-client tests in `internal/cli/control_client_additional_test.go` carry `// Validates: R-2.10.5` (`:18`, `:30`, `:40`, `:107`), the block-scope trial requirement (`spec/requirements/sync.md:131`); the control RPC requirement is R-2.9.3 (`sync.md:118`).

### 7.2 Proposed end state

Server constants in `control_socket.go` (values proposed):

```go
const (
    controlReadTimeout     = 10 * time.Second  // whole request including body
    controlIdleTimeout     = 30 * time.Second  // keep-alive connection with no request
    controlWriteTimeout    = 5 * time.Second   // writing a reply once it is ready
    controlCommandTimeout  = 5 * time.Second   // waiting for the loop to accept a command
    controlReloadTimeout   = 60 * time.Second  // waiting for an accepted reload to finish
    maxControlHeaderBytes  = 16 * 1024
    maxPerfCaptureDuration = 5 * time.Minute
)
```

- `http.Server{ReadTimeout, IdleTimeout, MaxHeaderBytes, ErrorLog}`. A server-wide `WriteTimeout` would cut a perf capture short, so each handler sets its own write deadline with `http.NewResponseController(w).SetWriteDeadline(...)`: status and stop = `controlCommandTimeout + controlWriteTimeout`; reload = `controlCommandTimeout + controlReloadTimeout + controlWriteTimeout`; capture = requested duration + `controlWriteTimeout`.
- `handleControlRequest` (`:308-346`) gets two bounded waits with different answers:
  - the loop did not **accept** the command within `controlCommandTimeout` → HTTP 503 with a new code `busy` ("sync is busy and did not answer"). Nothing was changed.
  - the loop accepted a reload but did not finish within `controlReloadTimeout` → HTTP 202 with code `reload_in_progress`. The reload is running.
- `internal/synccontrol/protocol.go`: add the two codes beside the existing ones (the documented list is at `sync-control-plane.md:480-482`).
- `perfCaptureOptionsFromRequest` (`:516-542`) rejects durations above `maxPerfCaptureDuration` with `invalid_request`.
- An unexpected `Serve` return reports on the `fatal` channel of item 2 and ends the watch with exit 1, instead of one Warn.

Client (`internal/cli/control_client.go`, `pause.go`):

- `controlReloadClientTimeout`, defined as the sum of the server's reload waits plus a margin, replaces the 2 s context in `reload`.
- `notifyDaemonWithOptions` (`pause.go:52-91`) prints by outcome: `busy` or a client timeout → "The running 'sync --watch' is busy; it will pick up the change when it is free." (true because of the config poll); `reload_in_progress` → "The running 'sync --watch' is applying the change."; the "applies when sync next starts" sentence remains only for the socket-gone case (`:82-84`).
- `status`: when the probe found a socket but the status call failed or timed out, print one line saying the running sync did not answer, instead of silently dropping the overlay.

### 7.3 Tests to write first

Real Unix sockets cannot live in a `testing/synctest` bubble (`internal/sync/virtual_clock_test.go:28-30`), so these use readiness channels and injected short timeouts.

- `internal/multisync/control_socket_hardening_test.go`: `TestControlSocketServer_SetsReadIdleAndHeaderLimits` (assert the `http.Server` fields); `TestControlSocket_SlowBodyIsCutOff` (connect, send headers and half a capture body, assert the server closes the connection); `TestHandleControlRequest_LoopNotAcceptingAnswersBusy` (nobody reads `commands`; inject a tiny `controlCommandTimeout`; assert 503 and code `busy`); `TestHandleControlRequest_SlowReloadAnswersInProgress` (a reader accepts the command and never answers; assert 202 and `reload_in_progress`); `TestPerfCaptureOptions_RejectsTooLongDuration` beside `TestOrchestrator_OneShotControlSocket_PerfCaptureRejectsInvalidDuration`.
- `internal/multisync/orchestrator_test.go`: `TestRunWatch_ControlServerExitEndsTheWatch`, beside `TestOrchestrator_ControlSocket_StatusAndStop` (`:3005`).
- `internal/cli/control_socket_semantics_test.go`: `TestNotifyDaemon_BusySyncSaysItWillPickTheChangeUp` and `TestNotifyDaemon_ReloadInProgressSaysSo`, beside `TestNotifyDaemon_ReloadRejectedByRunningSyncIsAWarning` (`:173`); extend `TestProbeControlOwner_ClassifiesOutcomes` (`:112`).
- `internal/cli/control_client_additional_test.go`: extend `TestControlSocketClientReloadAndPostJSON` (`:41`) for 202 and 503; change the four `// Validates:` lines to R-2.9.3.

### 7.4 Spec and docs

- R-2.9.3 (`spec/requirements/sync.md:118`): add `busy` and `reload_in_progress` to the typed errors, and "a client is never told a change waits for the next start while the running sync will apply it".
- New **R-2.9.7** (R-2.9.6 is the highest): "Every control socket read, write and wait for the sync loop has a deadline; a control channel that stops ends the watch."
- `sync-control-plane.md` "### Control Socket" `:427-493` (constants, codes `:480-482`, capture bound `:469`), "## Verified By".
- `cli.md` control protocol summary `:610-634`, "## Pause / Resume" `:636-640`.

### 7.5 Risks

- `controlReloadTimeout` only shapes the answer; the reload itself is bounded by item 1 and item 6, not by this.
- Ending the watch when the control server stops trades a degraded daemon for a restart. That is consistent with item 1's choice.

---

## 8. One-shot `sync` exit status when work is left

Owner decision already taken: a one-shot `sync` that leaves failed work exits non-zero.

### 8.1 Current behaviour

**[VERIFIED]** The command ignores item-level outcomes by design (`internal/cli/sync_flow.go:112-117`):

```go
// Item-level failures are not a failed run: each one is printed as it
// happens and the engine keeps it as a durable condition that status
// reports (a read-only shared folder, a locked file, throttling).
// Failing every run on such a state would make the exit status useless
// to cron. A drive that could not sync at all still fails the command.
return runOnceResultError(result)
```

`runOnceResultError` (`internal/cli/sync_render.go:36-78`) fails only when every selected mount is paused (`:37-39`), a mount was skipped at start-up (`:44-51`), or a mount report carries `Err` (`:53-61`). The same rule is written into R-6.6.28 (`non-functional.md:94`), `cli.md:324-326` and `:152-153`, and `docs/scripting.md:97-102`.

**[VERIFIED]** There is no partial-failure exit code: "Every other failure, transient or not, stays 1" (`internal/cli/failure_class.go:12-26`; R-6.6.31, `non-functional.md:97`).

**[VERIFIED]** The vocabulary a run produces:

| Quantity | Source | Meaning |
|----------|--------|---------|
| `tally.failed` | `internal/cli/sync_console.go:262-285`; classes Actionable, Fatal, Invalid (`internal/sync/engine_activity.go:96-106`) | an action ended with an error that is not retried in this run |
| `tally.retrying` / `willRetry()` | `sync_console.go:62-64`, `:262-285`; classes RetryableTransient, ScopeBlockingTransient | paths whose last attempt failed and is still to be retried; a later success removes the path |
| `Report.Failed` | `internal/sync/api_types.go:58`; summed over passes (`internal/sync/engine_run_once_report.go:85`) | cumulative; the console corrects it with `max(tally.failed, r.Failed-tally.retried)` (`sync_console.go:323`) |
| `Report.LeftForNextSync` | `api_types.go:50`; `internal/sync/engine_run_once.go:367-369`, `:164` | planned in the last pass and neither succeeded nor failed (throttle wait longer than the run waits, `:147`, `:170-173`) |
| `Report.DeferredByMode` | `api_types.go:40`; `internal/sync/actions.go:9-23` | suppressed by `--upload-only` / `--download-only`; printed "Held back by --…" (`sync_console.go:421-441`) |
| `Report.HeldLocalDeletes` / `HeldRemoteDeletes` | `api_types.go:44-45` | deletes awaiting `--confirm-deletes`; printed to stderr (`sync_console.go:402-418`) |
| `Report.ConflictCopies` | `api_types.go:37` | conflicts resolved by keeping both ("kept both versions", `sync_console.go:378`) |
| Paused drive in a multi-drive run | `sync_console.go:234-236` | "Skipped (paused)." |
| Durable leftovers | `internal/sync/store_inspect.go:94-115`: `RetryingWork`, `BlockedRetryWork`, `ActionableRetryWork` (R-2.10.71), `ObservationIssues`, `HeldDeletes` | what `status` shows after the run |

**[VERIFIED]** Two things exit 0 today that are plainly failures: a plan-invariant violation sets `report.Failed = len(plan.Actions)` and returns a nil error (`engine_run_once.go:326-337`); and a throttled run that leaves everything for the next sync.

**[VERIFIED]** The JSON summary disagrees with the text summary: `will_retry` omits `LeftForNextSync`, and `status` is always `"done"` for a drive that has a report, even with `failed > 0` (`internal/cli/sync_console_json.go:131-166`, `:151-153`; text version `sync_console.go:327`). R-6.6.33 already says a partly failed run "exits non-zero" (`non-functional.md:99`).

### 8.2 What counts as "failed work"

Proposed definition. A drive left failed work when, after its last pass, any of these is non-zero:

1. **failed** — the number the summary already prints: `max(tally.failed, r.Failed - tally.retried)`;
2. **will retry** — `tally.willRetry() + r.LeftForNextSync`;
3. **still open** — durable leftovers in the store at the end of the run: retrying work, work blocked under a pause scope, work refused by a condition only the user can clear, and observation issues that mean a file is not synced.

Not failed work (exit status unaffected):

- `DeferredByMode` (the user asked for one direction);
- held deletes awaiting `--confirm-deletes`, including the `deletes_held` issue type (`internal/sync/issue_types.go:37`);
- conflict copies (both versions are kept and synced);
- a paused drive in a run where another drive ran (all paused is already an error, `sync_render.go:37-39`);
- `--dry-run` (nothing was attempted).

**The definitional point the owner should confirm: clause 3.** A write into a read-only shared folder fails on the run that attempts it (clause 1). On later runs the engine may plan nothing for it — "observation may suppress the next plan and prune it" (`internal/sync/engine_runtime_completion.go:593-594`) — so clauses 1 and 2 alone would exit 1 once and 0 from then on, while the file is still not on OneDrive and `status` still lists it. With clause 3 the exit status says the same thing as `status` on every run. I recommend including clause 3; it deliberately reverses the old rationale in R-6.6.28 ("so a persistent state ... does not fail every run"). **[SUSPECTED]** the exact later-run behaviour, because I did not read the suppression path (section 10); the e2e tests in 8.5 settle it.

Clause 3 needs a classification of each issue type as "fails the run" or "deliberate". I read only part of the vocabulary (`issue_types.go:11`, `:20`, `:37`, `:62`), so the implementing PR must classify every constant in `issue_types.go:11-62` in one table beside them, with a test that fails when a new constant is unclassified.

### 8.3 Proposed end state

**Engine** (`internal/sync/api_types.go`, filled at the end of `RunOnce`, `internal/sync/engine_run_once.go:31-103`, from the store's own snapshot method, `internal/sync/store_inspect.go:188`):

```go
// Unresolved counts what the store still holds after the run that only a
// later run or the user can finish. Deliberate holds are not counted.
type UnresolvedCounts struct {
    Retrying   int // retry_work waiting on the retry curve
    Blocked    int // retry_work under a pause scope
    Actionable int // retry_work refused by a condition only the user can clear
    Issues     int // observation issues that leave a file unsynced
}

func (u UnresolvedCounts) Total() int
```

`Report` gains `Unresolved UnresolvedCounts`. A dry run leaves it zero.

**CLI** (`internal/cli/sync_render.go`):

```go
// errSyncLeftWork: the run finished but left files unsynced.
var errSyncLeftWork = errors.New("sync left work unfinished")

type leftWorkCounts struct{ failed, willRetry, stillOpen int }

func (c leftWorkCounts) any() bool

// leftWork is the single source for the text summary, the JSON summary and
// the exit status.
func leftWork(r *syncengine.Report, tally *activityTally) leftWorkCounts
```

- `reportSummary` (`sync_console.go:320-352`) and `jsonSummary` (`sync_console_json.go:131-166`) both take their numbers from `leftWork`. The text line gains "; N still not synced from earlier runs (see 'onedrive-go status')" when `stillOpen > 0` and stops printing "Up to date" in that case (`:336-338`).
- JSON: `will_retry` includes `LeftForNextSync`; a new `still_open` count; per-drive `status` becomes `"incomplete"` (new constant beside `syncJSONStatusDone` / `syncJSONStatusFailed`, `:37-40`) when `leftWork(...).any()`.
- `runOnceResultError(result, left map[mount]leftWorkCounts)` keeps its present order (all paused, start-up skips, mount errors) and then, when any drive left work, returns `newPresentedError(msg, errSyncLeftWork)`. Single drive: "sync left N files unsynced; run 'onedrive-go status' for details". Multi-drive: "N of M selected drives left files unsynced (see above)". The text is printed once by the root command (R-6.6.22).
- **Exit code 1.** `commandExitCode` needs no change: an untyped error is `exitFailure` (`failure_class.go:54-74`). That matches the table's meaning for 1, "may work if run again" (`docs/scripting.md:86-102`). A drive error typed 77 or 78 still wins, because mount errors are examined first.
- The `cc.seams.syncRunOnce` seam does not receive the activity hook (`sync_flow.go:38-46`, `:182-184`); `leftWork` therefore falls back to the report when the tally is empty, as `tallyFor` already does (`sync_console.go:287-318`).
- Replace the comment at `sync_flow.go:112-116`.

### 8.4 Unit tests to write first

- `internal/cli/sync_test.go` (the `TestRunSyncCommand_*` pattern, `:431-840`, via `cc.seams.syncRunOnce`): `TestRunSyncCommand_FailedActionsExitNonZero`, `..._WorkLeftForNextSyncExitsNonZero`, `..._StillOpenConditionsExitNonZero`, and four that pin exit 0: `..._DeferredByModeExitsZero`, `..._HeldDeletesExitZero`, `..._ConflictCopiesExitZero`, `..._DryRunExitsZero`, `..._PausedDriveBesideASyncedDriveExitsZero`. Each `// Validates: R-6.6.28`.
- `internal/cli/sync_console_test.go`: extend `TestSyncConsole_SummaryCountsWorkAThrottleLeftForTheNextSync` (`:166`), `TestSyncConsole_SummaryUpToDateAndDryRun` (`:131`, no "Up to date" with open conditions), `TestSyncConsole_JSONWritesOnlyJSONLines` (`:308`, `will_retry`, `still_open`, `status`), `TestSyncConsole_SingleDriveErrorIsLeftToTheReturnedError` (`:185`, new signature), `TestSyncConsole_SummaryDoesNotPromiseARetryOfWorkTheRunFinished` (`:347`).
- `internal/cli/failure_class_test.go`: `TestCommandExitCode` (`:34`) case `errSyncLeftWork` → 1.
- `internal/sync`: `TestRunOnce_ReportCountsUnresolvedWork` (seed retry work and an observation issue through the store helpers the existing `RunOnce` tests use; assert `Report.Unresolved`), `TestRunOnce_DryRunLeavesUnresolvedZero`, `TestIssueTypes_EveryTypeIsClassifiedForOneShotExit`, and one asserting the plan-invariant path (`engine_run_once.go:326-337`) now yields a non-zero exit through `leftWork`.

### 8.5 e2e tests that rely on exit 0 today and must change

**[VERIFIED]** by reading the helpers and the named tests:

- `runSyncThroughTransientFailures` (`e2e/e2e_test.go:571-610`) calls `runCLIWithConfig` on every attempt (`:587`), which requires success (`e2e/cli_runner_test.go:99-112`, `:108`), and then looks for left work in the output (`syncLeftWorkPattern`, `:553-555`). It must use `runCLIWithConfigAllowError` (`cli_runner_test.go:116-125`) and assert the pairing: left-work text ⇔ non-zero exit. Its 12 call sites: `e2e/edge_cases_e2e_test.go:41`, `:75`, `:110`, `:150`; `e2e/sync_hardening_full_e2e_test.go:126`, `:129`, `:133`, `:165`, `:167`, `:354`, `:356`, `:361`. The comment on `syncRecordedRetryWork` ("and still exits zero", `e2e_test.go:556-558`) becomes false.
- `runSyncWithMode` (`e2e/sync_full_test.go:503-508`) requires success.
- `TestE2E_Shared_ReadOnlyFolder_DiscoveryDriveAddAndBlockedWriteUX` (`e2e/shared_items_e2e_test.go:202`): `sync --upload-only` into a read-only shared folder, then `status` shows "Shared folder writes blocked" (`:208`). Must expect exit 1, and with clause 3 a second sync must also exit 1.
- `TestE2E_Shortcut_ReadOnlyBlockedUploadStatus` (`e2e/sync_shortcut_full_e2e_test.go:208`): same.
- `TestE2E_Status_JSON_IssueDetails` (`e2e/cli_commands_e2e_test.go:243`): an `invalid_filename` issue; exit 1 if that issue type is classified as failing the run.
- `TestE2E_SyncFull_RemoteDeleteGoesToLocalTrashAndRestores` (`e2e/sync_safety_full_e2e_test.go:137`): `runCLIWithConfig(..., "sync")` inside a polling loop; any transient left-work pass now fails the test.
- Already tolerant, to be pinned to the new contract: `TestE2E_SyncFull_DeleteHoldKeepsOneDriveUntilConfirmed` (`sync_safety_full_e2e_test.go:69`; `:80`, `:104`) should assert exit 0 with held deletes; `TestE2E_SyncFull_ReplacedSyncFolderIsRefusedUntilRebound` (`:171`); `requireSyncEventuallyConverges` (`e2e_test.go:491-551`).

**Not audited one by one:** the remaining direct `"sync"` invocations. Counts per file, for the PR's audit: `sync_full_test.go` 26, `sync_hardening_full_e2e_test.go` 16, `sync_shortcut_full_e2e_test.go` 14, `sync_watch_full_test.go` 12, `sync_recovery_e2e_test.go` 10, `sync_safety_full_e2e_test.go` 9, `cli_commands_e2e_test.go` 8, `sync_demoted_full_e2e_test.go` 7, `sync_watch_e2e_test.go` 7, `orchestrator_e2e_test.go` 7, `sync_edge_cases_full_test.go` 7, `sync_kill_mid_transfer_full_e2e_test.go` 5, `timing_helpers_test.go` 4, `shared_items_e2e_test.go` 3, `sync_shared_e2e_test.go` 3, `output_validation_e2e_test.go` 3.

### 8.6 Spec and docs

- R-6.6.28 (`non-functional.md:94`): rewrite the one-shot sentence to the definition in 8.2.
- R-6.6.31 (`:97`): state that a one-shot sync that left work exits 1.
- R-6.8.15 (`:158`): failed work stays durable **and** the run that leaves it exits non-zero.
- R-6.6.19 (`:85`) for the "still not synced" clause; R-6.6.33 (`:99`) for the JSON fields.
- `cli.md` "## Exit Status" `:123-153` (`:152-153`), `:324-326`, verification row `:50`.
- `docs/scripting.md:17-22`, `:86-102`; `docs/running-as-a-service.md:111-117`.

### 8.7 Risks

- Cron and timer users who run with `set -e` or alert on non-zero will start hearing about conditions that used to be silent. That is the decision; the release note must say it.
- With clause 3, one permanently refused file makes every run exit 1 until the user deals with it. Without clause 3, exit 0 can coexist with unsynced files. There is no third option that satisfies both.
- e2e against the live service: transient throttling now fails any test that asserts success on a single sync; the helper change in 8.5 has to land in the same PR.

---

## 9. Adjacent findings (not asked for, found while tracing)

1. **Run mode missing from runner equivalence** — section 1.3. Code VERIFIED, consequence SUSPECTED.
2. **Owner lock released before the runners are drained on a signal.** **[VERIFIED]** The control server's ctx-cancel goroutine calls `Close` (`internal/multisync/control_socket.go:138-150`), which removes the socket and releases the owner flock (`:253`, `:262`), concurrently with `RunWatch`'s deferred drain (`orchestrator_watch.go:63-71`). During a slow drain neither the socket nor the owner lock shows a running owner; only each engine's root lock does. `acquireSyncRootLock` has one production caller (`internal/sync/engine.go:155`), so `drive reset-sync-state` (`internal/cli/drive_reset_sync_state.go:110`, `:133-157`) does not take it. **[SUSPECTED]** that a reset during the drain can delete a store an engine is still closing; settle by reading `ResetStateDB`. Fix with item 1's PR: release the owner lock after the drain, and make the reset take the root lock.
3. **Catalog read-modify-write is unlocked** — section 6.1.
4. **`cli.md:183-185` says there is no SIGHUP reload** — section 3.1. VERIFIED drift.
5. **Wrong `// Validates:` and requirement IDs** — `internal/sync/panic_stack_test.go:13` (R-6.7.1, should be R-6.3.8); `internal/cli/control_client_additional_test.go:18`, `:30`, `:40`, `:107` (R-2.10.5, should be R-2.9.3); comment at `internal/cli/sync_runtime.go:230-233` (R-6.6.33, should be R-6.6.35). VERIFIED.
6. **JSON sync summary disagrees with the text summary** — section 8.1. VERIFIED.
7. **Perf capture duration is unbounded although the design says bounded** — section 7.1. VERIFIED.
8. **`graph.NewClient` silently defaults to `http.DefaultClient`** — section 4.1. VERIFIED.

---

## 10. Not read

Claims that depend on these are marked SUSPECTED above.

- Engine watch shutdown: how `Engine.RunWatch` ends after cancellation and any drain bound it owns (`internal/sync/engine_watch*.go` beyond `engine_watch.go:58`, `:167`, `:667`). Needed for `watchRunnerStopTimeout`.
- Whether the engine keeps the context passed to `engineFactory` after construction (`internal/multisync/mount_engine_config.go`, `internal/sync/engine.go` constructor). Needed to bound the runner start step.
- Durations and I/O of `buildRuntimeWorkSet`, `finalizeSuccessfulFinalDrainMounts`, the shortcut artifact purge.
- Shortcut root record transitions into the final-drain states (whether record fields other than the state change).
- The planner's suppression of work under a known condition (`internal/sync/condition_projection.go`, `engine_runtime_admission.go`, `engine_runtime_permissions.go`); whether a `ClassInvalid` failure leaves a `retry_work` row; how a `retry_work` row is cleared on success; the observation-issue path for `invalid_filename`.
- The full issue vocabulary in `internal/sync/issue_types.go` (only lines `:11`, `:20`, `:37`, `:62` recorded by name).
- How `internal/graph` and `internal/sync` classify an arbitrary error returned by a token source.
- `internal/cli/auth_login.go:1-150`, `internal/cli/auth_flow.go` (login sequence and its use of the session runtime); exact signatures of `graph.Login` / `graph.LoginWithBrowser` beyond `internal/graph/auth.go:41-50`, `:82-91`.
- Callers that pass a nil HTTP client to `graph.NewClient`.
- Transitive imports of `internal/graphtransport` (only direct imports of `graphtransport`, `retry`, `perf`, `tokenfile` were checked for a cycle with `graph`).
- Upstream `golang.org/x/oauth2` v0.30.0 (not in the module cache; no network).
- Identity-platform behaviour for a superseded refresh token.
- `ResetStateDB`; `internal/config/validated_state.go:140-180`; callers of `RenameDriveSections`.
- `internal/sdnotify/sdnotify_test.go`; exact type and constant names in `internal/synccontrol/protocol.go` (file read, names not recorded).
- Go runtime exit status of an unrecovered panic (`runtime/panic.go`) and SIGABRT behaviour (`os/signal/doc.go`); systemd start-limit defaults (`systemd.unit(5)`).
- The kill-mid-transfer e2e tests; the per-invocation audit of the e2e `sync` calls counted in 8.5.

---

## 11. Dependency-ordered PR breakdown

PRs 1 to 4 are independent of each other and of the rest. The others depend as stated.

| # | PR | Items | Depends on | Contents |
|---|----|-------|------------|----------|
| 1 | One-shot `sync` exits 1 when work is left | 8 | owner confirms clause 3 (8.2) | `Report.Unresolved`; `leftWork`; `runOnceResultError`; text and JSON summary; issue-type classification table and test; e2e helper and the tests in 8.5; R-6.6.28, R-6.6.31, R-6.8.15, R-6.6.19, R-6.6.33; `cli.md`, `docs/scripting.md`, `docs/running-as-a-service.md` |
| 2 | Control socket deadlines and honest client messages | 7 | — | server timeouts, `busy` / `reload_in_progress`, capture maximum, `ErrorLog`, client reload timeout and messages, `status` note; R-2.9.3, new R-2.9.7; fix the four `// Validates:` IDs |
| 3 | Bounded config lock | 6 | — | `configLocker`, `ErrConfigLocked`; R-4.4.4; `config.md` |
| 4 | One token source per sign-in file | 5 | — | `SessionRuntime.BootstrapClient`; route the seven sites; source-scan guard; R-6.8.27 |
| 5 | Auth requests get a tuned HTTP client | 4a | 4 (one remaining `TokenSourceFromPath` caller) | `graphtransport.AuthClient`; inject into the token source and both login flows; delete the `http.DefaultClient` fallback in `NewClient` |
| 6 | Drop the oauth2 fork; save failures are errors | 4b, 4c | 4, 5; owner picks design A or B (4.2) | delete `go.mod:41-42` and `OnTokenChange`; `tokenSaver` / `persistingTokenSource`; `SaveError` and `TokenSaveErrors`; one-shot exit 1 on an unsaved sign-in; R-6.8.26; `graph-client.md:598-601` |
| 7 | Runner identity, bounded joins, fail closed | 1 (+ 9.1, 9.2) | — | generation on runners and events; run mode in equivalence; sends select on `mountCtx`; `retireWatchRunner`; bounded `closeEngine`; `ErrWatchRunnerStuck` → exit 1; owner lock released after the drain; reset takes the root lock; R-6.3.9; `sync-control-plane.md:415-417`, `:597-602` |
| 8 | Panic boundaries everywhere | 2 | 7 (bounded drain so the loop's recover is reached; shared `fatal` channel); 2 touches the same server struct | `internal/panicguard`; recover at `RunWatch`, helper goroutines, CLI goroutines, one-shot goroutines, `mainWithWriters`; rewritten scan test plus the goroutine-boundary test; R-6.3.10; fix the R-6.6.33 comment and the R-6.7.1 test ID |
| 9a | Loop heartbeat, observe only | 3 | 7, 8, 3, 2 (every long loop step has a real bound to declare) | `loopHeartbeat`, `LoopAlive`, heartbeat tick, declared steps, Error log when the loop is silent; pings unchanged |
| 9b | Watchdog ping follows the heartbeat | 3 | 9a after a clean long watch soak; PR 6 if the daemon's save-error check rides on the heartbeat tick | conditional ping in `runWatchdog`; `STATUS=` on withhold; R-6.6.32; `cli.md:179-185` including the SIGHUP sentence; `docs/running-as-a-service.md:64-65`; unit comment |
