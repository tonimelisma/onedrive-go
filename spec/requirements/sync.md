# R-2 Sync

Bidirectional file synchronization between a local directory and OneDrive.

## R-2.1 Sync Modes [verified]

- R-2.1.1: When the user runs `sync`, the system shall perform one-shot bidirectional sync. [verified]
- R-2.1.2: When `--watch` is passed, the system shall run continuously, detecting changes via filesystem events (inotify/FSEvents) and remote delta polling. [verified]
- R-2.1.3: When `--download-only` is passed, the system shall still observe both local and remote truth, but it shall only execute remote-to-local reconciliation work. Local-to-remote mutations shall remain deferred until a mode that permits them. Real two-sided conflicts shall still be surfaced as conflicts; `--download-only` shall not authorize remote changes to silently overwrite divergent local content. [verified]
- R-2.1.4: When `--upload-only` is passed, the system shall still observe both local and remote truth, but it shall only execute local-to-remote reconciliation work. Remote-to-local mutations shall remain deferred until a mode that permits them. Real two-sided conflicts shall still be surfaced as conflicts; `--upload-only` shall not authorize local changes to silently overwrite divergent remote content. [verified]
- R-2.1.5: When `--dry-run` is passed, the system shall preview operations without executing. [verified]
- R-2.1.6: When `--full` is passed, the system shall perform a full remote refresh (fresh delta enumeration + orphan detection). [verified]

## R-2.2 Conflict Detection [verified]

When the same file has been modified on both the local filesystem and OneDrive since the last successful sync, the system shall detect a conflict.

- R-2.2.1: The system shall use content hash comparison (QuickXorHash) against the baseline as the primary conflict signal. [verified]
- R-2.2.2: The system shall use mtime as a fast-path optimization (skip hashing when timestamps match baseline). [verified]

## R-2.3 Conflict Resolution [verified]

- R-2.3.1: The default resolution shall preserve both versions: remote wins the original path, local version is renamed to `<name>.conflict-<timestamp>.<ext>`. [verified]
- R-2.3.2: Conflict handling shall be engine-owned and immediate. The product shall not persist conflict-request mailboxes or separate user-decision workflows for conflict resolution. [verified]
- R-2.3.3: When the user runs `status`, the system shall present one per-mount read view for every displayed mount, including all durable sync conditions and sync-state snapshots. `--drive` shall only filter which configured parent drives are displayed; it shall not switch `status` into a different output shape, and selected parent drives shall include their attached child mounts. [verified]
- R-2.3.4: The product shall not expose a separate history-only CLI surface for resolved conflicts. Conflict preservation is immediate executor behavior, not a durable user-facing history workflow. [verified]
- R-2.3.5: For edit/edit and create/create conflicts, the engine shall preserve both versions by renaming the local loser to a conflict copy and restoring the remote winner at the canonical path. [verified]
- R-2.3.6: For local-edit vs remote-delete conflicts, the engine shall auto-resolve with local-wins behavior by uploading local content to recreate the remote item. [verified]
- R-2.3.7: When `status` encounters more than 10 conditions of the same type for a displayed mount, the system shall group them under a single heading with count and show the first 5 paths. When `--verbose` is passed, the system shall show all paths. Default sampling shall apply equally to text and JSON output. [designed]
- R-2.3.8: When displaying scope-level conditions where drives have independent scopes, the system shall preserve those groups distinctly within each drive's status output. Separately configured standalone shared-folder drives appear as separate drives rather than embedded nested scopes. [verified]
- R-2.3.9: When displaying failures for a separately configured shared-folder mount, the system shall use that mount's user-facing identity rather than opaque internal identifiers. [verified]
- R-2.3.10: When `--json` is passed to `status`, the JSON contract shall stay the same regardless of whether `--drive` is used. Each displayed mount's nested `sync_state` shall expose structured `conditions` together with sampling metadata (`examples_limit`, `verbose`) and sampled-section totals. [verified]
- R-2.3.11: Shared-folder write blocks shall have no manual CLI retry or recheck command. The system shall revalidate them automatically during normal sync/watch blocker trials while blocked writes still exist. [verified]
- R-2.3.12: The product shall not expose replayable manual conflict-resolution or delete-approval mutations. Conflict preservation and permission recovery remain engine-owned automatic behavior. [verified]

## R-2.4 Observation Boundaries [verified]

Observation covers the whole configured drive root. The product no longer
supports user-configured bidirectional path narrowing or marker-file scope
control. Separately configured shared folders remain supported as their own
drives.

