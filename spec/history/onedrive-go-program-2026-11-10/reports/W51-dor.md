# W51 definition of ready and done

Worktree `~/src/onedrive-go-wt/crash-recovery-converges`, branch
`fix/crash-recovery-converges`, base `f82a5f84` (`git status`: only `.testdata`).

## Step 1: three replays of the unchanged base

Command as the brief gives it (`runq.py ... ONEDRIVE_GO_MODEL_SEED=318011 go test -count=1
-timeout 30m -run "^TestModel_CrashAtSeededPointsRecovers$" ./internal/sync`).

| Run | Result | Where | Final baseline row at `q%20r` |
|-----|--------|-------|-------------------------------|
| 1 (`W51-replay1.log`) | FAIL, test 5.8 s | `engine_model_based_test.go:519` via `:332`, "batch 4 after the crash: still changing after 8 passes" | `remote-file-14 folder` with file hashes; remote_state and local_state: `file` |
| 2 (`W51-replay2.log`) | FAIL, package 11.7 s | same | same |
| 3 (`W51-replay3.log`) | FAIL, package 13.8 s | same | same |

All three agree on the operations (batch 4: "crash with the WAL lost at delete.local_trashed"),
the message and the final state. Deterministic as far as these three runs show. (Replay 1's log
is kept in the scratch directory; logs 2 and 3 and the trace log were deleted for the /tmp
quota after the findings were written here.)

## Step 2: what each pass planned (batch 4 after the crash)

Source: a test-only trace (removed; tree back at base) that after every pass printed the
report counts, the state dump, and the plan the store then yields
(`loadCurrentInputsTx` + `buildCurrentActionPlanFromInputs`, the pattern of
`TestPlanning_UsesBaselineFromTheComparisonSnapshot`), with each action's view types; plus the
engine's own plan-summary and debug lines (`W51-trace.log`). A model pass is one `RunOnce`.

State the fresh engine opens: the lost WAL leaves the main database file of the checkpoint the
model took at the batch's start, so the baseline holds `q%20r` as folder `remote-folder-8` and
`q%20r/photos` as folder `created-folder-11`. Before the crash the first engine had deleted the
local folders and downloaded OneDrive's replacement: local `q%20r` is a file whose content equals
OneDrive's file `remote-file-14`.

| Pass | Engine passes inside | Planned (kind, path) |
|------|----------------------|----------------------|
| 1 | 2 (follow-up for a rename) | inner 1: 11 actions = conflict_copy `Renamed-photos/photos` (folder moved aside), download `Renamed-photos/photos` (`remote-file-15`), 4 baseline_update (one is `q%20r`, view B folder/`remote-folder-8`, R file/`remote-file-14`, L file; the other three paths are not printed by the engine), 5 cleanup (paths not printed). inner 2: folder_create `Renamed-photos/photos.conflict-<stamp>` on OneDrive, 1 baseline_update (path not printed). After the pass the baseline row is `q%20r remote-file-14 folder` with the file's hashes. |
| 2 | 1 | baseline_update `q%20r` (B folder/`remote-file-14`, R file, L file) |
| 3 | 1 | baseline_update `q%20r`, same view |
| 4 | 1 | baseline_update `q%20r`, same view |
| 5 | 1 | baseline_update `q%20r`, same view |
| 6 | 1 | baseline_update `q%20r`, same view |
| 7 | 1 | baseline_update `q%20r`, same view |
| 8 | 1 | baseline_update `q%20r`, same view; the plan after it is the same again |

Passes 2 to 8: delta enumeration events=0, local scan events=0, and the state dump is identical
after each.

**The cycle.** Every pass from 2 on repeats pass 2: one `baseline_update` at `q%20r`. The
comparison sees a baseline folder where both sides hold the same file (`equal_again`,
`sqlite_compare.go:545`, `baseline_update` at `:670`), which is right. The publication
(`publicationMutationFromAction`, `store_write_baseline.go:391-393`) takes the observed remote's
item ID and hashes but puts the baseline's item type back whenever the observed type is
`ItemTypeFile` (the zero value) and the baseline's is not, so the row stays a folder and the next
pass plans the same update.

## Step 3: which answer

(i), a product defect: nothing changes between passes 2 and 8 (no observation events, identical
state), and each pass plans and "succeeds" at the same publication, which writes the same
folder-typed row. The model is right to fail it (its quiescence check). The same state arises
without a crash: a synced folder that both sides replace with the same file gives the same view,
so `sync` would report one baseline update on every run, forever.

Not (c): the cycle is the type of `q%20r`, not the conflict copy at `Renamed-photos/photos` (that
converges in pass 1) and not a move. No store generation, planner row, outcome kind or issue type
changes (b).

