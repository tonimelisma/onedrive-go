# A5 report: e2e waits check content, not names

Commit `47e26008` on `test/e2e-content-waits` (base `90c60a8f`), 10 files. 17 racy name-only waits and the 2 inline
content loops converted; #860 had already fixed `TestE2E_SyncWatch_PauseResume`. `pollForDrive2File` removed.

## What changed
- `e2e/remote_read_helpers_test.go`: new `waitForRemoteStatSizeForDrive` and pure `remoteStatSizeReady`; `waitForRemoteStatSize` delegates with `""`; `waitForRemoteFileContent` drops its drive and timeout parameters (waits `daemonPollTimeout`) and its timeout message names the expected content; new `TestRemoteStatSizeReady`.
- `e2e/e2e_test.go`: `daemonPollTimeout` moved here from `sync_watch_full_test.go`, so the `e2e`-only helper file can use it.
- `e2e/sync_watch_e2e_test.go`: 3 sites converted, the #860 call updated to the new signature; `pollForDrive2File` and the `strings` import removed.
- `e2e/sync_watch_full_test.go`: 8 sites and both inline content loops converted.
- `e2e/orchestrator_e2e_test.go`: 4 sites converted; `WatchPausedDrive` runs each drive-2 stat as one statement (funlen).
- `e2e/socketio_e2e_test.go` (fast tier) and `e2e/sync_shortcut_full_e2e_test.go`: 1 site each.
- `spec/reference/live-recurring-families.md`: new family "Name-Only Wait Stops The Daemon Mid-Create And Leaves An Empty Placeholder".
- `spec/reference/live-incidents.md`: LI-20261110-01 names the 17 sibling waits, the removed helper and the family.
- `spec/design/system.md`: Verification Policy bullet stating the wait rule and naming the three helpers.

## Tests added
- `TestRemoteStatSizeReady` (tag `e2e`): ready only on a successful `stat --json` with exactly the size; rejects the 0-byte item a create leaves, a failed stat, human output and empty output.

## Touched tests and the fact each new wait checks (reviewer runs each live once)
- `e2e/sync_watch_e2e_test.go` `TestE2E_SyncWatch_BasicRoundTrip`: exact remote content before SIGTERM.
- `e2e/sync_watch_e2e_test.go` `TestE2E_SyncWatch_PauseResume`: unchanged fact (content); call signature only.
- `e2e/sync_watch_e2e_test.go` `TestE2E_SyncWatch_ControlSocketReload`: drive-1 content before the reload; drive-2 size before SIGTERM.
- `e2e/sync_watch_full_test.go` `TestE2E_SyncWatch_Bidirectional`: size of the local-to-remote file.
- `e2e/sync_watch_full_test.go` `TestE2E_SyncWatch_ConflictDuringWatch`: size of the baseline before the remote put.
- `e2e/sync_watch_full_test.go` `TestE2E_SyncWatch_FileModification`: size of version 1 before the edit; content of version 2.
- `e2e/sync_watch_full_test.go` `TestE2E_SyncWatch_FileDeletion`: size before the local delete.
- `e2e/sync_watch_full_test.go` `TestE2E_SyncWatch_FolderCreation`: size of `a/b/c/deep.txt`.
- `e2e/sync_watch_full_test.go` `TestE2E_SyncWatch_MultipleFiles`: size of each of the five files.
- `e2e/sync_watch_full_test.go` `TestE2E_SyncWatch_RapidChurn`: content "final version" (loop replaced).
- `e2e/sync_watch_full_test.go` `TestE2E_SyncWatch_GracefulShutdown`: size before SIGTERM.
- `e2e/sync_watch_full_test.go` `TestE2E_SyncWatch_TimedPauseExpiry`: size after the pause expires.
- `e2e/orchestrator_e2e_test.go` `TestE2E_Orchestrator_WatchSimultaneous`: size on drive 1 and on drive 2.
- `e2e/orchestrator_e2e_test.go` `TestE2E_Orchestrator_WatchDriveIsolation`: size on drive 1.
- `e2e/orchestrator_e2e_test.go` `TestE2E_Orchestrator_WatchPausedDrive`: size on the active drive.
- `e2e/socketio_e2e_test.go` `TestE2E_SyncWatch_WebsocketDisabledLongPollRegression`: size before SIGTERM.
- `e2e/sync_shortcut_full_e2e_test.go` `TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget`: size on the shared selector.
- `e2e/remote_read_helpers_test.go` `TestRemoteStatSizeReady` (new, non-live, runs under the suite's TestMain).

## Checks run
- `go build ./...`: pass. `go vet -tags 'e2e e2e_full' ./e2e/...` and `go vet -tags e2e ./e2e/...`: pass. gofumpt, goimports: clean.
- `devtool verify lint`: first run found funlen (`WatchPausedDrive`, 51 statements) and unparam (`waitForRemoteFileContent` always got `""` and 3 min); fixed; second run 0 issues. No `integration` file touched; no e2e test run.

## Deviations
- New helper `waitForRemoteStatSizeForDrive` (the brief names it, but it did not exist): three sites need a size wait on a non-default drive, and `waitForRemoteStatSize` hard-codes the default. Same pattern as `getRemoteFile`/`getRemoteFileForDrive`.
- `waitForRemoteFileContent`'s interface narrowed (no drive, no timeout; the timeout stays 3 min for every caller) and its timeout message names the expected content, for the #860 caller too.
- `FolderCreation` and `MultipleFiles` now read the file path with `stat --json` instead of listing the parent.
- `TestRemoteStatSizeReady` has no `// Validates:` line: no requirement covers e2e harness helpers, matching its neighbours (`TestDeleteDisappearanceReady`, `TestClassifyFixtureSeedVisibilityAttempt`).
- Docs: no e2e README or testing reference names these helpers, so the policy went into `system.md` Verification Policy.

## Recommendations
1. Drop two SAFE name waits that the new waits now cover: `RapidChurn` (right before the content wait) and `GracefulShutdown`'s final `before-shutdown.txt` check.
2. Do the optional strengthening from 01d A2: `sync_full_test.go` name waits on a modified pre-existing file (near :593, :647) and the `cli_commands_e2e_test.go` content asserts after `--force` with no wait.
3. Make `pollRemoteEventually`'s timeout report the last size or content seen; for `get`, stdout is not the file's bytes.
4. Add a repository rule: no name-only wait in a test that starts a daemon (`cmd.Start`, `startDaemon`, `startFastDaemon`, `withShortcutWatchDaemon`).
5. Run a full-tier `workflow_dispatch` after merge: 16 of the 17 touched live tests run only nightly.

## Unfinished, and packages for the reviewer
- Nothing unfinished in code; the live proof is the reviewer's runs of the tests above, `./e2e` only, one at a time. No product package changed.