- R-2.4.1: Removed path-filtering configuration keys shall be rejected as unknown configuration rather than silently ignored. [verified]
- R-2.4.2: Observation shall not invent out-of-scope filtered remote state; separately configured standalone shared-folder drives are independent drives with independent observation roots. [verified]
- R-2.4.3: The parent sync engine shall suppress embedded shared-folder link and shortcut placeholder items from normal content planning and surface them as topology facts. The namespace control plane owns automatic child-mount lifecycle for those facts, and no control-plane path shall rediscover parent-drive remote state through Graph. [verified]
- R-2.4.4: Observation shall cover the whole configured local root without marker-file-driven scope changes. [verified]
- R-2.4.5: The product shall not expose user-configured bidirectional path subsets inside a drive. [verified]
- R-2.4.6: Symlink targets shall be observed at the alias path with cycle protection; there is no user-configurable symlink-skip switch. [verified]
- R-2.4.7: When an item belongs to the Personal Vault, the system shall exclude it. Vault auto-locks after 20 minutes, causing locked items to appear deleted in delta responses — syncing vault items would cause data loss. [verified]
- R-2.4.8: For selected standalone namespace mounts, the parent engine shall be the only parent-drive Graph remote-state observer and shortcut alias mutator. It shall surface OneDrive shortcut placeholders whose `remoteItem` points at a folder as parent-owned topology facts instead of content events, persist parent shortcut-root state in its sync store, and keep protected child-root paths suppressed from parent content planning. Complete topology batches are authoritative even when empty, while empty incremental topology batches are non-events. The control plane shall consume parent-declared child topology, cache it only for runner orchestration, start runnable children, skip parent-blocked children, and final-drain retiring children. Successful child final drain shall be acknowledged to the live parent engine; the parent must release its protected alias root or promote a waiting same-path replacement before multisync stops and forgets the retiring child runner. A previously materialized child root shall store a filesystem identity; same-parent local shortcut alias rename is supported and mutates only the shortcut alias while preserving the child mount/state DB; local alias delete removes only the shortcut alias and never target content; cross-parent behavior is unsupported by design and shall not compare children across parent engines. Ambiguous/unsafe identity matches or mutation failures shall stay protected and surface concise status with the issue, protected paths, auto-retry status, and required user action when any is needed. If persisting parent topology or parent shortcut-root lifecycle state fails, the parent engine shall not commit the remote observation cursor for those facts, startup shall continue durable standalone parents and unchanged durable children, and recovery shall retry from durable truth instead of blocking unrelated mounts. [verified]
- R-2.4.9: Managed child mounts shall own independent engine state and nested status rows. They shall not synthesize or add explicit `shared:` drive sections to `config.toml`; separately configured shared-folder drives remain standalone configured mounts. [verified]
- R-2.4.10: Shortcut projection conflicts shall be durable parent-owned or global lifecycle facts. Duplicate automatic child projections for the same namespace/content root are marked by the parent engine, and an explicit standalone shared-folder mount for the same content root suppresses the automatic child projection in the control plane because that requires global mount-graph knowledge. Local child-root file, final-symlink, symlinked-ancestor, or traversal collisions are recorded in parent `shortcut_roots` instead of letting the child engine start on an unsafe path. [verified]

## R-2.5 Crash Recovery [verified]

- R-2.5.1: When the process is killed mid-sync, the next run shall resume cleanly by reloading durable truth (`baseline`, `local_state`, `remote_state`, `observation_issues`, `retry_work`, `block_scopes`, `observation_state`, and catalog-owned account auth state), reobserving current state, and replanning from that truth. [designed]
- R-2.5.2: The sync state store shall provide durable, transactional writes that survive process kill. [verified]
- R-2.5.3: On startup, the system shall not require persisted in-progress lane state to recover interrupted work. Incomplete actions shall be rediscovered from durable truth plus transfer/session artifacts and then replanned normally. [verified]
- R-2.5.4: When a prior run has already advanced remote observation, later runs shall still rediscover already-observed remote drift from durable `remote_state` and reconcile it without requiring synthetic pending-state bridge rows or fresh delta events. [verified]
- R-2.5.5: When an existing per-drive state DB cannot be opened under the current schema or store generation, sync startup shall not destroy or recreate it automatically. Startup shall skip that drive, keep read-only inspectors non-mutating, and surface explicit guidance to pause the drive, rerun with `--drive` selecting only other drives, or run `onedrive-go drive reset-sync-state --drive <drive>`. [verified]
- R-2.5.6: The sync state DB shall bootstrap directly into the current canonical schema, stamp an explicit supported store-generation marker, reject non-canonical existing state stores loudly at the store boundary, and expose one explicit per-drive reset command (`drive reset-sync-state`) that requires confirmation unless `--yes` is supplied. [verified]

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
- R-2.8.4: The system shall run periodic full remote refresh (default every 24 hours) to detect missed delta deletions. The cadence shall survive process restarts by persisting the last successful full primary remote observation. [verified]
- R-2.8.5: The system shall support WebSocket subscription for near-instant remote change notification. [verified]

