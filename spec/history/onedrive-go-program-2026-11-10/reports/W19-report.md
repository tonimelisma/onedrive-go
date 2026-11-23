# W19 report: `status` answers within a bound when OneDrive is slow

Commit `404a9909` on `fix/status-live-budget` (the writer handed back `0af5966e`; review edits amended into it), PR #891.

## What changed
- `internal/cli/status_live.go`: `statusLiveBudget` (10 s) + `errStatusLiveBudgetSpent` cause; clients built on the command goroutine (command ctx, for the token source), every account asked in a `WaitGroup.Go` worker on one `context.WithTimeoutCause`, all awaited; `statusLiveUnansweredReason`; `printStatusLiveNotes`; chain at INFO.
- `status_snapshot.go`: notes printed after the overlay; `runStatusCommand(ctx, cc, showPerf bool)`; `history` gone; four test-only helpers and `accountNameReader` deleted. `root.go`: seam `cliSeams.statusLiveBudget`.
- `status_runtime.go`: no runtime note when the command's own context has ended.
- `status_render.go`, `status_render_sections.go`, `account_view_snapshot.go` (`statusAccounts` only), `status.go`: `history` dropped; `printMountSyncSections`, `printConditionSection` deleted. `auth_health.go`: unused `accountAuthChecker` deleted.
- Tests: new `status_live_test.go`; `status_runtime_test.go`, `status_test.go`, `status_conditions_test.go`, `status_golden_test.go`, `status_perf_test.go`, `status_live_test.go` updated.
- Specs: R-6.8.28 (new), R-2.9.3 (a stopped status says nothing about the running sync); `cli.md` (Implements, two Verified By rows, status bullet); test renames in `sync-store.md`, `sync-shortcut.md`, `sync-control-plane.md`.

## Tests added
- `TestStatus_ASignInFailureStillMarksTheAccount` (pin), `..._PrintsLocalStateWhenOneDriveDoesNotAnswerInTheBudget`, `..._AsksAccountsAtTheSameTime`, `..._StorageCutShortByTheBudgetIsNoted`, `..._SaysWhyOneDriveGaveNoDetails`, `..._AStoppedCommandAddsNoNote` (now: stderr empty as a whole), as in the first pass.
- `TestRunStatusCommand_AStoppedStatusSaysNothingAboutTheRunningSync`: busy control socket, stopped command: local state, empty stderr. Failed before the fix with the "context canceled" note.

## Where the tests of the deleted helpers went (none dropped)
- `buildStatusAccountsWith` (11 tests) -> `TestBuildStatusAccountsFromViews_*`, same assertions. Names, auth and sync state now come from hand-built views (`statusAccountView`: NeverSynced, EmptySyncDir, PersonalDriveUsesAccountFacingName, SyncStatePopulated, NilSyncState). Grouping, names and sign-in now come from `buildAccountViews` over a seeded catalog and token files (`statusAccountsThroughViews`: SingleAccount, MultiAccountGrouping, AuthenticationRequiredStates with a real missing and a real unreadable token file, SharePointGrouping, PausedOverridesNoToken, which also asserts missing_login now, EmptyConfig).
- `buildSingleAccountStatusWith` (1) -> `TestBuildStatusAccountsFromViews_NestsChildMountsUnderParent` (children through the `shortcutRoots` map).
- `buildChildStatusMount` (5) -> `buildChildStatusDrive(..., nil)`; tests renamed `TestBuildChildStatusDrive_*`, design-doc citations updated.
- `querySyncState` (7) -> `queryLiveSyncState`, a test helper over `liveSyncStateQuerier.QuerySyncState`, the querier status uses.
- `printMountSyncSections`/`printConditionSection` (4) -> `printDriveSyncSections`/`printIssueSection`; two tests renamed `TestPrintIssueSection_*`.

