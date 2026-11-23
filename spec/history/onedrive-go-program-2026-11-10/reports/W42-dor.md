# W42 Definition of Ready

Worktree `~/src/onedrive-go-wt/managed-folders-outside-sync-dirs`,
branch `fix/managed-folders-outside-sync-dirs`, base `3a80f900` (follows
`9d08ad70`).

**First pass: stopped before any edit. Answer (ii) is yes**, a stop condition
of the brief. Answer (iv) is no rule, so (iv) does not stop.

**Refresh after the decision (option A, binding details 1-7):** the replacing
move takes the verified destination out by the local delete rule
(`DetachRegularFile` into the trash, `RemoveVerifiedRegularFile` without one,
`ErrDetachRaced` as a stale precondition), then `RenameNoReplace`. Items 6, 8
and 12 below are now PASS. No new outcome kind, issue type, planner row or
store change is needed: a taken destination ends as `errActionPreconditionChanged`
(answer i), and one new crash point (`exec.local_move_destination_trashed`, a
test hook between the two steps) is added.

## The five DoR answers

(i) **A local move whose destination is taken, today.** Precondition
`validateLocalMoveDestinationAbsent` (`internal/sync/executor_preconditions.go:767-787`)
lstats the destination; anything there (other than the source itself on a
spelling-only move, and other than a `ReplaceLocalTarget` move) returns
`stalePreconditionError("local move destination %s already exists")`
(`:780`), i.e. `errActionPreconditionChanged`. `classifyResult`
(`internal/sync/engine_result_classify.go:88-94`) makes it `resultSuperseded`,
`persistNone`; `applySupersededCompletion`
(`internal/sync/engine_runtime_completion.go:170-198`) counts it, clears its
retry work (or parks it after `supersedeParkThreshold` consecutive
supersedes), completes its dependent subtree and calls `markReplanNeeded`:
the action is not failed and not retried as is; it is replanned after a new
observation. Proof: `TestExecutor_LocalMove_ReplacesOnlyTheVerifiedDestination`
("changed destination is stale", `executor_test.go:3767`) asserts
`errActionPreconditionChanged`. The window: the check runs before
`clearLocalMoveDestination` and the plain `syncTree.Rename`
(`internal/sync/executor.go:523`, `:601`), so a destination that appears
after the check is replaced silently. Item 3's taken destination would map
to the same outcome (stale precondition, superseded): no new outcome.

(ii) **YES. The planner and three tests rely on a local move replacing a file
the planner knows about.** `composedDisplacement`
(`internal/sync/planner_move_composition.go:1143-1163`) gives a synced file
OneDrive no longer has, whose local hash equals the baseline's,
`moveDisplaceReplace`; `destinationFree` (`:966-998`) then lets another item
move onto its path, and `composedLocalMoveActions` (`:1327-1329`) sets
`Action.ReplaceLocalTarget` (`actions.go:102-106`, D-058). No delete of the
displaced file is planned: the move itself replaces it. The executor
(`executor_preconditions.go:773`, `:797-830`) re-verifies the destination;
with a trash it calls `PreserveRegularFile` (hard link or copy into the
trash, the file stays in the tree) and then the plain `Rename` replaces it;
without a trash it verifies and the `Rename` replaces it. Tests that need
the replacement: `TestRunOnce_RemoteMoveOverExistingFileReplacesWithoutResurrecting`
(`engine_move_composition_test.go:413`),
`TestRunOnce_RemoteMoveOverIdenticalFileConvergesInOnePass` (`:481`),
`TestExecutor_LocalMove_ReplacesOnlyTheVerifiedDestination`
(`executor_test.go:3767`, case "unchanged destination is replaced"). A
no-replace rename makes each of them fail with ErrTargetExists.
(`TestExecutor_LocalMove_StaleSourceLeavesTheReplacedDestinationInPlace`,
`executor_local_trash_test.go:218`, builds a replacing move too but asserts
the stale-source refusal, so it does not depend on the replacement.) The other displacement, `moveDisplaceConflictCopy`, and a
delete planned at the move's destination are ordered by the planner:
`moveOrderDependency` (`internal/sync/planner_dependencies.go:462-475`, last
line) makes a move depend on a delete, cleanup or conflict copy at its own
path, so the destination is free before the move runs; those need no
replacement.