## R-2.9 RPC / Control Socket [verified]

- R-2.9.1: When running `sync` or `sync --watch`, the system shall own a JSON-over-HTTP API on a Unix domain socket so other sync owners cannot run concurrently. One-shot owners expose status and live perf endpoints; watch owners additionally expose reload and stop. Unsupported control requests return typed application errors. [verified]
- R-2.9.2: The RPC API shall support `GET /v1/status`, `GET /v1/perf`, `POST /v1/perf/capture`, `POST /v1/reload`, and `POST /v1/stop`. [verified]
- R-2.9.3: The RPC API shall use typed `{status, code, message}` application errors for invalid requests, one-shot foreground conflicts, and perf-capture failures. [verified]

## R-2.10 Failure Management [verified]

Failure tracking, scope-based classification, and lifecycle management. Each failure is scoped to a key (file, directory, drive, account, or service) that determines retry policy and blast radius.

- R-2.10.1: When a transfer fails with HTTP 507, the system shall classify it as an actionable failure with issue type `quota_exceeded`, scoped to the affected configured drive via `quota:own`. The system shall not retry 507 at the transport level. [verified]
- R-2.10.2: When a user resolves a file-scoped actionable failure (by renaming, moving, or deleting the file), the system shall automatically detect the resolution on the next scanner pass and remove the stale failure entry. [verified]
- R-2.10.3: The system shall detect scope-level failure patterns: 429 immediate target-scope from a single response; 507 after 3 consecutive from different files in 10s; 5xx after 5 consecutive from different files in 30s; 503 with Retry-After immediate service-scope. Scope blocks shall prevent dispatching actions in the affected scope only. [verified]
- R-2.10.4: When displaying sync status, the system shall show grouped condition families per drive with scope context (file, directory, drive, account, service) together with retry and blocker information. Text output shall render those groups human-readably, and `--json` shall preserve them structurally with condition key, count, scope kind, and optional humanized scope label. [designed]
- R-2.10.5: When a block scope is active, the system shall test for recovery by periodically dispatching one real blocked action as a trial. On success: clear the block scope and unblock matching `retry_work`. On failure that proves the same scope still persists: keep the blocked work and extend the trial interval. On inconclusive trial outcomes: re-arm the current scope interval, but only while blocked `retry_work` still exists for that scope. If refresh or cleanup leaves the scope with no blocked work, discard the scope immediately. [designed]
- R-2.10.6: For `quota_exceeded` block scopes, trial timing shall follow R-2.10.14 (unified timing). Detection: 507 sliding window (3 unique paths / 10s). [verified]
- R-2.10.7: For `rate_limited` block scopes, trial timing shall start at the `Retry-After` duration from the server response (no cap — server is ground truth). The block scope shall affect all action types for the exact throttled target only. When a persisted `throttle:target:*` scope was created from server `Retry-After`, restart shall keep that timing and schedule a trial when it expires only while blocked `retry_work` still exists for the scope; empty scopes are discarded instead of being preserved independently. Unsupported older state-store generations shall require explicit state-DB reset instead of preserving or migrating superseded persisted shapes. [verified]
- R-2.10.8: For `service_outage` block scopes, trial timing shall follow R-2.10.14 (default local timing). When `Retry-After` is present, it overrides per R-2.10.7. Detection: 5xx sliding window (5 unique paths / 30s) or 503+Retry-After immediate. Server-timed `service` scopes shall survive restart exactly like server-timed `throttle:target:*` scopes. [verified]
- R-2.10.9: When a timed write-denied scope trial or a real blocked write proves the boundary is writable again, the system shall clear the block scope and release all blocked actions under that boundary immediately. [verified]
- R-2.10.10: When observation proves a previously unreadable file or subtree is accessible again, the system shall clear the corresponding observation issue or boundary tag so planning may resume from current truth without any separate blocker scope. [verified]
- R-2.10.11: When a block scope clears (via trial success or recheck), the system shall release all blocked actions immediately with `NotBefore` set to now. Individual action `failure_count` shall be preserved. [verified]
- R-2.10.12: When a local file operation fails with `os.ErrPermission`, the system shall check parent directory accessibility. If the directory is inaccessible: record one `local_permission_denied` at directory level and suppress operations under it. If the directory is accessible: record at file level. [verified]
- R-2.10.13: Local write-denied directory-level conditions shall be revalidated through the normal timed block-scope trial path while blocked `retry_work` still exists. Local read-denied boundaries shall clear only when later observation proves readability. [verified]
- R-2.10.14: Trial timing uses a fixed 60-second interval for locally timed scopes. Server-provided Retry-After values are honored exactly with no cap. Inconclusive outcomes re-arm the current interval, but only for non-empty scopes that still have blocked `retry_work`. Blocker timing is owned by `block_scopes`. [designed]
- R-2.10.15: When a block scope is set, at most `transfer_workers` actions may be in-flight. These shall complete normally and route through standard retry. [verified]
- R-2.10.16: Every `WorkerResult` shall carry the action's authoritative remote drive identity (`DriveID`) so scope classification and drive-scoped retries can route results to the correct mounted boundary. [verified]
- R-2.10.17: Scope routing shall use action drive context for 507/403/429 scope keys. 429 shall route to `throttle:target:*` using the failed request's own-drive or shared-target identity. 5xx shall continue to route to `service` regardless of drive identity. [verified]
- R-2.10.18: Sliding windows shall be independent per scope key and per configured drive boundary. A 507 on one drive shall not count toward another drive's scope windows. [verified]
- R-2.10.19: When 507 occurs on own-drive, scope key `quota:own` shall block own-drive uploads only. Shortcut uploads, downloads, deletes, and moves shall continue. [verified]
- R-2.10.20: When 507 occurs on a separately configured shared-folder mount, it shall block uploads only for that configured mount. Other configured mounts shall continue independently. [verified]
- R-2.10.21: Trial actions for `quota:own` shall select uploads within the affected configured drive. [verified]
- R-2.10.22: The per-drive `status` view shall identify quota and permission scopes by the affected drive's user-facing identity, not opaque internal IDs. [verified]
- R-2.10.23: When 403 occurs on a separately configured shared-folder mount, permission boundary walking shall use that mount's configured remote root and drive identity, not another mount's root. [verified]
- R-2.10.24: Permission or quota failures on one configured drive shall not affect another configured drive, even if both point at content owned by the same sharer. [verified]
- R-2.10.25: When a configured shared-folder mount's remote root itself is read-only, the system shall record a block scope at that configured root level without walking above it. [verified]
- R-2.10.26: When 429 occurs, the system shall block all action types only on the exact remote target implicated by the failed request. Current persisted target scopes use `throttle:target:drive:$targetDriveID`. [verified]
- R-2.10.27: When a 429 target scope clears, only the blocked actions for that exact throttled target shall be released. Unrelated drives shall remain unaffected. [verified]
- R-2.10.28: When 5xx block scopes are active, they shall affect all drives, since the Graph API is shared infrastructure. [verified]
- R-2.10.29: The service-scope sliding window shall accept 5xx from any action drive. Five consecutive 5xx from different drives within 30s shall trigger a block. [verified]
- R-2.10.30: During `service` block scopes, the system shall suppress remote observation polling globally. During drive-target throttle blocks, suppression shall remain limited to the affected drive/target boundary. [verified]
- R-2.10.31: During `quota:own` block scopes, observation of the affected drive shall continue read-only. Other configured drives shall be unaffected. [verified]
- R-2.10.32: The `status` command shall show per-block scope status as separate entries per drive, preserving separate condition groups even when the condition key is the same. [designed]
- R-2.10.33: Shared blocker timing and scope identity shall live in `block_scopes`, while exact blocked work shall live in `retry_work`, so `status` can group scoped conditions without depending on a mixed failure table. Every persisted block scope requires blocked retry work; empty scopes are discarded immediately. Observation-owned read-denied subtree boundaries use `ScopeKey` on `observation_issues`, not persisted `block_scopes`. [designed]
- R-2.10.34: The `scope_key` format shall be: `quota:own`, `perm:remote:read:{localPath}`, `perm:remote:write:{localPath}`, `perm:dir:read:{localPath}`, `perm:dir:write:{localPath}`, `throttle:target:drive:$targetDriveID`, `service`, `disk:local`. Persisted scope keys use stable internal identifiers where required for correctness. Unsupported legacy scope-key shapes are not migrated in place; startup requires explicit reset of that DB instead. [verified]
- R-2.10.35: Engines shall not coordinate block scopes across engine boundaries. Each engine shall discover scope conditions independently. [verified]
- R-2.10.36: When 429 is discovered independently per engine (same token), no shared state shall be required. [verified]
- R-2.10.37: Scope blocks shall be engine-internal. One drive engine's scope state shall have no effect on another drive engine. [verified]
- R-2.10.38: When a configured drive is removed, its scope state disappears with that drive's state DB and shall not leak into other drives. [verified]
- R-2.10.39: Two separately configured standalone shared-folder drives to the same sharer's drive shall remain independent. A 507 on one shall not auto-block the other. [verified]
- R-2.10.40: Permission boundary walking on standalone shared-folder drives shall not walk above the configured remote root. [verified]
- R-2.10.41: When a download, delete, or move action succeeds, the system shall clear any corresponding retry-work rows that no longer apply to that path. Observation-owned durable current-truth rows clear only after a later observation pass proves they no longer apply. [designed]
- R-2.10.42: The scope detection sliding window shall accept results from concurrent workers. A success from any path in the scope shall reset the unique-path failure counter, preventing false block scopes from interleaved results. [verified]
- R-2.10.43: When available disk space falls below `min_free_space`, the system shall set a `disk:local` block scope suppressing all downloads. Trial timing follows the unified local block-scope interval in R-2.10.14. [verified]
- R-2.10.44: When available disk space is above `min_free_space` but below file size plus `min_free_space`, the system shall record a per-file failure without scope escalation. Smaller files that fit within available space may still download. [verified]
- R-2.10.45: When a worker result returns HTTP 401, the system shall persist the account auth requirement in the managed catalog with reason `sync_auth_rejected` and terminate the current one-shot pass or watch session. It shall not fabricate a per-path durable status row for the 401 or persist any account-auth state in `block_scopes`. Trial 401 results shall not be treated as proof that the account recovered. CLI presentation remains account-level and shall not synthesize fake path rows. [designed]
- R-2.10.46: When a persisted catalog account-auth requirement exists at startup, the system shall revalidate it exactly once with `DriveVerifier.Drive(ctx, driveID)`. Successful proof shall clear the catalog auth requirement and continue startup. Unauthorized proof shall keep the requirement and abort startup. Non-auth probe failures, or a missing `DriveVerifier`, shall leave the requirement untouched and abort startup. [verified]
- R-2.10.47: Offline read-only CLI surfaces (`status`) shall never mutate persisted account-auth state. Successful authenticated live CLI proof surfaces may clear the persisted catalog account-auth requirement for the proved account after the first successful authenticated Graph response. Pre-authenticated upload or download URL success shall not count as proof. [verified]

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

