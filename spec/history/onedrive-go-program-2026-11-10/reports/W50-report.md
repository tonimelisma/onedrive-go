# W50 report

Worktree `~/src/onedrive-go-wt/crash-conflict-copy-leftover`, branch
`fix/crash-conflict-copy-leftover`, one commit `a688a430` on `12a48bb2`, not pushed.
Written by a first writer who was stopped before committing, then checked, changed and
finished by a second writer (section "Interrupted work").

**Replays of the unchanged base** (second writer, tree put back to `12a48bb2`): 3 of 3 FAIL,
identical (batch 2, power loss at `upload.created_unrecorded`, `sync_invariants_test.go:181`,
path `docs/c.conflict-<stamp>.md`). Details and the mechanism are in `W50-dor.md`.

**Mechanism**: the conflict copy (a rename, `executor_conflict.go:186`) is uploaded as a new file;
the create's empty item exists on OneDrive when the crash point is reached
(`executor_transfer.go:631`); the power-loss copy keeps only the main database file
(`crash_points_test.go:87-88`), losing the intent committed at `executor_transfer.go:536`; with no
pending-create row (`store_pending_creates.go:973-976`) the restart sees create/create
(`planner_actions.go:476-483`), renames the copy again and downloads the empty item at its name.

**Where the defect is**: the invariant, not the product or the restart. The product commits the
intent durably (WAL, `synchronous(FULL)`) before the request (R-5.6.15); without a record the
specified behavior is to download the empty item, "the lesser harm" (`sync-store.md`). The same
outcome at an ordinary name passes every invariant (probe on the unchanged tree: `c.txt` empty,
`c.conflict-*.txt` holding `c local`; probe deleted). A product fix needs a planner change to
create/create with an empty OneDrive side: a new planner row that changes the no-crash conflict
(decision b, stop condition), so none was made.

## What changed
- `internal/sync/sync_invariants_test.go`: `syncHarness.placeholdersWithoutIntent` (item IDs);
  the check is "no empty file at a conflict-copy name" and allows exactly those items; header and
  message no longer name a reservation.
- `internal/sync/crash_points_test.go`: `reopen` names, on a power loss only, every empty OneDrive
  file the snapshot's database has no pending-create row and no baseline row for
  (`placeholdersWithoutIntentIn`); header comment; `TestCrashRecovery_EveryCrashPointConverges`
  cites R-6.5.1 instead of R-6.8.9.
- `internal/sync/engine_model_based_test.go`: `recoverFrom` carries the names to the next engine;
  `TestModel_CrashAtSeededPointsRecovers` cites R-6.5.1, R-6.5.2 instead of R-6.8.9.
- `internal/sync/scenario_remote_test.go`: `emptyFiles()`, a locked read of the fake's empty files.
- `internal/sync/crash_conflict_copy_test.go` (new): the focused test and its helpers.
- `spec/design/sync-store.md`: what a restart does when a power loss took the intent, citing the
  new test. `sync-execution.md` not edited: no restart behavior changed.

