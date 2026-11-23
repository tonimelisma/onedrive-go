# W66 fix report

**Outcome:** I fixed all four findings in commit `8bc8aaa0` on `fix/error-names-path-once`, on top of `acd7c659`. Worktree: `~/src/onedrive-go-wt/error-names-path-once`. Not pushed, no PR, no rebase.

It is installed: origin/main `b00e6da3` is an ancestor of the branch, and `onedrive-go --version` prints `8bc8aaa0`.

## What each finding got

### B1 (blocking): a failed single `get` now names the remote file once

- **The fix:**
  - `downloadOneFile` (`internal/cli/get.go`) takes the subject. It returns `newRemoteOpError("downloading", subject, err)` instead of `download file: %w`, as `put` already does.
  - `get.go` passes the typed remote path; `get_shared.go` passes `cc.SharedTarget.Selector()`.
  - The driveops wrap of a failed fetch now says `receiving the file: %w` instead of `downloading: %w`, so the line does not say "downloading" twice.
- **What the user sees for a transport error:**
  - main: `Error: download file: downloading <local target>: stream reset`
  - acd7c659: `Error: download file: downloading: stream reset`
  - now: `Error: downloading "/Docs/notes.txt": receiving the file: stream reset`
- **Integrity mismatch:** the same pattern, now `Error: downloading "/Docs/notes.txt": the downloaded file doesn't match OneDrive's copy (received 5 B, OneDrive reports 99 B)`.
- **Side effect:** a not-found or access-denied answer during the download now names the path in the plain sentence (`/Docs/notes.txt was not found on OneDrive.`) instead of "the item".
- **Spec:** `drive-transfers.md` now states that the command names the file once, whatever the cause.

### S1: the sync console test is its own test again

The four lines spliced in (verbose, reset, the `busy.txt` retry, the "will retry" assertion) are back at the end of `TestSyncConsole_FailuresGoToStderrAndRetriesOnlyWhenVerbose`. That test is byte-identical to main. `TestSyncConsole_ALocalFileFailureNamesTheFileOnce` stands alone.

### S2: a vanished partial file no longer reads "<folder> does not exist."

`localFailureText` names the partial's folder only in the permission, read-only and too-large sentences. For `ErrNotExist` on an owned partial name it returns "", so the error itself is printed, naming the partial once.

### S3: the rule is pinned directly

New `TestFailures_KeepAnOSErrorThatNamesAnotherPath` checks the full text: `creating directory <file>/sub: mkdir <file>: not a directory`.

## Callers that relied on the transfer's or localpath's text to carry the path

The texts below were measured by tests or live, or built by reading where marked.

| Caller | main | acd7c659 | now |
|---|---|---|---|
| single get, not local (`get.go:83` → `downloadOneFile`) | `download file: downloading <T>: …` | `download file: …` (no path) | `downloading "<remote>": …` |
| single get, local failure (details) | `download file: creating partial file P: opening P: open A: …` | `download file: creating the partial file: opening P: …` | `downloading "<remote>": creating the partial file: opening P: …` |
| shared single get (`get_shared.go:41`) | as main above | as acd7c659 above | `downloading "<selector>": …` |
| single put (`put.go:117`) | `uploading "<remote>": uploading <P>: …` | `uploading "<remote>": sending the file: …` | unchanged from acd7c659 |
| shared put (`put_shared.go:52`) | the selector, as for put | as acd7c659 put | unchanged |
| folder get/put line and `--json` errors (`folder_transfer.go`) | `Failed: P: …` with P repeated | `Failed: <LocalPath>: <cause without paths>` | unchanged; a failed fetch reads `receiving the file: …` |
| folder put root (`put.go:210`) | `uploading "<remote>": reading upload root: opening P: open A: …` | `…: opening P: …` | unchanged |
| folder get root (`get.go:231`) | `listing "<remote>": list children: …` | same | unchanged |
| sync downloads and uploads (`executor_transfer.go:95,227,539,807`) | activity `Failed to … <rel>: <innermost>` | unchanged | unchanged (reading). The stale comment at `:809` ("driveops names the file") is corrected |
| sync warn log `Not syncing <rel>: <ErrMsg>` (`engine_runtime_completion.go:651`) | ErrMsg repeats the absolute path | path once, via `<rel>` | unchanged (reading) |

