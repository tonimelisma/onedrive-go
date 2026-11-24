# Track 1A — Delete-safety invariants, onedrive-go @ 26b3bc8b

Read-only adversarial trace. No tests were run; `go build`/`go vet` were not run either because the reviewer's shell was unavailable (all greps went through a read-only helper). Every claim below is anchored to a file:line read during the review. Where a claim rests on filesystem/crash semantics rather than code, it is marked PLAUSIBLE or SPECULATIVE.

Production context assumed throughout: sync folder and data directory (state DB + local trash) on separate filesystems of the dev host's storage. That makes the **cross-filesystem** trash path the normal path, not the exception.

---

## 1. Per-invariant trace

### S1 — R-6.2.1: never delete remote on local absence without a synced baseline entry

**Enforcing code**
- `internal/sync/sqlite_compare.go:529` `comparison_kind = 'local_missing'` requires `baseline_present = 1 AND local_present = 0 AND remote_present = 1`; `:635` `'remote_delete'` is emitted only from `local_missing` (plus `remote_changed = 0 AND remote_item_replaced = 0`). No-baseline + remote-only is `'remote_only_create'` → `download` (`:524`, `:621-622`).
- Go emission sites for `ActionRemoteDelete`: `planner_actions.go:407` (from the `remote_delete` row) and `:551` (`replace_remote`, a type change — `diverged`, baseline present). Move composition's cross-drive decomposition also starts from a baselined item.

**Other paths that delete remotely**
- `executor_delete.go:352` `DeleteItemIfMatch` — only reached from `ActionRemoteDelete`.
- `shortcut_alias_mutation.go:38` `itemsClient.DeleteItem(... BindingItemID)` — deletes a **shortcut placeholder** from local absence of the alias folder. It is keyed by a `shortcut_roots` row with `LocalRootIdentity != nil` (`shortcut_root_lifecycle.go:246`), i.e. an alias the engine itself materialized, so there is a "baseline" of sorts, but it is not the planner's baseline and it does not go through S5/S10 (see 1A-2).

**Tests cited**: `TestPlannerPlanCurrentState_LocalAbsenceDeletesRemoteOnlyWithBaseline` — exercises exactly the no-baseline/remote-only cell. The golden matrix `TestReconcileMatrix_Golden` covers every (baseline, local, remote) combination.

**Verdict: HOLDS** for planner-emitted deletes. Caveat: the shortcut-alias delete is a remote delete decided from local absence outside this machinery (1A-2).

---

### S2 — R-6.2.2: never process deletions from an incomplete enumeration

**Local side**
- Root unreadable: `scanner.go:406-411` returns `ErrMountRootUnavailable` instead of a boundary at `"."`; `scanner_unavailable.go:57-63` the root device read failing also aborts. Walk errors below root → `recordUnreadableObservedBoundary` + `SkipDir` (`scanner.go:413-425`); `fs.WalkDir` does not iterate a partially read directory after the callback returns `SkipDir`, so partial `getdents` output is never used.
- Root identity: `scanner.go:232` `expectedRoot.Validate(tree)` on every `FullScan`. Watch replans run a `FullScan` before every plan (`engine_watch_replan.go:41` → `refreshAndCommitLocalCurrentState` → `observeLocal` → `FullScan`), so inotify-scoped batches never reach the planner on their own; they are dirty hints.
- Items the client chose not to convert: not applicable locally (everything observed is in `observed`); special files / unfollowed symlinks are marked `observed[...] = true` and get an `issueLocalUnsyncableReplacement` (`scanner.go:445-451`, `:600-607`).

**Remote side**
- Full drive enumeration: `seen` is the raw item-ID set (`engine_run_once.go:449-457`, D-008); orphans = baseline rows not in `seen` (`baseline_orphans.go:11-38`) plus `remote_state` leftovers (`:43-79`); every orphan is **confirmed by `GetItem`** before an event is emitted (`engine_mount_root.go:222-260`): 404 → confirmed; `IsDeleted` facet → confirmed; live and `inside == nil` (drive root) → kept; any other error → `undecided`, kept, next refresh pulled forward.
- Mount-root (shared folder) listing: parent chain validated (`engine_mount_root.go:173-192`, D-009); orphans confirmed by lookup, and a live item is treated as gone only if the ancestor walk provably reaches a drive root without passing the mount (`:272-292`); a failing lookup, deleted parent, empty parent or >64 depth all mean "still inside".
- Incremental delta: a window that cannot be materialized is `errRemoteObservationIncomplete` (`observer_remote.go:254`) and nothing is committed (`FullDeltaWithShortcutTopology` returns before `commitObservedItems`, `engine_current_plan.go:205-218`).
- Shortcut placeholders missing from a complete listing: `confirmShortcutCompleteOmissions` (`engine_current_plan.go:240`) — **not traced in depth in this track**.

**403 / 404 / 410 in the live lookup**: only `graph.ErrNotFound` and the deleted facet confirm; 403 and 410 are `err != nil` → undecided → kept. Correct direction. However the sync transport is deliberately unretried (R-6.8.8), so a *single transient* 404 (R-6.7.12 documents that Graph does this) confirms the orphan (1A-5).

**Tests cited**: `TestFullScan_RootReadDirEIOReturnsError`, `TestRootDeviceForScan_UnreadableRootAbortsScan`, `TestRunOnce_EmptiedMountPointPlansNoRemoteDeletes`, `TestFullRefresh_UndecidedOrphanPullsTheNextRescanForward`, `TestRunOnce_MountRootUndecidedOrphanPullsTheNextRescanForward`. The orphan-undecided tests exercise "lookup failed → kept"; there is no test for "lookup returned a transient 404 for an item that exists".

