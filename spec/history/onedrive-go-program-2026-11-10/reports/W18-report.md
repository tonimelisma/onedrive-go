# W18 report: one rule for the modes of folders the client creates in a user's tree

Commit `14c89097` on `fix/user-tree-folder-modes` (base `27e76cc3`), 31 files, amended in a second pass, not pushed.

## Folder-creating sites in product code (devtool is tooling, not scanned); lines after the change

| Kind | File:line | What | Mode before → after |
|---|---|---|---|
| user tree | `sync/executor.go:300` | sync local folder create, `syncTree.MkdirAll(folder)`: no mode at the call | `localDirPerms` 0o755 → the sync tree's own (below) |
| user tree | `sync/shortcut_root_lifecycle.go:226`, `:527` | shortcut alias folder and its parents; parents a moved alias needs, `syncTree.MkdirAllNoFollow(...)`: no mode at the call | `shortcutRootDirPerm` 0o700 → the sync tree's own (below) |
| user tree | `synctree/synctree.go:867` (`Root.MkdirAll`, through `rootOps.mkdirAll`), `:1226` (`ensureNoFollowDirectory`, each folder of `MkdirAllNoFollow`) | where the sync tree names the mode | callers' mode → `localpath.UserTreeDirPerm` 0o777 |
| user tree | `cli/get.go:386` (`createDownloadFolder`) | every folder of a folder `get`, drive path and shared (were `get.go:273`, `get_shared.go:155`) | `defaultDirPerm` 0o755 → `UserTreeDirPerm` |
| user tree | `driveops/transfer_target.go:54` | missing parent of a `get` target | `newDirPerms` 0o777 → `UserTreeDirPerm` |
| user tree | `localtrash/localtrash.go:404` | missing parents of a restored file | trash `dirPerm` 0o700 → `UserTreeDirPerm` |
| sync root | `cli/sync_dir.go:27` | sync folder at login / drive add | `syncRootDirPerms` 0o700, unchanged |
| managed | `cli/auth_flow.go:269`, `cli/auth_login.go:283`, `tokenfile/tokenfile.go:87`, `:91` | sign-ins | `tokenfile.DirPerms`, `tokenDirPerms` 0o700 |
| managed | `config/write.go:770`, `:774`, `config/mutation_lock.go:77` | config and catalog folders | `configDirPermissions` 0o755 (0o700 in the open fsroot PR) |
| managed | `driveops/session_store.go:202`, `logfile/logfile.go:34`, `multisync/control_socket.go:132`, `perf/capture.go:115` | sessions, logs, control socket, perf capture | own constants, 0o700 |
| managed | `localtrash/localtrash.go:183`, `:215`, `synctree/detach_unix.go:419` | trash entries; folder a detached file is copied into | `entryDirPerm` (renamed from `dirPerm`), `detachDirectoryPerm`, 0o700 |
| managed | `sync/store.go:105`, `sync/engine_locks.go:224`; `perf/capture.go:121`, `sync/store_scratch.go:63` (`MkdirTemp`, no mode) | state store, engine locks; perf and dry-run scratch | own constants 0o700; Go's 0o700 |

Totals: 6 user-tree call sites (7 before; the two `get` walks are one), 3 of them through the sync tree, which names the mode in 2 places; 1 sync-root; 18 managed (16 with a mode, 2 `MkdirTemp`). Pass-through wrappers create nothing of their own: `localpath.MkdirAll`/`atomicWrite`/`AtomicWrite`, `fsroot.Root.MkdirAll`/`AtomicWrite`/`atomicWriteIn`, `synctree`'s `osRootHandle` and its `rootOps.mkdirAll` default, `cli.writeManagedFile`, the synctest shims.