## Tests

"Failed on acd7c659" means the test was run against acd7c659's product code.

- **`TestDownloadOneFile_AFailureThatIsNotLocalNamesTheRemotePathOnce` (cli, new):** integrity and transport cases. It pins the exact text and that no local path appears.
  - On acd7c659 it does not build: `downloadOneFile` takes no subject there.
  - The review's probe recorded acd7c659's text for the same two cases.
- **`TestRunGet_AnUnwritableFileIsNamedOnce` (cli, updated):** the remote path appears once, the partial once, the absolute path never. Failed on acd7c659 by assertion.
- **`TestRunGet_ASharedFileThatFailsNamesTheSelectorOnce` (cli, new):** the selector appears once, the partial once. Failed on acd7c659 by assertion.
- **`TestCommandErrorText_APartialFileIsNamedByItsFolderOnlyWhereThatIsTrue` (cli, new):** permission, read-only, too large and missing. Failed on acd7c659 (`/home/u/Docs does not exist.`).
- **`TestTransferManager_DownloadError_NamesNoLocalPath` (driveops, updated):** expects `receiving the file: connection reset`. Failed on acd7c659.
- **Pins that pass on acd7c659 and on this commit:**
  - `TestFailures_KeepAnOSErrorThatNamesAnotherPath` (localpath, new). The review's mutant m5 would fail it.
  - `TestSyncConsole_ALocalFileFailureNamesTheFileOnce` (now standalone).
  - The restored `TestSyncConsole_FailuresGoToStderrAndRetriesOnlyWhenVerbose`.

## Checks

- **Build and vet:** `go build ./...`, `go vet ./...` and `go vet -tags=e2e,e2e_full,integration,stress ./...` pass.
- **Plain tests:**
  - localpath, driveops and cli pass.
  - Every localpath importer passes: perf, synctest, config, logfile, tokenfile, localtrash, synctree, fsroot, testutil, graph and multisync.
- **Sync tree:** plain `go test ./internal/sync/...` behind the lock passes (392 s). The first attempt waited past the 2-hour background limit behind another job holding the lock and was stopped; the rerun completed.
- **`-race` behind the lock:** driveops, cli and localpath pass.
- **Repo rules:** `RealRepoIsClean` gives 4 PASS.
- **Lint:** `devtool verify lint` reports `0 issues.` and actionlint passes.
- **Spec citations:** the 7 test names newly cited exist (checked by script).
- **Formatting:** gofumpt and goimports are clean.

## Live check (test account)

Without fault injection I could only provoke a local failure: a `get --verbose` of an existing test-drive file into a read-only scratch folder outside any synced tree. Nothing was written and nothing uploaded; the scratch folder is deleted.

- **Installed acd7c659:** `Details: download file: creating the partial file: opening <scratch>/ro/.onedrive-go.<name>~v….partial: permission denied`.
- **This commit:** `Details: downloading "/<file>": creating the partial file: opening <scratch>/ro/.onedrive-go.<name>~v….partial: permission denied`.
- **Both:** the plain line was `Error: Permission denied for <scratch>/ro. Check its file permissions.`

Integrity and transport failures were not run live; they need fault injection.

## Not fixed / unknown

1. A network failure's plain sentence (`can't reach OneDrive …`) names no path, as on main. The `--verbose` details now name the remote path. I left it as is.
2. Whether the remaining `drive reset-sync-state` wraps (`store_reset.go`) and the `localFailureText` absolute-path form need changes is unchanged from the first report. Both are listed there.