(iii) **Folders the client manages, and where their paths come from.**
Data directory: `config.DefaultDataDir()` (`internal/config/paths.go:58`):
`$XDG_DATA_HOME/onedrive-go`, else `~/.local/share/onedrive-go`, on macOS
`~/Library/Application Support/onedrive-go`. It holds sign-ins
(`DriveTokenPath`, `token_resolution.go:21`), the catalog
(`CatalogPathForDataDir`), every state DB and its sidecars
(`DriveStatePath`/`MountStatePathForDataDir`, `drive.go:424-438`,
`mount_state_path.go:16`), the email-change journal, the engine lock files
(`engine_locks.go:134-165`), upload sessions (`engine.go:217-218`), the
control socket when its path fits (`paths.go:119-137`), and the trash:
`localtrash.DirForStatePath` = `<dir of state DB>/trash/<state DB base>`
(`internal/localtrash/localtrash.go:177-183`). Config directory:
`config.DefaultConfigDir()` (`paths.go:36`): `$XDG_CONFIG_HOME/onedrive-go`,
else `~/.config/onedrive-go`, on macOS the same folder as the data
directory; it holds `config.toml` unless `--config`/`ONEDRIVE_GO_CONFIG`
names another file (validation does not know that path). State directory:
there is none apart from the data directory. Detach destination: the trash
placement's `ItemPath` (`localtrash.go:219-237`), passed to
`DetachRegularFile`/`PreserveRegularFile` by the executor, so it lies in the
trash. Not managed folders: the control socket fallback and perf captures in
`os.TempDir()`, and an opt-in `log_file` the user names.

(iv) **A sync_dir that holds one of them, today: no rule keeps it out.**
`checkSyncDirOverlap` (`internal/config/validate_drive.go:194-221`) compares
sync_dirs only with each other. The local filter `contentFilter.isIgnored`
(`internal/sync/content_filter.go:132-154`) skips only artifact names
(transfer partials, spelling-rename, detach and restore artifacts), dotfiles
when `ignore_dotfiles` is set (default false), junk files when enabled, and
the user's ignore lists; nothing names the data or config directory. With
`sync_dir = "~"` the scan observes `.local/share/onedrive-go` and
`.config/onedrive-go` and uploads them: sign-in tokens, the state DBs, the
trash (files sync deleted come back as new uploads), and `config.toml`.
`trash restore` then fails, since `MoveInNoReplace` refuses a source inside
the root (`internal/synctree/rename_noreplace_unix.go:103`).

