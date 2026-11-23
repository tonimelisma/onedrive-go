# A21 Definition of Ready: the stress lane's fifty-times steps get declared members and a budget

Worktree `~/src/onedrive-go-wt/stress-x50-members`, branch `ci/stress-x50-members`, HEAD `c0792e98`. Measurements on this 2-core host, 2026-11-15, logs under the scratch directory `a21/`.

## Measurements (brief item 1)

**(a) What `stressSyncConcurrencyPattern` selects** (`go test -list PATTERN ./internal/sync/...`, `a21/list-pattern.log`): 245 top-level tests, all in the one package of the tree, `internal/sync` (the tree has no other package). By alternative, matches and the tests it alone brings in (no other alternative matches them):

| Alternative | matches | alone |
|---|---|---|
| `TestRunWatch` | 24 | 17 |
| `TestWatch` | 121 | 106 |
| `TestOneShot` | 9 | 5 |
| `TestWorkerPool` | 16 | 12 |
| `Shutdown` | 24 | 11 |
| `Drain` | 50 | 27 |
| `Concurren` | 8 | 8 |
| `Cancel` | 40 | 19 |

170 are in a prefix family; 75 come in by the loose substrings only (10 of those match two loose substrings).

**(b) Which exercise goroutines, timers, cancellation or shutdown** (read per family; a marker scan of every body and its test helpers, `a21/classified.tsv`, then the doubtful families read by hand):

| Family | selected | member | non-member | why |
|---|---|---|---|---|
| `TestRunWatch*` (watch entry, loop, until-quiescent) | 24 | 24 | 0 | runs the watch |
| `TestWatch*` runtime, loop, observers, watch scenarios | 114 | 114 | 0 | watch runtime and observers |
| `TestWatchCondition*` (summary counts, fingerprint, warn-once) | 5 | 0 | 5 | deterministic log summary |
| `TestWatchRechecks_`, `TestWatchRegistry_` | 2 | 0 | 2 | a path recheck and a map structure |
| `TestOneShot*` | 9 | 9 | 0 | one-shot result loop |
| `TestWorkerPool*` | 16 | 16 | 0 | worker pool |
| `*Concurren*` (baseline, dep graph, cache, root bind, protected roots) | 8 | 8 | 0 | goroutines |
| cancellation and shutdown paths (canceled contexts, timers, drains of running work, shutdown completions, observer exit) | 46 | 46 | 0 | cancellation, timers, shutdown |
| shortcut-root `FinalDrain` state (store and engine transitions) | 8 | 0 | 8 | "Drain" names a root state |
| `TestRunOnce_FinalDrain*` and the next-drain release (removed shared folder) | 10 | 0 | 10 | "Drain" names a removed folder's last pass |
| `TestReleaseLocalTree_*` | 3 | 0 | 3 | release rules, no goroutines |
| total | 245 | 217 | 28 | |

**(c) One race pass of the selection, timed per test** (`go test -json -race -count=1 -shuffle=on -timeout=30m -run PATTERN ./internal/sync/...` behind the heavy lock; read by `a21/timing.py`). Two passes, for the variance stop condition:

| Pass | tests | sum | package wall | members' sum | slowest member | slowest non-member |
|---|---|---|---|---|---|---|
| 1 (`sync-race-pass1.log`) | 245 | 217.3 s | 182.4 s | 154.8 s | 20.96 s | 25.26 s |
| 2 (`sync-race-pass2.log`) | 245 | 156.5 s | 139.7 s | 117.7 s | 8.42 s | 10.59 s |

Ten slowest of pass 1: `TestRunOnce_AReleaseRefusedForAnUnseenChangeUploadsItOnTheNextDrain` 25.26 s, `TestWatchRuntime_ReplanKeepsAFreshCreateIntentForTheBatchThatShowsItsFile` 20.96, `TestRunOnce_FinalDrainMovesASyncedFileIntoANewFolderAndReleasesBoth` 10.35, `TestRunWatch_FinishesWorkTheBootstrapMovesLeaveForTheNextPass` 10.10, `TestWatchRuntime_RunNonDrainingWatchStepPrioritizesReadyReplanOverDispatch` 8.28, `TestRunOnce_FinalDrainReleasesASyncedTreeToTheTrash` 6.99, `TestRunOnce_FinalDrainRemovesBundledJunkUnderTheConfiguredRule` 6.54, `TestRunWatch_FinishesTheRemoteEditBehindARekey` 6.44, `TestReleaseLocalTree_ARowWithoutAModificationTimeWaitsForTheNextDrain` 6.20, `TestWatchObserver_SeesBindingMadeAfterItWasBuilt` 5.75. The non-members are 29% (pass 1) and 25% (pass 2) of the summed time. The slowest member varied 2.5 times between the passes; a 90-second per-member budget on 2 cores holds both and is under ten times the slowest member of either, so the variance stop condition does not apply.