**Verdict: HOLDS WITH CAVEAT** (1A-5; shortcut omission confirmation not traced).

---

### S4 — R-6.2.4 / R-6.4.1: verify local file against baseline before deleting; mismatch → preserve and replan

**Enforcing code**
- `executor_delete.go:246-287` `verifyLocalDeleteContent`: baseline `LocalHash` present → hash through the open descriptor (`e.hashReader(f)`); absent → `localDeleteUnchangedWithoutHash` (`:443-449`) requires `LocalSizeKnown` and equal size **and** mtime, else `errActionPreconditionChanged`. No evidence → refuse. Matches the three-tier claim.
- Inode binding (TOCTOU): `synctree/detach_unix.go:197-245` opens with `openat(O_NOFOLLOW)` via the parent dir fd, stamps (size, mtime, ctime) **before** verify, verifies through the fd; `setAsideVerified` (`:250-280`) re-checks the full stamp, renames by name to a `.onedrive-go.<rand>.detach` sibling, then proves the sibling is the verified (dev, inode) (`sameFileAt`) and unchanged by size+mtime (`unchangedSince`); mismatch → `linkBackNoReplace` (EEXIST-safe) + `ErrDetachRaced`. A file replaced between verify and rename therefore renames the *replacement* aside and links it back; the verified inode (held open) is untouched. Correct.
- Remaining acknowledged weakness: after the set-aside rename only size+mtime are compared (ctime moved by the rename), so an in-place writer that preserves both (`rsync --inplace -t`, mmap writes without mtime update) slips through. Documented in `sync-execution.md`; the trash catches the outcome.
- Folder deletes: `DeleteLocalFolder` (`executor_delete.go:123-191`) never removes content: `RemoveEmptyDirNoFollow` (rmdir) after an emptiness check; only junk-named entries are `RemoveAll`'d, and only when `ignore_junk_files` is set and the junk-named subtree is itself all junk (`findNonDisposable`). Folder identity checked against the *plan's* identity (`executor_preconditions.go:689-730`).
- Remote-delete mirror: `validateRemoteDeleteLocalAbsence` (`executor_delete.go:474-507`) re-validates the sync-root identity and re-checks absence by on-disk spelling before every remote delete.
- Worker-start freshness (`action_freshness.go:235-256`) is a weaker pre-check on `local_state`, gated on `local_truth_complete`; the executor check is the real guard.

**Tests cited**: `TestExecutor_LocalDelete_HashMismatch_ReturnsStalePrecondition`, `TestExecutor_LocalDelete_NoBaselineHashAndChangedFile_PreservesLocalContent`, `TestExecutor_LocalDelete_NoBaselineEvidence_PreservesLocalContent`, `TestDeleteLocalFile_FileReplacedMidHashSurvives`, `TestDetachRegularFile_FileReplacedDuringVerifyIsKept`, `TestDetachRegularFile_InPlaceWriteAfterVerifyIsKept`, `TestCopyVerified_WriteDuringCopyDiscardsTheCopy`. These do exercise the failure branches (not only the happy path).

**Verdict: HOLDS WITH CAVEAT** (documented size+mtime residual; junk `RemoveAll` bypasses the trash, 1A-10).

---

### S5 — R-6.2.5 / R-6.4.2: delete-safety hold

**Enforcing code**: `planner_delete_safety.go:55-117` `applyDeleteSafetyHold`, called from the one shared planning stage `engine_current_plan.go:602` (`buildCurrentPlanStage`). All four entry points go through it: one-shot live (`:30`), dry-run (`:51`), watch bootstrap (`:80`), watch steady-state (`:98`). `RunOptions{}` is passed in watch, so `ConfirmDeletes` is never honoured by watch; follow-up one-shot passes force it false (`engine_run_once.go:88`).

- Counting: `deleteSafetyVerdict` counts `ActionLocalDelete`/`ActionRemoteDelete` in `plan.Actions` (files **and** folders; a folder subtree is one action per descendant). Denominator `countBaselineItems` counts files and folders (`:243-254`), despite the parameter name `baselineFiles`. Threshold `max(50, ceil(30%))` (`:190-193`). Moves are `ActionLocalMove`/`ActionRemoteMove` and are never counted.
- Structural rule: `localSideIsEmpty` ignores root, `lost+found`, and owned transfer artifacts (`:261-278`); `remoteSideIsEmpty` ignores root (`:280-287`). Inputs are the planner-visible (filtered, pruned) rows.
- Closure: held deletes plus transitive dependents are removed and dependencies rebuilt (`:195-240`).
- Persistence: `held_deletes` replaced on every plan by `reconcileRuntimeState` (`engine_current_plan.go:670`, `store_held_deletes.go:24-50`); dry-run does not write it (`keepBuiltCurrentPlan`). Retry rows for held deletes are pruned (`PruneRetryWorkToCurrentActions`), so a held delete cannot resurface via retry.
- Release: only `sync --confirm-deletes` (one-shot) reads the table as `Approved` (`engine_current_plan.go:595-601`); matching is (path, side, and item ID when the action has one) (`planner_delete_safety.go:121-151`); unapproved deletes are judged again. No timer or retry path touches `Approved`.

