# T1: per-PR test time (handed back 2026-11-21 ~08:10 PT, final chain running)

Head `d2756bcf` (local, not pushed). It adds two commits on top of 52af46ec:
- `9d165658`: a store opened through another driver (the fault wrapper) compiles every query; a failed prepare falls back to the in-transaction query; mutex guard comment. This fixes TestFindMutexDocViolations_RealRepoIsClean and TestPlanningLoadError_SameTextInOneShotAndWatch, both re-run green.
- `d2756bcf`: under -short, `newTestEngineInputs` calls `syncfixture.SkipUnsampledInShort`. A test that builds an engine runs only if the FNV hash of its top-level name is 0 mod 8. The full tiers run all of them.

Running detached under the heavy lock: `scratchpad/T1-final.sh`. In order it runs the pre-pr gate (`prepr.log`), the single timing run `go test -short -p 1 ./internal/sync/...` (`timing.out`, with `/usr/bin/time` in `timing.time`), and the item-2 agreement run, non-short `./internal/sync/ ./internal/sync/internal/syncplan/` (`agree.out`). It writes one line per step to `~/.cache/onedrive-go-tmp/t1/final.txt`, then `FINAL-DONE`.

Left: read final.txt; push; `onedrive-go-install`; agent-repo CHANGELOG and CLAUDE.md; delete the base worktree `~/.cache/onedrive-go-tmp/t1/base` and the scratchpad scripts `T1-measure.sh`, `T1-pairs.sh` and `T1-final.sh`. The ship-time race run should become short-tier only (`-race -short`); that is the orchestrator's command, not code.

# T1: per-PR test time (handed back 2026-11-21 ~01:10 PT, A/B measurement series running)

**Update 01:10:** the method is now the orchestrator's: interleaved A/B pairs, decided on CPU time. `scratchpad/T1-pairs.sh` runs detached (setsid) and takes the heavy lock for each run. It warms both trees, then runs (a) `go test -short -count=1 ./...` as base, branch, base, branch, then (b) `go test -race -count=1 -p 1 ./internal/sync/ ./internal/sync/internal/syncplan/` the same way. `T1-measure.sh` writes one line per run (wall, user+sys CPU from `/usr/bin/time`, load at start and end) to `~/.cache/onedrive-go-tmp/t1/results.txt`, the output to `<label>.out`, and `SERIES-DONE` at the end. The (b) pairs could take 4-6 h. To stop it: `pkill -f T1-pairs.sh; pkill -f T1-measure.sh`, then kill the go test it started.


State: **three commits on local branch `test/per-pr-speed` (head `52af46ec`), not pushed, no PR, not installed.** All my processes are killed and I hold no lock.

## Commits (worktree `~/src/onedrive-go-wt/per-pr-speed`, base main `f2392533`)
- `bbc7c9d7` Open test stores without waiting for the disk. Adds `syncstore.Hooks.SkipFsync`, which opens the store with `synchronous(OFF)`. Set by `syncstoretest.TestStoreHooks`, `newTestEngineInputs` and `exQuietStore`. Zero hooks keep FULL: `TestHooks_ZeroValueIsProduction`, `TestHooks_SkipFsyncTurnsSynchronousOff` and `TestEngineHooks_ZeroValueIsProduction` assert it. The delete-boundary crash test stays on FULL through `keepRealFsync` (`TestTestEngines_SkipFsyncUnlessTheTestKeepsIt`). The convention check (R-6.10.29) refuses production code that sets a hook.
- `00710d14` Prepare the planner's two big queries once per store. `BeginPlannerTx` and `plannerStatements` live in `syncstore/store_planner_statements.go`, and `perfTx.QueryContext` routes the cached queries through `Tx.StmtContext`.
- `52af46ec` Run the two costliest exhaustive tests whole only in the full tier. `syncfixture.FullTier()` is `testing.CoverMode() != ""`. Only `verify default` and `verify public` build with coverage (`runUnitTests`: `-coverpkg -coverprofile`), and the nightly runs `verify public`. In the full tier both tests are unchanged. Elsewhere reverse dispatch runs every 8th scenario and the exhaustive check every 400th pair. That covers the race profile on push to main, stress, the ship-time race run and a plain `go test`.

## Verified
- `go build ./...` and `go vet ./internal/sync/...` are clean.
- `go test -count=1 ./internal/sync/syncstore/` passed (64 s).
- The new and changed engine tests passed: TestHooks_*, TestEngineHooks_*, TestTestEngines_*, TestCrashRecovery_DeleteBoundaries*, TestBeginPlannerTx* (17 s).

## Not done
- Item 2's agreement and golden tests over the whole tree (planned: the after-(b) race run, plus `go test -cover ./internal/sync/` for the full reverse dispatch).
- Every before/after measurement. None are valid. The host was never quiet: CPU 95-100% busy, load 18-40, free RAM about 120-160 MB with about 6-7 GB in swap, from the nightly backup's checksumtool, immich/postgres, other sessions and an immich-mcp prettier run. The orchestrator's fallback before-(b) is R7's `internal/sync` race -p 1 = 6516 s.
- The pre-pr gate was started and killed in lint after about 40 minutes (lint had not finished at that load). No findings were seen.
- Item 4, push, install, the agent-repo CHANGELOG and CLAUDE.md, and the PARKED lines (none recorded).

## Left for the resume (delete when T1 closes)
- Detached base worktree `~/.cache/onedrive-go-tmp/t1/base` (main f2392533). Remove it with `git -C ~/src/onedrive-go worktree remove`.
- Scratchpad scripts `T1-measure.sh` (a = `verify pr`, b = `go test -race -p 1 ./internal/sync/... ./internal/devtool/... ./testutil/...`, both with `GOFLAGS=-count=1`) and `T1-series.sh` (waits for CPU busy under 40% over 20 s, rechecked inside the heavy lock, then runs before-a, after-a, before-b and after-b). Gate on CPU busy, not load average: the load here is mostly I/O wait on the dev host's storage.