(v) **A config item 1 refuses, and its text.** For example
`["personal:<account>"]` with `sync_dir = "~"` (or `"/"`, or the data
directory's own parent) on a host with the default directories. The planned
text, one line per managed folder inside it (data and config are one line on
macOS, where they are one folder), as `onedrive-go sync` shows it, exit 78:
`Error: loading config: config validation failed: drive "personal:<account>" sync_dir /home/u contains /home/u/.local/share/onedrive-go, the folder onedrive-go keeps its own data in (sign-ins, sync state, trash); set sync_dir to a folder that does not contain it`
(as implemented; the example folder was dropped, since the default differs per
drive type), and for the config directory the same with
`/home/u/.config/onedrive-go, the folder onedrive-go keeps its configuration in`.
The informational commands (status, drive list) load leniently and show it
as a warning (`internal/config/load.go:153`).

## Checklist

1. PASS **Task intent**: a folder the client manages never lies inside a sync folder, and a local move never replaces what it did not expect at its destination.
2. PASS **Non-goals**: `internal/cli` beyond error text, `internal/graph`, `internal/multisync`, `internal/devtool`, e2e, the chaos layer, the shortcut child's trash; no new issue type, outcome kind, store generation or decision-table row.
3. PASS **Governing docs**: `spec/design/config.md` (config), `spec/design/system.md` (synctree, localtrash, rooted mutation), `spec/design/sync-execution.md` (local move), `spec/requirements/non-functional.md` R-6.4.8, `spec/requirements/sync.md` R-2.17.9 (D-058), the H9 report "Not fixed" and second pass.
4. PASS **Authority owner**: config validation owns refusing the sync_dir (`internal/config/validate_drive.go`); synctree owns containment of detach and of the trash source (`detach_unix.go`, `rename_noreplace_unix.go`); the executor owns the local move (`internal/sync/executor.go`), the planner owns whether a move may displace its destination (`planner_move_composition.go`).
5. PASS **Code scope**: `internal/config/validate_drive.go` (+ test), `internal/synctree/detach_unix.go`, `rename_noreplace_unix.go` (+ tests), `internal/sync/executor.go` and possibly `executor_preconditions.go` (+ tests), docs listed in the brief.
6. FAIL **Regression plan**: items 1, 2, 4, 5 have their tests designed (config validation test with sync_dir above XDG data and config dirs; detach and preserve with a dest inside the root; detach with a FIFO at the parent behind an interposed lstat that reports a folder, which blocks today; MoveInNoReplace with a trash path reached through a symlink into the root). Refreshed: item 3 is tested by a destination hidden from the precondition (file and folder), a file that takes the freed name at the new crash point (file and folder source), a crash between the two steps recovered by the capture harness, and the three replacing-move tests with trash-once and identity assertions.
7. PASS **Verification plan**: as the brief lists: build, both vets, gofumpt, `./internal/sync/...` whole under the lock, `-race` on the named tests, the four other packages plain and under `-race`, the devtool source checks, one lint last.
8. PASS **Data safety** (refreshed): the replacing move's destination leaves by the verified local delete rule into the trash, then the rename replaces nothing; a file that takes the freed name is kept and the move is superseded; a crash between the steps leaves the old file in the trash and the next plan finishes the move without deleting on OneDrive.
9. PASS **Secret and credential safety**: no tokens, `.env`, `.testdata` or account names read or written.
10. PASS **Live provider and destructive operations**: none; no live test is run.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. PASS **Unknowns** (refreshed): the question how a `ReplaceLocalTarget` move makes way was decided (option A). Remaining: Question: does the plan after a crash between the two steps finish the move without a remote delete or conflict copy? Current assumption: yes. Validation step: `TestCrashRecovery_ReplacingMoveBetweenItsTwoStepsFinishesTheMove` (passes). Earlier question for reference: Owner document/code path affected if wrong: `executor_preconditions.go` (`clearReplacedLocalMoveTarget`), `executor.go` (`renameLocal`), `spec/design/sync-execution.md` (local move), R-6.4.8, R-2.17.9.

## Options for item 3 (for the decision)

A. The replacing move takes the verified destination out of the tree first,
bound to the verified inode (`DetachRegularFile` into the trash, or
`RemoveVerifiedRegularFile` without a trash; a mismatch is
`ErrDetachRaced`, mapped to a stale precondition as today), then
`RenameNoReplace`. Every local move is then no-replace and the R-6.4.8
exception goes; a file that lands between the two is kept and the move is
superseded, with the displaced file already where the plan meant it to go
(the trash). No planner, outcome or decision-table change; the three tests
above keep their assertions. Cost: the destination name is briefly absent.

B. Keep the plain rename only for `ReplaceLocalTarget` (it replaces the file
it verified) and use no-replace for every other move. Smaller; R-6.4.8 keeps
a narrower exception naming the replacing move and its window.