**Successive-pass bypass**: the hold has no memory. Each plan counts only the deletes *in that plan* against the *current* baseline. In watch mode a replan runs after a 5 s debounce / 30 s max wait; a process deleting ≤50 files per window (or ≤30% of what remains) is never held, and each pass commits the remote deletes and shrinks the baseline. 129,000 files can be removed from OneDrive without ever tripping the hold if the deletion is slow (1A-3). This is P-1 by design, but the spec text ("deletes toward one side exceed max(50, 30%)") reads as a property of the deletion, not of one plan.

**Tests cited**: all eleven listed in `sync-planning.md` exist; `TestRunOnce_ConfirmDeletesKeepsDeletesThatAppearedAfterTheUserLooked` (read in full) is a real engine run: 70 held, 70 more deleted, `--confirm-deletes` deletes exactly 70 online and holds the new 70. `TestPlanner_EmptiedLocalSideHoldsEvenFewDeletes` covers the structural rule. None exercises a multi-pass slow deletion.

**Verdict: HOLDS WITH CAVEAT** (1A-3; shortcut alias delete is outside the hold, 1A-2).

---

### S10 — R-6.2.12: state bound to sync-folder identity

**Enforcing code**
- Identity: `synctree/root_identity_unix.go:110-140` (one descriptor: inode, fsid from `fstatfs`, birth time via statx through the fd, token from `fgetxattr`). `SameRoot` (`:73-94`): token decides when recorded; otherwise inode, birth time when both known, fsid when both known (device fallback; btrfs subvolume special case).
- Startup: `engine.go:299-313` `openBoundSyncTree` → `resolveExpectedSyncRoot` (`observation_root_identity.go:138-190`): recorded identity mismatch → refuse with the `drive rebind-root` hint; no recorded identity **and** baseline non-empty **and** no `SyncRootRebind` flag → refuse (`:164-178`); otherwise take the live identity with `Token = ""` and mark bind pending.
- Bind: `engine.go:334-386` `commitLocalSnapshot` writes a fresh random token (`MarkRoot`, read back to prove the fs keeps it), then records identity **in the same transaction** as the first `local_state` snapshot (`store_local_state.go:88-131`, `bindSyncRootIdentityTx` never overwrites an existing binding, `store_observation_state.go:533-552`). Mark-and-bind serialized by `bindMu`; a lost race re-marks the folder with the recorded token.
- Dry-run: `loadDryRunCurrentObservation` writes the local snapshot to a scratch store with `ReplaceLocalState` (`engine_current_plan.go:427`) and never calls `commitLocalSnapshot`, so **no xattr and no binding are written by `--dry-run`**. Confirmed.
- Enforcement points: every `FullScan` (`scanner.go:232`), every remote delete (`executor_delete.go:475`), and startup. Shortcut children get a parent-issued identity.
- Rebind: `cli/drive_rebind_root.go` shows remembered vs present file counts, requires `REBIND` or `--yes`; `RebindSyncRoot` → `ClearSyncRootIdentity` also clears `mount_boundaries` (`store_observation_state.go:560-571`).
- No xattr support: `SetRootMark` maps ENOTSUP/EOPNOTSUPP/EPERM and a token that does not read back to `ErrRootMarkUnsupported`; binding falls back to inode+fsid+birth time. Read failure of the xattr reads as "no token" → a recorded token mismatches → refuse (safe direction).
- Bind mount / volume reassembly: the token survives both; without a token, ext4's fsid is UUID-derived so device renumbering is tolerated by design. A bind mount of the same filesystem elsewhere is the same inode/fsid/token → accepted as the same folder (correct).

**Caveats**
- A folder copied or restored *with* xattrs (`rsync -X`, `tar --xattrs`) carries the token and is accepted as the same folder; the hold is the backstop for whatever it lacks. By design (token alone decides) but worth knowing for a storage rebuild from backup.
- The shortcut-alias lifecycle consults neither `syncRootIdentity.Validate` nor the hold before deleting a shortcut placeholder (1A-2).
- First sync into an empty mount point (the sync volume not mounted at first start) binds the mount-point directory on the root filesystem and downloads there. Not a delete-safety issue; operational.

**Tests cited**: all twenty in `sync-observation.md` exist; `TestRunOnce_SyncRootReplacedWhileEngineRunsIsRefused` (read) really replaces the root between passes and asserts `ErrMountRootUnavailable` and no remote deletes. `TestNewEngine_UnboundStateWithSyncedItemsIsRefusedUntilRebind` covers the no-record case.

**Verdict: HOLDS WITH CAVEAT** (1A-2).

---

### S11 — R-6.2.13: read failure on the sync folder stops the scan

`scanner.go:406-411` (root ReadDir error → `ErrMountRootUnavailable`), `scanner_unavailable.go:57-63` (root device), `synctree.go:1094-1098` (openRoot error → returned). `syncRootExists` + identity validation precede the walk (`scanner.go:222-239`). Watch: `rescanUnlessRootGone` (`observer_local_handlers.go:~188-200`) and `handleWatcherError` (`:~242-246`) return `ErrMountRootUnavailable`; multisync logs "sync folder is unavailable" and does not restart into deletes (`multisync/orchestrator_watch.go:577`).

**Tests cited**: `TestFullScan_RootReadDirEIOReturnsError`, `TestRootDeviceForScan_UnreadableRootAbortsScan`, `TestFullScan_NonexistentSyncRoot_ReturnsError`.

**Verdict: HOLDS.**

---

### S12 — R-6.2.14: unrepresentable synced path is unavailable truth, subtree-wide for folders

