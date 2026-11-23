# Report: a status line that cannot be written is recorded and read; one helper words every count

Branch `fix/cli-counts-and-dropped-writes`, one commit 9d27525a on base
`17564167` (origin/main, #942), not pushed. DoR: `W93-dor.md`.

## What changed (one line per file)

- `internal/cli/format.go`: `writeWarningf` returns its error; new `CLIContext.warnf` records a refused warning; `Progressf` and `ProgressDone` record theirs after releasing `statusMu` (`drawProgress`, `clearProgress`); `clearLine` constant; `countOf` (generic over int and int64, "retries" for a consonant-y noun) moves here; `bulkTransferError` takes the context.
- `internal/cli/root.go`: `mainWithWriters` reads the record (`statusStreamFailure`, one warn record) and applies `exitCodeAfterStatusStream`; `writeCommandError` returns its error; `rootCLIContext` replaces three copies of the root lookup; the logging warnings move from the builders to `warnAboutLogging` (builders deterministic, `consoleLogFormat` holds the format list, `buildLoggerDualWithStatusWriter` returns the open error); field comments.
- `drive_catalog.go`, `pause.go`, `auth_flow.go`, `rm.go`, `folder_transfer.go`: warnings through `cc.warnf`; `confirmRmParentVisibility` takes the context.
- `sync_startup_messages.go`, `sync_runtime.go`: start warnings return their error; the watch logs a refused one at warn.
- Count sites through `countOf`/`countItems`: `trash.go`, `drive_rebind_root.go`, `recycle_bin_flow.go` (2), `sync_notify.go` (3), `auth_flow.go` (minutes), `stat.go` (bytes), `status_perf.go`, `status_sync_state.go`, `auth_health.go`, `drive_add.go`, `sync_console.go` (activity parts, held back, held deletes), `status_render.go`, `status_render_sections.go`, `sync_render.go` (3); `sync_cli_format.go` loses `itemNoun`, `status_render.go` loses `formatStatusCount`.
- Tests: new `status_stream_test.go`, `count_wording_test.go`; edits in 17 test files; `e2e/sync_safety_full_e2e_test.go` asserts the new held-deletes text (4 places, not run).
- `spec/design/cli.md`: exit status 1 row and one paragraph (Exit Status); warnings clear progress; one sentence on `countOf` and its check.

## Tests added (each seen failing first; logs `scratchpad/w93/red1.log`, `red2.log`, `red3-restored-site.log`)

- `TestWarnings_AFailedWriteIsRecorded`, `TestUseLoggingConfig_AWarningItCannotWriteIsRecorded`: a refused warning is in the record (failed: record empty).
- `TestProgressf_RecordsAFailedWrite` (a pseudo-terminal whose writes fail; skips where the platform's is not a terminal), `TestProgressDone_RecordsAFailedWrite`: failed on an empty record.
- `TestMainWithWriters_AStatusStreamThatFailsAWriteFailsAWorkingCommand`: `resume` exits 1, one WARN record in the log file (failed: exit 0, no record). `..._AFailedCommandKeepsItsStatusWhenTheStatusStreamFails`: 78 stays (a guard, passed before).
- `TestRunSyncDaemonWithFactory_AStartWarningThatCannotBeWrittenIsLogged`: one WARN record (failed: none).
- `TestCLIStrings_EveryCountIsWordedByCountOf`: failed on 11 sites first, then on the one restored site `trash.go:258`; `TestPluralAfterNumberVerb_FindsTheShapeThatBypassesCountOf`.
- Count 1, whole line: trash empty, rebind-root (`TestConfirmRebindRootIntent_CountsOneFileInTheSingular` and the run test), recycle-bin empty and its failure, device-code minutes, stat bytes, perf HTTP line, retrying paths, held back, held deletes: each failed on today's "1 <plural>". Guards (passed before and after): state databases, the status summary line. `TestCountOf_OneIsSingular` moved to `format_test.go` and gained the "retries", "days" and int64 cases after `countOf` changed, so those were not seen failing.
- `TestUseLoggingConfig_WarnsAboutAnUnknownLogFormat`, `..._UnknownLogLevelOfAnOpenLogFile`, `..._ALogFileItCannotOpen`: the moved warnings keep their texts.

## Checks

- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test -count=1 ./internal/cli/...`: pass; with `-race` behind the lock: pass (24 s).
- `go test -run "RealRepoIsClean|TestStressRepeatedStepsDeclareEveryTestTheirRuleNames" ./internal/devtool/`: 4 RealRepoIsClean PASS and the stress test PASS.
- `devtool verify lint` behind the lock: 0 issues, actionlint pass (`W93-lint.log`). The first run found an unused assignment in a test and an unused nolint directive; both fixed, and the plain and race cli runs repeated on the final tree (pass).
- Cited tests in `cli.md` checked by hand. No live run (not required of this writer).
- Installed: origin/main was still `17564167` after a fetch, so `onedrive-go-install` ran (`--version` 9d27525a); agent `CLAUDE.md` (branch and hash only) and `CHANGELOG.md` updated.

## Deviations that change behaviour, scope or an interface

- The logging-config warnings are written by the command context (`useLoggingConfig`), not by the builders; `buildLoggerDualWithStatusWriter` returns the open error. Texts and order unchanged.
- The status-stream failure is logged at warn for a failed command too (its exit status is unchanged).
- Beyond the seven sites: 14 more count sites (DoR Part 2), `itemNoun` and `formatStatusCount` folded into `countOf`; files beyond those named: `drive_catalog.go`, `sync_runtime.go`, `stat.go`, `auth_health.go`, `drive_add.go`, `sync_console.go`, `status_perf.go`, `status_sync_state.go`, `status_render.go`, `status_render_sections.go`, `sync_cli_format.go`, `sync_render.go`, `e2e/sync_safety_full_e2e_test.go`.
- The held-deletes line's noun moved next to its count: "Held 60 deletes on OneDrive and 2 deletes on this computer for confirmation: ...".

## Also fixed

- `root.go`: the `WithGroup` comment sat on `withTimestamps`; both now carry their own.
- `root.go`: the clear-line escape spelled twice; one constant.
- `rm_test.go`: `context.Background()` became `t.Context()`; the note is asserted whole.
- `root_test.go`: `TestResolveFileLogLevel_UnknownLogLevel` wrote its warning to the real stderr; the builder no longer writes, and the test runs in parallel.

## Not fixed (seen)

- `internal/cli/sync_console.go:581` `syncConsole.write` drops every write error, stdout and stderr alike, so a sync summary or failure line stderr refused is not recorded. Fix: record the stderr half through the context like `warnf` (with the planned stdout change).
- `internal/sync/report_left_work.go:122` (and its `countNoun`): `internal/sync` words its own counts with a second helper. Fix: none here by decision (no new package); revisit if a shared wording package is ever wanted.

## Unfinished / for the reviewer

- Nothing unfinished. Another package's tests were not run: none depends on the changed cli functions; `e2e` compiles (tagged vet) and was not run.

## Fix round

Commit amended: `9d27525a` became `541026ca`, still one commit on `17564167`. Files changed in this round: `internal/cli/{sync_runtime.go, sync_startup_messages.go, format.go, root.go, count_wording_test.go, status_stream_test.go}`, `spec/design/cli.md`, and two files new to the commit, `docs/scripting.md` and `docs/safety.md`.

1. Watch start warning (should-fix 1). `runSyncDaemonWithFactory` takes `cc := cliContextFrom(ctx)`; the `StartWarning` hook records a refused write with `cc.recordStatusError` and logs `startWarningUnwrittenMessage` only when `cc` is nil. I kept `writeWatchStartWarnings(statusWriter, ...)` and recorded its joined error, instead of rewriting it around `cc.warnf`. That was the smaller change: in product, `statusWriter` is `cc.Status()`. New test `TestRunSyncDaemonWithFactory_AStartWarningThatCannotBeWrittenIsRecorded` asserts `cc.StatusError()`. It failed before the change ("An error is expected but got nil") and passes after. The old log test is now `..._IsLoggedWithoutACommandContext`, and both share the helper `runWatchThatWarnsAtStart`. The four texts that said "no command context" were reworded (format.go, cli.md, the test comment, the commit message).
2. Main's last-line plumbing (should-fix 2). I removed the dead plumbing rather than make it report, because nothing exists for it to report to: the exit status is already non-zero and the log file is closed. `lastLinesErr` is gone. The five writes in `mainWithWriters` and `reportMainPanic` are discarded with `_ = ... //nolint:errcheck`; the repo's errcheck has `check-blank: true`. The reason is in each function's doc comment. `reportMainPanic` returns `exitFailure` directly. The `exitCodeAfterStatusStream` comment, the cli.md sentence and the commit message are corrected. There is no test, because behaviour does not change.
3. Count check (should-fix 3). The regex became `numberVerbAndWords` (argument index, one optional second word) plus `pluralAfterNumberVerb(s)`. It skips a match whose first word is in `notCountNoun` (always, as, does, has, is, its, plus, this, was). It flags the match when the first word, or the second word, is plural-shaped and is not in that list. This differs from the suggested first-or-last skip: with that skip, "%d files as JSON" would pass. The table test gained 6 refused cases (compound nouns, `%[1]d`, "files as") and 7 allowed ones ("%d is the limit", "%v was renamed", "%d on this computer", ...). I saw 10 of them fail on the old regex. The list is a regex, not a map: a map failed gochecknoglobals, and a switch with the literal "was" failed goconst (sync_render.go has "was"). A scratch scan of the base tree (`scratchpad/w93fix/scan`) finds the 11 old sites plus `auth_health.go:305`, `drive_add.go:440` and the second count of `status_perf.go:255`. Nothing is flagged on the commit's tree.
4. Claims wider than the code (should-fix 4). The cli.md paragraph and the commit message now name the two kinds of stderr line that are not recorded: the lines `sync` writes through `syncConsole` (a failed or retried file action, a drive's error, held deletes), and main's own last lines. Left for a later change: recording `syncConsole.write`'s stderr half through `cc` (sync_console.go:578-587); the code is unchanged here.
5. Exit-code tables (should-fix 5). Row 0 of cli.md and of docs/scripting.md now adds "unless stderr refused a write", and cli.md's "Chosen by" cell reads "`err == nil` and no refused stderr write recorded". Row 1 of docs/scripting.md adds "or a command that worked but could not write to stderr".
6. History comment (should-fix 6). The comment now says what the check cannot see ("%d %s", and a number passed as a string, "%s files"), then "such a site words its count with countOf itself".

Notes fixed: docs/safety.md now quotes the held-deletes line as "Held N deletes on OneDrive for confirmation" (or "on this computer"). The commit message now names the `status --json` retrying_paths text among the reworded counts. Notes left: 8 and 9 asked for nothing. Note 7's inventory needed no other change.

Behaviour that differs from 9d27525a:
- A `sync --watch` whose warning that a drive did not start was refused by stderr now ends with exit 1 on a clean stop (it was 0), with one "could not write to stderr" warn record. With a command context it no longer writes the immediate "could not write a drive's startup warning" warn record.
- No other output line or exit code changes. The main-line change and the count-check change touch no product output.

Checks, on the final tree:
- `go build ./...` passed.
- `go vet ./...` passed, and so did both lint vets: `-tags="e2e e2e_full" ./e2e` and `-tags=integration ./internal/graph`.
- `go test -count=1 ./internal/cli/...` passed.
- `-race` under the lock passed (17.9 s).
- devtool `RealRepoIsClean` (4 PASS) and the stress-declaration tests (`TestStressRepeatedStepsDeclareEveryTestTheirRuleNames`, `TestStressDeclarationsAreInNameOrder`) passed.
- `verify lint` passed on the third run. The first two failed: gochecknoglobals on the map, then goconst on "was". Both are fixed as described in item 3.
- Nothing tagged e2e was run, and no live run.

Install: after `git fetch origin main`, origin/main is still `17564167`, an ancestor of HEAD. I installed `541026ca` (`onedrive-go --version` prints 541026ca), updated CLAUDE.md and CHANGELOG.md, then committed and pushed them (`31f8293`).


## Shipped

- Rebased onto main `238bea94` with no conflict: main renamed no test in `e2e/sync_safety_full_e2e_test.go`. Build, `go vet` (plain and all tags), the devtool e2e placement tests (every full-tier test placed by its name exactly once), the source-rule tests (4 RealRepoIsClean PASS) and the stress-declaration tests pass. No stress declaration is needed: the commit adds no test in `internal/sync` or `internal/multisync`.
- Live proof on the test account, in a private tree and a proof folder removed afterwards: one file put, then `get` of it with stderr on `/dev/full`. A build of main: exit 0, the file downloaded, no warn record in the log file. A build of this commit: exit 1, the file downloaded, one warn record, "could not write to stderr". With stderr captured both exit 0.
- Full-tier e2e tests whose assertions read a reworded line, each run alone behind the lock: the two held-deletes tests passed. The edit-edit directional test failed on its upload-only case: the held-back line read "1 download" and the test looked for "downloads". Three directional assertions and the durable-mirror test looked for the plural of a count of one ("downloads", "uploads", "local deletes"). The commit was amended (`56b14157`) so they look for "1 download", "1 upload" and "1 local delete"; then the edit-edit, edit-delete, create-create and durable-mirror tests passed.
- `internal/cli` plain and under the race detector pass on the final tree.
- Start stage passed twice (before and after the amend): the latest nightly passed, nothing claimed. Pre-pr gate passed (lint 0 issues, then the quick tier).
- PR #944. Pre-merge passed (CI green, no threads). Squash-merged as `6421bf98`. Post-merge passed; the worktree is removed. Installed from main: `onedrive-go --version` prints 6421bf98.
- Not covered, planned as its own change: a refused stderr line of the sync console and of main's own last lines.
