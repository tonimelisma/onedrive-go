# W50 definition of ready and done

Worktree `~/src/onedrive-go-wt/crash-conflict-copy-leftover`, branch
`fix/crash-conflict-copy-leftover`, base `12a48bb2`.

A first writer was stopped mid-task with uncommitted work. The second writer saved that work
to `scratchpad/W50-wip/`, put the tree back to `12a48bb2` (`git status`: only `.testdata`),
and reran every step below on it. The tables are the second writer's own runs.

## Step 1: three replays of the unchanged base

Command as the brief gives it (`runq.py ... ONEDRIVE_GO_MODEL_SEED=318012 go test -count=1
-timeout 30m -run "^TestModel_CrashAtSeededPointsRecovers$" ./internal/sync`).

| Run | Result | Where | Path named |
|-----|--------|-------|------------|
| 1 (`W50-base-replay1.log`) | FAIL, 7.8 s | batch 2, `sync_invariants_test.go:181` via `engine_model_based_test.go:338` | `docs/c.conflict-20261115-210323.md` |
| 2 (`W50-base-replay2.log`) | FAIL, 4.1 s | same | `docs/c.conflict-20261115-210348.md` |
| 3 (`W50-base-replay3.log`) | FAIL, 3.7 s | same | `docs/c.conflict-20261115-210411.md` |

All three agree: batch 1 "crash at store.commit_outcome", batch 2 "power loss at
upload.created_unrecorded", the same invariant, the same path apart from the wall-clock stamp.
Deterministic as far as these three runs show. (The first writer's three runs, at 14:43-14:46,
agree with these.)

## Step 2: mechanism (replay 1's log and state dump)

1. Batch 2 edits `docs/c.md` on both sides. `renameToConflictCopy` renames the local file to
   `docs/c.conflict-<stamp>.md` (`executor_conflict.go:186`); nothing is created (log line 441).
2. The next pass uploads the copy as a new file: `uploadNewFile` commits the create intent
   (`executor_transfer.go:536`, store `journal_mode(WAL)` + `synchronous(FULL)`, `store.go:149-150`),
   the create makes the empty item `created-9`, and its hook reaches `upload.created_unrecorded`
   (`executor_transfer.go:631`) before the item is recorded.
3. The model drew power loss: it checkpointed at the batch's start (`engine_model_based_test.go:375`)
   and the power-loss snapshot keeps the main database file alone (`crash_points_test.go:87-88`),
   so the committed intent, in the WAL, is gone.
4. The restart finds no pending-create row at the path, so observation does not claim the item
   (`store_pending_creates.go:973-976`). Local file with content + OneDrive file without, neither in
   the baseline: create/create (`planner_actions.go:476-483`, R-2.3.5). The copy is renamed to
   `...conflict-<stamp>.conflict-<stamp2>.md` and `created-9` is downloaded, 0 bytes, to the first
   copy's name (log lines 567-571). That download is the empty file.
5. Nothing later fills or removes it: both sides agree it is an empty synced file (baseline row
   `created-9`, zero hash). Focused test: `TestCrashRecovery_ConflictCopyWhoseUploadWasCutAfterTheCreate`;
   on the unchanged tree its power-loss subtest fails at `sync_invariants_test.go:181`, its crash
   subtest passes (`W50-base-red.log`).

## Step 3: where the defect is

The invariant, not the product and not the restart. Evidence:
- R-5.6.15 asks for the intent to be recorded durably before the create; it is, on a WAL store with
  `synchronous(FULL)`. Only the model's power-loss variant ("a disk that never honored the fsyncs")
  discards a committed transaction. With no record, the specified behavior is that the empty item
  reads as an OneDrive file and is downloaded, "the lesser harm" (`spec/design/sync-store.md` 600-602).
  Nothing is lost: the content is in the second copy.
- The invariant flags that outcome only at a conflict-copy name. The same power loss at an ordinary
  name (`c.txt` in `TestCrashRecovery_EveryCrashPointConverges/upload.created_unrecorded/power-loss`)
  ends with `c.txt` empty and `c.conflict-*.txt` holding `c local`, and passes every invariant
  (temporary probe test on the unchanged tree, `W50-probe.log`, deleted after).