- Symlink not followed / FIFO / socket / device: `recordSyncedPathReplaced` (`scanner_unavailable.go:98-110`), `BlocksReadBoundary` for folders.
- Invalid / too long / excluded name: `recordSyncedItemMovedOutOfReach` matches the skipped entry's (device, inode) to a baseline row, confirmed with `SameIdentity` (birth time), refuses files with `Nlink > 1` (`:115-145`).
- Unstat-able entry: `recordUnreadableEntry` (`:226-236`) — only ENOENT is absence; `d.Info()` EACCES → `local_read_denied` (`scanner.go:522-527`).
- Unreadable content: hash EACCES → `local_read_denied`; other I/O error → `hash_panic` with the message "could not read the file" (`scanner.go:305-326`). Both block local truth.
- Oversized (>250 GB) synced file: not in the requirement's list, but `issueFileTooLarge` is in `observationIssueBlocksLocalTruth` (`truth_status.go:112-125`) so it is unavailable, not a delete. Checked because `scanner.go:612-620` returns without `observed[...] = true`.
- Truth overlay: `truth_status.go:46-105`; boundary issues tagged `perm:dir:read:<path>` cover the subtree via `mostSpecificObservationReadBoundary`.
- Documented remaining gap (`sync-observation.md`, "Unavailable Is Not Absent"): a synced item moved *into* an excluded folder is still a remote delete (recycle bin), because the walk does not descend excluded folders and never sees its identity.

**Tests cited**: `TestRunOnce_SyncedItemsMadeUnrepresentableAreNotDeletedOnline`, `TestFullScan_EntryLstatEIOIsReadBoundaryNotDelete`, `TestFullScan_HashEIOProducesNoUploadAndNoDelete` exist and target the failure branches.

**Verdict: HOLDS** (one documented gap, not re-flagged).

---

### S13 — R-6.2.15: mount points are read boundaries; a remembered crossed mount found on the parent fs is unavailable

- `isForeignMountPoint` compares the folder's device with its parent's, both read in this scan (`scanner_unavailable.go:154-174`); boundary skipped item + `SkipDir`, `observed[...] = true` (`scanner.go:622-635`). Under `cross_filesystems` the path is appended to `crossedMounts`.
- `flaggedMountIsGone` (`:182-192`) → `local_mount_boundary` read boundary, not deletes. Flags come from `mount_boundaries`, loaded for one-shot (`engine_current_plan.go:367-371`) and watch (`engine_watch.go:367-371`).
- Recording: `recordMountBoundariesTx` (`store_local_state.go:497-508`) inserts the crossed set and prunes paths absent from both baseline and `local_state`. The watch safety scan commits through `ReplaceLocalStateReportingChange(ctx, rows)` → `replaceLocalState(..., nil, nil)` (`store_local_state.go:66-72`), so **a mount first crossed during a watch session is never recorded** until the next bootstrap/one-shot scan (the design doc says so). With `cross_filesystems = true` only (1A-7).

**Tests cited**: `TestIsForeignMountPoint_*`, `TestRunOnce_UnmountedNestedDiskWithFewFilesIsNotDeleted`, `TestFlaggedMountIsGone_MountedButEmptiedDiskIsNotABoundary`, `TestClearSyncRootIdentity_ForgetsRecordedMountPoints` exist.

**Verdict: HOLDS WITH CAVEAT** (1A-7; `cross_filesystems` is off by default).

---

### R-6.4.9 — local trash

- Every planner local file delete goes through `removeVerifiedLocalFile` → `trash.Reserve` (entry.json `pending` written **before** the move) → `DetachRegularFile` → `Commit` (`executor_delete.go:291-327`; `localtrash.go:170-232`). Download overwrite: `validateDownloadTargetBeforeReplace` → `PreserveRegularFile` (hard link or copy through the verified fd) → `Commit` (`executor_preconditions.go:581-642`; `detach_unix.go:151-180`).
- Same filesystem: one `renameat` from the `.detach` sibling into `<trash>/<id>/item` (`detach_unix.go:361-366`); a crash leaves either the sibling (reported every scan as `local_interrupted_removal`, never deleted — `scanner.go:488-498`) or the item with a pending record (`TestTrash_CrashBetweenMoveAndCommitIsRecoverable`, read: pending + file → never purged, restorable).
- Cross filesystem (production): `copyVerified` → `copyThroughDescriptor` writes `.copy-*` in the entry dir, fsyncs the **file**, renames to `item`, then `unlinkat` of the `.detach` sibling (`detach_unix.go:368-380`, `:401-437`). No directory fsync anywhere in `localtrash` or `detach_unix.go`; `localpath.AtomicWrite` (entry.json) also does not call `SyncDir` (`localpath.go:374-422`, `SyncDir` exists at `:272` but is unused here). Meanwhile the download path fsyncs the sync-folder directory (`transfer_download.go:318`). Purge treats an entry with a record but no `item` as a dead reservation after 1 h and `RemoveAll`s the whole entry directory (`localtrash.go:520-530`, `:554-576`); `scanEntry` only looks for `item` (`:273-295`), so a `.copy-*` that holds the full content is neither listed nor protected (1A-1).
- Copy failure (SSD full, EIO): `copyVerified` error → `DetachRegularFile` links the sibling back (`detach_unix.go:114-119`) → delete fails as an ordinary error, file stays, placement abandoned. Safe. If the final `unlinkat` fails after a good copy, the sibling is linked back and the trash copy stays (Abandon refuses because it holds a file) — duplicate but safe.
- Size: no cap, no free-space check on the data-dir filesystem; `min_free_space` only gates downloads (1A-4).
- Purge: committed entries after 30 d; uncommitted never; uncheckable item never (`TestTrash_UncheckableItemIsNeverPurged`). Restore never overwrites, refuses symlinked ancestors, copies across filesystems.
- Not through the trash: junk-named children removed by `removeDisposableChildren` (`executor_delete.go:402-416`) and `DeleteLocalSymlink` (the link only). 1A-10.