## Checks
- `go build ./...`, `go vet ./...`: pass, after the last edit.
- Whole `go test -count=1 ./internal/cli/...`: pass, after the last edit.
- `-race -run` on every touched test (status, runtime, rewritten builder/child/querier/render tests): pass.
- golangci-lint `./internal/cli/...`: `0 issues.` (`W19-lint.log`). It ran twice: the first run found 2 issues (a goconst `"Alice"`; unparam, the view helper's email was always the same), both fixed. No other package got lint, since no formatter moved.
- The gate was waited out once (35 s); no package-wide `-race`.
- Live (first pass, unchanged by this pass): branch binary on the test account, private tree, `timeout 40`, degraded Graph: exit 0 in 10.0 s, one JSON document, the note; installed binary killed at 40 s.

## Commands bounded or left
- Bounded: `status` overlay, which never reaches `probeAccountIdentity` (`runStatusCommand` -> `loadAccountViewSnapshot`).
- Keep the retry ladder (the live request is their answer): `drive list`, `drive search`, `shared`, `drive add`, `login`, file commands, `recycle-bin`. Identity probe through `loadAccountViewSnapshotWithBestEffortIdentityRefresh` (account_view_snapshot.go:86) or root.go `Session`: owned by a later change. Control socket only: `status --perf`, `perf`, `pause`, `resume`.

## Deviations
- Storage failures other than the budget's stay DEBUG with no note. A token refresh is not bounded by the budget.
- `account_view_snapshot.go` changed, but only `statusAccounts` (the `history` parameter); the probe and its callers are untouched.
- Entry 6 (one duration formatter) is not done, see Not fixed 3.

## Also fixed
- Stopped `status` printed the runtime note "could not read the running sync's live state (... context canceled)" (status_runtime.go:40).
- Dead `history` parameter and variadic `showPerf` (status_snapshot.go:183 and its chain).
- Test-only production helpers `buildStatusAccountsWith`, `buildSingleAccountStatusWith`, `buildChildStatusMount`, `querySyncState`, plus `printMountSyncSections`, `printConditionSection` (dead `nextIndent`), `accountNameReader`, `accountAuthChecker`.
- The old builder tests checked drive state against the real data directory (no test home); they now set one. A stale comment ("drive is still ready" over a not_synced assertion) is corrected.
- From the first pass: the overlay's dead nil check and its duplicated 401 branches; its Warn with no action.

## Not fixed
1. Planned separately: `internal/graph/client.go:68`, `TokenSource.Token()` has no context, so a refresh can outlast a caller's budget by up to 60 s.
2. Planned separately: `internal/retry/transport.go:303`, a backoff that ends after the deadline is still announced and slept.
3. `internal/cli/sync_console.go:586` vs `internal/retry/notices.go:79`: the two formatters differ in more than precision, so they cannot become one without changing text. Elapsed: tenths below a minute, seconds truncated above one, "2 min 0 s" kept, hours "1 h 5 min". Wait: whole seconds rounded, floor 1 s, "2 min" with no "0 s", no hours ("75 min"). A precision switch that keeps both outputs is two formatters in one body. Shared rules, with precision only below a minute, change three cases: an exact-minute elapsed time ("2 min 0 s" -> "2 min"), seconds rounded instead of truncated above a minute, and waits of an hour or more ("75 min" -> "1 h 15 min"). Needs a decision to accept those changes; then it is one function in `retry` with a table test.

## Review (2026-11-12)
Accepted. Both passes read in full; no defect in the product change.
- Fixed in review, in the same commit: three more helpers only tests called (moved into the test files or deleted), one function with no caller at all (`printStatusNextLine`, with its one test), and the registry row of the retired backlog ID `B-036`, whose last citing comment the second pass had removed. The first pre-pr gate caught the registry row; a targeted lint does not run that check.
- Not fixed 1 and 2: planned as W24 (a request deadline reaches the token refresh and the retry wait).
- Not fixed 3 (the formatters): decided. One package renders durations and sizes, with the precision as an argument; the three text changes are accepted. Planned as W26. W19 changes no duration text.
- Seen in review: the status model carries each drive under two sets of fields with a copy step between them. Planned as W25.
- Seen in review: a sweep of the tree found 39 unexported names that only test files mention. Planned as W27, with a gate so they cannot come back.
- Gate: pre-pr passed on the second run (3m55s). PR CI: its `e2e` and `integration` jobs fail on the Graph `/me` incident and are rerun when it ends.