## Definition of ready

1. PASS **Task intent**: a folder replaced by a file, whose baseline still records the folder while both sides hold the same file (after a crash that lost the WAL, or after both sides replaced it), converges in one pass: the baseline update records the file.
2. PASS **Non-goals**: the model's pass bound (8 stays; the case needs 2 passes); `actionItemType` and its other users (`planner_dependencies.go`, `executor_delete.go`, owned by branches in flight); the planner; `internal/cli`, `multisync`, `graph`, `devtool`, e2e, workflows; any other seed.
3. PASS **Governing docs**: `spec/design/sync-store.md` (GOVERNS `store_write_baseline.go`; publication-only actions, line 391-395; the epoch precedent, line 258-265), `spec/design/sync-engine.md` (one-shot passes, `oneShotMaxPasses`, line 365-392; no model bound stated there), `spec/requirements/sync.md` R-2.17.4 (a type change never stalls), `spec/requirements/non-functional.md` R-6.5.2.
4. PASS **Authority owner**: the store's publication boundary owns what a `baseline_update` writes (`publicationMutationFromAction`); the planner owns deciding one; observed truth (remote_state, local_state) owns the item type.
5. PASS **Code scope**: `internal/sync/store_write_baseline.go` (remove the override; fill hashes and sizes only from a baseline of the same type, a smell found in the same function), one new test file, `dispatch_order_test.go` (register the new `TestRunOnce_`), `spec/design/sync-store.md`. None is in another in-flight branch's list.
6. PASS **Regression plan**: a `TestRunOnce_` scenario (both sides replace a synced folder with the same file: pass 1 publishes, pass 2 plans nothing) and a `TestCrashRecovery_` scenario (the drawn case: crash with the WAL lost after the replacing file's download, recovery quiet on its second pass), plus a unit test of the publication; all fail on the base for the named reason; the seed replays green three times plain and once under `-race`.
7. PASS **Verification plan**: build, vet (plain and tagged), gofumpt; `go test -count=1 ./internal/sync/...`; `-race` on the new tests, the publication tests in `executor_test.go` and `TestReverseDispatch_RunOnceScenariosPass/<new test>`; `-count=20` plain and race on the new tests; `RealRepoIsClean`; the package's `ListsEvery|Table|Governs|Exhaustive|Registered` tests; golangci-lint on `./internal/sync/...` last.
8. PASS **Data safety**: the change writes only the baseline's item type, from observed truth on both sides that are already equal; no transfer, delete or move is planned differently; the tree a user holds is unchanged in every case.
9. PASS **Secret and credential safety**: no tokens, `.env`, `.testdata` or account names are read or written; fake OneDrive only.
10. PASS **Live provider and destructive operations**: none; no live test, no reset, no dependency change, no history rewrite.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: why the override exists. Current assumption: it reads `ItemTypeFile`, the zero value, as "unknown"; it arrived in a refactor (`9fb2ab8a`, #516) with no test naming it. Validation step: the whole `internal/sync` suite, the publication unit tests and the model seeds pass without it. Owner if wrong: `store_write_baseline.go`, `spec/design/sync-store.md`.
    - Question: the three other baseline updates and five cleanups of pass 1. Current assumption: they do not matter to the cycle (pass 2 on plans only `q%20r`). Validation step: the seed replays green after the fix. Owner if wrong: this report.

## Definition of done (results)

- Commit 1de94ac7 on fix/crash-recovery-converges, one commit on f82a5f84, not pushed.
- Red on the base: the TestRunOnce_ scenario and the lost-WAL crash variant fail with the baseline row a folder (expected file) and a second pass planning one baseline update; the unit test fails on the type (folder replaced by a file) and on both hashes (file replaced by a folder). The plain-crash variant passes on the base too (no baseline row is left at the path), as its comment says.
- Seed 318011 after the fix: 3 of 3 plain pass (3.4 s, 3.2 s, 3.9 s), 1 of 1 under -race (78.9 s).
- New tests: 20 of 20 plain, 20 of 20 under -race (with every TestCommitMutation_, TestPublicationMutation_, TestUpsertBaselineRowTx_, TestSQLUpsertBaseline_ test); TestReverseDispatch_RunOnceScenariosPass/TestRunOnce_AFolderBothSidesReplacedWithTheSameFileConverges under -race: pass.
- go test -count=1 ./internal/sync/...: ok (473.5 s). go build ./..., go vet ./..., go vet with e2e,e2e_full,integration,stress: clean. gofumpt -l: nothing. RealRepoIsClean: four PASS. golangci-lint ./internal/sync/...: 0 issues (W51-lint.log).
- Pass bound unchanged (8): the case needs 2 passes; spec/design/sync-engine.md unchanged.