**Tests cited**: all exist. **None exercises the cross-filesystem detach** (`TestTrash_RestoreAcrossFilesystemsCopies` and `TestTrash_CrossFilesystemRestoreKeepsModeAndTime` cover restore only; `TestCopyVerified_WriteDuringCopyDiscardsTheCopy` covers a torn copy, not durability or the `.copy-*` leftover).

**Verdict: GAP** (1A-1), plus caveats 1A-4, 1A-8, 1A-10.

---

### R-6.5 — state store durability

- SQLite opened with `journal_mode(WAL)` and `synchronous(FULL)` (`store.go:99`). Every destructive action's baseline mutation is one `CommitMutation` transaction (`store_write_baseline.go:395-...`); local snapshot + binding + mount boundaries one transaction (`store_local_state.go:88-131`); held deletes one transaction.
- SIGKILL between filesystem effect and commit: local delete → file is in the trash (pending or committed), baseline row stays, next pass sees both sides absent → `baseline_remove`. Remote delete → next delta reports the tombstone → cleanup. Download replace → equal hashes → `baseline_update`. No path re-deletes on replay. Workers commit outcomes on a detached context (`TestWorkerPool_CommitsCompletedOutcomeAfterPoolCancellation`).
- R-6.5.4 ("atomic managed-file writes shall fsync the directory the rename landed in") is **not** what `localpath.AtomicWrite` does (no `SyncDir`). Whether other managed files use a different helper is outside this track; for the trash record it matters only as part of 1A-1.

**Verdict: HOLDS WITH CAVEAT** (AtomicWrite vs R-6.5.4 text; see 1A-1/1A-9).

---

## 2. Findings

