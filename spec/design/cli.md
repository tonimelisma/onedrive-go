# CLI

GOVERNS: main.go, internal/cli/*.go, internal/logfile/logfile.go

Implements: R-1 [implemented], R-3.1 [verified], R-3.7 [verified], R-4.7 [verified], R-4.8.4 [verified], R-1.9 [verified], R-1.2.4 [verified], R-1.2.5 [verified], R-1.3.4 [verified], R-1.3.5 [verified], R-1.4.3 [verified], R-1.4.4 [verified], R-1.5.1 [verified], R-1.6.1 [verified], R-1.6.2 [verified], R-1.7.1 [verified], R-1.8.1 [verified], R-1.9.4 [verified], R-2.3.3 [verified], R-2.3.4 [verified], R-2.3.5 [verified], R-2.3.6 [verified], R-2.3.7 [verified], R-2.3.8 [verified], R-2.3.9 [verified], R-2.3.10 [verified], R-2.3.11 [verified], R-2.3.12 [verified], R-2.8.3 [verified], R-2.10.4 [verified], R-2.10.32 [verified], R-2.10.47 [verified], R-2.14.3 [verified], R-2.14.5 [verified], R-3.1.6 [verified], R-3.3.10 [verified], R-3.3.11 [verified], R-3.3.12 [verified], R-3.6.6 [verified], R-3.6.7 [verified], R-6.6.11 [verified], R-6.8.16 [verified], R-6.10.6 [verified], R-6.10.13 [verified]

## Overview

The root package is a thin process entrypoint. The Cobra command tree, CLI bootstrap, output formatting, and command handlers live in `internal/cli`.

`internal/cli/root.go` handles global flags (`--config`, `--drive`, `--verbose`, `--quiet`, `--debug`, `--json`), config loading via `PersistentPreRunE`, and drive filtering or drive resolution as appropriate for the command. Cobra `RunE` functions are intentionally thin: they parse command-local flags and delegate to command-family helper functions that take `*CLIContext` plus explicit inputs. For larger command families, the `*.go` Cobra wiring file stays small while sibling `auth_*.go`, `drive_*.go`, `status_*.go`, `resolve_*.go`, `recycle_bin_*.go`, `recover_*.go`, and `sync_*.go` files own the workflow logic for those commands. `internal/cli/sync.go` stays in the same package because multi-drive sync reuses the same Phase 1 CLI context but performs its own multi-drive resolution.
Sync-specific report rendering and watch bootstrap helpers stay in the same package, but they live outside the Cobra wiring file so `internal/cli/sync.go` remains focused on command construction, flag parsing, and multi-drive resolution.
`drive add` and `drive search` follow the same rule now: each workflow lives top-to-bottom in its command-owned file instead of bouncing through separate generic helper files.

`internal/cli` is also the composition root for Graph-facing runtime assembly.
Each command/watch runtime owns one `driveops.SessionRuntime`, which chooses
bootstrap, target-scoped interactive, or sync HTTP profiles without making
`internal/cli` the long-term owner of transport constants, retry mechanics,
or Graph client caches.

`CLIContext` owns two distinct output boundaries:

- `OutputWriter`: primary command output (default `stdout`)
- `StatusWriter`: progress/status output (default `stderr`)

This keeps human/JSON command results separate from progress/status messages and avoids direct process-global stdout/stderr writes inside command families and CLI bootstrap paths.

## Ownership Contract

- Owns: Cobra command wiring, CLI bootstrap, signal handling, control-socket client routing, output formatting, user-facing reason/action text for failures and issues, and dependency composition for Graph-facing HTTP profiles.
- Does Not Own: Graph wire behavior, config semantics, sync planning/execution policy, or durable sync-state mutation rules.
- Source of Truth: `CLIContext`, parsed flags/args, and the domain results returned by lower layers.
- Allowed Side Effects: stdout/stderr output, log-file creation, signal handling, control-socket RPCs, durable user-intent writes when no daemon is running, and calls into CLI-owned helper families plus lower-layer packages.
- Mutable Runtime Owner: Each command invocation owns its own runtime state. `internal/cli` keeps no package-level mutable state and relies on lower layers for long-lived watch ownership.
- Error Boundary: The CLI turns lower-layer results into exit behavior plus human-readable reason/action text. It consumes the domain model from [error-model.md](error-model.md) instead of inventing a second classification scheme.

## Verified By

| Behavior | Evidence |
| --- | --- |
| The user-facing sync CLI uses `status` for read-only sync health, `resolve` for user decisions, and `recover` for state-DB recovery. | `internal/cli/root_test.go` (`TestNewRootCmd_Subcommands`, `TestMainWithWriters_StatusResolveRecoverHelpSucceeds`), `internal/cli/resolve_recover_test.go`, `internal/cli/status_test.go`, `e2e/cli_commands_e2e_test.go` (`TestE2E_Status_PerDrive_NoVisibleProblems`, `TestE2E_Status_History_ShowsResolvedStrategies`, `TestE2E_InternalBaselineVerification_AfterSync`) |
| CLI account-email reconciliation updates selector state and reloads the resolved drive before interactive session work continues. | `internal/cli/root_test.go` (`TestCLIContextSession_ReconcilesEmailChangeAndReloadsDrive`), `internal/config/token_resolution_test.go`, `internal/driveid/canonical_test.go` |
| Watch-mode CLI wiring remains injectable at both the top-level watch-runner seam and the lower daemon-orchestrator seam, and target-scoped interactive/shared flows still work under live E2E coverage. | `internal/cli/signal_test.go` (`TestRunSyncWatch_FirstSignalCancelsWatchRunner`, `TestRunSyncWatch_FirstSignalCancelsDaemonOrchestrator`, `TestShutdownContext_FirstSignalCancels`, `TestShutdownContext_SecondSignalForcesExit`), `e2e/sync_watch_full_test.go` (`TestE2E_SyncWatch_ConflictDuringWatch`), `e2e/sync_full_test.go` (`TestE2E_Sync_EditDeleteConflict`, `TestE2E_Sync_EditEditConflict_ResolveKeepRemote`) |
| Command-scoped log-file cleanup is rooted in the top-level CLI runner, so the active closer still shuts down exactly once when a leaf command replaces the logger and then returns an error before Cobra post-run hooks would fire. | `internal/cli/root_test.go` (`TestCLIContextReplaceCommandLogger_ClosesReplacedCloserAndTopLevelCloseClosesActiveCloser`, `TestCloseRootCommandLogger_ClosesActiveLoggerAfterCommandError`) |
| CLI control-socket probing classifies watch owners, one-shot owners, missing sockets, path-unavailable sockets, and ambiguous probe failures distinctly so durable-intent fallback is intentional and daemon notifications stop collapsing protocol failures into "no daemon". | `internal/cli/control_socket_semantics_test.go` (`TestProbeControlOwner_ClassifiesOutcomes`, `TestResolveDeletes_WritesDirectDBIntentForOneShotOwner`, `TestResolveConflict_FallsBackToDBIntentWhenNoDaemonSocketExists`, `TestResolveDeletes_FallsBackToDirectDBWhenControlSocketPathIsUnavailable`, `TestResolveDeletes_DoesNotFallbackWhenControlProbeIsAmbiguous`, `TestNotifyDaemon_ReportsAmbiguousProbeFailureClearly`), `internal/cli/sync_test.go` (`TestRunSyncCommand_FailsLoudlyWhenControlSocketPathCannotBeDerivedForOneShot`, `TestRunSyncCommand_FailsLoudlyWhenControlSocketPathCannotBeDerivedForWatch`) |
| Read-only CLI sync-state surfaces consume store-owned one-shot projection helpers instead of managing inspector or writable-store lifecycle directly. | `internal/cli/status_test.go` (`TestQuerySyncState_UsesReadOnlyProjectionHelper`, `TestStatusCommand_DamagedStateStoreSurfacesRecoverHint`), `internal/cli/auth_health_test.go` (`TestClearAccountAuthScopes_ClearsPersistedAuthScope`, `TestStatusCommand_DoesNotClearPersistedAuthScope`) |
| Production-visible perf surfaces stay split by intent: INFO log summaries are the durable history, `status --perf` is the live human view, and `perf capture` is the explicit deep-dive bundle command. | `internal/perf/logging_test.go` (`TestSession_EmitsPeriodicUpdateAndSummary`, `TestRuntimeCapture_DefaultManifestOmitsDriveSnapshots`, `TestRuntimeCapture_FullDetailManifestIncludesDriveSnapshots`), `internal/cli/status_perf_test.go` (`TestStatusPerf_SummaryJSON_WithLivePerf`, `TestStatusPerf_SummaryText_WithPerfAndNoActiveOwner`, `TestStatusPerf_FilteredJSON_WithPerfUnavailableFromActiveOwner`), `internal/cli/perf_test.go` (`TestMainWithWriters_PerfCaptureJSON_ForOneShotOwner`, `TestMainWithWriters_PerfCaptureRejectsInvalidDuration`, `TestMainWithWriters_PerfCaptureFailsWhenNoOwnerIsRunning`) |
| When `/me/drives` retry exhaustion forces authenticated degraded discovery, CLI logs preserve one shared degraded-discovery evidence shape (`account`, `endpoint`, quirk attempts) instead of flattening the failure to one opaque error string or letting each caller invent its own fields. | `internal/cli/degraded_discovery_log_test.go`, `internal/graph/quirk_retry_error_test.go`, `e2e/auth_preflight_helpers_test.go` |

## Command Structure

Implements: R-6.2.8 [verified], R-1.3.6 [verified], R-1.5.2 [verified], R-1.7.2 [verified]

| Command | File | Description |
|---------|------|-------------|
| `login`, `logout`, `whoami` | `auth.go` | Authentication (device code, PKCE, token management) |
| `ls` | `ls.go` | List remote files |
| `get` | `get.go` | Download files (via `driveops.TransferManager`, with disk space pre-check) |
| `put` | `put.go` | Upload files (via `driveops.TransferManager`) |
| `rm` | `rm.go` | Delete remote items (recycle bin by default; `--permanent` bypasses the recycle bin) |
| `mkdir` | `mkdir.go` | Create remote folders |
| `mv` | `mv.go` | Server-side move/rename |
| `cp` | `cp.go` | Server-side async copy with polling |
| `stat` | `stat.go` | Display item metadata |
| `sync` | `sync.go` | Multi-drive sync command (see [sync-control-plane.md](sync-control-plane.md)) |
| `pause`, `resume` | `pause.go`, `resume.go` | Pause/resume sync through CLI-owned config-mutation helpers. `resume` also cleans up stale config keys from expired timed pauses (paused=true + past paused_until). |
| `status` | `status.go` | Display account/drive status. `status.go` owns Cobra wiring, `status_snapshot.go` owns account/drive snapshot assembly, `status_sync_state.go` owns raw sync-state shaping, and `status_render*.go` plus `status_issue_descriptors.go` own final human/JSON rendering. |
| `perf` | `perf.go` | Inspect live sync-owner performance and capture an explicit local profile bundle through the control socket. |
| `resolve` | `resolve.go` | Record durable user decisions for held deletes and conflicts (`resolve deletes`, `resolve local|remote|both`, optional `--all`) |
| `recover` | `recover.go` | Repair, rebuild, or reset the selected drive's sync database after explicit confirmation |
| `drive` | `drive.go` | Drive management (list/add/remove/search) |
| `recycle-bin` | `recycle_bin.go` | Recycle bin operations (list/restore/empty) via recycle-bin helper functions |

For metadata-changing file commands that return a single destination path,
success means more than "Graph accepted the mutation." `mkdir`, single-file
`put`, and `mv` all call `driveops.Session.WaitPathVisible()` before emitting
their final success output. That keeps immediate follow-on CLI reads from
seeing a transient `itemNotFound` even though the mutation already completed.

For path-oriented deletes, success also means more than "one DELETE request
returned 204." `rm`, `mv --force`, and `cp --force` delete the destination
through `driveops.Session.DeleteResolvedPath()` so a transient delete-route
`itemNotFound` is reconciled against the remote path instead of surfacing a
spurious failure. That same delete-intent resolver can recurse through an
ancestor collection when the immediate parent path route is also in a transient
`itemNotFound` gap. `rm` additionally waits for the parent path to be readable
again before printing success for non-root parents, but once delete intent has
already proved the target path is gone it downgrades a pure bounded
`PathNotVisibleError` on that follow-on parent read to a warning. Other
parent-read failures still fail the command.

## Auth Lifecycle, Auth Health, And Proof Boundaries

Implements: R-3.1.3 [verified], R-3.1.4 [verified], R-3.1.5 [verified], R-3.1.6 [verified], R-2.10.47 [verified]

The CLI presents one user-facing umbrella state for auth problems:
`Authentication required`. Internally it keeps two sources of truth separate on
purpose:

- token and account-profile discovery own "missing or invalid saved login"
- `scope_blocks.auth:account` owns "sync proved that OneDrive rejected the saved login"

`status` is an intentionally offline snapshot. It uses local
token/account-profile state plus persisted sync state to show best-known auth
health, but they never probe Graph and never mutate `auth:account`.
Offline auth-scope detection now goes through the read-only
`sync.HasScopeBlockAtPath` boundary as an exact `auth:account`
scope-block query; CLI auth-health helpers do not open the mutable
`SyncStore` path just to read persisted auth state.

`internal/authstate` is the leaf package that owns the shared auth-health
vocabulary and copy:

- states: `ready`, `authentication_required`
- reasons: `missing_login`, `invalid_saved_login`, `sync_auth_rejected`
- shared title/reason/action text for CLI auth surfaces and `IssueUnauthorized`

Shared account-catalog snapshot helpers build one offline account catalog from configured drives,
discovered account profiles, discovered token files, and persisted
`auth:account` scope presence. `status`, `whoami`, `drive list`, `drive search`,
and `shared` all read from that same catalog so account discovery and auth
projection stay consistent across surfaces. For `drive search`, that means
account-level `accounts_requiring_auth` projection comes from every matching
business account in the catalog, not just configured drives, so orphaned
profiles and token-discovered business accounts do not disappear from the
caller boundary when saved login state is missing or invalid. `shared` uses the
same rule across all account types, so auth-required accounts are reported from
the shared catalog even when only orphaned profile or token state remains.

`whoami`, `drive list`, `drive search`, and ordinary single-drive file commands
are proof surfaces because they already perform authenticated Graph requests.
The first successful authenticated Graph response for an account clears stale
`auth:account` scope blocks across that account's state databases. This uses a
CLI-owned authenticated-success hook installed on live `graph.Client` and
`driveops.Session` instances. Pre-authenticated upload and download URLs
intentionally bypass that hook and do not count as proof.
That repair path is strictly best-effort: successful direct API commands must
not fail or emit user-visible warnings just because stale auth-scope cleanup
could not open a state DB. Repair failures stay in debug logs so file commands
remain independent from sync-store health.

Those same authenticated boundaries are also where CLI-owned email-change
reconciliation runs (R-3.7). After a command learns the current `/me`
identity, it compares the stable Graph user GUID to stored account profiles of
the same account type and asks `config.ReconcileAccountEmail()` to mutate
durable state when the email changed. Trigger points are:

- `login` after account discovery
- `whoami` authenticated account lookup
- shared account-catalog snapshot helper best-effort account-catalog refresh for `drive list`,
  `drive search`, `shared`, and name-based `drive add`
- shared-target account bootstrap before resolving share URLs/items
- ordinary file-command `CLIContext.Session()` bootstrap
- sync bootstrap before multi-drive session creation

Offline snapshots (`status`) do not probe Graph and therefore do
not trigger reconciliation.

When reconciliation fires during the current invocation, CLI owns the runtime
repair around the config mutation:

- exact old `--account` and exact old canonical `--drive` selectors are remapped in memory
- one concise status message is emitted for the renamed account
- single-drive file/session bootstrap flushes the `SessionRuntime` token cache,
  reloads config, re-resolves the selected drive, and then creates the session
- shared-target bootstrap rewrites the transient recipient account email before
  the share URL or shared selector is resolved further

`login` and plain `logout` are explicit auth-boundary transitions. Successful
`login` clears stale `auth:account` scope blocks for the account. Plain
`logout` clears those scope blocks from preserved state databases before
removing token/config state, and it preserves account profiles plus drive
metadata so offline `whoami` / `drive list` surfaces can still explain what
needs re-authentication. `logout --purge` removes the state databases
entirely.

`drive remove` and `drive remove --purge` are drive-boundary operations, not
account-boundary operations. They remove the selected drive's config and
drive-owned state, but they preserve the account token and `account_*.json`
profile so the remaining logged-in account catalog stays intact for `whoami`,
`drive list`, and `drive search`.

`whoami` no longer has a special "logged out accounts" concept. It reports
`accounts_requiring_auth`, which may come from:

- missing saved login
- invalid saved login on disk
- persisted sync-time `auth:account` rejection

`accounts_requiring_auth` is intentionally distinct from
`accounts_degraded`. The degraded bucket means the CLI proved the account is
authenticated (`/me` succeeded) but live drive-catalog discovery could not be
completed after the bounded `/me/drives` quirk retry. In that state:

- `whoami` still returns the authenticated user
- `whoami` falls back to `/me/drive` and returns the primary drive when
  available
- `drive list` keeps configured drives plus any independent live discovery that
  does not depend on `/me/drives`
- both commands surface `accounts_degraded` with reason
  `drive_catalog_unavailable`

This is a CLI-owned degraded mode, not an auth failure. It exists because
`internal/graph` owns the narrow `/me/drives` retry, but only the caller knows
how to turn retry exhaustion into useful user-facing output instead of dropping
usable local state or mislabeling the account as logged out.

When the degraded transition is caused by an exhausted documented Graph quirk
retry, the CLI also logs one shared degraded-discovery warning per
account+endpoint after the caller-owned snapshot is built. That warning
projects the attached retry evidence through `graph.ExtractQuirkEvidence`
(`graph_quirk`, attempt count, per-attempt request IDs/statuses/codes) and
always includes the degraded discovery endpoint (`/me/drives` today). Deep
discovery helpers return data/errors only; they do not own operator-facing log
policy. The evidence is for operators and incident triage only; human-readable
and JSON command output stay on the existing `accounts_degraded` contract.

After a plain logout, the account is no longer in config but its
`account_*.json` profile file remains on disk. `whoami` still discovers these
orphaned profiles via `config.DiscoverAccountProfiles()`, but it renders them
through the shared auth-required model instead of a dedicated logged-out bucket.
When `whoami` is using the authenticated Graph path, drive selection still
obeys the normal `MatchDrive` contract: unknown or ambiguous selections are
surfaced as user-facing errors rather than silently degrading into offline
auth-required output. The only soft fallback is "no configured drives", which
lets orphaned-profile discovery work after logout.

With `--purge`, `resolveLogoutAccount()` falls back to
`discoverOrphanedEmails()` when config has zero accounts. It auto-selects a
single orphan or requires `--account` for multiple. `executeLogout()` then
calls `purgeOrphanedFiles()`, which removes state DBs (via
`DiscoverStateDBsForEmail`), drive metadata (via
`DiscoverDriveMetadataForEmail`), and account profiles for both personal and
business CID variants.

## Output Formatting (`format.go`)

Two modes: human-readable (default) and JSON (`--json`). Primary command output goes through `OutputWriter` (default `stdout`), while progress/status/warning text goes through `StatusWriter` (default `stderr`).

All commands with `--json` support use extracted `printXxxJSON(w io.Writer, out T) error` functions that encode via `json.NewEncoder(w)` with 2-space indent. This enables unit testing via `bytes.Buffer` roundtrip without CLI wiring.

Timestamp presentation follows one rule across command families: zero `time.Time` means "unknown". Human-readable output renders that as `unknown`; JSON output emits an empty string instead of a fabricated RFC3339 value or Go's year-0001 zero timestamp.

`whoami`, `drive list`, and `shared` expose account-warning arrays in
JSON output:

- `accounts_requiring_auth`: missing or invalid saved login, or persisted
  sync-time `auth:account` rejection
- `accounts_degraded`: authenticated account whose live discovery could not be
  fully completed after the owning command exhausted its bounded recovery path

For `whoami` and `drive list`, `accounts_degraded` currently means
`drive_catalog_unavailable` after `/me/drives` retry exhaustion. For `shared`,
and for the shared-folder portion of `drive list` / name-based `drive add`, it
means `shared_discovery_unavailable` after live shared-item search fails for an
otherwise authenticated account. Live shared search `unauthorized` is not
degraded; it moves the account into `accounts_requiring_auth` with the
sync-auth-rejected reason.

Human-readable output mirrors that split with separate
`Accounts requiring authentication:` and `Accounts with degraded live discovery:`
sections so degraded discovery never reads like a login failure.

Human-readable one-shot sync output also distinguishes executable work from
directionally deferred work. `No changes detected` is only correct when the
sync report has neither executable actions nor planner-owned deferred counts.
When `--upload-only` or `--download-only` observes work that is real but not
legal to execute in that direction, the CLI renders a `Deferred by mode:`
section and omits the empty `Results:` block.

| Command | JSON function(s) | Schema type(s) |
|---------|------------------|----------------|
| `ls` | `printItemsJSON` | `lsJSONItem` |
| `get` | `printGetJSON`, `printGetFolderJSON` | `getJSONOutput`, `getFolderJSONOutput` |
| `put` | `printPutJSON`, `printPutFolderJSON` | `putJSONOutput`, `putFolderJSONOutput` |
| `rm` | `printRmJSON` | `rmJSONOutput` |
| `mkdir` | `printMkdirJSON` | `mkdirJSONOutput` |
| `stat` | `printStatJSON` | `statJSONOutput` |
| `mv` | `printMvJSON` | `mvJSONOutput` |
| `cp` | `printCpJSON` | `cpJSONOutput` |
| `recycle-bin list` | `formatRecycleBinJSON` | `recycleBinJSONItem` |
| `recycle-bin restore` | `printRecycleBinRestoreJSON` | `recycleBinJSONItem` |
| `whoami` | `printWhoamiJSON` | `whoamiOutput` |
| `drive list` | `printDriveListJSON` | `driveListJSONOutput` |
| `drive search` | `printDriveSearchJSON` | `driveSearchJSONOutput` |
| `status` | `printStatusJSON` | `statusOutput` |

## Signal Handling (`signal.go`)

Two-signal shutdown for watch mode:

- first SIGINT/SIGTERM = cancel the shared shutdown context, let the engine
  seal new admission, and allow already-admitted work to follow the normal
  shutdown path
- second SIGINT/SIGTERM = force process exit with status 1

There is no timer-based escalation between the first and second signal.

Config reload is not signal-based. `pause`, `resume`, and daemon operators use
the sync control socket to request reload from a live watch owner.

## Control Socket (`control_client.go`)

Implements: R-2.8.1 [verified], R-2.8.2 [verified], R-2.9.1 [verified], R-2.9.2 [verified], R-2.9.3 [verified], R-6.3.3 [verified]

Single-instance enforcement and live-daemon mutation routing use the Unix
control socket, not legacy signal/file IPC. The CLI probes `GET /v1/status`
through one shared owner-classification helper instead of collapsing the
result to a bool. That helper returns exactly one of:

- `watch_owner`: live watch daemon, live RPC allowed
- `oneshot_owner`: live foreground sync owner, no live mutation RPC allowed
- `no_socket`: no daemon reachable
- `path_unavailable`: socket path derivation proved no daemon can bind
- `probe_failed`: status/transport/protocol ambiguity at the owner boundary

Mutating sync-adjacent commands use socket RPC only for `watch_owner`.
`oneshot_owner`, `no_socket`, and `path_unavailable` fall back to direct
durable DB intent. `probe_failed` is authoritative and does not fall back.
After a successful `watch_owner` probe, one narrower fallback remains: if the
watch socket disappears between status probe and POST, the CLI falls back to
direct durable intent. Typed daemon application errors and other ambiguous
POST failures are authoritative and are reported without fallback.

`resolve deletes` and `resolve local|remote|both` share one CLI-owned durable
intent routing helper so this policy lives in exactly one place. The helper
decides between watch-mode RPC and direct store mutation; neither command
opens its own ad hoc engine or duplicates socket fallback rules.

The socket path is config-owned. It normally resolves under the app data
directory, with a stable hash-based runtime fallback when isolated test or user
paths would be too long for Unix-domain sockets. If neither the normal data-dir
path nor the hashed runtime fallback fits inside the Unix socket length budget,
`config.ControlSocketPath()` returns an explicit error instead of silently
pretending no socket exists. Sync-owner startup treats that as fatal before the
orchestrator starts. Durable-intent commands treat the same derivation failure
as proof that no live daemon can bind the socket and fall back to direct DB
intent writes. Daemon-reload notifications use the same owner probe, so they
now distinguish "socket path unavailable", "no running daemon", "foreground
one-shot owner", and "control socket probe failed" instead of reporting every
non-watch case as "no daemon".

Socket-routed commands:

- `pause` / `resume`: update config, then `POST /v1/reload` if a daemon is live.
- `resolve deletes`: `POST /v1/drives/{canonical-id}/held-deletes/approve`.
- `resolve local|remote|both`: `POST /v1/drives/{canonical-id}/conflicts/{conflict-id}/resolution-request`.

The CLI never opens an ad hoc sync engine to execute held deletes or conflicts.
It only records intent; the running or next normal sync engine owns the
destructive side effects.

## Logging (`internal/logfile/logfile.go`)

Implements: R-6.6.1 [verified], R-6.6.2 [verified], R-6.6.3 [verified], R-6.6.4 [verified]

Log file creation with parent directory auto-creation. Append mode. Retention-based rotation (`log_retention_days`).

## Production Performance Surfaces

Implements: R-6.6.14 [verified], R-6.6.15 [verified], R-6.6.16 [verified]

- The root runner creates one `internal/perf.Session` per command. Ordinary
  commands emit periodic INFO `performance update` logs every 30 seconds while
  still running, then one final INFO `performance summary` on completion.
- `sync --watch` uses the same session model but with a 5-minute update
  interval so long-lived owners surface periodic production-visible health
  without flooding logs.
- Always-on perf logs intentionally carry aggregate counters and timings only:
  request counts, retry/backoff totals, DB timing, transfer counts/bytes,
  observe/plan/execute/reconcile totals, and watch activity counts. They do
  not log paths, account emails, drive IDs, item IDs, or transfer URLs.
- `status --perf` is a live overlay, not a historical report. It probes the
  active sync owner over the control socket, overlays any returned per-drive
  snapshot onto the ordinary status snapshot, and prints a concrete
  unavailable reason when no live owner or live perf snapshot exists.
- `perf capture` is the explicit deep-dive surface. It probes the live owner,
  validates a bounded capture duration, and prints either a machine-readable
  JSON response or the local artifact paths for the resulting bundle.
- `--full-detail` only widens the explicit capture bundle manifest. It does
  not change the redaction policy of always-on logs or `status --perf`.

## Design Constraints

- Config flows through Cobra context (`CLIContext` stored via `context.WithValue` with unexported key type). No global flag variables.
- Two-phase `PersistentPreRunE`: Phase 1 (all commands) reads flags + creates logger. Phase 2 (data commands only) loads config + resolves drive. Commands skip Phase 2 via `skipConfigAnnotation` in `Annotations`.
- Command handlers are wiring only. Workflow-owned CLI files own the runtime behavior around `CLIContext`: `auth_*.go` handles login/logout/whoami flows, `drive_*.go` handles list/add/remove/search, `resolve_*.go` owns held-delete approval and conflict-request routing, shared/account-catalog helpers own discovery and auth-required projection, `status_*.go` owns account/drive aggregation plus read-only sync-state inspection, sync-control helpers own pause/resume config mutation, `recycle_bin_*.go` owns list/restore/empty flows, `sync_*.go` owns multi-drive command assembly, and `recover_*.go` owns sync-database recovery.
- Shared account-catalog helpers own lenient config loading, warning logging, offline account/auth projection, the best-effort account-identity refresh used before live discovery commands build their catalogs, and the typed drive-list snapshot (`configured`, `available`, `accounts_requiring_auth`, `accounts_degraded`) that drive helpers render.
- Shared discovery helpers are the CLI-owned live-discovery boundary for shared items. They own live search, target normalization, enrichment, deduplication, and auth-vs-degraded classification for `shared`, the shared-folder portion of `drive list`, and name-based `drive add`. They consume whatever refreshed account-catalog slice the caller passes; caller-owned account filtering stays outside this core so `drive list` can remain inventory-consistent while `shared` and name-based `drive add` still honor `--account`. Per-account discovery tries all available token IDs before surfacing auth-required or degraded output. It does not perform its own `/me` reconciliation pass.
- `SessionRuntime` caches `TokenSource`s by token file path — multiple drives sharing an account share one `TokenSource`, preventing OAuth2 refresh token rotation races.
- `CLIContext` owns one `driveops.SessionRuntime` per command/watch runtime. Bootstrap/auth-discovery flows use its bootstrap metadata client. Once both account and remote target identity are known, the runtime chooses target-scoped interactive clients directly: configured drives key by drive ID, configured shared roots key by remote root item, and direct shared-item commands key by `(remoteDriveID, remoteItemID)`. Sync paths request the runtime's non-retrying sync sessions instead. HTTP profile constants stay in `internal/graphhttp`; runtime reuse lives in `driveops`, not `internal/cli`.
- `CLIContext` is the sole owner of command-scoped log-file closers. When sync bootstrap rebuilds the logger from the raw logging config, it swaps the active logger through the CLI context, closes the old closer before installing the replacement, and relies on the top-level `mainWithWriters` runner to close the final active closer exactly once after `Execute()` returns. This top-level cleanup is intentionally outside Cobra post-run hooks so command-log shutdown still happens when a leaf command exits early with an error.
- `CLIContext` is also the sole owner of command-scoped perf sessions. The top-level `mainWithWriters` runner completes the active session after `Execute()` returns so final perf summaries still emit when Cobra returns an error.
- CLI handlers use `cmd.Context()` for signal propagation. Exception: upload session cancel paths use `context.Background()` because the cancel must succeed even when the original context is done.
- Production command code writes primary output through `CLIContext.OutputWriter`, and status/warning/error text through the CLI-owned status writer boundary instead of raw process-global stdout/stderr. This keeps command output injectable in tests and prevents hidden process-global output dependencies from creeping back into helpers, bootstrap warnings, or sync-daemon cleanup.
- `go run ./cmd/devtool verify` enforces that production `internal/cli` files do not call `fmt.Print*`, `fmt.Fprint*(os.Stdout|os.Stderr, ...)`, or direct `os.Stdout` / `os.Stderr` writer methods. The only allowed process-global output boundary is the tiny process entrypoint outside `internal/cli`.
- Browser auth URLs are validated against loopback or Microsoft auth hosts before launching the platform browser command. Validation and launch failures must not echo the full auth URL or any query tokens. The remaining inline `gosec` suppression on the `exec.CommandContext` call is intentional: the command comes from a fixed allowlist, but the linter cannot prove that through the helper boundary.
- Control-socket probing and log file opens use resolved paths from the CLI/config layer; the socket itself is owned by `internal/multisync`.
- If the configured log file cannot be opened, CLI bootstrap warns through the CLI status writer and falls back to console-only logging instead of failing the command before any user-facing work can run.
- Direct `runSync` and helper-level tests cover caller-visible failure paths such as config-load errors, all-drives-paused/no-drives guidance, and log-file-open fallback warnings through the injected status/output writers rather than process-global stderr assumptions.
- The status command uses testable helper seams with narrowed interfaces (`accountMetaReader`, `accountAuthChecker`, `syncStateQuerier`), decoupling status aggregation from Cobra wiring. `status.go` stays thin, `status_snapshot.go` owns account/drive aggregation, `status_sync_state.go` turns store snapshots into CLI payloads, and `status_render*.go` plus `status_issue_descriptors.go` own presentation. CLI code does not open writable SQLite paths for read-only sync-state views.
- Offline auth-health projection uses `sync.HasScopeBlockAtPath` for
  persisted `auth:account` checks, so read-only CLI account discovery no
  longer pays the writable-store checkpoint/close path.
- Control-socket status reporting shares that same read-only store boundary.
  CLI status aggregation and daemon `GET /v1/status` both consume
  `internal/sync` read helpers instead of opening writable `SyncStore` handles
  just to count durable intent.
- Sync-domain issue/status presentation uses the shared
  [`sync.SummaryKey`](../../internal/sync/summary_keys.go)
  contract. `status` consumes the store-owned issue projection instead of
  rebuilding visible-issue math locally.
- `status` now preserves the store-owned issue-group snapshot instead of
  flattening it to totals. Per-drive sync state includes grouped visible issue
  families with `summary_key`, `count`, `scope_kind`, and optional humanized
  `scope` in JSON, while text output renders the same groups under each drive's
  sync-state section before the aggregate retry counters. This is the CLI-side
  contract for `R-2.10.4`.
- `status` also surfaces store-owned durable-intent counters:
  approved deletes waiting plus queued/applying conflict requests. Text output
  adds action-oriented `Next:` hints, while JSON exposes the same counts plus
  per-entry `action_hint` fields and per-drive `next_actions[]`.
- `status` is the explicit call-to-action surface. Each displayed drive's text
  section shows exact `Next:` lines for grouped issues, held deletes, approved
  deletes, unresolved conflicts, queued/applying conflict requests, and damaged
  state stores. The matching JSON contract adds per-entry `action_hint`,
  per-drive `next_actions[]`, and preserves `state_store_recovery_hint` for
  damaged DB flows.
- Informational commands (`drive list`, `status`, `whoami`) use lenient config loading (`LoadOrDefaultLenient`) that collects validation errors as warnings instead of failing. This allows users to inspect their configuration and see drive status even when config has errors. Each of these commands (and `drive search`) must have `skipConfigAnnotation` on the leaf Cobra command — not just the parent — because Cobra checks annotations on the executing command, not parent commands. Safety net: `TestAnnotationTreeWalk` walks the entire command tree and fails if any leaf command with `RunE` is not explicitly classified as either a data command (no annotation) or an annotated command. New commands must be added to the `dataCommands` set or given the annotation. [verified]
- The `perf` command is also a skip-config leaf. Live perf inspection depends only on the control-socket owner and must remain available even if the config file is missing or invalid.
- `loadAndResolve` passes errors from `ResolveDrive` unwrapped. `ResolveDrive` already wraps `LoadOrDefault` errors with `"loading config: "`, and `MatchDrive` errors are user-facing messages that read better without a prefix (e.g., `"no drives configured — ..."` instead of `"loading config: no drives configured — ..."`). [verified]
- CLI presentation is the final error boundary. `classifyCommandError` and `commandFailurePresentationForClass` map the domain classes from [error-model.md](error-model.md) to process exit behavior and user-facing reason/action text, while `authErrorMessage` specializes the user-facing auth copy for saved-login failures without re-inspecting raw transport payloads.
- The root error boundary maps both `graph.ErrNotLoggedIn` and `graph.ErrUnauthorized` into the same user-facing family, `Authentication required`, with cause-specific detail and a shared remediation path (`onedrive-go login`).
- Offline auth surfaces (`status`) never mutate `auth:account`. Live proof surfaces clear `auth:account` only after an authenticated Graph response succeeds. Pre-authenticated upload/download URL success is not treated as auth proof.
- The authenticated-success proof recorder logs one attributed scope-repair event per account and proof source (`whoami`, `drive-list`, `drive-search`, `drive-session`) when it clears stale `auth:account` blocks. It does not log every successful request and does not create a persistent audit trail.
- Checked-in golden tests lock the human and JSON output shapes for `status`. Formatting changes are intentional and use the standard `-update` flow in `internal/cli/golden_test.go`.
- `internal/cli` unit test coverage is above the current target: `go test ./internal/cli/... -cover` reports 67.8%. [verified]

## Status And Resolve

Implements: R-2.3.3 [verified], R-2.3.4 [verified], R-2.3.5 [verified], R-2.3.6 [verified], R-2.3.7 [verified], R-2.3.8 [verified], R-2.3.9 [verified], R-2.3.10 [verified], R-2.3.12 [verified], R-2.14.3 [verified], R-2.14.5 [verified], R-6.6.11 [verified], R-6.6.15 [verified]

- **Grouped display**: >10 failures of same `issue_type` → single heading with count, first 5 paths shown. `--verbose` shows all paths.
- **Per-scope sub-grouping**: 507 quota and shared-folder write blocks are grouped by scope (own drive vs each shortcut). Different scopes = different owners = different user actions.
- **Human-readable names**: Shortcut-scoped failures display local path name, not internal drive IDs.
- **Scope-aware reason/action copy**: Failure text is selected from `issue_type` plus the raw scope key, so shortcut-scoped quota failures say the shared-folder owner is out of space instead of implying the user's own drive is full.
- **Single read surface**: `status` is the only sync read command. It always renders the same per-drive sync-health contract for the displayed drives, and `--drive` only filters which drives are shown.
- **Remote drift line**: `status` shows already-observed remote-side drift in
  exactly two visible places: the per-drive `Remote drift: N items` line and
  the aggregate summary's `N remote drift` count. That number means "remote
  truth has moved ahead of baseline," not "all pending sync work."
- **Strict command grammar**: CLI mutation lives under one explicit noun, `resolve`, with separate actions for delete approval (`resolve deletes`) and conflict requests (`resolve local|remote|both [path-or-id]`, optional `--all`). Recovery lives under one explicit noun, `recover`.
- **JSON shape**: `status --json` always emits one top-level `summary` plus `accounts[]`, and each displayed drive may include one nested `sync_state` object with `issue_groups`, `delete_safety`, `delete_safety_total`, `conflicts`, `conflicts_total`, optional `conflict_history`, `conflict_history_total`, `examples_limit`, `verbose`, per-entry action hints, and per-drive `next_actions`.
- **Live perf overlay**: `status --perf` augments that same per-drive snapshot with live sync-owner perf when available. Displayed drives gain `sync_state.perf` in JSON and a `PERF` section in text. When no owner or no live snapshot exists, the CLI emits `perf_unavailable_reason` instead of inventing persisted history.
- **Derived shared-folder issues**: `perm:remote` is displayed from held blocked-write rows, not from a standalone boundary issue. The CLI shows one visible issue per denied boundary only while blocked write intent still exists.
- **Automatic shared-folder recovery**: shared-folder write blocks have no manual CLI controls. The engine rechecks permission state automatically during normal sync/watch passes while blocked writes still exist.
- **Shared summary keys, CLI-owned descriptors**: Every sync issue is grouped by the shared `SummaryKey`, while CLI-owned descriptor tables in `status_issue_descriptors.go` turn that key into user-facing title/reason/action text. This keeps logs, store projections, and `status` aligned on grouping without making `internal/sync` own CLI copy.
- **Store-owned snapshot**: `status` renders one store-owned `DriveStatusSnapshot` per displayed drive, while narrower read-only helpers serve aggregate or daemon-only projections. The CLI does not rebuild issue/conflict/delete-safety state locally.
- **Auth scope display**: `auth:account` renders as an account-level `Authentication required` issue with no path list.
- **Held-delete approval**: held deletes remain visible under detailed `status`, and `resolve deletes` moves only that drive's held-delete rows from `held` to `approved`. The engine consumes approved rows after successful matching delete execution.
- **Conflict split**: `status` lists unresolved conflicts from the fact table, `status --history` includes resolved conflicts, and `resolve local|remote|both` queues the keep-local/keep-remote/keep-both request for engine-owned execution. Active request workflow is visible as request metadata on unresolved conflicts, not as a separate top-level noun.
- **Replay-safe mutations**: `resolve deletes` and repeated `resolve local|remote|both` calls are replay-safe. Repeating an approval or a resolution request does not create duplicate durable state, and queued conflict requests remain mutable only until the engine begins applying them.