## What changed
- `localpath/localpath.go`: `UserTreeDirPerm = 0o777`, the one home; `MkdirAll` documented; uncalled `Mkdir` removed.
- `synctree/synctree.go`: `Root.MkdirAll(rel)` and `Root.MkdirAllNoFollow(rel)` take no mode and ask for `UserTreeDirPerm` themselves; `MkdirAll` goes through `rootOps.mkdirAll` (it was dead) like `remove`, `rename`, `rmdir`, `chtimes`, which tests inject. `synctree` already imported `localpath` (`detach_unix.go`) and no depguard rule restricts it, so the constant stays in `localpath`.
- `sync/executor.go`, `sync/shortcut_root_lifecycle.go`: call without a mode; `localDirPerms`, `shortcutRootDirPerm` and both `localpath` imports removed. Callers in synctree, synctest tests updated (none needed a mode).
- `driveops/transfer_target.go`: `UserTreeDirPerm`, `newDirPerms` removed; comment cites R-5.10.2, not "P-8".
- `cli/get.go`: one walk `downloadTree` for both flavors (listings keyed by item ID), `createDownloadFolder`, `downloadTreeFile`, `downloadOneFile`, `finishFolderDownload`, `configuredMinFreeSpace`; `cli/get_shared.go` uses them.
- `localtrash/localtrash.go`: restore parents use `UserTreeDirPerm`; `entryDirPerm`/`entryFilePerm`; `requireSyncFolder`; `refuseSymlinkedAncestors` reports a `Rel` error; `maxListEntries` gone, `scan` allocates `len(dirEntries)`. `cli/sync_dir.go`: why the sync folder is 0o700.
- `synctest/umask.go`: `RunUnderUmask`, `GroupWritableUmask`. Contract tests `localpath/folder_modes_test.go`.
- Specs: R-5.10.2, R-1.2.4, R-6.4.9; `drive-transfers.md`, `sync-execution.md`, `sync-engine.md`, `cli.md`, `system.md` (three kinds, the sync tree's mode, contract tests, umask helper).

## Tests added (seen failing first unless marked pin)
- `TestFolderModes_EveryCreatedFolderNamesAClassifiedMode` (R-5.10.2): failed on `localDirPerms`, `defaultDirPerm` x2, `newDirPerms`, `shortcutRootDirPerm` x2, trash `dirPerm` x3, unused names. `TestFolderModes_TheSyncTreeCreatesOnlyUserTreeFolders` (R-5.10.2): failed against the first pass's `synctree.go` ("Root.MkdirAll takes a folder mode from its caller", "creates no folder"). `TestFolderModes_OneHomeForTheUserTreeMode` (pin).
- Under umask 002 in a child binary: `TestExecutor_CreatedFolderFollowsTheUmask` (0755 → 0775), `TestEngine_ShortcutRootFoldersFollowTheUmask` (0700 → 0775; a moved alias keeps 0700), `TestDownloadTree_CreatedFolderFollowsTheUmask` (0755 → 0775), `TestTrash_RestoreCreatesMissingFoldersThatFollowTheUmask` (0700 → 0775), `TestRoot_CreatedFoldersFollowTheUmask` (synctree, both methods; an existing folder keeps 0700; pin of the new shape), `TestDownloadToFile_NewFileAndParentHonourUmask` (now under 002: file 0664, parent 0775), `TestRunUnderUmask_RunsTheTestUnderTheUmask` (pin).
- `TestRoot_MkdirAll_FailureInjection` (R-2.10, R-5.10.2): the seam is called, asked for `UserTreeDirPerm`, its errno kept.
- `TestDownloadTree_CountsOnlyTheFoldersItCreates` (3 → 1), `TestDownloadTree_FileWhereAFolderBelongsIsReported`, `TestConfiguredMinFreeSpace`, `TestRunGet_SharedFolderDownloadsItsTree` (pin), `TestTrash_RestoreRefusesAMissingSyncFolder`, `TestRefuseSymlinkedAncestors_RefusesATargetItCannotPlaceInTheSyncFolder`. Renamed `TestDownloadRecursive_*` → `TestDownloadTree_*`.

## How the contract tests classify, and what they cannot catch
go/parser over main.go, cmd, internal, pkg (devtool, tests excluded). The scan collects every function, method and struct-field function literal, then finds to a fixed point the ones that pass a parameter on as a folder mode, starting from calls named `Mkdir`, `MkdirAll`, `MkdirAllNoFollow`. With no type information a call is resolved by its name, argument count and the packages its file imports (a package-qualified call by that package only); a call into a package outside the module, or a two-argument method call of those names (the `os.Root` form), takes its last argument as the mode; an ambiguous call is checked at every argument any candidate takes as a mode. A mode that is the enclosing function's parameter passes through; any other must be a constant listed in `classifiedFolderModes` with its kind, the user-tree kind holding one entry; a listed constant nothing uses fails too. `TestFolderModes_TheSyncTreeCreatesOnlyUserTreeFolders` adds that no method of `synctree.Root` takes a folder mode and that `Root.MkdirAll` and `ensureNoFollowDirectory` create folders and ask only for `UserTreeDirPerm`. They cannot catch: a folder outside the sync tree that belongs to a user's tree but is created with a listed managed-state or sync-root constant (the restore parents were exactly that); a folder made by a call they do not know (`syscall.Mkdir`, `unix.Mkdirat`, a function value passed around, a wrapper reachable only through an unnamed literal); a new folder-creating function inside `internal/synctree` other than those two that asks for a listed managed-state constant (the detached-copy folder in the trash legitimately does). Sync code can no longer pass a mode at all.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...`: pass.
- `go test -count=1` over `./internal/cli/... ./internal/driveops/... ./internal/localpath/... ./internal/localtrash/... ./internal/synctest/... ./internal/synctree/... ./internal/multisync/...`: ok.
- `internal/sync`: `-run 'TestExecutor|TestEngine_ShortcutRootFoldersFollowTheUmask|TestEngine_ReconcileShortcutRoot|TestEngine_ShortcutAlias|TestEngine_RemovedShortcut|ListsEvery|Table|Governs|Exhaustive|Registered'`: ok. (No sync test calls the sync tree's `MkdirAll` directly.)
- `-race -run` on the tests added or changed in this pass (localpath, synctree, synctest, driveops) and the sync tests whose product calls changed (`TestExecutor_CreatedFolderFollowsTheUmask`, `TestExecutor_CreateLocalFolder*`, `TestEngine_ShortcutRootFoldersFollowTheUmask`, `TestEngine_ReconcileShortcutRootLocalStateMoves*`): ok.
- Lint of the touched packages (cli, driveops, localpath, localtrash, synctest, synctree, sync), once, last: `0 issues.` (W18-lint.log).
- Every test the touched design docs cite exists (script check); no B-NNN comment removed; no P-n label on a line this commit writes. Live proof: none (Graph fails `GET /me` for the test account); the unit tests are the proof.

## Deviations
- Shortcut alias folders, the parents a moved alias needs, and a restored file's missing parents are user-tree folders too: under umask 022 a new one is now 0755 where it was 0700.
- No "P-8" text exists in the repository; the rule is written in R-5.10.2.
- `trash restore` refuses a sync folder that is gone instead of creating it.
- Folder `get`: `folders_created` counts only folders it made; a shared folder walks inline, records a cancelled file, errors name the local path; a shared file `get` into a folder lands under its remote name (R-1.2.6).
- `synctree.Root.MkdirAll`/`MkdirAllNoFollow` lost their mode parameter (an interface change inside the module).
- The umask tests use a child test binary: the kernel applies a folder's umask at mkdir, which `TransferManager`'s probe cannot stand in for.

## Also fixed
- `localpath.Mkdir` dead; `refuseSymlinkedAncestors` swallowed a `Rel` error; the trash's generic `dirPerm`/`filePerm` names.
- `get_shared.go`: a goroutine per subfolder; a cancelled file recorded nothing; no unit test; ignored `localFileTarget` (R-1.2.6). `get.go`/`get_shared.go`: `min_free_space` parsed three times and an unreadable value silently disabled the check; `folders_created` counted existing folders; errors named the path twice.
- `get_test.go`: `TestGetFolderJSONOutput_EmptyErrors` asserted nothing about errors; positional struct literals; six tests without `Validates:`; wrong R-6.7.1 citation. `sync-execution.md`: a broken, duplicated trash passage.
- Principle labels: the one on a line this commit wrote (`driveops/transfer_target.go`, "P-8") now cites R-5.10.2; no other added or rewritten line carries one.
- `internal/localtrash/localtrash.go`: `maxListEntries` bounded only a preallocation whose size was known; removed, `scan` allocates `len(dirEntries)` (lint accepts it).
- `internal/synctree/synctree.go`: `rootOps.mkdirAll` was never called; `Root.MkdirAll` now goes through it.
- `internal/synctree/synctree.go`: `MkdirAll`/`MkdirAllNoFollow` took a mode though every folder they make is a user-tree folder; the parameter is gone and the sync tree applies `UserTreeDirPerm`.
- `internal/driveops/transfer_manager_test.go`: `TestDownloadToFile_NewFileAndParentHonourUmask` compared with the runner's umask and could not tell a 0755 parent from 0777 under 022; it runs under 002 and asserts 0664 and 0775.

## Seen and not fixed
- `internal/localpath/localpath.go`: exported functions without doc comments, and `cleanPath:22` dead `clean == ""`. Handled outside this commit.
- `spec/requirements/non-functional.md:169` R-6.8.23 sits under "R-6.8 Network Resilience". Handled outside this commit.
- `spec/design/sync-control-plane.md:31` calls the data directory 0755. Handled outside this commit.
- A shared single-file `get` has no command-level test. Handled outside this commit.

## Unfinished
- Nothing.

## For the reviewer to run
- The whole `internal/sync` package.

## Review (2026-11-12, both passes; second pass `14c89097`)
- Read: the first pass's whole product diff and test diff; the second pass's whole code diff against the first (`118ea870` → `14c89097`, 15 files), the spec word-diff and the commit message. No defect.
- The six items of the second pass are done: the sync tree's two methods take no mode and name the constant themselves; the `rootOps.mkdirAll` seam is routed and has a failure-injection test; the contract test resolves a call by name, argument count and imports and has the sync-tree test beside it; `maxListEntries` is gone; the download umask test runs under 002; the `LocalTarget` comment cites the requirement.
- Checked by hand: the contract test's resolution on the three call shapes that matter (`e.syncTree.MkdirAll(folder)`, `root.MkdirAll(name, perm)` on an `os.Root`, `r.ops.mkdirAll(root, clean, perm)`): each resolves as the report says. `localpath.go` changed only in the constant's comment.
- Ids and labels on added lines (plan ids, `B-NNN`, `P-n`) against `27e76cc3`: one hit, the removal of main's `newDirPerms` comment. Clean.
- Mine: `go test -count=1` over cli, driveops, localpath, localtrash, synctest, synctree, multisync and the whole non-race `internal/sync`: see the PR's verification. Pre-pr gate before the push.
- Accepted deviations: all six listed above. The 0700 → 0755 change for shortcut folders and restored parents is the rule's point (they are user-tree folders) and is in the PR's behavior list.
- Waits for the rebase after the fsroot change merges (mine): doc comments on `localpath`'s exported functions and the dead `clean == ""` check; R-6.8.23 moves from Network Resilience to Safety with every citation (by script); `sync-control-plane.md` line 31.
- Planned elsewhere: the principle labels (their own docs change); a command-level test of a shared single-file `get` (rider on the tree-transfers refactor).
- Live proof: done 2026-11-13 23:52 Pacific with the client built from main after #900 (test account, private configuration and data tree, one small remote folder included; `scratchpad/w18live.py`). Under umask 002 a one-shot `sync` and a `get` each made their folder 0775 and their file 0664; under umask 022 they made 0755 and 0644. All four commands exited 0. PR #894's body holds the same lines.