**(d) multisync**: one race pass (`multisync-race-pass1.log`): 177 tests, 25.2 s summed, package wall 22.4 s, slowest `TestRunOnce_APanicAroundADrivesShortcutsIsThatDrivesReport` 1.20 s. Top-level tests: 160 at `27e76cc3`, 175 at `2b1c5afa` (177 at `c0792e98`), counted from `git show` copies of each test file. Commits in between touching the package: #896, #897, #898, #900, #901, #903, #905. The 16 tests added (one removed) take 9.3 s of the 25.2 s (37%); eight of them, the panic-boundary tests of #897 (`b8991f4b`, `panic_boundary_test.go`), each rerun the test binary in a child process (`synctest.RunInChild`) and are not parallel: 1.03 to 1.20 s each, 8.6 s a pass, run one after another. The step grew from 4m43 to 11m33 on CI, 8.2 s a pass over fifty passes. **Explained**: the eight serial child-process tests account for the growth.

**(e) Declared membership the tree already uses**: the e2e-full buckets, `fullE2EParallelMiscTestNames` and its two siblings (`internal/devtool/verify_e2e.go:61`, `:123`, `:161`), exact names returned by a function, a run pattern `^(a|b|...)$` built from them (`fullE2EBucketRunPattern`, `verify_e2e.go:230`), held to the tagged sources by `TestFullE2EExecutionManifestCoversTaggedTestsExactlyOnce` (`verify_runner_test.go:645`); and the seeded families, `seededFamilies` (`verify_seeds.go:77`), its pattern `seedsRunPattern` (`:98`), held by `TestSeedsRunPatternNamesExistingTests` (`verify_seeds_test.go:123`). The new declaration takes that form.

## Checklist

1. PASS **Task intent**: repository maintenance. The stress lane's two fifty-times steps repeat declared members (by exact name, per package) instead of a name pattern, each step's go test deadline is its stated pass budget times the repetition count times a stated margin, and a member whose slowest run is over a per-member budget fails its step.
2. PASS **Non-goals**: the whole-tree race step and its deadline; the seeds, race and e2e profiles; any test of `internal/sync` or `internal/multisync` (slow members are reported, not sped up); the nightly reader; any workflow change beyond the `stress` job's timeout and its comment.
3. PASS **Governing docs**: `spec/design/system.md` (governs `internal/devtool/*.go`; the stress profile paragraph), `spec/requirements/non-functional.md` R-6.10.8 and R-6.10.26, `spec/reference/live-incidents.md` LI-20261115-02 (names the 80-minute literal as not derived), the repository's `CLAUDE.md` verification-tier paragraph, `.github/workflows/ci.yml` `stress` job.
4. PASS **Authority owner**: `internal/devtool/verify_stress.go` owns the stress steps, their deadlines and the per-member check; a new `verify_stress_members.go` owns the declared members and non-members; the membership check is a devtool test that reads the test sources (as the e2e and seeds declarations are held). User-facing output: the steps' status lines and the budget failure line.
5. PASS **Code scope**: `internal/devtool/verify_stress.go`, `verify.go` (two deadline constants and their comment only), new `verify_stress_members.go` and `verify_stress_members_test.go`, stress tests in `verify_runner_test.go` and `verify_every_step_test.go`, `.github/workflows/ci.yml` (stress job timeout comment), docs. Not touched: product code, `internal/sync` and `internal/multisync` tests, the quick tier's package list, the nightly reader.
6. PASS **Regression plan**: new tests, each seen failing first: an unplaced phrase-named test fails the membership check naming it and both lists; a declared member the tree lacks fails it; the step's `-run` is exactly the anchored members, and a run of no member fails the step; a member over the per-member budget fails the step with one line naming test, time and budget, and a test failure stays reported beside it; both deadlines equal pass budget x runs x margin, and `TestCIJobTimeoutsCoverEveryGoTestDeadline` still holds.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...` and with the tags, `gofumpt -l`, `go test -count=1 ./internal/devtool/ ./cmd/devtool/` and with `-race` behind the heavy lock, `RealRepoIsClean` (4 PASS), one by-hand `-count=1` run of the exact sync step command behind the lock to show the selection equals the declared count, lint last behind the lock.
8. PASS **Data safety**: developer tool only; no sync, store or filesystem code changes.
9. PASS **Secret and credential safety**: no tokens, `.env`, accounts or live runs; every planted test name is invented.
10. PASS **Live provider and destructive operations**: none; nothing runs against OneDrive; no history rewrite, no dependency change.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does the "about 300 names" stop count both steps' lists together? Current assumption: it is per declared list; the sync list holds 245 names (217 members, 28 non-members) and the multisync list 177 (every test of the package, as the step repeats today). Validation step: named in the report for the caller. Owner affected if wrong: `verify_stress_members.go`.
    - Question: per-test times of the fifty-times steps on a CI runner. Current assumption: a runner pass costs the step time over fifty (sync 47.3 s, multisync 14.3 s on 2026-11-15); per-member times on a runner are not recorded anywhere, so the per-member budget on a runner is set above the slowest member measured on this slower host. Validation step: the steps now record their slowest tests in the summary. Owner: `verify_stress.go` budget constants.
    - Question: why the same 244 tests took 29.5 and 39.4 minutes on the two runs of 2026-11-15. Current assumption: unknown; not measured here. Validation step: none in scope. Owner: none.