- It still names a "reservation" no code makes: `renameToConflictCopy` creates nothing
  (`executor_conflict.go:174`).
- A product fix would have to tell this client's empty item from another device's empty file with no
  record: a planner change to create/create with an empty OneDrive side, which is a new planner row
  (decision b) and changes the ordinary no-crash conflict (stop condition). So none is made.

## Done when

- The focused test fails on the unchanged tree (done above) and passes after, 20/20 plain and 20/20
  under `-race`.
- The invariant says what it checks (no empty file at a conflict-copy name) and allows exactly the
  case the specification allows: an empty OneDrive item that a power-loss snapshot holds with no
  pending-create row and no baseline row, by item ID, so a later move of it is still known. The
  allowance is computed for power loss only (after a plain crash the intent always survives, so an
  empty item there is still a failure) and carried to the next engine a later crash opens.
- The seed passes three times plain and once under `-race`.
- Docs: `spec/design/sync-store.md` (pending creates) states what a restart does when a power loss
  took the intent, citing the new test. `spec/design/sync-execution.md` is not edited: the restart's
  behavior does not change. No file of another branch in flight is edited (`crash_points.go`,
  `executor*.go` untouched).
- Checks of the brief pass (build, vet x2, gofumpt, whole `./internal/sync/...`, focused race run,
  RealRepoIsClean, lint 0 issues).

## Definition of Ready (repository format)

1. PASS **Task intent**: a crash-recovery run whose upload of a conflict copy loses its create
   intent to a power loss converges with every invariant holding; the invariant checks an empty
   file at a conflict-copy name instead of an obsolete reservation, and allows only the item the
   lost intent left.
2. PASS **Non-goals**: no product change to conflict planning or uploads; no store generation,
   planner row, outcome kind or issue type; not `internal/cli`, `internal/multisync`,
   `internal/graph`, `internal/devtool`, e2e, workflows; not the other crash seed; not the number of seeds.
3. PASS **Governing docs**: `spec/design/sync-execution.md` (conflict copies, crash rows 43-52),
   `spec/design/sync-store.md` (pending creates, lines 580-640), `spec/requirements/transfers.md`
   R-5.6.15, `spec/requirements/sync.md` R-2.3.5, `spec/requirements/non-functional.md` R-6.5.1, R-6.5.2.
4. PASS **Authority owner**: the sync store owns pending-create records and their claim
   (`store_pending_creates.go`); the executor owns conflict copies and uploads; the test harness
   (`crash_points_test.go`, `sync_invariants_test.go`, the model) owns the crash snapshot and the
   invariants. Only the harness changes.
5. PASS **Code scope**: `internal/sync/sync_invariants_test.go`, `internal/sync/crash_points_test.go`,
   `internal/sync/engine_model_based_test.go` (carry the allowance across recoveries),
   `internal/sync/scenario_remote_test.go` (one read of the fake's empty files), one new test file,
   `spec/design/sync-store.md`. Not touched: `crash_points.go`, `executor*.go`,
   `spec/design/sync-execution.md` (other branches).
6. PASS **Regression plan**: the focused test fails on the unchanged tree (power-loss case) and passes after.
7. PASS **Verification plan**: focused test 20x plain and 20x `-race`; seed 3x plain, 1x `-race`;
   whole `./internal/sync/...` behind the lock; build, vet, vet with tags, gofumpt, RealRepoIsClean, lint.
8. PASS **Data safety**: test-only change; no product path changes.
9. PASS **Secret and credential safety**: no tokens, `.env` or `.testdata` read; no account names.
10. PASS **Live provider and destructive operations**: none; no live run, no push.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns**: Question: can another seed reach an empty conflict-copy file the
    allowance does not cover? Current assumption: only the lost-intent item is allowed, and it keeps
    its ID through moves. Validation step: the default model and crash seeds in the whole-package
    run. Owner document/code path affected if wrong: `sync_invariants_test.go`.