## Tests added
- `TestCrashRecovery_ConflictCopyWhoseUploadWasCutAfterTheCreate` (R-5.6.15, R-2.3.5, R-6.5.2), two
  subtests: `upload.created_unrecorded` (the intent claims the empty item, the copy finishes into it,
  no empty file, nothing allowed) and `.../power-loss` (exactly the cut create's item ID is allowed,
  the empty item sits at the copy's name, the edit is in a second copy). The power-loss subtest
  failed on the unchanged tree at `sync_invariants_test.go:181` (`W50-base-red.log`); the crash
  subtest passed there.

## Checks (second writer)
- Focused test 20/20 plain PASS (`W50-focused20b.log`); 20/20 `-race` PASS (691 s, `W50-focused20-race2.log`).
- Seed after the change: 3/3 plain PASS (`W50-fix-replay1..3.log`); 1x `-race` PASS together with
  `TestCrashRecovery_EveryCrashPointConverges` (`W50-race-changed.log`).
- Mutation: without the carry in `recoverFrom` the seed fails in batch 3 (`W50-mut-carry.log`): the
  model swapped the empty file to another conflict-copy name, so a path-keyed allowance would miss it.
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: PASS.
- `gofumpt -l`, `goimports -l` on the five Go files: no output.
- `go test -count=1 ./internal/sync/...` behind the lock: PASS (462 s, `W50-pkg-sync2.log`).
- `go test -count=1 -v -run RealRepoIsClean ./internal/devtool/`: 4 PASS.
- Lint `./internal/sync/...` into `W50-lint.log`, behind the lock: `0 issues.` (223 s).
- Spec citation by hand: the cited test exists; R-2.3.5, R-5.6.15, R-6.5.1, R-6.5.2 exist.

## Interrupted work: kept and changed
- Kept: the diagnosis, the focused test's steps and assertions, the `sync-store.md` paragraph
  (reworded), the crash-harness header comment.
- Changed: the allowance was computed after every crash, by path. After a plain crash the intent
  survives, so an empty unrecorded item there is a lost claim, which the allowance would have
  hidden at exactly the names the check looks at; it is now computed on a power loss only. It was
  keyed by path, and the model moves conflict copies; it is now keyed by item ID and carried
  across recoveries (the mutation above). Items the snapshot's baseline holds are excluded too.

## Deviations
- The fix is in the test harness and the invariant (step 3), not the product: the invariant
  allows the items a power-loss snapshot holds as empty files with no pending-create row and no
  baseline row; everywhere else it is the same check, worded for what it is now.

## Also fixed
- The invariant header and message no longer name a "reservation".
- `crash_points_test.go` and `engine_model_based_test.go`: the crash tests cited R-6.8.9 ("the
  executor shall not contain a retry loop"); they cite R-6.5.1 (durable writes survive a kill).

## Seen and not fixed
- `internal/sync/sync_invariants_test.go:195`: the empty-file check sees conflict-copy names only,
  so a create's empty item downloaded at an ordinary name (a lost claim) passes every invariant.
  Fix: check every empty file of a converged tree against `placeholdersWithoutIntent`, after
  finding which scenario tests sync zero-byte files on purpose and giving them their own allowance.
- R-6.8.9 is cited by about a dozen other tests in `internal/sync` (e.g. `dispatch_order_test.go:351`,
  `engine_config_test.go:316`, `chaos_remote_test.go:787`) that do not test the executor's retry
  loop. Fix: review each citation and point it at the requirement it proves.

## Unfinished
- None.

## Other crash seed
- Nothing learned about the other seed; it was not run.

## Packages the reviewer should test
- None beyond `internal/sync` (test files and one spec file only).

## Review fixes

Commit `b5fc58de` (amended from `b541fb13`; still one commit on `a0dbe10b`). Files: the five
test files below; `scenario_remote_test.go` and `sync-store.md` unchanged from `b541fb13`.

- Should-fix 1, `crash_conflict_copy_test.go:48-52`: the plain-crash subtest reads the recovered
  fake at the copy's path (`recovered.remote.getItemByPath(placeholder)`, `require.NoError`) and
  asserts the item there is the cut create's (`placeholderID`). The assertion that was true by
  construction (`placeholdersWithoutIntent` empty) is gone; `:48` now says only what it checks
  (the local edit stays at the copy's name). Seen failing once with `placeholderID+"-wrong"`:
  expected `created-1-wrong`, actual `created-1` at `:52` (`w50fix/red-wrong-id.log`); restored.
- Should-fix 2: `sync_invariants_test.go:36-38` (the intent "the crash harness's lost-WAL variant
  then took with the WAL"), `crash_conflict_copy_test.go:56` ("the lost WAL took the intent"),
  the test comment `crash_conflict_copy_test.go:25-27`, and the commit message ("when a lost WAL
  took the intent").
- Should-fix 3, commit message first paragraph: "That is what R-2.3.5 and the lesser-harm rule for
  a dropped intent give, and the pending-create design now states it." Widest line 71; trailer
  `Co-Authored-By: Claude <noreply@anthropic.com>`.
- Note 6, `engine_model_based_test.go:395-402`: `recoverFrom` carries an allowed ID only while
  `recovered.remote.emptyFiles()` still holds it. Mutation case kept: with the carry disabled
  seed 318012 fails in batch 3 at `sync_invariants_test.go:196` (`w50fix/mut-carry.log`); with
  the narrowed carry it passes (`w50fix/seed-replay.log`).
- Note 5, rename to lost WAL: field `crashCapture.lostWAL` (`crash_points_test.go:53`), the
  parameters and loop variables in `crash_points_test.go`, `crash_conflict_copy_test.go`,
  `engine_model_based_test.go` and `engine_shortcut_release_test.go:894-928` (a reader of the
  field this commit had not touched); subtest suffix `/power-loss` is now `/lost-wal`
  (`crash_points_test.go:420`); comments at `crash_points_test.go:30-40, 90, 250, 286, 326`,
  `engine_model_based_test.go:363-364, 395`, `sync_invariants_test.go:52`; the model's log label
  is "crash with the WAL lost" (`engine_model_based_test.go:389`, read only by the failure
  context). The harness header now says a power loss on a disk that honors the fsyncs leaves no
  such state (`synchronous(FULL)`, `store.go:150`). Readers: the grep the brief names, then the
  whole tree outside `.git` and `.testdata`: hits only in those five test files and
  `sync-store.md:613`. No seed, replay command, nightly key or CI job reads the old name.
  `sync-store.md:613` ("The intent survives a power loss") is about a real power loss and true:
  unchanged.
- Note 9 interim, `crash_points_test.go:437-443`: `TestCrashRecovery_EveryCrashPointConverges`
  asserts after a plain crash at `upload.created_unrecorded` that `c.txt` holds its local
  content; `c.txt` and its content are constants now (`:346-347`), and R-5.6.15 joins the test's
  Validates line (`:426`). Seen failing when flipped onto the lost-WAL case: expected
  `"c local\n"`, actual `""` (`w50fix/mut-ctxt.log`); restored.

Checks (logs in `S/w50fix/`): `go vet ./internal/sync/` PASS; `gofumpt -l` clean;
`TestCrashRecovery_ConflictCopyWhoseUploadWasCutAfterTheCreate`,
`TestCrashRecovery_DeleteBoundariesDeleteOnceAndKeepTheTrashEntry`,
`TestRunOnce_ACrashDuringTheReleaseResumesWithNoRemoteDelete` PASS; behind the lock
`TestCrashRecovery_EveryCrashPointConverges` PASS (17.6 s) and `TestModel_CrashAtSeededPointsRecovers`,
`TestModel_SameSeedReplaysIdentically`, `TestRunOnce_RandomizedBidirectionalEditsConverge` PASS
(91 s); seed 318012 PASS; `ListsEvery|Table|Governs|Exhaustive|Registered` PASS;
`RealRepoIsClean` (devtool) PASS. No lint run, as instructed.

Not done: note 11 (moving the spec paragraph) and note 4 (product change) were not in the list;
the full note 9 fix (every empty file against an explicit allowance) stays a separate row.


## Merged as #928

- Merge commit `f82a5f84` on main, 2026-11-15 20:51 PST. Pre-pr gate and PR CI green; pre-merge gate passed with no review threads; post-merge stage passed. Installed 21:21 PST.
- The nightly claim on crash model seed 318012 was held by this branch; the scheduled seeds run of 02:17 PST draws new seeds, so the proof that the seed passes is the test run in this PR.
