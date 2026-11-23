# Definition of Ready: a status line that cannot be written is recorded and read; one helper words every count

Worktree `~/src/onedrive-go-wt/cli-counts-and-dropped-writes`, branch
`fix/cli-counts-and-dropped-writes`, base `origin/main` at `17564167` (#942),
written 2026-11-17T05:32-08:00 before any edit.

## Part 1: the writers and the record (facts read on the base)

Writers in `internal/cli/format.go`: `writeWarningf` (31) returns on a failed
write and drops the error; `Statusf` (57) records through `recordStatusError`
(68), which takes `statusMu`; `StatusError` (81) has one caller, a test
(`format_test.go:120`), so no product code reads `statusErr`; `Progressf` (294)
and `ProgressDone` (310) drop the write error while holding `statusMu`.

Every caller of `writeWarningf` (found by search; `drive_search.go` no longer
calls it, `drive_catalog.go` does since #942):

| Site | Command context at hand | What it says |
| --- | --- | --- |
| `drive_catalog.go:486` `printSharePointGapNotes` | yes (`cc`) | "Note: this listing is incomplete." |
| `drive_catalog.go:495` `printSharePointSourceNotes` | yes | "Note: could not search the SharePoint sites of A: R." |
| `drive_catalog.go:498` same | yes | "Note: could not list the libraries of SharePoint site S of A: E." |
| `format.go:263` `bulkTransferError(w, ...)` | in its caller (`folder_transfer.go:60`, `r.cc.Status()`; `:56` passes nil) | "Failed: <item>: <error>" per failure |
| `pause.go:106` `reportDaemonReloadError` | yes | "Warning: <the running sync's reload error>" |
| `auth_flow.go:193` browser login callback | yes (closure over `cc`) | "Open this URL in your browser: <url>" |
| `auth_flow.go:206` device-code callback | yes | the device sign-in instructions |
| `rm.go:124` `confirmRmParentVisibility(..., statusWriter)` | in its caller (`rm.go:93`, `cc.Status()`) | "Note: deleted P; OneDrive may take a moment to show the change in /Q." |
| `sync_startup_messages.go:149` `writeWatchStartWarnings` | no: called from the orchestrator's `StartWarning` hook built in `runSyncDaemonWithFactory`, which holds a status writer, not the context | "Warning: D did not start: M" |
| `root.go:969` `resolveFileLogLevelWithStatusWriter` | no (free function), but its only product chain is `cc.useLoggingConfig` -> `buildLoggerDualWithStatusWriter` -> `buildFileLogger` | "Warning: unknown log_level ..." |
| `root.go:998` `buildHandlerWithStatusWriter` | no; same chain (the bootstrap logger passes a nil config and cannot warn) | "Warning: unknown log_format ..." |
| `root.go:1183` `buildFileLogger` | no; same chain | "Warning: cannot open log file ..." |
| `root.go:697` `writeCommandError` | no: the command has returned and its log file is closed | main's final error |
| `root.go:785` `mainWithWriters`, `root.go:833` `reportMainPanic` | no: after the log file closed | "Warning: could not close the log file: E" |

What a recorded failure does to each command's exit status today: nothing,
for every command (no reader). `mainWithWriters` (754) decides the status at
`commandExitCode(err, ctx.Err() != nil)` (789) after `closeRootCommandLogger`
(780). The command context is reached after the command returned through the
root command's context, as `closeRootCommandLogger` (651) does:
`initializeCLIContext` stores `cc` under `cliContextKey` and sets that context
on the executed command and on the root (536-541). When `PersistentPreRunE`
never built it (usage errors, `--help`, cobra's own commands) there is none.

A failed write on Linux: a standard error on a full device (`/dev/full`)
returns `ENOSPC` ("no space left on device"); a hung-up terminal returns
`EIO`. A standard error closed before start is never seen closed: the Go
runtime's `checkfds` (`runtime/fds_unix.go`) reopens descriptors 0 to 2 on
`/dev/null`, so writes succeed. `os/signal` documents SIGPIPE: a program that
has not called `Notify` for SIGPIPE and writes to a broken pipe on descriptor 1
or 2 exits with SIGPIPE; only with `Notify` does the write return `EPIPE`.
`internal/cli` notifies only SIGINT and SIGTERM (`signal.go:35`), so a broken
pipe on standard error ends the process before any error is seen (stated from
the documentation, not tested against descriptor 2).

Tests that build a context with a failing status writer: one,
`TestStatusf_RecordsWriterError` (`format_test.go:111`, `errWriter`).
`flakyWriter` (`sync_console_test.go:494`) is the sync console's and the JSON
writer's; no test runs `mainWithWriters` with a failing standard error.

## Part 2: counts (search of non-test `internal/cli`, a number verb, a space, a lower-case word ending in s)

| Hit | Count of things | Reads "1 <plural>" today |
| --- | --- | --- |
| `trash.go:258` "Permanently deleted %d files" | yes | yes |
| `drive_rebind_root.go:125` "remembers %s files; %s holds %d files now" | yes (two counts) | yes |
| `recycle_bin_flow.go:123` "Permanently deleting %d items..." | yes | yes |
| `recycle_bin_flow.go:135` "%d of %d items failed to delete" | yes | yes ("1 of 1 items") |
| `sync_notify.go:140` "Watching %d drives" | yes | no: reached only above 3 roots |
| `sync_notify.go:166`, `:172` | yes | no: a hand-written case for 1 |
| `auth_flow.go:335` "(in %d minutes)" | a time unit | yes when one minute is left |
| `stat.go:116` "(%d bytes)" | a size unit | yes for a one-byte file |
| `status_perf.go:255` "%d req, %d retries, %d transport errors" | yes ("req" is an abbreviation) | yes |
| `status_sync_state.go:128` "(%s, %d attempts)" | yes | yes |

The wider search (a word between the verb and the noun, `%d %s` with a noun
argument, other plural helpers) adds: `auth_health.go:305` "%d state
databases" (a hand-written singular case), `drive_add.go:440` "%d shared
folders match" (always two or more), `sync_console.go:437` `heldBackSummary`
("%d %s" with "uploads", "downloads", "deletes on OneDrive", "local deletes",
"moves", "folder creates": reads "1 uploads"), `sync_console.go:416-422`
`heldDeletesSummary` ("Held 1 on OneDrive deletes"), `sync_console.go:368`
`activityTally.parts` (its own singular by `TrimSuffix`), and two more count
helpers: `itemNoun` (`sync_cli_format.go:19`, used by `status_render.go:471`
and `status_render_sections.go:53`) and `formatStatusCount`
(`status_render.go:523`, used there and by `sync_render.go:54`, `:153`,
`:159`). Total count sites: 27 counted one per count, 21 one per format; not
more than about 25 format sites, so no stop. Units: none stays an exception,
both are worded by the helper ("1 minute", "1 byte"). "retry" needs the plural
"retries", so the helper turns a final consonant-y into "ies".

What the tests expect today: `trash_test.go:75` asserts "Permanently deleted 1
files" and `drive_rebind_root_test.go:60` "holds 1 files now" (both assert the
fault); `:161` "holds 2 files now" (right); `sync_notify_test.go` asserts whole
`STATUS=` lines, "Watching 4 drives", "1 drive could not start", "2 drives
could not start", "1 drive paused" (right); `status_perf_test.go:168` asserts
"4 req, 1 retries, 2 transport errors" (the fault); `sync_delete_safety_test.go:33`
asserts the part "60 on OneDrive".

`e2e/` search for each old text: only `e2e/sync_safety_full_e2e_test.go:86`,
`:110`, `:122` (a "does not contain"), `:215` hold "deletes for confirmation",
which the held-deletes line changes; they change to the line's new fixed part.
"Held back by --upload-only:" (`sync_full_test.go`, `sync_recovery_e2e_test.go`)
and "Size: 21 B (21 bytes)" (`remote_read_rules_test.go:398`) are unchanged.
Nothing tagged is run.

The fifty-times stress rule (`internal/devtool/verify_stress_members.go`)
declares tests of `./internal/sync/...` only (`verify_stress.go:21`, `:24`);
no cli test needs a declaration.

## The 12 items

1. PASS **Task intent**: a status, warning or progress line the CLI cannot write is recorded, and `mainWithWriters` reads the record after the command returned: a command that worked ends with exit 1 and the failure goes once to the command logger at warn level; a command that failed keeps its own message and status. Every count the CLI prints is worded by `countOf`, so no line reads "1 files", held by a source check.
2. PASS **Non-goals**: primary-output (stdout) write failures, including `syncConsole.write`'s; the JSON writer; the structure of the sync summary and of the status model (only count wording there); `internal/sync` and `internal/multisync` (their own counts, e.g. `report_left_work.go:122`, stay); wording of any line beyond its count; a retry or a second stream.
3. PASS **Governing docs**: `CLAUDE.md` (repository) lines 1-369; `spec/design/cli.md` (Exit Status 146-240, Console Output 283-, progress 628-638); `spec/requirements/non-functional.md` R-6.6.22, R-6.6.28, R-6.6.29, R-6.6.31; `spec/reference/cli-output-guidelines.md`; `go doc os/signal`.
4. PASS **Authority owner**: `CLIContext` owns the status stream's record (`statusErr` under `statusMu`); `mainWithWriters` owns the exit status and is the record's one reader; `countOf` (moving to `format.go`) owns count wording; the command logger owns the one warn record.
5. PASS **Code scope**: `internal/cli` product files `format.go`, `root.go`, `folder_transfer.go`, `drive_catalog.go`, `pause.go`, `auth_flow.go`, `rm.go`, `sync_startup_messages.go`, `sync_runtime.go`, the count sites above (`trash.go`, `drive_rebind_root.go`, `recycle_bin_flow.go`, `sync_notify.go`, `stat.go`, `status_perf.go`, `status_sync_state.go`, `auth_health.go`, `drive_add.go`, `sync_console.go`, `status_render.go`, `status_render_sections.go`, `sync_cli_format.go`, `sync_render.go`), their tests, a new source-check test file, `e2e/sync_safety_full_e2e_test.go`, `spec/design/cli.md`. Not touched: `internal/sync`, `internal/multisync`, the JSON writer, stdout writes.
6. PASS **Regression plan**: tests first, each seen failing: the warning method, `Progressf` (through a pseudo-terminal, since it draws only on a terminal) and `ProgressDone` with a failing writer, each proving the record holds the failure; `mainWithWriters` with a failing status writer: a command that worked exits 1 and logs one warn record, a failed command keeps its status; one test per count site with the count 1, asserting the whole line; the source check, seen failing on one restored site.
7. PASS **Verification plan**: `go build ./...`; `go vet ./...`; `go vet -tags=e2e,e2e_full,integration,stress ./...`; `go test -count=1 ./internal/cli/...` and with `-race` behind the heavy lock; `go test -run RealRepoIsClean ./internal/devtool/`; the stress-declaration test; `devtool verify lint` once, last, behind the lock. No full verify, no live run (the live proof is not this writer's).
8. PASS **Data safety**: output and exit status only; no file, store or remote operation changes.
9. PASS **Secret and credential safety**: no token, `.env` or account name is read or written; tests use fakes.
10. PASS **Live provider and destructive operations**: none; no live run, no history rewrite, no dependency change.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment. (Corrected by the launch message: the start stage has not run here and is the shipping step's.)
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does a pseudo-terminal master count as a terminal for `isatty` on macOS? Current assumption: yes on Linux and macOS. Validation step: the Progressf test runs on Linux here; it skips, saying why, where the master is not a terminal. Owner path affected if wrong: `format_test.go`.
    - Question: is the log-config warning better written by the command context than by the logger builders? Current assumption: yes; the builders become deterministic and `useLoggingConfig` (a context method) writes the three warnings with the recording method. Validation step: the existing warning tests move to `useLoggingConfig` and keep their texts. Owner path affected if wrong: `root.go`.
    - Question: does the watch's start warning, delivered by the orchestrator outside the context, need the record? Current assumption: no; its write error goes to the watch's logger at warn, by name. Validation step: a test with a failing writer. Owner path affected if wrong: `sync_runtime.go`.
