# W101 report: a rebind keeps every disk that is not mounted

Branch `fix/rebind-keeps-unmounted-disks` (worktree `~/src/onedrive-go-wt/rebind-keeps-unmounted-disks`), on main `5122e6a4`. One commit, `c797a410`, pushed, no PR, nothing installed.

## Option chosen: (a)

`ClearSyncRootIdentity` now clears only the sync-folder fingerprint (and sets the rebind flag); it no longer runs `DELETE FROM mount_boundaries`. A vanished crossed mount stays "Disk not mounted" after a rebind until the disk is back or its folder is removed.

Why (a) keeps the rebind's purpose:
- The rebind exists to accept a sync folder that was moved, restored or re-pointed on purpose (root fingerprint). Recorded mount points are relative paths inside the folder and do not take part in the root check, so a rebind works without forgetting them.
- Forgetting them was added in #805's second review round only to give a dead disk a way out. That way out exists without the rebind: remove the empty folder. The scan then takes its content as deleted like any removed folder (the hold judges it), and `recordMountBoundariesTx` prunes the flag at the next snapshot, once the folder is neither in the baseline nor in local_state. A new test proves this.
- (b) would still forget the mount points and turn an unplugged disk into held deletes that one `--confirm-deletes`, perhaps meant for another delete, would run. (c) adds a prompt and still deletes once answered. (a) is the only option where a rebind never leads to those deletes.

## DoR findings