- R-2.14.1: When a write to a separately configured shared-folder mount returns HTTP 403, the system shall call the Graph permissions API to confirm whether the folder is truly read-only, fail open when the evidence is writable or inconclusive, and walk parent folders to the highest denied ancestor without walking above the configured remote root. [verified]
- R-2.14.2: When confirmed read-only shared-folder state still has blocked remote-mutating work, the planner shall treat that subtree as download-only — suppressing uploads, folder creates, remote moves, and remote deletes while still allowing downloads. [verified]
- R-2.14.3: The system shall surface read-only shared-folder state in `status` only while blocked remote-mutating intent exists, grouping one condition per denied boundary and forgetting the state immediately when the last blocked write disappears. [designed]
- R-2.14.4: The system shall revalidate visible read-only shared-folder blockers through normal timed scope trials while blocked remote-mutating work still exists, and release them when write access is restored or the blocked work disappears. [verified]
- R-2.14.5: Read-only shared-folder state shall not expose manual retry or manual recheck commands. Recovery shall happen only through automatic permission revalidation and normal scope release when blocked writes disappear. [verified]

## R-2.15 Delta Checkpoint Integrity [verified]

- R-2.15.1: The system shall track individual delayed work independently of the delta token, since the delta checkpoint only appears on the final page and cannot be partially committed. [designed]

## R-2.16 Eventual Consistency [verified]

- R-2.16.1: The system shall not re-query file metadata immediately after upload, as OneDrive properties may be temporarily in flux during server-side processing (thumbnails, indexing). [verified]
- R-2.16.2: Incremental one-shot sync shall treat remote delta visibility as eventually consistent. A direct REST item read succeeding elsewhere is not proof that the current incremental delta pass will already observe the same mutation. [verified]
- R-2.16.3: Default one-shot `sync` shall not add hidden settle loops to chase delta lag. The documented one-shot strong-freshness mode remains `sync --full`, which enumerates remote truth instead of relying on a single incremental delta pass. [verified]
