# R-2 Sync

Bidirectional file synchronization between a local directory and OneDrive.

## R-2.1 Sync Modes [verified]

- R-2.1.1: When the user runs `sync`, the system shall perform one-shot bidirectional sync. [verified]
- R-2.1.2: When `--watch` is passed, the system shall run continuously, detecting changes via filesystem events (inotify/FSEvents) and remote delta polling. [verified]
- R-2.1.3: When `--download-only` is passed, the system shall only download remote changes. [verified]
- R-2.1.4: When `--upload-only` is passed, the system shall only upload local changes. [verified]
- R-2.1.5: When `--dry-run` is passed, the system shall preview operations without executing. [verified]
- R-2.1.6: When `--full` is passed, the system shall perform full reconciliation (fresh delta enumeration + orphan detection). [verified]

## R-2.2 Conflict Detection [verified]

When the same file has been modified on both the local filesystem and OneDrive since the last successful sync, the system shall detect a conflict.

- R-2.2.1: The system shall use content hash comparison (QuickXorHash) against the baseline as the primary conflict signal. [verified]
- R-2.2.2: The system shall use mtime as a fast-path optimization (skip hashing when timestamps match baseline). [verified]

## R-2.3 Conflict Resolution [verified]

- R-2.3.1: The default resolution shall preserve both versions: remote wins the original path, local version is renamed to `<name>.conflict-<timestamp>.<ext>`. [verified]
- R-2.3.2: The system shall persistently record conflicts with metadata (path, timestamp, hashes, final resolution status) and keep active conflict-resolution requests, including the last failed layout-establishment error, as separate durable user intent. [verified]
- R-2.3.3: When the user runs `status` with exactly one selected drive, the system shall present one detailed per-drive read view that includes ordinary issue groups, delete-safety entries, unresolved conflicts, and state-store health. [verified]
- R-2.3.4: When the user runs `status --history` with exactly one selected drive, the system shall include resolved conflict history in that detailed view. [verified]
- R-2.3.5: When the user runs `resolve local <path>`, `resolve remote <path>`, or `resolve both <path>`, the system shall durably request one resolution strategy (`keep-local`, `keep-remote`, `keep-both`) and the next running sync engine shall execute that request. If a daemon is running the request shall go through the control socket; otherwise it shall be written directly to the drive state DB. `resolve ... --all` shall record the same strategy for all unresolved conflicts on the selected drive. [verified]
- R-2.3.6: When the user runs `resolve deletes`, the system shall mark all currently held delete-safety entries for the selected drive as `approved` without affecting other issue types. If a watch daemon is running the approval shall go through the control socket; otherwise it shall be written directly to the drive state DB. Approved deletes shall execute only when the planned delete matches drive, action type, path, and item ID. Approved rows for a reused path shall be pruned by the engine only when a live plan proves the same drive/action/path now targets a different item ID. [verified]
- R-2.3.7: When the single-drive `status` view encounters more than 10 failures of the same issue type, the system shall group them under a single heading with count and show the first 5 paths. When `--verbose` is passed, the system shall show all paths. [verified]
- R-2.3.8: When displaying scope-level issues where drives have independent scopes (507 quota, shared-folder write blocks), the system shall sub-group by scope (own drive vs each shortcut). [verified]
- R-2.3.9: When displaying shortcut-scoped failures, the system shall use the shortcut's local path name (human-readable), not internal drive IDs or scope keys. [verified]
- R-2.3.10: When `--json` is passed to single-drive `status`, the detailed read model shall expose structured arrays for `issue_groups`, `delete_safety`, and `conflicts`, plus optional `conflict_history` when `--history` is selected. [verified]
- R-2.3.11: Shared-folder write blocks shall have no manual CLI retry or recheck command. The system shall revalidate them automatically during normal sync/watch permission checks while blocked writes still exist. [verified]
- R-2.3.12: Repeated `resolve deletes` and repeated conflict-resolution attempts shall be replay-safe. Repeating the same mutation shall either be a no-op or return a stable already-queued/already-resolved result, without duplicate durable effects or partial scope release. Concurrent conflict requests are last-write-wins while queued, but once a request is `applying` or the conflict is already resolved the current engine-owned state is authoritative. [verified]

