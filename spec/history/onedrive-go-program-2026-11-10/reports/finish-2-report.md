# PR 2 (finish-2) build report, 2026-11-21

Branch `refactor/finish-2` on `origin/refactor/finish-1` (58fde1d1), pushed.
Worktree `~/src/onedrive-go-wt/finish-2`. Checks per row: build, vet, and
`go test -short` of the changed packages. F5 also ran the two engine-model
tests without `-short`, because the row's own test needs them.

## Rows

- E5: 088e83d5. Six phases and five events on one transition table; inputs enabled per phase.
- B10: 6467ee65. Loop heartbeat. Observe only: logs one Error on silence and keeps pinging.
- E9: cf012026. `observation` type for the off-loop goroutines, plus 069fe3bf (stress lists for E5's tests).
- E10: skipped, far bigger than an s row. Deleting `engineFlow.engine` needs the `shortcutRoots` owner over about 26 Engine methods. The analysis leaves its field set SUSPECTED. It also needs the observer chain's fetchers named one by one. E9 moved the pointer into `observation`, so 58 uses still reach it.
- E12: skipped, far bigger. The `syncsched` package replaces about 60 direct map sites, and the tests that reach into internals must be rewritten first (05 §3.6).
- F5: 919e06b9. Wall-clock ban (forbidigo); the model's clock is fake; seeded jitter.
- F6: skipped, far bigger. An in-memory `synctree` needs synctree's descriptor-level operations rerouted (rmdir, detach, no-replace rename, birth time, ctime, root mark). Uploads, hashing and followed-symlink reads that go through `localpath` would have to move too.
- F7: skipped. It depends on F6 (B-8 needs B-6 and B-7), and a watch-mode model is a new model.
- I1: skipped, far bigger. An identity-keyed planner plus a reviewed `matrix.identity.golden` over the 99 cells is research-sized.
- I2: skipped, needs I1.
- I3: skipped, needs I1.
- G9: 168c6539. One detector, `DirNames` (on-disk names only). The baseline name index and `RecentLocalDeletes` are deleted. The rider is done: `onDiskSiblingName` and `localPathStillPresent` now use `namekey.FindStored`.
- G10: closed without a measurement. A replan of a large unchanged tree hashes nothing today: the baseline fast path covers synced files and the local hash cache covers new files. The saving exists only per new watch-created file (O5) or per modified-but-not-uploaded file (O6), never per tree. So the row's measurement could not show a saving, and running one would also have disturbed the running watch client on `~/tommi`.
- G11: 37824821. Watch-side collision detection deleted. The agreement test passed in G9 first, then became the scan-only `TestLocalCollisions_ScanJudgesTheNamesOnDisk`.

## For the review: behaviour changes

- E5: a stop that overtakes bootstrap now ends in Stopped. `finishBootstrapAfterActions` is skipped (no BootstrapQuiesced event, no `upkeep.afterSync`, no refresh timer armed), and RunWatch returns without starting observers. Before, it reached `startObservers` and relied on its refusal. The drain step no longer sets the intake channels to nil; the phase table gates them instead.
- B10: new Error log "the sync loop has made no progress past its limit". WATCHDOG=1 is still sent unconditionally. The "starting a drive" step limit (5 min) is only observed; it is not a deadline, because starting a runner has no bound of its own.
- F5: the one-shot pause wait now runs on the run clock. Context deadlines and perf durations stay on wall time through `syncdomain.WallNow`/`WallSince`. The model's clock only moves when the model advances it or when a timer or sleep it armed ends. A frozen clock with real timers hung the errno-shim ENOSPC test, because the admit wait kept re-arming.
- G9 (file operations): watch mode no longer treats a baseline spelling that is gone from disk as a rival. Before, a case-only rename's new name could be held back unless the delete had been seen first (C2). The scan's verdict is the same as before for the existing tests: an unchanged synced sibling keeps syncing, and a changed one waits too. A twin that `admitSyncedEntry` sets aside is removed from the scan's names, so a twin is reported only once.
- G11 (file operations): watch mode emits collider creates and writes instead of dropping them. Peer re-emission on delete is gone, so the survivor syncs on the next pass instead of at once. Blocking is now only the replan scan's finding. The spec (R-2.12.2, R-2.12.3, R-2.13.2, sync-observation.md, system.md, sync-domain.md) is updated to match.

## Other

- `internal/devtool` `TestDODPrePRRunsThePRProfileByDefault` failed on this branch. Expected: golangci-lint first in the pr profile; actual: no lint step. This branch changes only `verify_stress_members.go` in devtool. Unverified, possibly from the base's "lint only the changed packages" commit; the cause is unknown. The stress-list test passes.
- The temporary worktree `finish-2-g9`, used to draft G9 while tests ran, is removed along with its branch. No processes left running.