### 1A-1 — Cross-filesystem trash is not crash-durable, and its crash debris is purged after one hour
- **Severity**: high (data loss possible; requires a power loss / kernel crash within seconds of a cross-filesystem delete or download-overwrite; the production layout makes every such operation cross-filesystem)
- **Where**: `internal/synctree/detach_unix.go:361-380` (`moveDetachedOut`), `:401-437` (`copyThroughDescriptor`: `tmp.Sync()` then `os.Rename`, no directory fsync); `internal/localtrash/localtrash.go:208-217` (`writeEntry` → `AtomicWrite`), `:273-295` (`scanEntry` recognizes only `item`), `:520-530` (`Purge` removes a record-without-item after `incompletePlacementGrace` = 1 h), `:554-576` (`RemoveAll` of the entry dir); `internal/localpath/localpath.go:374-422` (`AtomicWrite` never calls `SyncDir`); contrast `internal/driveops/transfer_download.go:318` (sync-folder side **is** directory-fsynced).
- **Rule**: R-6.4.9 "A crash at any point shall leave the file recoverable… uncommitted files are never purged automatically"; R-6.5.4.
- **Scenario**: sync folder on `/srv` (ext4 journal A), data dir on the SSD (ext4 journal B). (a) Remote delete observed → `DeleteLocalFile`: `.detach` rename on A, `.copy-XXXX` written and fsynced on B (the fsync commits B's running transaction, so the entry dir, `entry.json`=pending and the temp file are durable), `rename(.copy-XXXX → item)` on B (only journaled), `unlinkat(.detach)` on A (only journaled). Power loss within the commit interval (~5 s default). On reboot, A's journal may have committed the unlink while B's had not committed the rename: the file is gone from the sync folder and exists only as `<trash>/<id>/.copy-XXXX`. `trash list` does not show it (`hasItem=false`), and the next housekeeping run more than 1 h later removes the entry directory with it. (b) Download overwrite is the same but worse: `PreserveRegularFile` copies to B without a directory fsync, then the download renames over the target **and fsyncs the directory on A** (`transfer_download.go:318`), so A is durable by construction while B's rename is not; the previous version survives only as `.copy-*` until the purge.
- **Evidence**: `copyThroughDescriptor` ends with `os.Rename(tmpName, dest)` and returns; `Purge`: `if found.hasItem {...}; if found.entry.State == StateCommitted {...}; return t.now().Sub(found.reservedAt) > incompletePlacementGrace`.
- **Existing coverage**: `TestTrash_CrashBetweenMoveAndCommitIsRecoverable` (same-fs shape: `item` present, record pending — passes by construction). No test writes a `.copy-*` leftover or simulates EXDEV detach.
- **Proposed regression test**: (1) unit: create `<trash>/<id>/entry.json` (pending) and `<trash>/<id>/.copy-abc` with content, no `item`; advance the clock 2 h; `Purge()` must not remove the directory and `List()` must show the entry (or an orphan) with that file. (2) `detach_unix`: inject a `noReplaceOps`-style seam that forces EXDEV and assert the sequence `fsync(file) → rename → fsync(entry dir) → unlink`, i.e. that the trash directory is synced before the source is unlinked. (3) For downloads, assert the same ordering before `transfer_download.go` renames the partial.
- **Confidence**: CONFIRMED by tracing that no directory fsync exists and that `.copy-*` leftovers are purged with the reservation; PLAUSIBLE for the cross-journal ordering (standard ext4 behaviour, not demonstrated here).

### 1A-2 — Shortcut placeholder deleted on OneDrive from local absence, outside S5/S10
- **Severity**: medium (wrong remote mutation; the shortcut, not the shared content, is removed; recovery is re-adding the shortcut, and the child mount's state is torn down)
- **Where**: `internal/sync/shortcut_root_lifecycle.go:156-171` (missing alias → `reconcileMissingMaterializedShortcutRoot`), `:240-296`; `internal/sync/shortcut_root_planner_local.go:441-466` (`len(candidates) == 0` → `shortcutRootMissingAliasDelete`); `internal/sync/shortcut_alias_mutation.go:38` (`DeleteItem`). Callers: `engine_current_plan.go:249` (one-shot remote stage, runs **before** the local scan validates the root), `engine_observation_postprocess.go:67` (every watch remote batch), `engine_watch_loop.go:330`.
- **Rule**: R-6.2.12 (mismatched folder → refuse to plan), R-6.2.5 (deletes toward one side on an empty side are held), R-6.2.1 in spirit.
- **Scenario**: watch running with one or more shortcut aliases under `<production sync folder>`. `/srv` is unmounted (or the sync folder is replaced by an empty folder) while the watch runs. The next remote poll applies its batch → `reconcileShortcutRootLocalState`: `PathStateNoFollow(alias)` → not exists → `planShortcutRootLocalObservation` returns `shortcutRootLocalNoop` (`shortcut_root_planner_local.go:144-153`, `LocalRootIdentity != nil`) → candidates from stale `local_state` (none at another path) and from `ReadDir` of the empty root (none) → `DeleteItem(BindingItemID)` → record dropped. No `syncRootIdentity.Validate`, no hold, no confirmation. When the disk comes back, the alias folder with its content is ordinary local content and is **uploaded as a new folder** into the user's own drive (quota, duplicate), while the shared folder is no longer linked. The engine's own content planner would have refused the same situation at `FullScan`.
- **Evidence**: `case 0: return shortcutRootLocalPlan{Action: shortcutRootMissingAliasDelete, Mutation: shortcutAliasMutation{Kind: shortcutAliasMutationDelete, BindingItemID: normalized.BindingItemID}}`.
- **Existing coverage**: `TestEngine_ShortcutAliasDeleteMarksParentRootFinalDrain` (the intended case). `TestRunOnce_EmptiedMountPointPlansNoRemoteDeletes` checks content deletes only.
- **Proposed regression test**: parent engine with a materialized shortcut alias; replace the sync root with an empty directory (or re-point a symlinked root) between two watch remote batches; assert no `DeleteItem` on the binding item and that the record enters an unavailable state. Also a one-shot variant: engine opened, root replaced before `RunOnce`, assert the remote stage does not delete the placeholder.
- **Confidence**: CONFIRMED by tracing the decision path; PLAUSIBLE that nothing earlier in the watch batch path stops it (the batch-apply code between `engine_observation_postprocess.go:40-80` and `applyShortcutObservationBatch` was read; no root check was found). Only relevant if shortcuts are configured.

### 1A-3 — The hold is per plan; a sustained slow deletion is never held
- **Severity**: medium (safety invariant weaker than the requirement text suggests; P-1 accepts it)
- **Where**: `internal/sync/planner_delete_safety.go:158-193`; watch window: dirty buffer 5 s debounce / 30 s max wait (`sync-observation.md`, "Dirty Buffer").
- **Rule**: R-6.2.5 "when the deletes toward one side exceed max(50, 30% of synced items)".
- **Scenario**: a misbehaving app, a cleanup script, or a slowly failing disk removes files from the sync folder at <50 per 30 s. Every watch replan sees ≤50 deletes against a baseline of 129k, executes them, and commits; the baseline shrinks; nothing is ever held. After a day the drive is empty on OneDrive (recycle bin only). Likewise a one-shot cron every N minutes with ≤50 deletions in between.
- **Evidence**: `deletes` is counted over `actions` of the current plan only; `held_deletes` is replaced, never accumulated; no deletion rate or window is recorded anywhere.
- **Existing coverage**: none for multi-pass accumulation.
- **Proposed regression test**: engine with 200 synced files; delete 40, run a pass; delete 40, run a pass; repeat five times. Decide and assert the intended behaviour explicitly (today: 200 remote deletes, 0 held). If the product decision is to keep per-plan semantics, say so in R-6.2.5.
- **Confidence**: CONFIRMED (design).

### 1A-4 — Local trash has no size cap, no free-space check, and lives on a different disk than the data
- **Severity**: medium (operational; with the production layout every deleted or replaced byte is copied from the sync filesystem to the data filesystem and kept 30 days; a full data filesystem also hosts the state DB)
- **Where**: `internal/localtrash/localtrash.go:135-141` (`DirForStatePath`: beside the state DB, not configurable — no `trash` key in `internal/config`), `internal/synctree/detach_unix.go:368-380` (EXDEV → copy), `internal/sync/executor_preconditions.go:624-642` (every download overwrite copies the old version).
- **Rule**: R-6.4.9 as written has no cap; R-6.2.6 `min_free_space` covers downloads only.
- **Scenario**: the user replaces a 40 GB video or OneDrive-side edits a large archive 10 times in a month → 400 GB copied to the SSD. Or a bulk remote delete of 300 GB of photos → 300 GB copied before the files leave the sync filesystem. When the data filesystem fills, deletes start failing (safe for the file; `copyVerified` → link back) and are retried on backoff forever; SQLite writes on the same full disk start failing too, which stalls everything.
- **Evidence**: no `Statfs`/free-space check in `localtrash` or `detach_unix.go`; `Purge` only by age.
- **Existing coverage**: none.
- **Proposed regression test**: (a) config knob or documented placement of the trash on the sync folder's filesystem (e.g. `<sync_root>/../.onedrive-go-trash` or a configurable `trash_dir`) so the move is a rename; (b) a test that a copy failing with ENOSPC leaves the file in place and reports a user-actionable condition rather than a retrying failure.
- **Confidence**: CONFIRMED.

### 1A-5 — A single unretried 404 confirms a full-refresh orphan as deleted
- **Severity**: low-medium (local copy goes to the trash and is re-downloaded by the next refresh; requires Graph to both omit the item from the enumeration and answer a transient 404 to `GetItem`)
- **Where**: `internal/sync/engine_mount_root.go:234-237` (`case errors.Is(err, graph.ErrNotFound): confirmed = append(...)`), R-6.8.8 (sync uses raw transport, one request per dispatch), R-6.7.12 (Graph returns transient `itemNotFound` 404s).
- **Rule**: R-6.2.2 "shall be confirmed gone by a live lookup".
- **Scenario**: periodic full refresh during a Graph cross-datacenter incident; 30 items are omitted from the enumeration and the confirming `GetItem` for each answers 404/`itemNotFound` transiently. 30 local deletes are planned (below the hold), files go to the trash, and the next delta/refresh downloads them again. For a shared-folder (mount-root) listing the same single-shot lookup applies per pass.
- **Existing coverage**: undecided-orphan tests cover `err != nil` not-404; none covers a transient 404 followed by success.
- **Proposed regression test**: fake `itemsClient` whose `GetItem` answers `ErrNotFound` once then the live item; assert the orphan is **not** confirmed (e.g. require two agreeing lookups, or a second lookup after a short delay, or treat 404 as undecided on the first refresh and confirm only on the next).
- **Confidence**: PLAUSIBLE.

### 1A-6 — `--confirm-deletes` approves local deletes by path only
- **Severity**: low
- **Where**: `internal/sync/planner_delete_safety.go:121-151`; `planner.go:113-128` (`makeAction` takes `ItemID` from `view.Remote`, nil for a local delete whose remote is gone, so `heldDelete.ItemID == ""`).
- **Rule**: R-6.2.5 "apply exactly the recorded held set".
- **Scenario**: 70 local deletes held (OneDrive deleted a folder). Before the user confirms, the user replaces one of those files locally with new content of the same name but the baseline's size/mtime happen to match (or the file is unchanged). On confirm the delete at that path is approved by path alone. Since S4 verifies content against the baseline (hash) at execution, a genuinely edited file is still preserved; only an identical-content replacement is removed, which is the same outcome the user approved. Practically harmless; noted because the matching rule is weaker than the sentence "matched by path, side and item ID" implies for the local side.
- **Existing coverage**: `TestDeleteSafetyHold_HeldRowWithoutItemIDDoesNotApproveAKnownItem` covers the remote direction.
- **Confidence**: CONFIRMED (mechanism), SPECULATIVE (impact).

### 1A-7 — Watch safety scans never record newly crossed mount points
- **Severity**: low (needs `cross_filesystems = true`, non-default)
- **Where**: `internal/sync/engine_watch_local_observation.go:40-48` → `ReplaceLocalStateReportingChange` → `replaceLocalState(ctx, rows, nil, nil)` (`store_local_state.go:66-72`); recording only in `commitLocalSnapshot` (`engine.go:334`).
- **Rule**: R-6.2.15 "A mount point crossed under `cross_filesystems` shall be remembered".
- **Scenario**: watch running; a USB disk is mounted at `<root>/Backup` (an existing empty synced folder); the safety scan crosses it and uploads its 40 files; the disk is unplugged; the next scan sees an empty folder on the parent device, `flaggedMountIsGone` is false (never recorded), 40 remote deletes are planned and, being below max(50, 30%), executed. A restart before the unmount would have recorded it.
- **Existing coverage**: `TestIsForeignMountPoint_CrossingRecordsTheBoundary` (one-shot path only).
- **Proposed regression test**: watch runtime with `cross_filesystems`; a safety scan whose result has `CrossedMounts` non-empty must land in `mount_boundaries`.
- **Confidence**: CONFIRMED.

### 1A-8 — Download overwrite: window between preserve and rename is unprotected
- **Severity**: low
- **Where**: `internal/sync/executor_preconditions.go:581-642` (`validateDownloadTargetBeforeReplace` → `PreserveRegularFile`), then the partial's rename in `driveops` (`transfer_download.go` around `:318`).
- **Rule**: R-6.4.9 "the previous version of every local file a download replaces".
- **Scenario**: the user's editor atomically saves the file in the milliseconds between the hard link/copy of the verified inode and the `rename(partial → target)`. The rename replaces the *new* save; the trash holds the *older* version. The design doc records the equivalent residual for local moves but not for downloads; `RENAME_NOREPLACE` cannot be used for an overwrite, but `renameat2(RENAME_EXCHANGE)` + verify-then-unlink, or a link-then-exchange dance, would close it on Linux.
- **Existing coverage**: `TestExecutor_Download_TargetAppearsBeforeRenameReturnsStalePrecondition` covers appearance, not replacement in the window.
- **Confidence**: CONFIRMED window exists; SPECULATIVE exploitability.

### 1A-9 — `localpath.AtomicWrite` contradicts R-6.5.4
- **Severity**: low (folded into 1A-1 for the trash; listed separately because the requirement is marked `[verified]`)
- **Where**: `internal/localpath/localpath.go:374-422` (no `SyncDir` after `os.Rename`), used by `localtrash.Placement.writeEntry`.
- **Rule**: R-6.5.4 "Atomic managed-file writes shall fsync the directory the rename landed in".
- **Confidence**: CONFIRMED for this call site; other managed-file writers were not traced in this track.

### 1A-10 — Junk-named content removed by a folder delete bypasses the trash
- **Severity**: low (gated on `ignore_junk_files`; only junk-named entries and junk-only subtrees)
- **Where**: `internal/sync/executor_delete.go:139-186`, `:402-416` (`RemoveAll`), junk list `content_filter.go:250-305` (includes `*.tmp`, `*.partial`, `~*`, `*~`, `.~*`, `._*`, `#*#`, `.Trash-*`, `__MACOSX`, `$RECYCLE.BIN`, `*.swp`).
- **Rule**: R-6.4.9 "Every local file the sync deletes… shall be kept in a per-drive trash".
- **Scenario**: OneDrive deletes folder `Project/`; locally it still holds `notes.tmp` (user content with an unlucky name) and a vim swap for an open file. The folder delete `RemoveAll`s them permanently and rmdirs the folder.
- **Confidence**: CONFIRMED.

---

## 3. Checked and found sound (so coverage is auditable)

- **Planner emission of remote deletes** is baseline-gated in SQL (`local_missing` requires `baseline_present = 1`), and the only Go emission sites are the `remote_delete` row and the type-change replacement.
- **Hold placement**: single shared planning stage; one-shot, dry-run, watch bootstrap and watch steady-state all pass through `applyDeleteSafetyHold`; follow-up passes clear `ConfirmDeletes`; watch never confirms; retry rows for held deletes are pruned; dependents of held deletes are held transitively; `lost+found` and owned artifacts excluded from the structural rule; folders counted; moves not counted; thresholds are constants (R-6.4.3).
- **Watch mode local truth**: every steady-state replan runs a full `FullScan` (identity-validated) before planning, so a burst of inotify deletes, a dropped-event rescan, or an unmount cannot produce remote deletes from scoped batches alone. `HandleDelete` treats any stat/readdir error as "gone" (`localPathStillPresent`, `observer_local_handlers.go:1224-1247`), which is looser than the scanner's D-005 rule, but the row deletion is overwritten by the replan's full scan and can only make in-flight actions *superseded*, never destructive.
- **Watch start with an unmounted/empty folder**: refused at `NewEngine` (recorded identity mismatch, or no record with a non-empty baseline); a fresh state binds whatever is there, which is correct for a first sync (operational caution above).
- **S4 inode binding**: open-once/verify-through-fd/rename-aside/compare-identity is correct against atomic-save races; link-back is `EEXIST`-safe; cross-fs copy re-checks the stamp; folder deletes are rmdir-only with identity checked against the plan.
- **Remote delete preconditions**: root identity re-validated, local absence re-checked by on-disk spelling with type-change and case-rename exceptions, live `GetItem` preflight, `If-Match` on the eTag, recursive child verification bounded at 10,000 with a durable issue beyond.
- **Orphan handling direction**: 403, 410, timeouts and transport errors all keep the item (undecided) and pull the next refresh forward; drive-root live items omitted from an enumeration are kept; mount-root items must provably have left the mount.
- **Root identity**: token-first comparison; fsid tolerates device renumbering; birth time catches inode reuse; xattr failures only ever produce a mismatch (safe); mark-and-bind in one transaction with the first snapshot; dry-run writes neither xattr nor binding; `rebind-root` is interactive with file counts and also forgets mount boundaries.
- **Scanner**: root errors abort; sub-directory errors become boundaries and partially listed directories are not consumed; unstat-able entries (non-ENOENT) and unreadable content are boundaries/issues; special files and unfollowed symlinks at synced paths are unavailable; renamed-to-excluded items are matched by identity with hard-link guard; `file_too_large` also blocks local truth.
- **Store**: WAL + `synchronous(FULL)`; destructive outcomes commit in one transaction; replay after SIGKILL between effect and commit converges to cleanup/baseline update without a second delete.
- **Trash record ordering**: `Reserve` writes `pending` before any move; `Commit` after; `Abandon` refuses to drop an entry that holds a file; `Purge` never removes an uncommitted file that is present; `Restore` never overwrites and refuses symlinked ancestors.
- **Spec-cited tests**: every test name cited for S1, S2, S4, S5, S10–S13 and R-6.4.9 in the three design docs was confirmed to exist; the ones read in full (`TestRunOnce_ConfirmDeletesKeepsDeletesThatAppearedAfterTheUserLooked`, `TestRunOnce_SyncRootReplacedWhileEngineRunsIsRefused`, `TestTrash_CrashBetweenMoveAndCommitIsRecoverable`) exercise the failure they are cited for, with the caveat that the trash crash test models only the same-filesystem shape.

**Not traced in this track**: `confirmShortcutCompleteOmissions` (shortcut placeholder omission confirmation), `applyLocalStatePatch` internals, the full `observer_remote.go` page loop beyond the incomplete-window return, and `composeMoves` internals beyond its delete emission.