## R-2.4 Filtering [verified]

Filter settings support global defaults with per-drive overrides. The
`skip_*` filters remain local-observation-only, while `ignore_marker` and
`sync_paths` define bidirectional sync scope. Remote observation still trusts
the server at the raw delta boundary; sync scope is applied after observation
so out-of-scope items become filtered state instead of being silently erased
from durable truth. Generation tracking and pending re-entry are based on the
persisted scope projection rather than unrelated metadata.

- R-2.4.1: When `skip_dotfiles = true`, the system shall exclude files and folders starting with `.`. [verified]
- R-2.4.2: When `skip_dirs` is set, the system shall exclude matching directory names. [verified]
- R-2.4.3: When `skip_files` is set, the system shall exclude matching file patterns. [verified]
- R-2.4.4: When a directory contains a file matching the `ignore_marker` name (default `.odignore`), the system shall exclude that directory from sync. The marker file is a presence-only check — its contents are not read. The marker file itself is not synced. Marker create/delete/rename shall update the effective sync scope without fabricating deletes for items that merely became out-of-scope. [verified]
- R-2.4.5: When `sync_paths` is set, the system shall sync only the specified paths. Scope shrink shall stop managing excluded paths without synthesizing deletes. Scope expansion shall trigger targeted remote re-entry reconciliation so previously filtered remote items can become active again. [verified]
- R-2.4.6: When `skip_symlinks = true`, the system shall exclude symlinks. When `skip_symlinks = false` (default), the system shall follow symlink targets and observe them as ordinary files and directories at the symlink path. Directory-symlink cycles shall stop at the alias boundary instead of recursing forever. [verified]
- R-2.4.7: When an item belongs to the Personal Vault, the system shall exclude it. Vault auto-locks after 20 minutes, causing locked items to appear deleted in delta responses — syncing vault items would cause data loss. [verified]

## R-2.5 Crash Recovery [verified]

- R-2.5.1: When the process is killed mid-sync, the next run shall resume cleanly from the last checkpoint. [verified]
- R-2.5.2: The sync state store shall provide durable, transactional writes that survive process kill. [verified]
- R-2.5.3: On startup, the system shall detect items stuck in `syncing` state and reset them for re-planning (reconciler). [verified]
- R-2.5.4: When `ResetInProgressStates` resets items to pending state, the system shall create corresponding `sync_failures` entries so the `FailureRetrier` can rediscover and re-process them. Without this bridge, items that crashed mid-execution become zombies — the delta token was already advanced, so no new events arrive. [verified]
- R-2.5.5: The product shall provide one supported `recover` command that performs store-owned sync-state recovery: it first attempts deterministic safe in-place repair, then rebuilds the DB while preserving recoverable durable user intent, and finally resets the DB from scratch if salvage is impossible. The recovery path shall report what it did and shall require explicit user confirmation before mutation. [verified]
- R-2.5.6: The sync state DB shall use an embedded goose migration history, reject existing state stores that contain user tables without migration history, and provide clear rebuild/migrate guidance instead of silently guessing at or erasing durable user intent. [verified]

## R-2.6 Pause / Resume [verified]

- R-2.6.1: When the user runs `pause [duration]`, the system shall stop data transfers while continuing to track changes. [verified]
- R-2.6.2: When the user runs `resume`, the system shall resume transfers with a complete picture of accumulated changes. [verified]

## R-2.7 Verification [verified]

The repository shall provide an internal verification capability that re-hashes
local files against the sync baseline for tests and developer diagnostics. It
is not part of the normal product CLI.

- R-2.7.1: The internal verification capability shall expose structured mismatch data with verified count and per-path discrepancies. [verified]

## R-2.8 Watch Mode Behavior [verified]