- Readers of `mount_boundaries`: `listMountBoundaries` (store_local_state.go), called by `engine_current_plan.go:388` (one-shot/bootstrap scan) and `engine_watch.go:384` (watch safety scans), which feed `SetMountBoundaryFlags` -> `flaggedMountIsGone` (scanner_unavailable.go) -> `vanishedMountSkippedItem` (scanner.go:628). Writers: `recordMountBoundariesTx` (insert crossed, prune paths in neither baseline nor local_state) and, before this change, `ClearSyncRootIdentity`. Schema in schema.go.
- History: the clear came in 332f2b56 (#805), commit "a crossed mount is gone only when back on its parent's device; rebind forgets mount points", as the dead-disk exit. Nothing else depended on it.
- A folder that really is a new disk: a nested mount that is mounted is on its own device and reads as mounted whatever the flags say; a flag only matters for a folder found on its parent's device. So the flags hurt a rebind only when a restore turned a crossed disk into a plain folder holding its content on the parent disk. With (a), that folder stays "Disk not mounted" (nothing deleted, nothing uploaded); the way out is to move the content aside, remove the folder and sync, which deletes and re-uploads. Safe, but clumsy: parked (one line).
- Watch and unmount: fsnotify v1.10.1 (`backend_inotify.go:446`) drops the watch on IN_UNMOUNT/IN_IGNORED and emits no event, so a watch plans no deletes from an unmount; the next safety scan marks the folder from the flags, which (a) now keeps after a rebind.

## What changed

- `internal/sync/store_observation_state.go`: `ClearSyncRootIdentity` keeps `mount_boundaries`; comment says why and how a dead disk is let go of.
- `internal/sync/scanner_unavailable.go`: the scan warning says the rebind keeps the folder unavailable and that removing the empty folder deletes its content on OneDrive.
- `internal/sync/issue_types.go`: `issueLocalMountMissing` comment.
- `internal/cli/status_condition_descriptors.go`: "Disk not mounted" action: rebind does not clear it; remove the empty folder for a disk gone for good (hold applies).
- `internal/cli/drive_rebind_root.go`: `--help` says the rebind keeps every recorded mount point.
- Docs: `docs/safety.md` (mount bullet and rebind bullet), `docs/troubleshooting.md` (Disk not mounted row and rebind paragraph), `spec/design/cli.md` rebind row, `spec/design/sync-observation.md` mount_boundaries bullet, `spec/requirements/non-functional.md` R-6.2.15.
- Tests (`internal/sync/engine_unavailable_not_absent_test.go`):
  - `TestRunOnce_ARebindAfterADiskIsGoneDeletesItsContentOnOneDrive` replaced by `TestRunOnce_ARebindKeepsEveryDiskThatIsNotMounted` (one disk with 3 files, one with 51, two disks): after the rebind the next sync deletes and holds nothing, the condition stays; after each disk is mounted again nothing is transferred, deleted or moved, the condition clears, files intact.
  - New `TestRunOnce_ADiskGoneForGoodIsForgottenOnceItsFolderIsRemoved`: removing the empty folder deletes its content, the flag is pruned on the next sync, and a folder made there again syncs as ordinary.
  - `TestClearSyncRootIdentity_ForgetsRecordedMountPoints` became `..._KeepsRecordedMountPoints`.
  - New helper `requireIssueOfType`, also used in `TestRunOnce_UnmountedNestedDiskWithFewFilesIsNotDeleted` in place of its hand loop.
  - `dispatch_order_test.go` list and `internal/cli/status_condition_descriptors_test.go` (action text and comment) updated.

## Checks

- Red on main code (new tests, product files unchanged): `TestRunOnce_ARebindKeepsEveryDiskThatIsNotMounted` FAIL in all three cases (3, 4 deletes run; 51 held) and on the warning text; `TestClearSyncRootIdentity_KeepsRecordedMountPoints` FAIL. `TestRunOnce_ADiskGoneForGoodIsForgottenOnceItsFolderIsRemoved` passed on main too (it pins the documented way out, which already worked).
- Green, focused (the tests above plus the vanished-mount, mount-missing, remount and edit-while-unmounted tests): ok, 10.2 s.
- `go build ./...`: ok. `go vet ./internal/sync/ ./internal/cli/`: ok. gofumpt: clean.
- `go test -count=1 ./internal/sync/ ./internal/cli/` (under the lock): ok, sync 726 s, cli 10 s.
- `go run ./cmd/devtool verify lint`: pass (11m44s) on the second attempt. The first attempt stalled under host load (6 CPU-minutes in 2 hours, past its 45-minute timeout, idle on a futex); I killed it and reran.
- No `-race` (the ship step runs it). No live e2e (not needed: store and scanner behaviour is covered by the engine tests).

## PARKED lines added

- W101-n1: a recorded mount point whose folder now holds the disk's content as a plain folder (whole-tree restore) stays "Disk not mounted"; the only exit deletes and re-uploads; a way to forget one recorded mount point is the owner's choice.

## Review fixes (first review, should-fix 1): commit `11f3b382` (squashed, replaces `c797a410`)

Finding: probe P3 rebinds onto a folder that lacks the empty mount-point folder. The next sync then deleted 3 files and the folder on OneDrive with nothing held. Decision: this is part of the fix, not just a text change.

- Product: the delete-safety hold has a third rule (`internal/sync/planner_delete_safety.go`). A delete toward OneDrive of a recorded mount point whose folder is not in the local snapshot, or of anything below it, is held whatever the count, with the new reason `mount_point_gone` (`domain_delete_safety.go`). The plan's other deletes are judged by the existing two rules. Such a mount point whose folder is still there plans no deletes when the disk is not mounted, and its deletes are ordinary when it is mounted and emptied. The engine passes `listMountBoundaries` into `deleteSafetyInput.MountBoundaries` (`engine_current_plan.go`). `heldDeleteClosure` now takes the per-action seeds (`heldDeleteSeeds`) instead of per-type reasons. No store generation change: `held_deletes.reason` is free text.
- Tests, failing before the change and passing after:
  - `TestDeleteSafetyHold_DeletesOfAMissingMountPointAreHeldWhateverTheirNumber` (planner; the mounted-and-emptied `usb` delete, a `nasty.txt` prefix lookalike and an unrelated delete still run; confirming releases the held ones).
  - `TestRunOnce_ARebindOntoAFolderWithoutAMountPointHoldsItsDeletes` (P3).
  - `TestRunOnce_ADiskGoneForGoodIsForgottenOnceItsFolderIsRemoved`, now held, then confirmed, then the flag is forgotten.
  - Shared helper `requireMissingMountPointDeletesHeldThenConfirmed`. Both RunOnce tests are added to the reverse-dispatch list.
  - Red run on the code before the change (with only the field and constant added so it compiles): all three FAIL (planner: 0 held; RunOnce: 4 deletes run unheld).
- Texts: `drive rebind-root --help`, `docs/troubleshooting.md` (Disk not mounted row, Deletes held row, rebind paragraph), `docs/safety.md` (hold bullet, mount bullet, rebind bullet), the status actions of "Disk not mounted" and "Deletes held for confirmation" (and its test), R-6.2.5 and R-6.2.15, `spec/design/sync-planning.md` (new "mount point gone" rule), `sync-observation.md`, `data-model.md` (reason values).
- Checks:
  - Focused hold, mount and rebind tests: ok (sync 9.1 s, cli 0.8 s).
  - `go build ./...` and `go vet ./internal/sync/ ./internal/cli/`: ok. gofumpt: clean.
  - `go test -count=1 ./internal/sync/ ./internal/cli/` under the lock: ok (sync 594 s, cli 6.6 s). An earlier attempt failed to link with "disk quota exceeded" (`/tmp` full at that moment; transient).
  - `verify lint`: pass (1m46s). The first attempt failed on funlen in `describeStatusCondition` (101 > 100), caused by the longer held-deletes reason; that was fixed by reflowing the string.
- No new PARKED lines.
