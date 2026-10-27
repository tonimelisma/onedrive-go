# CLI

GOVERNS: main.go, internal/cli/*.go, internal/logfile/logfile.go, internal/sdnotify/sdnotify.go

Implements: R-1 [implemented], R-2.3.3 [verified], R-6.3.6 [verified], R-2.5.5 [verified], R-2.5.6 [verified], R-2.8.3 [verified], R-2.9 [verified], R-2.10.4 [designed], R-2.10.47 [verified], R-6.6.8 [verified], R-6.6.11 [verified], R-6.6.17 [verified], R-6.6.18 [verified], R-6.6.19 [verified], R-6.6.20 [verified], R-6.6.21 [verified], R-6.6.22 [verified], R-6.6.23 [verified], R-6.6.24 [verified], R-6.6.25 [verified], R-6.6.26 [verified], R-6.6.27 [verified], R-6.6.28 [verified], R-2.6.5 [verified], R-6.6.29 [verified], R-6.6.31 [verified], R-6.6.32 [verified], R-6.6.33 [verified], R-6.8.16 [verified]

## Overview

`main.go` is a thin entrypoint. `internal/cli` owns:

- Cobra command wiring
- CLI bootstrap and shared flags
- user-facing rendering
- control-socket client logic
- dependency composition for Graph-facing command runtimes

The CLI is a presentation and composition boundary, not a second sync policy
engine.

Durable account and drive inventory mutation is not CLI-owned. Commands may
discover account/drive facts, delete token files, and invoke config entrypoints
that edit `config.toml`, but `internal/config` owns how `catalog.json` records
are inserted, updated, pruned, and validated.

## Ownership Contract

- Owns: command grammar, output shaping, reason/action copy, control-socket client routing, and command-scoped runtime assembly
- Does Not Own: Graph wire semantics, sync planning/execution policy, SQLite schema rules, or durable catalog lifecycle mutation rules
- Source of Truth: parsed args/flags plus lower-layer domain results
- Allowed Side Effects: stdout/stderr output, log-file setup, control-socket RPCs, and ordinary command-layer calls into lower packages
- Mutable Runtime Owner: `CLIContext` owns command-scoped mutable state for one invocation, including logger replacement, status writer error state, and test seams. Long-lived daemon runtime state remains owned by `internal/multisync` and `internal/sync`.
- Error Boundary: CLI commands translate lower-layer failures into user-facing guidance and command exit errors, but transport, store, and engine packages remain responsible for their own domain error semantics.

## Verified By

| Behavior | Evidence |
| --- | --- |
| Status JSON identifies each mount with a stable opaque `mount_ref` and still withholds canonical, mount, namespace, and remote drive IDs. | `TestStatusJSON_MountRefIsOpaqueAndDoesNotLeakIdentifiers`, `TestStatusJSON_MountRefDistinguishesSiblingChildren`, `TestMountRef_MatchesStateFileDigest`, `TestMountRef_IsStableUniqueAndOpaque`, `TestMountRef_EmptyForBlankMountID`, `TestStatusOutputGoldenJSON` |
| `status` stays read-only and remains the only sync-health command surface. | `TestStatusOutputGoldenText`, `TestStatusOutputGoldenJSON`, `TestQuerySyncState_UsesReadOnlyStatusSnapshotHelper`, `TestStatusCommand_JSONSurfacesSyncAuthRejectedOffline`, `TestStatusCommand_UnreadableStateStoreFallsBackToEmptySyncState`, `TestStatusCommand_NoAccountsDoesNotMutateManagedState`, `TestFilterStatusSnapshot_IntersectsAccountAndDriveSelectors`, `TestBuildChildStatusMount_FinalDrainGuidesAccessRestore`, `TestPrintMountStatus_RendersGuidedShortcutRecovery`, `TestStatusRuntime_SummaryJSONWithActiveOwner`, `TestStatusRuntimeOverlayApplyAndSummary`, `TestPrintMountStatus_ShowsRuntimeState`, `TestE2E_Status_NoLegacyHistorySurface`, `TestE2E_RoundTrip` |
| `trash list/restore/empty` read and change one configured drive's local trash offline; restore never overwrites and empty needs `--confirm`. | `TestTrashCommands_ListRestoreAndEmpty`, `TestTrashRestore_NeverOverwrites` |
| `drive rebind-root` shows the remembered and present file counts, needs `--yes` or a typed REBIND, clears only the recorded sync-folder fingerprint, keeps the rest of the state, needs `--drive`, and refuses while a sync owner holds the drive. | `TestRunDriveRebindRoot_RequiresDrive`, `TestRunDriveRebindRoot_KeepsStateAndReportsRebind`, `TestRunDriveRebindRoot_RequiresConfirmationWithoutYes`, `TestRunDriveRebindRoot_RefusesLiveSyncOwner` |
| `drive reset-sync-state` remains the only destructive sync-state recreate surface and requires explicit drive selection plus confirmation. | `TestNewDriveResetSyncStateCmd_HasYesFlag`, `TestRunDriveResetSyncStateWithInput_RequiresDrive`, `TestRunDriveResetSyncStateWithInput_RequiresInteractiveConfirmationWithoutYes`, `TestRunDriveResetSyncStateWithInput_ResetsAndRecreatesStateDB`, `TestRunDriveResetSyncStateWithInput_RefusesLiveSyncOwner` |
| `pause` and `resume` remain CLI-owned config mutations rather than direct sync-store writes. | `TestPauseCommand_PersistsTimedPause`, `TestResumeCommand_ClearsPausedKeys`, `TestClearPausedKeys_RemovesBothKeys` |
| Watch and one-shot sync command wiring stays inside the CLI composition boundary and delegates runtime ownership to the sync daemon/orchestrator seam. | `TestDryRunFlagSurfaceOnlySyncCommand`, `TestRunSyncCommand_UsesConfigDryRunWhenFlagUnset`, `TestRunSyncCommand_DryRunOpensLogFileAndWarnsOnFailure`, `TestRunSyncCommand_DryRunFailsWhenControlSocketPathCannotBeDerived`, `TestRunSyncCommand_WatchRejectsEffectiveDryRun`, `TestRunSyncCommand_PassesMissingSyncDirToRunOnce`, `TestRunSyncCommand_DryRunPassesMissingSyncDirWithoutCreatingIt`, `TestRunSyncCommand_PassesPausedInvalidDriveToRunnerAsPaused`, `TestRunSyncWatch_UsesInjectedRunner`, `TestRunSyncDaemonWithFactory_CallsOrchestrator`, `TestSyncConsole_MultiDriveNamesEachDrive` |
| `sync` names every finished file, summarizes in human units, prints errors once, and renders console logs as human or journal text. | `TestSyncConsole_NamesEveryCompletedFile`, `TestSyncConsole_FailuresGoToStderrAndRetriesOnlyWhenVerbose`, `TestSyncConsole_SummaryInHumanUnits`, `TestSyncConsole_SingleDriveErrorIsLeftToTheReturnedError`, `TestSyncConsole_WatchLinesAreTimestampedAndAnnounceStartAndStop`, `TestConsoleLogHandler_JournalLinesCarryPriorityAndNoTimestamp`, `TestCommandLogger_PerfSummaryInLogFileNotDefaultConsole`, `TestCommandLogger_SyncSchemaFieldsOnlyInLogFileAndVerboseConsole`, `TestMainWithWriters_SkipConfigCommandLogsPerfSummaryToLogFile`, `TestMainWithWriters_BootstrapRecordsFollowTheCommandLogger`, `TestCommandErrorText_NetworkFailureIsOnePlainLine`, `TestUnauthorizedAuthReason_DistinguishesExpiredLogin` |
| Shortcut child lifecycle status is formatted from sync-owned `ShortcutRootStatusView` values, and the CLI supplies the managed data directory to multisync rather than letting the control plane derive ambient paths. | `TestBuildChildStatusMount_RendersLifecycleState`, `TestBuildChildStatusMount_SurfacesProtectedPaths`, `TestRunSyncDaemonWithFactory_CallsOrchestrator` |
| Command failures map into the shared error classes, and the exit status is chosen from typed errors: 77 sign-in required, 78 invalid config, 2 usage, 130 user stop, 1 everything else. | `TestClassifyCommandError`, `TestCommandExitCode`, `TestCommandExitCode_EveryFailureClassHasAStatus`, `TestMainWithWriters_ConfigErrorExits78`, `TestMainWithWriters_SignInRequiredExits77`, `TestFormatWatchStartupError_ExitStatusFollowsSharedCause`, `TestConfigFailuresAreTaggedInvalidConfig` |
| `sync --watch` reports READY/STATUS/RELOADING/STOPPING/WATCHDOG to systemd through `internal/sdnotify`, and nothing outside systemd. | `TestRunSyncDaemon_NotifiesSystemdOfLifecycle`, `TestRunSyncDaemon_SignInFailureSetsStatusAndExitCode`, `TestRunSyncDaemon_NoDrivesIsAConfigFailure`, `TestSyncNotifierWatchingStatus`, `TestOpenSyncNotifier_BadSocketWarnsAndContinues`, `TestNotifier_SendsProtocolMessages`, `TestOpen_AbstractSocket`, `TestOpen_WatchdogInterval`, `TestRunWatch_ReportsLifecycleTransitions` |
| User-facing output agrees across commands: `drive list` and `status` derive a configured drive's state through one function (`configuredDriveState`); `stat` prints local times like `ls`; `ls` on a file lists that file; every transport failure, before or while reading the response, and every sign-in outcome is one plain line; health checks do not log a token load. | `TestBuildConfiguredDriveEntries_StateMatchesStatus`, `TestFormatExactTime_IsLocalWithZone`, `TestLs_OnAFileListsThatFile`, `TestCommandErrorText_EveryTransportFailureIsPlain`, `TestCommandErrorText_SignInOutcomesSayRunLoginAgain`, `TestInspectSavedLogin_DoesNotLogATokenLoad` |
| Every runnable command honors `--json` (one document on stdout, or JSON lines for `sync`) or refuses it as a usage error; the mutating commands report their result as JSON. | `TestCommandTree_EveryLeafHonorsJSON`, `TestDriveAdd_JSONWithoutSelectorIsAUsageError`, `TestPauseCommand_JSONReportsDriveAndEndTime`, `TestResumeCommand_JSONReportsEveryDriveOutcome`, `TestDriveRemove_JSONReportsWhatWasRemoved`, `TestDriveAdd_JSONReportsAddedThenAlreadyConfigured`, `TestLogoutCommand_JSONNamesAccountAndDrives`, `TestRecycleBinEmpty_JSONListsDeletedAndFailedAndStillFails` |
| Command-level side-effect contracts are tested at the CLI boundary: read-only commands do not mutate managed state, and mutating commands fail selector/path validation before remote mutation. | `TestRunLs_DoesNotMutateManagedState`, `TestRunRm_RequiresExplicitPathBeforeGraphMutation` |

## Log Retention Deletes Only Its Own Logs

`log_file` is an arbitrary path the user chooses, and it has no default, so the
directory it lands in is very likely shared -- a home logs directory, or a
system one. Retention therefore deletes only the configured log file and its
rotations, matched by name.

Deleting every `*.log` in that directory, which is what retention used to do,
removes files this program never created and knows nothing about. That is not
retention; the fact that it happens quietly, on a best-effort path that ignores
its own errors, makes it worse. Nothing else in the product deletes outside
what it owns, and a log directory is not owned merely by being written to.

## Account Names Are Untrusted Input

Account names reach the CLI from the config file and the catalog, both of which
the user can edit by hand. Building a canonical ID from one with a `Must`
constructor treated a hand-edited name as a programmer error: an empty account,
or one containing a colon, panicked `status` and the auth health check with a
stack trace instead of reporting a problem.

Token fallback now constructs the canonical ID through the checked constructor
and degrades to the zero value, which every caller already treats as "no login
for this account" -- `DriveTokenPath` returns an empty path and
`ResolveAccountNames` returns empty names. The degraded answer is the correct
one: a name that cannot form a canonical ID cannot have a token on disk.

## Signal Handling

Signal handling is installed once, at the process entry point, so every command
is interruptible rather than only `sync`. The first SIGINT/SIGTERM cancels the
command context and the command unwinds through its own cleanup; the second is
the escape hatch for a stuck process.

Previously only `sync` installed a handler, so a signal during any other
command killed the process outright and nothing deferred ran. Interrupting a
transfer left its temporary state on disk with no opportunity to clean up, and
recovery depended entirely on the next run noticing.

The handler is per invocation, not per process, so the context's cancel must be
called when the command finishes: it stops the signal goroutine and unregisters
the handler. A test binary that runs the CLI repeatedly is the case that makes
this matter.

The signal logger writes to the status stream because the configured command
logger does not exist yet when the handler is installed -- log configuration is
resolved from flags after the command starts. It renders plain lines
("Stopping... (press Ctrl-C again to force quit)"), not log records, because
the person who pressed Ctrl-C is its reader.

## Exit Status

`commandExitCode` (`failure_class.go`) is the one place that turns a command's
final error into the process status (R-6.6.31). The values are a contract for
scripts and service managers:

| Status | Meaning | Chosen by |
| --- | --- | --- |
| 0 | success (also `sync --watch` stopped by a signal: the watch finishes cleanly) | `err == nil` |
| 1 | any other failure, transient or not | default |
| 2 | command-line mistake | `*usageError` |
| 77 (EX_NOPERM) | sign-in required | `graph.ErrNotLoggedIn` or `graph.ErrUnauthorized` (`ErrLoginExpired` travels with it) |
| 78 (EX_CONFIG) | invalid configuration | `config.ErrInvalidConfig` |
| 130 | stopped by the user | shutdown context done and the error is a cancellation |

77 and 78 mean "running again unchanged fails the same way", so a unit can set
`RestartPreventExitStatus=77 78`. A sign-in the user declined or let expire
during `login` exits 1: rerunning `login` is the fix.

Classification is by sentinel only. `internal/config` tags every failure at its
exported read/parse/validate/resolve entrypoints (`Load*`, `Validate*`,
`ResolveDrive(s)`, `MatchDrive`, the config-vs-catalog check of
`LoadValidatedState`) with `ErrInvalidConfig` through a wrapper whose
`Error()` is the original text, so messages are unchanged; catalog and
token-file I/O are not config errors. "No drives configured" is raised in the
CLI and carries the same sentinel. Composed messages keep their cause through
`presentedError`. A watch whose drives all failed to start keeps the drives'
causes only when every one is sign-in or every one is config
(`startupFailureError`): with a mix, restarting can still bring a drive up, so
it exits 1. One-shot `sync` keeps its first drive failure as the cause
(`runOnceResultError`).

## systemd Notify

`internal/sdnotify` owns the process's only access to `$NOTIFY_SOCKET`,
`$WATCHDOG_USEC` and `$WATCHDOG_PID` (R-6.6.32). It speaks the datagram
protocol with the standard library: one connected `unixgram` socket, a path or
an abstract `@name` (Go maps the leading `@` to the abstract namespace), a
bounded write per message, `STATUS=` flattened to one line, and
`MONOTONIC_USEC=` from `CLOCK_MONOTONIC`. A nil `*Notifier` is the no-op used
outside systemd, so callers do not branch.

`runSyncWatch` opens the notifier (through the `notifyGetenv` seam, `os.Getenv`
in production) and closes it; an unusable socket logs one warning and sync
continues. `runSyncDaemonWithFactory` hands `syncNotifier.lifecycle` to
`multisync.OrchestratorConfig.WatchLifecycle`, which the watch loop calls on
its own goroutine:

- `WatchReady` (all startable drives are running watchers) sends `READY=1` with
  `STATUS=Watching <roots>` plus "sign-in required for <drive>" or "N drives
  could not start" for the drives that did not start.
- `WatchReloading` sends `RELOADING=1` and `MONOTONIC_USEC=`; `WatchReloaded`
  always sends `READY=1` again, saying when the previous settings were kept.
- `WatchStopping` sends `STOPPING=1` before the runners drain.

A watch that ends with an error sets a final `STATUS=` ("Sign-in required: run
'onedrive-go login'", "Invalid config: ...", "Failed: ..."). When the unit sets
`WatchdogSec=`, a goroutine owned by `runSyncDaemonWithFactory` sends
`WATCHDOG=1` at half the timeout until the watch returns. The keepalive proves
the process is alive and scheduling, not that every drive is healthy; drive
health is in `STATUS=` and `status`. Reloads are started by the control socket
(`pause`, `resume`, `drive add`, `login`); there is no SIGHUP reload, so units
use `Type=notify`, not `Type=notify-reload`. An example user unit is
`contrib/systemd/onedrive-go.service`.

## Console Output

Rules: [cli-output-guidelines.md](../reference/cli-output-guidelines.md).

`sync` output is owned by `syncConsole` (`internal/cli/sync_console.go`):

- before a one-shot run, one line per drive: "Syncing ~/OneDrive with
  user@example.com..." (R-6.6.19)
- one line per finished file action, fed by the engine activity hook through
  `OrchestratorConfig.ActivityHook` (R-6.6.18). Successes and dry-run plans go
  to stdout, because for `sync` they are the command's output; failures that
  need the user go to stderr; failures the engine keeps as retry work appear
  only with `--verbose`
- one summary line per drive in human units, plus a "Held back by
  --download-only" line for work a one-way mode skipped (R-6.6.19)
- watch mode: a startup line per folder and account, the Ctrl-C hint on a
  terminal, time-of-day prefixes off the journal, and "Stopped." (R-6.6.20)
- `--quiet` keeps only failures

Errors print once (R-6.6.22). For a single drive the console prints nothing
for its error and `main` prints the returned error through
`commandErrorText`: sign-in problems become the auth message, unreachable
network becomes one plain line naming the cause, the full chain is added with
`--verbose`/`--debug`. Messages composed for display wrap their cause in
`presentedError` so classification still sees it; flattening them with `%s`
previously hid `ErrNotLoggedIn` and doubled the output. With several drives,
each drive's error is printed under its name and the command error only counts
them.

Console logging (R-6.6.21): with `log_format = "auto"` the console is never
JSON. Default verbosity uses `consoleLogHandler` ("Warning: msg (k=v)"),
`--verbose`/`--debug` use timestamped key=value, and when stderr is the
journal (`stderrIsJournal`, `$JOURNAL_STREAM` matched against stderr's
device:inode) each line carries a `<N>` priority prefix and no timestamp.

The command logger is one console handler and, when `log_file` is set, one
JSON file handler, each with its own level (R-6.6.2, R-6.6.3). The console
level comes only from the flags: WARN by default, INFO with `--verbose`, DEBUG
with `--debug`, ERROR with `--quiet`. The file level comes only from
`log_level` (INFO by default). INFO records are operational history -- the
performance summaries (R-6.6.14, which `devtool bench` reads back from the log
file), plan counts, runner lifecycle -- so they reach the log file always and
the console only with `--verbose`/`--debug`. What a person must see at default
verbosity (activity, summaries, startup lines) is written by the command
itself (`syncConsole`), not logged, so the split hides nothing they need. At
default verbosity `consoleLogHandler` also drops the R-6.6.13 schema
identifiers other than `path` (`run_id`, `action_id`, `action_type`,
`condition_key`, `failure_class`, `log_owner`, `scope_key`,
`trial_scope_key`, `drive_id`, `http_status`), plus the diagnostic
`supersede_streak` and `error_detail` (a raw error chain with item IDs, kept
for the log file while the line itself carries a plain `reason`); the log file and
`--verbose`/`--debug` keep them, and so does the journal under
`--verbose`/`--debug`.
An `error` attribute logged as an error value (`slog.Any`, as multisync's
"Stopped syncing <drive>; retrying in ..." warning does) renders at default
verbosity as `plainErrorReason` (the network or sign-in wording, else the
innermost cause); the log file and `--verbose`/`--debug` keep the full chain.
Text-valued `error` attributes are shown as given.

`initializeCLIContext` attaches the configured log file before any command
work, for every command (`attachConfiguredLogging`, a lenient best-effort read
of the logging settings). Commands that skip the strict config load -- auth,
`drive`, `status`, `sync`, `pause`/`resume`, `perf`, shared targets --
previously kept the flag-only bootstrap logger, so their performance summary
and graph logs never reached the log file. `useLoggingConfig` is the one
place that builds the command logger; a later call with the same settings
(strict drive resolution, `sync` re-reading its config) keeps the logger, so
the file is opened and its warnings printed once. The few records logged
while the config file is still being found go to a `bootstrapLogBuffer` and
are replayed through the command logger once it exists, so they follow the
same console rules and `log_format`, and reach the log file too.

`login` (R-6.6.24) shows the code, its expiry, the account to use, a passkey
hint, and that it is waiting; success says where files will sync and what to
run next. A refresh token rejected with `invalid_grant` is `ErrLoginExpired`
(which also matches `ErrUnauthorized`) and is presented as sign-in required
everywhere (R-6.6.23). A canceled or declined sign-in and an expired sign-in
code are one line that says to run `login` again. `drive list` and `status`
take a configured drive's state from the same `configuredDriveState`: paused,
"not synced yet" without a state database, then issues or pending changes as
the state database records them (R-6.6.25). The auth health check behind both
only checks the token file (`graph.CheckSavedLogin`); the command's real load
is the one that logs "loaded saved token".

Transport failures are recognized whether they happen before the response
(`*url.Error`) or while reading it (`*graph.ResponseReadError`, set on every
response body by the Graph client): timeouts, including a response-header
timeout, are "OneDrive did not answer in time"; certificate, TLS record and
TLS alert failures are "the secure connection to OneDrive failed"; EOF,
unexpected EOF, resets, closed connections and HTTP/2 stream or connection
errors (matched by net/http's fixed "stream error:"/"http2: " text, since those
types are unexported) are "the connection to OneDrive was interrupted". A disk
error io.Copy returns during a download carries neither mark and is never
described as a OneDrive problem.

`ls` on a file lists that one file, as ls(1) does: when OneDrive refuses to
list a path's children with HTTP 400/422, `ls` looks the path up once and
prints it if it is a file; any other failure is reported unchanged.

File commands wrap failures in `remoteOpError` (action, path, cause) instead
of `fmt.Errorf("<action> %q: %w")`; its `Error()` text is unchanged, but
`commandErrorText` uses the path to print one plain sentence for the Graph
failure classes and missing or unreadable local files (R-6.6.26). Command-line
mistakes become `usageError` through cobra's flag-error hook, a wrapper on
every command's `Args` validator, and a flag-group check that runs before
`initializeCLIContext`; `main` prints them with a `--help` pointer and exits 2.
Only a stop the user requested (the shutdown context is done) is silent and
exits 130; `context.DeadlineExceeded` from anything else is an error. Under
the journal, `writeCommandError` prefixes each line with `<3>` (R-6.6.27).

Commands never report success for work they skipped (R-6.6.28). One-shot sync fails
only when a drive could not sync; per-file failures are printed and kept as
durable engine conditions for `status`, not turned into a failed run. Folder `get`/`put` go through
`bulkTransferError`, which lists each failed item on stderr and fails the
command (under `--json` the failures stay in the document, and only the exit
status changes). `sync --json` swaps the console's text for JSON lines
(`sync_console_json.go`), and `status --json` prints an empty document
instead of prose.

### JSON Output Everywhere

`--json` is a persistent root flag, so it reaches every command; a command that
ignored it would hand a script prose where it asked for JSON (R-6.6.33). Each
runnable command therefore does one of two things:

- **Prints its result as JSON.** One document on stdout (`sync` streams JSON
  lines), written once at the end through `writeJSONDocument` or the command's
  own printer. The prose confirmation ("Removed drive ...", "Token removed for
  ...") is dropped: helpers that print it take `CLIContext.resultWriter()`,
  which is `io.Discard` under `--json`. Warnings, prompts (the
  `reset-sync-state` RESET prompt, the sign-in code) and errors stay on
  stderr. When a bulk command partly fails (`resume` across drives,
  `recycle-bin empty`), the document lists what succeeded and what failed, and
  the exit status still reports the failure.
- **Refuses `--json` as a `usageError`** (exit 2) before any side effect, when
  that form has no result to print: today only `drive add` with no drive,
  which prints guidance and points at `drive list --json`.

The mutating commands' documents:

| Command | Document |
| --- | --- |
| `login` | `account`, `drive`, `drive_type`, `organization` (business), `sync_dir`, `added` (false on a re-login) |
| `logout` | `account`, `token_removed`, `purged`, `drives` |
| `pause` | `drive`, `paused`, `paused_until` (UTC; omitted for an open-ended pause) |
| `resume` | `drives[]` of `drive`, `result` (`resumed`, `expired_pause_cleared`, `not_paused`, `failed`), `error` |
| `drive add` | `drive`, `display_name`, `sync_dir`, `added` (false when already configured) |
| `drive remove` | `drive`, `removed_from_config`, `purged` |
| `drive reset-sync-state` | `drive`, `reset` |
| `drive rebind-root` | `drive`, `rebound`, `synced_files`, `files_in_folder` |
| `trash list` | `drive`, `entries[]` of `id`, `path`, `sync_root`, `reason` (`deleted`, `replaced`), `size`, `trashed_at`, `state` (`committed`, `pending`, `orphan`) |
| `trash restore` | `drive`, `id`, `restored_to` |
| `trash empty` | `drive`, `removed` |
| `recycle-bin empty` | `deleted[]` and `failed[]` of `id`, `name` (and `error`) |

`TestCommandTree_EveryLeafHonorsJSON` walks the real command tree, runs every
runnable leaf with `--json` against fakes (an in-memory Graph drive, a fake
control socket, the sync runner seam) and asserts stdout parses as exactly one
JSON value (or JSON lines), or that the flag is refused. A new command without
a case fails the test. Two seams exist only so this can run offline:
`completeLogin` is everything after the Microsoft sign-in exchange, and
`cliSeams.cpAwaitCopy` stands in for polling the copy monitor, whose URL must
be HTTPS on a Microsoft host. Cobra's generated `completion` and `help`
commands are outside the tree the test walks; they print shell scripts and
help text and ignore `--json`.

`pause` parses its duration before writing `paused = true`, and `pause`/`resume`
resolve `--drive` through `config.MatchDrive`, the same selector rules as
`sync`. `drive remove` and `drive reset-sync-state` use
`resolveExactDriveSelector`, which accepts an exact canonical ID (so leftovers
of an unconfigured drive can still be purged) or a display name, never a
partial match (R-2.6.5). Human-readable output shows local time (`ls`
compactly, `stat` exactly with the zone name); `--json` timestamps are always
UTC.

Transfer progress goes through `CLIContext.Progressf`, which redraws one line
(`\r` + erase-line) on a terminal and prints nothing otherwise; `Statusf` and
main's final error clear it first (R-6.6.29).

### Commands Must Consume The Command Context

Installing the handler is only half of the contract. A command that calls
`context.Background()` on its own behalf silently opts out of cancellation: the
signal still fires, the command context is still canceled, and the work the
command is actually doing never learns about it.

`status` did exactly that. It read `cmd.Context()` only to recover the
`CLIContext` from it and then discarded the context itself, so the account
snapshot load, the live overlay (which performs an OAuth token refresh and
Graph calls), the runtime overlay, and the perf overlay all ran under a
detached background context. Interrupting `onedrive-go status` could not stop a
token refresh already in flight.

The rule is therefore stated positively: a command entry point threads
`cmd.Context()` into its command function as the first parameter, and every
helper that performs I/O takes that context through. `context.Background()`
belongs only at the process entry point in `root.go`, where the root context is
created and the signal handler is attached to it.

This is a behavioral requirement rather than a style preference, which is why
R-6.3.6 is now evidenced by a test asserting the command's context reaches the
live overlay, not only by a test asserting the handler cancels the context it
owns. The two are independently falsifiable and the second does not imply the
first.

## Command Surface

| Command family | Purpose |
| --- | --- |
| `login`, `logout` | auth and account session lifecycle |
| `ls`, `get`, `put`, `rm`, `mkdir`, `mv`, `cp`, `stat` | file operations |
| `drive` | drive management and explicit per-drive sync-state reset |
| `shared*` | shared-item discovery and add flows |
| `sync`, `pause`, `resume` | sync control |
| `status` | read-only account and sync health |
| `perf` | live owner perf view and capture |
| `recycle-bin` | recycle-bin operations |

Sync intent is derived from observation snapshots and planner reconciliation,
then applied by the sync executor through concrete file and remote side effects.

`sync` refuses to run as root (effective UID 0) unless `--allow-root` is
given (P-9): a root sync writes root-owned files into the user's folder that
the user's later syncs and editors cannot change. The refusal is a usage
error raised before config is read (`refuseSyncAsRoot`,
`TestSync_RefusesRootWithoutAllowRoot`).

`cp --force` and `mv --force` replace an existing destination file through
`replaceDestination` (`replace_destination.go`): the existing file is renamed
aside to `.onedrive-go-replacing-<unix-ns>-<name>` (trimmed to 255 bytes on a
rune boundary), the copy or move runs (for `cp`, until the monitor reports
`completed`), and the aside file then goes to the recycle bin through
`DeleteResolvedPath`, an already-missing one counting as done. If that cleanup
still fails after the replacement landed, the command succeeds and warns,
naming the leftover file. On any failure of the operation, a canceled
context included, the aside file gets its name back under a 30-second bound.
Graph has no atomic replace for either operation, and deleting first left a
user whose copy failed with neither file (D-082).

A process killed between the rename aside and the cleanup leaves a
`.onedrive-go-replacing-*` sibling. Every later `--force` run to that name
lists the destination folder and prints a one-line note naming any such
sibling, so the previous version can be recovered by renaming it.

A completed file copy is then PATCHed with the source's modification time,
and every monitor status other than `inProgress`/`notStarted`/`waiting`/
`updating` ends the poll (`TestCpForce_FailedCopyRestoresTheOriginal`,
`TestCpForce_SuccessReplacesAndKeepsSourceMtime`,
`TestCpForce_CleanupFailureWarnsAndSucceeds`,
`TestCpForce_HintsAboutAnInterruptedReplacement`,
`TestMvForce_ReplacesWithoutDeletingFirst`,
`TestReplaceDestination_CanceledOperationStillRestores`,
`TestReplaceDestination_RestoreIsBounded`, `TestCopyStatusOutcome`).

Configured standalone mount-root mounts keep `/` anchored at the configured
mount root, not the backing drive root. Path-oriented file operations such
as `mkdir` therefore walk and mutate only inside that configured subtree.

## Status And Read-Only Sync State

`status` is intentionally read-only and account-centric. It is the only
sync-health command. The public presentation is account -> configured drives ->
shared folders. Internal runtime concepts such as mount IDs and stored
condition keys remain private implementation details.

- account identity comes from the validated config+catalog snapshot, enriched
  by `/me` when authentication succeeds
- configured drive rows come only from config/catalog; status never enumerates
  unconfigured Microsoft Graph drives into the output
- shared folder shortcuts discovered by a parent engine render as nested
  `Shared folders` under that configured parent drive
- sync-state snapshots come from store-owned raw authority reads
- the CLI translates sync-owned stored condition groups into public `Issues:`
  sections and JSON `issues`, with user-facing titles, reasons, actions,
  scope labels, ordering, and path sampling
- `status_sync_state.go` only assembles the high-level sync-state payload;
  `status_condition_descriptors.go` owns the public issue presentation while
  still keying descriptors from sync-owned `ConditionKey` values
- live account overlay fetches `/me` only for account display/sign-in proof, and
  fetches storage only for configured drives (`/me/drive` for the configured
  primary personal/business drive, or a narrow drive-by-ID lookup for a
  configured non-primary drive that already has a known remote drive ID)
- live runtime ownership comes from the active control socket when reachable;
  it may contribute summary/runtime perf facts but does not expose mount IDs in
  public status JSON
- live perf comes from the active owner over the control socket when requested

When both `--account` and `--drive` selectors are present, `status` applies
them as an intersection. `--drive` still selects configured standalone parent
drives, but each selected parent row automatically includes its attached shared
folder shortcut rows. The CLI does not widen the result to the union of independently
matched accounts and configured drives.

The JSON surface follows the user model:

- `summary.total_drives` counts configured top-level drive rows
- `summary.total_shared_folders` is present only when nested shared folders are
  displayed
- accounts use `accounts[].drives`
- shortcut projections use `shared_folders`
- sync-state issue groups use `issues`
- healthy account auth omits auth fields; `sign_in_required` appears only when
  the user needs to sign in
- public JSON omits `mount_id`, `namespace_id`, `canonical_id`,
  `projection_kind`, `drive_id`, `user_id`, `auth_state: ready`, and
  `live_drives`

Drive rows expose `kind`, `name`, `folder`, `state`, optional `storage`,
optional lifecycle fields, optional `sync_state`, and optional
`shared_folders`. User-facing states are `up_to_date`, `syncing`, `pending`,
`paused`, `issues`, and `unavailable`. Text output uses `Folder`, `Storage`,
`Files`, `Remote changes`, `Retrying`, and `Issues` labels. It never prints an
empty healthy issue section, never prints "No active conditions", and never
prints a healthy `Auth: ready` line.

Child lifecycle rows expose `state`, `state_reason`, `state_detail`,
`protected_current_path`, `protected_reserved_paths`, typed
`issue_class`/`recovery_class`, `recovery_action`, `auto_retry`, and, only
under `--verbose` or `--debug`, `state_debug_detail`, from sync-owned
`ShortcutRootStatusView` values plus child sync-state snapshots. `state`,
`state_reason`, `issue_class` and `recovery_class` are stable machine tokens;
`state_detail` (the view's `Reason`) and `recovery_action` are one plain
sentence each in user vocabulary (shared folder, folder, OneDrive), per
[cli-output-guidelines.md](../reference/cli-output-guidelines.md);
`state_debug_detail` (the view's `Detail`) is the raw diagnostic recorded with
the state, such as a Graph or filesystem error chain. Text output prints
`Status: <plain label>`, `Reason: <state_detail>` and `Action:
<recovery_action>`, never the machine tokens, and adds `Detail:` only under
`--verbose`/`--debug`; `runStatusCommand` drops the debug detail otherwise,
for text and JSON alike. The CLI does not read raw `shortcut_roots` fields
such as protected-path bookkeeping, blocker detail, or waiting replacement
internals; sync owns that projection and its copy table
(`shortcutRootStatusMetadata`). Evidence: `TestBuildChildStatusMount_RendersLifecycleState`,
`TestStatusOutputGoldenText`, `TestStatusOutputGoldenTextVerbose`,
`TestDropStatusDebugDetail_ClearsNestedRawDetailOnly`,
`TestShortcutRootStatusView_SeparatesReasonFromRawDetail`.

For `removed_final_drain`, status must make the retry/discard choice explicit:
the child keeps retrying while its state DB owns dirty content state; the user
can restore access to the shared folder for normal retry, or delete its local
folder to discard the changes that have not synced and let parent release
cleanup purge the child state automatically.

Shortcut child status distinguishes the recovery class rather than collapsing
all failures into one blocked row: target unavailable, local root unavailable,
unsafe/blocked path, cleanup blocked, and retryable final-drain work use
distinct `state_reason`/`state_detail` values. Retryable final drain and
cleanup states set `auto_retry=true`; cleanup or purge failures keep the child
visible until the next reconciliation succeeds. If the user deletes the local
shortcut projection to discard it, parent release cleanup removes the child row.

The target `status` surface projects the full sync model directly from:

- `observation_issues`
- `retry_work`
- `block_scopes`
- `baseline` / `remote_state` counts and drift facts
- account/auth/degraded overlays
- optional live perf

`status` is the single read-only sync-health surface. It renders current
issues, drive/shared-folder state, sign-in-required overlays, and optional live
perf; handled sync work is represented by current planning and execution state
rather than a separate history-only status section.

Minimal-config direct file-operation coverage keeps that contract explicit:
the full-suite `TestE2E_RoundTrip` status check asserts that healthy output
omits auth/debug lines and empty issue sections, and rejects any reintroduced
`Last sync:` history line.

## Control Socket

The control socket is JSON-over-HTTP over a Unix domain socket. The CLI uses
it for:

- owner/status probing
- `status --perf`
- `perf capture`
- watch-owner `reload`
- watch-owner `stop`

One-shot owners expose status/perf and reject live control mutations. Watch
owners expose the remaining daemon controls above.

The control-socket protocol is now mount-shaped:

- `GET /v1/status` returns `mounts`, not `drives`
- `GET /v1/status` may include transient `shortcut_cleanup_failures` from the
  current owner so debug/status clients can distinguish child artifacts still
  remaining from artifacts already purged but not acknowledged by the parent
  owner; those transient rows are cleared when a retry succeeds or when the
  current parent snapshot no longer contains cleanup work for that source
- `GET /v1/perf` returns per-mount live snapshots keyed by mount ID
- the CLI status/perf overlay matches those mount IDs against the runtime mount
  rows it renders
- `status --perf` renders stale-work, local-observation, and replan-idle
  aggregate lines only when those counters are nonzero; the text remains
  path-free and ID-free.

The socket is runtime control and read-only observation only. Sync intent comes
from observation, planner reconciliation, and executor side effects; clients
cannot submit sync decisions over the socket.

## Pause / Resume

`pause` and `resume` remain config mutations owned by the CLI. After writing
config, the CLI notifies a running watch owner to reload when possible. If no
watch owner is running, the updated config takes effect on the next start.

## Logout Recovery

`logout --purge` is also the recovery path when local config/catalog state is
damaged. The CLI first tries validated state, but if that fails it falls back
to best-effort config and catalog loads, logs warnings, resolves the selected
account from the recoverable durable facts, and continues the purge instead of
refusing all cleanup.

## State DB Handling

The CLI owns the only destructive operator surface for per-drive sync state:
`onedrive-go drive reset-sync-state --drive <drive>`.

- `--drive` is mandatory and must resolve to exactly one configured drive
- the default flow requires typing `RESET`; `--yes` is the non-interactive bypass
- the command refuses to run while that drive has a live sync owner
- the command deletes only the selected drive's state DB family and recreates a
  fresh canonical DB immediately

`status` remains read-only. Missing or unreadable DBs collapse to an empty sync
snapshot for that drive; status does not mutate state and does not become a DB
health control surface.

`sync` and `sync --watch` never auto-delete an existing DB. When startup sees
an unreadable, incompatible, or unsupported existing DB, the CLI renders the
same guidance in one-shot and watch flows: pause that drive first, rerun with
`--drive` selecting only other drives, or run `drive reset-sync-state --drive
...`. One-shot renders startup-ineligible drives through the shared startup
message path, reports completed runs separately, and exits non-zero after
reporting any affected drives. Watch mode warns immediately about each skipped
drive and continues healthy mounts unless none can start.

That standalone-drive guidance is never rendered for managed child mounts. A
child startup or state-store warning names the child `mount_id`, may include the
child state DB path for diagnosis, and states that control belongs to the
parent drive or the OneDrive shortcut. It must not suggest `--drive ''`,
`--mount`, child pause/resume, or child reset commands.

One-shot sync resolves the full selected drive set, but it validates only
runnable non-paused drives before startup. Paused drives remain in the startup
summary as skipped drives; an invalid paused drive must not block unrelated
runnable drives from executing.

For `sync` and `sync --watch`, the CLI/config composition boundary passes the
managed data directory into `multisync.OrchestratorConfig`. Lower layers use
that explicit value for child state DBs, catalog, transfer scratch, and upload
session cleanup. `internal/multisync` must not call `config.DefaultDataDir()`,
`config.MountStatePath(...)`, or derive child runner/cleanup paths from ambient
process state.

Login and `drive add` materialize the configured `sync_dir` when they enroll or
repair a drive. The CLI validates the post-tilde `sync_dir` is absolute before
creating any directory, so stale relative config cannot create directories
under the caller's current working directory. If login cannot materialize the
sync root after moving the token and recording catalog metadata, it rolls those
login side effects back to the pre-login token/catalog/config state before
returning the failure. One-shot sync, watch startup, dry-run, and
control-socket reloads never create a missing root. They compile resolved
drives into mount configs and let engine startup report a missing root as a
per-mount `ErrMountRootUnavailable`, so a missing `sync_dir` on one drive does
not prevent other runnable mounts or reload changes from proceeding.

Dry-run one-shot sync resolves the dry-run decision before setup only so watch
mode can reject an effective dry-run before doing work. `sync --dry-run` is the
only command-line dry-run flag surface; config `dry_run=true` is still honored
for one-shot sync and rejected for watch unless the user explicitly passes
`sync --watch --dry-run=false`. Valid dry-run one-shot sync then follows the
same bootstrap as a live one-shot run: sync-bootstrap email/config
reconciliation, normal OAuth token sources with refresh persistence, `log_file`
open/create, and control-socket ownership all remain active. The command
resolves selected drives and delegates preview reporting to multisync/engine
with `DryRun=true`; lower layers use that flag to suppress plan execution and
sync-progress commits, not CLI/process setup. The dry-run product contract is
limited and explicit: no local sync-tree content mutation and no remote
OneDrive content mutation. Operational side effects from command setup,
authenticated Graph access, managed state open/checkpoint, and scratch
planning are still allowed.

`login`, `drive add`, and `drive remove` ask a running watch owner to reload
configuration through the control socket after successful config mutation. The
daemon process is not restarted; `internal/multisync` diffs the new mount set
and starts, stops, or keeps per-drive runners in place.

Generated follow-up commands in startup messages are shell-safe. Canonical IDs
and other user-controlled values are single-quoted before they are rendered
into suggested `pause` or `drive reset-sync-state` commands.

## CLI Boundary

The CLI owns command parsing, configuration mutations, presentation, and control
requests to a running sync owner. Sync decisions come from sync-owned
observation, planning, execution, and store transactions; the CLI does not write
planner decisions or executor outcomes directly.