- R-2.8.1: The system shall reload `config.toml` on control-socket reload request. [verified]
- R-2.8.2: The system shall use the Unix control socket as the single live sync-owner lock. [verified]
- R-2.8.3: The system shall support two-signal shutdown. First SIGINT/SIGTERM cancels watch mode, seals new work admission, and lets already-admitted work follow the normal shutdown path; second signal forces immediate exit. [verified]
- R-2.8.4: The system shall run periodic full reconciliation (default every 24 hours) to detect missed delta deletions. [verified]
- R-2.8.5: The system shall support WebSocket subscription for near-instant remote change notification. [verified]

## R-2.9 RPC / Control Socket [verified]

- R-2.9.1: When running `sync` or `sync --watch`, the system shall own a JSON-over-HTTP API on a Unix domain socket so other sync owners cannot run concurrently. One-shot owners expose status and reject mutating RPCs with a typed foreground-sync-running response; watch owners accept mutation RPCs. [verified]
- R-2.9.2: The RPC API shall support `GET /v1/status`, `POST /v1/reload`, and `POST /v1/stop`. [verified]
- R-2.9.3: The RPC API shall support durable user-intent mutations for held-delete approval and conflict-resolution requests in watch mode, use typed `{status, code, message}` application errors, and report pending durable-intent counts in status. [verified]

## R-2.10 Failure Management [verified]

Failure tracking, scope-based classification, and lifecycle management. Each failure is scoped to a key (file, directory, drive/shortcut, account, service) that determines retry policy and blast radius.

- R-2.10.1: When a transfer fails with HTTP 507, the system shall classify it as an actionable failure with issue type `quota_exceeded`, scoped to the quota owner (`quota:own` for own-drive, `quota:shortcut:$remoteDrive:$remoteItem` for shortcuts). The system shall not retry 507 at the transport level. [verified]
- R-2.10.2: When a user resolves a file-scoped actionable failure (by renaming, moving, or deleting the file), the system shall automatically detect the resolution on the next scanner pass and remove the stale failure entry. [verified]
- R-2.10.3: The system shall detect scope-level failure patterns: 429 immediate target-scope from a single response; 507 after 3 consecutive from different files in 10s; 5xx after 5 consecutive from different files in 30s; 503 with Retry-After immediate service-scope. Scope blocks shall prevent dispatching actions in the affected scope only. [verified]
- R-2.10.4: When displaying sync status, the system shall show grouped visible issue families per drive with failure scope context (file, directory, drive/shortcut, account, service) alongside retry information, identifying which drive or shortcut is affected for per-drive scopes. Text output shall render those groups human-readably, and `--json` shall preserve them structurally with summary key, count, scope kind, and optional humanized scope label. Status shall also surface durable-intent counts for approved deletes and conflict-resolution requests, plus action-oriented next-step hints in text/JSON. [verified]
- R-2.10.5: When a scope block is active, the system shall test for recovery by periodically releasing one real action from the held queue as a trial. On success: clear the scope block and release all held actions. On failure that proves the same scope still persists: re-hold and extend the trial interval. On inconclusive trial outcomes: preserve the original scope at the existing interval, keep or re-home candidate state according to the more specific failure, and do not treat missing usable candidates as proof of recovery. [verified]
- R-2.10.6: For `quota_exceeded` scope blocks, trial timing shall follow R-2.10.14 (unified timing). Detection: 507 sliding window (3 unique paths / 10s). [verified]
- R-2.10.7: For `rate_limited` scope blocks, trial timing shall start at the `Retry-After` duration from the server response (no cap — server is ground truth). The scope block shall affect all action types for the exact throttled target only. When a persisted `throttle:target:*` scope was created from server `Retry-After`, restart shall preserve the deadline and schedule a trial when it expires rather than auto-clearing the scope. Legacy persisted `throttle:account` rows shall be released at startup instead of being preserved or migrated. [verified]
- R-2.10.8: For `service_outage` scope blocks, trial timing shall follow R-2.10.14 (default local timing). When `Retry-After` is present, it overrides per R-2.10.7. Detection: 5xx sliding window (5 unique paths / 30s) or 503+Retry-After immediate. Server-timed `service` scopes shall survive restart exactly like server-timed `throttle:target:*` scopes. [verified]
- R-2.10.9: When `recheckPermissions()` discovers a previously-denied folder is now writable, the system shall clear the scope block and release all held actions under that folder immediately. [verified]
- R-2.10.10: When the scanner observes a previously-blocked file or directory is now accessible, the system shall clear the failure and release held actions. [verified]
- R-2.10.11: When a scope block clears (via trial success or recheck), the system shall release all held actions immediately with `NotBefore` set to now. Individual action `failure_count` shall be preserved. [verified]
- R-2.10.12: When a local file operation fails with `os.ErrPermission`, the system shall check parent directory accessibility. If the directory is inaccessible: record one `local_permission_denied` at directory level and suppress operations under it. If the directory is accessible: record at file level. [verified]
- R-2.10.13: The system shall recheck `local_permission_denied` directory-level issues at the start of each sync pass, and auto-clear when the directory becomes accessible. [verified]
- R-2.10.14: Trial timing uses default constants of 5 seconds initial, 2× backoff, 5-minute cap for locally timed quota/service scopes. Server-provided Retry-After values are honored exactly with no cap (server is ground truth). Preserve outcomes re-arm the current interval without incrementing `trial_count`. For scoped-failure-backed restart policies, preserve durability is stored on `scope_blocks` so restart does not drop a preserved scope solely because candidate rows changed shape. Scope-specific overrides may define different local timing curves, such as `disk:local` in R-2.10.43. [verified]
- R-2.10.15: When a scope block is set, at most `transfer_workers` actions may be in-flight. These shall complete normally and route through standard retry. [verified]
- R-2.10.16: Every `WorkerResult` shall carry target drive identity (`TargetDriveID`, `ShortcutKey`). Own-drive actions use empty `ShortcutKey`; shortcut actions use `remoteDrive:remoteItem`. [verified]
- R-2.10.17: Scope routing shall use target drive context for 507/403/429 scope keys. 429 shall route to `throttle:target:*` using the failed request's own-drive or shared-target identity. 5xx shall continue to route to `service` regardless of target drive. [verified]
- R-2.10.18: Sliding windows shall be independent per scope key. Own-drive 507s shall not count toward shortcut scope windows, and vice versa. [verified]
- R-2.10.19: When 507 occurs on own-drive, scope key `quota:own` shall block own-drive uploads only. Shortcut uploads, downloads, deletes, and moves shall continue. [verified]
- R-2.10.20: When 507 occurs on a shortcut, scope key `quota:shortcut:$remoteDrive:$remoteItem` shall block that shortcut's uploads only. Own-drive and other shortcut operations shall continue. [verified]
- R-2.10.21: Trial actions for `quota:own` shall select own-drive uploads. Trial actions for `quota:shortcut:*` shall select uploads targeting that shortcut. [verified]
- R-2.10.22: The detailed single-drive `status` view shall identify shortcut-scoped 507 by local path name (e.g., "Shared folder 'Team Docs'"), not opaque drive IDs. [verified]
- R-2.10.23: When 403 occurs on a shortcut, the scope boundary shall be that shortcut's `RemoteDriveID`. Permission boundary walking shall use the shortcut's drive, not the primary drive. [verified]
- R-2.10.24: When 403 occurs on shortcut A, it shall not affect shortcut B. Each shortcut has independent permissions from an independent owner. [verified]
- R-2.10.25: When a shortcut root itself is read-only, the system shall record a scope block at shortcut root level without walking above the shortcut boundary. [verified]
- R-2.10.26: When 429 occurs, the system shall block all action types only on the exact remote target implicated by the failed request. Own-drive traffic shall block `throttle:target:drive:$targetDriveID`; shortcut/shared-root traffic shall block `throttle:target:shared:$remoteDrive:$remoteItem`. Other drives and shortcuts shall continue unless they independently throttle. [verified]
- R-2.10.27: When a 429 target scope clears, only the held actions for that exact throttled target shall be released. Unrelated drives and shortcuts shall remain unaffected. [verified]
- R-2.10.28: When 5xx scope blocks are active, they shall affect all drives including shortcuts, since the Graph API is shared infrastructure. [verified]
- R-2.10.29: The service-scope sliding window shall accept 5xx from any target drive. Five consecutive 5xx from different drives within 30s shall trigger a block. [verified]
- R-2.10.30: During `service` scope blocks, the system shall suppress shortcut observation polling globally. During `throttle:target:shared:*` scope blocks, it shall suppress observation only for that exact shared target. `throttle:target:drive:*` shall not suppress other shortcut observation. [verified]
- R-2.10.31: During `quota:shortcut:*` scope blocks, observation of that shortcut shall continue (read-only). Other observations shall be unaffected. [verified]
- R-2.10.32: The `status` command shall show per-scope block status as separate entries per drive/shortcut, preserving separate visible issue groups even when the summary key is the same. [verified]
- R-2.10.33: The `sync_failures` table shall store a `scope_key` column for scope-level failures, enabling detailed `status` grouping without re-deriving scope. [verified]
- R-2.10.34: The `scope_key` format shall be: `auth:account`, `quota:own`, `quota:shortcut:$remoteDrive:$remoteItem`, `perm:remote:{localPath}`, `perm:dir:{localPath}`, `throttle:target:drive:$targetDriveID`, `throttle:target:shared:$remoteDrive:$remoteItem`, `service`, `disk:local`. Persisted scope keys use stable internal identifiers where required for correctness; human-readable naming for `status` output is derived at display time from shortcut metadata. Legacy `throttle:account` keys may still be parsed during startup repair, but new runtime state shall not persist them. [verified]
- R-2.10.35: Engines shall not coordinate scope blocks across engine boundaries. Each engine shall discover scope conditions independently. [verified]
- R-2.10.36: When 429 is discovered independently per engine (same token), no shared state shall be required. [verified]
- R-2.10.37: Shortcut scope blocks shall be engine-internal. A shortcut in Engine A shall have no effect on Engine B's shortcuts. [verified]
- R-2.10.38: When a shortcut is removed while a scope block exists for it, the system shall clear the block and discard held actions. [verified]
- R-2.10.39: Two shortcuts to the same sharer's drive shall have independent scope keys. A 507 on one shall not auto-block the other. [verified]
- R-2.10.40: Permission boundary walking on shortcuts shall not walk above the shortcut root. The shortcut root is the natural boundary. [verified]
- R-2.10.41: When a download, delete, or move action succeeds, the system shall clear any corresponding `sync_failures` entry for that path. [verified]
- R-2.10.42: The scope detection sliding window shall accept results from concurrent workers. A success from any path in the scope shall reset the unique-path failure counter, preventing false scope blocks from interleaved results. [verified]
- R-2.10.43: When available disk space falls below `min_free_space`, the system shall set a `disk:local` scope block suppressing all downloads. Trial timing shall start at 5 minutes, double on failure, with a maximum of 1 hour. [verified]
- R-2.10.44: When available disk space is above `min_free_space` but below file size plus `min_free_space`, the system shall record a per-file failure without scope escalation. Smaller files that fit within available space may still download. [verified]
- R-2.10.45: When a worker result returns HTTP 401, the system shall activate scope key `auth:account` in `scope_blocks` with issue type `unauthorized` and terminate the current one-shot pass or watch session. It shall not fabricate a per-path `sync_failures` row for the 401. Trial 401 results shall not be treated as proof that the blocked scope persists or recovered. CLI presentation of `auth:account` shall remain account-level and shall not synthesize fake path rows. [verified]
- R-2.10.46: When a persisted `auth:account` scope exists at startup, the system shall revalidate it exactly once with `DriveVerifier.Drive(ctx, driveID)`. Successful proof shall clear the scope and continue startup. Unauthorized proof shall keep the scope and abort startup. Non-auth probe failures, or a missing `DriveVerifier`, shall leave the scope untouched and abort startup. [verified]
- R-2.10.47: Offline read-only CLI surfaces (`status`) shall never mutate persisted `auth:account`. Successful authenticated live CLI proof surfaces may clear persisted `auth:account` for the proved account after the first successful authenticated Graph response. Pre-authenticated upload or download URL success shall not count as proof. [verified]

## R-2.11 Filename Validation [verified]

The system shall validate filenames against OneDrive naming restrictions before upload and during remote observation.

- R-2.11.1: The system shall reject local files with characters invalid on OneDrive (`"`, `*`, `:`, `<`, `>`, `?`, `/`, `\`, `|`) before upload. [verified]
- R-2.11.2: The system shall reject local files with OneDrive reserved names (case-insensitive): `.lock`, `desktop.ini`, `CON`, `PRN`, `AUX`, `NUL`, `COM0`-`COM9`, `LPT0`-`LPT9`. [verified]
- R-2.11.3: The system shall reject local files matching OneDrive reserved patterns: names starting with `~$`, names containing `_vti_`, and `forms` at root level on SharePoint drives. [verified]
- R-2.11.4: The system shall reject local files with trailing dots or leading/trailing whitespace. [verified]
- R-2.11.5: When the scanner filters a file due to naming restrictions, the system shall record it as an actionable issue (not silently skip with only a DEBUG log). [verified]

## R-2.12 Case Collision Handling [verified]

- R-2.12.1: Before uploading, the system shall detect local case-insensitive filename collisions (e.g., `file.txt` vs `File.txt`) — including collisions between a new local file and an already-synced file in the baseline — and flag them as conflicts rather than attempting upload. [verified]
- R-2.12.2: When a case collision is detected, neither colliding file nor directory shall be uploaded until the collision is resolved. Children of a colliding directory shall also be suppressed, since the parent path is unresolvable. In watch mode, collision detection applies to both files and directories, with N-way peer tracking for immediate re-emission on delete resolution. [verified]

## R-2.13 Unicode Normalization [verified]

- R-2.13.1: The system shall normalize filenames to NFC form before comparison, to handle macOS NFD paths correctly. [verified]

## R-2.14 Read-Only Shared Items [verified]

- R-2.14.1: When a write to a shared item returns HTTP 403, the system shall call the Graph permissions API to confirm whether the folder is truly read-only, fail open when the evidence is writable or inconclusive, and walk parent folders to the highest denied ancestor without walking above the shortcut root. [verified]
- R-2.14.2: When confirmed read-only shared-folder state still has blocked remote-mutating work, the planner shall treat that subtree as download-only — suppressing uploads, folder creates, remote moves, and remote deletes while still allowing downloads. [verified]
- R-2.14.3: The system shall surface read-only shared-folder state in detailed `status` only while blocked remote-mutating intent exists, grouping one visible issue per denied boundary and forgetting the state immediately when the last blocked write disappears. [verified]
- R-2.14.4: At the start of each sync pass, the system shall recheck visible read-only shared-folder scopes against the Graph API and release them when write access is restored or the evidence is inconclusive. [verified]
- R-2.14.5: Read-only shared-folder state shall not expose manual retry or manual recheck commands. Recovery shall happen only through automatic permission revalidation and normal scope release when blocked writes disappear. [verified]

## R-2.15 Delta Checkpoint Integrity [verified]

- R-2.15.1: The system shall track individual item failures independently of the delta token, since the delta checkpoint only appears on the final page and cannot be partially committed. [verified]

## R-2.16 Eventual Consistency [verified]

- R-2.16.1: The system shall not re-query file metadata immediately after upload, as OneDrive properties may be temporarily in flux during server-side processing (thumbnails, indexing). [verified]
- R-2.16.2: Incremental one-shot sync shall treat remote delta visibility as eventually consistent. A direct REST item read succeeding elsewhere is not proof that the current incremental delta pass will already observe the same mutation. [verified]
- R-2.16.3: Default one-shot `sync` shall not add hidden settle loops to chase delta lag. The documented one-shot strong-freshness mode remains `sync --full`, which enumerates remote truth instead of relying on a single incremental delta pass. [verified]
