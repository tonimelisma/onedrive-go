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
- R-2.3.2: The system shall persistently record conflicts with metadata (path, timestamp, hashes, resolution status). [verified]
- R-2.3.3: When the user runs `issues`, the system shall list all unresolved conflicts and failures. [verified]
- R-2.3.4: When the user runs `issues resolve <path>`, the system shall allow resolution (keep-local, keep-remote, keep-both). [verified]
- R-2.3.5: When the user runs `issues clear <path>`, the system shall dismiss a conflict. [verified]
- R-2.3.6: When the user runs `issues retry <path>`, the system shall retry a failed item. [verified]
- R-2.3.7: When the `issues` command encounters more than 10 failures of the same issue type, the system shall group them under a single heading with count and show the first 5 paths. When `--verbose` is passed, the system shall show all paths. [planned]
- R-2.3.8: When displaying scope-level issues where drives have independent scopes (507 quota, 403 permissions), the system shall sub-group by scope (own drive vs each shortcut). [planned]
- R-2.3.9: When displaying shortcut-scoped failures, the system shall use the shortcut's local path name (human-readable), not internal drive IDs or scope keys. [planned]
- R-2.3.10: When `--json` is passed, `issues` shall output structured JSON with unified conflicts and failures array. [verified]

## R-2.4 Filtering [implemented]

All filter settings are per-drive (no global filter defaults).

- R-2.4.1: When `skip_dotfiles = true`, the system shall exclude files and folders starting with `.`. [verified]
- R-2.4.2: When `skip_dirs` is set, the system shall exclude matching directory names. [verified]
- R-2.4.3: When `skip_files` is set, the system shall exclude matching file patterns. [verified]
- R-2.4.4: When a directory contains a file matching the `ignore_marker` name (default `.odignore`), the system shall exclude that directory from sync. The marker file is a presence-only check — its contents are not read. The marker file itself is not synced. [planned]
- R-2.4.5: When `sync_paths` is set, the system shall sync only the specified paths. [planned]
- R-2.4.6: When `skip_symlinks = true`, the system shall exclude symlinks. Symlinked directories are always excluded from watch mode. [verified]
- R-2.4.7: When an item belongs to the Personal Vault, the system shall exclude it. Vault auto-locks after 20 minutes, causing locked items to appear deleted in delta responses — syncing vault items would cause data loss. [verified]

## R-2.5 Crash Recovery [verified]

- R-2.5.1: When the process is killed mid-sync, the next run shall resume cleanly from the last checkpoint. [verified]
- R-2.5.2: The sync state store shall provide durable, transactional writes that survive process kill. [verified]
- R-2.5.3: On startup, the system shall detect items stuck in `syncing` state and reset them for re-planning (reconciler). [verified]

## R-2.6 Pause / Resume [verified]

- R-2.6.1: When the user runs `pause [duration]`, the system shall stop data transfers while continuing to track changes. [verified]
- R-2.6.2: When the user runs `resume`, the system shall resume transfers with a complete picture of accumulated changes. [verified]

## R-2.7 Verification [verified]

When the user runs `verify`, the system shall re-hash local files and compare against the baseline and remote state, reporting discrepancies.

- R-2.7.1: When `--json` is passed, `verify` shall output structured JSON with verified count and mismatches. [verified]

## R-2.8 Watch Mode Behavior [verified]

- R-2.8.1: The system shall reload `config.toml` on SIGHUP. [verified]
- R-2.8.2: The system shall use a PID file with flock for single-instance enforcement. [verified]
- R-2.8.3: The system shall support two-signal shutdown (first SIGINT = drain, second = force). [verified]
- R-2.8.4: The system shall run periodic full reconciliation (default every 24 hours) to detect missed delta deletions. [verified]
- R-2.8.5: The system shall support WebSocket subscription for near-instant remote change notification. [future]

## R-2.9 RPC / Control Socket [planned]

- R-2.9.1: When running `sync --watch`, the system shall expose a JSON-over-HTTP API on a Unix domain socket. [planned]
- R-2.9.2: The RPC API shall support polling (`GET /status`) and push (`GET /events` via SSE). [planned]
- R-2.9.3: GUI frontends shall connect to the control socket for real-time status, pause/resume, and conflict resolution. [planned]

## R-2.10 Failure Management [planned]

Failure tracking, scope-based classification, and lifecycle management. Each failure is scoped to a key (file, directory, drive/shortcut, account, service) that determines retry policy and blast radius.

- R-2.10.1: When a transfer fails with HTTP 507, the system shall classify it as an actionable failure with issue type `quota_exceeded`, scoped to the quota owner (`quota:own` for own-drive, `quota:shortcut:$remoteDrive:$remoteItem` for shortcuts). The system shall not retry 507 at the transport level. [planned]
- R-2.10.2: When a user resolves a file-scoped actionable failure (by renaming, moving, or deleting the file), the system shall automatically detect the resolution on the next scanner pass and remove the stale failure entry. [planned]
- R-2.10.3: The system shall detect scope-level failure patterns: 429 immediate account-scope from single response; 507 after 3 consecutive from different files in 10s; 5xx after 5 consecutive from different files in 30s; 503 with Retry-After immediate service-scope. Scope blocks shall prevent dispatching actions in the affected scope. [planned]
- R-2.10.4: When displaying sync status, the system shall show failure scope context (file, directory, drive/shortcut, account, service) alongside retry information, identifying which drive or shortcut is affected for per-drive scopes. [planned]
- R-2.10.5: When a scope block is active, the system shall test for recovery by periodically releasing one real action from the held queue as a trial. On success: clear the scope block and release all held actions. On failure: re-hold and extend trial interval. [planned]
- R-2.10.6: For `quota_exceeded` scope blocks, trial timing shall start at 5 minutes, double on each failed trial, with a maximum of 1 hour. [planned]
- R-2.10.7: For `rate_limited` scope blocks, trial timing shall start at the `Retry-After` duration from the server response. The scope block shall affect all action types for the account. [planned]
- R-2.10.8: For `service_outage` scope blocks, trial timing shall start at 60 seconds (or `Retry-After` if present), double on failure, with a maximum of 10 minutes. [planned]
- R-2.10.9: When `recheckPermissions()` discovers a previously-denied folder is now writable, the system shall clear the scope block and release all held actions under that folder immediately. [planned]
- R-2.10.10: When the scanner observes a previously-blocked file or directory is now accessible, the system shall clear the failure and release held actions. [planned]
- R-2.10.11: When a scope block clears (via trial success or recheck), the system shall release all held actions immediately with `NotBefore` set to now. Individual action `failure_count` shall be preserved. [planned]
- R-2.10.12: When a local file operation fails with `os.ErrPermission`, the system shall check parent directory accessibility. If the directory is inaccessible: record one `local_permission_denied` at directory level and suppress operations under it. If the directory is accessible: record at file level. [planned]
- R-2.10.13: The system shall recheck `local_permission_denied` directory-level issues at the start of each sync pass, and auto-clear when the directory becomes accessible. [planned]
- R-2.10.14: Trial timing shall be per scope type: `rate_limited` starts at Retry-After (max 10 min); `quota_exceeded` starts at 5 min (2x backoff, max 1 hour); `service_outage` starts at 60s or Retry-After (2x backoff, max 10 min). [planned]
- R-2.10.15: When a scope block is set, at most `transfer_workers` actions may be in-flight. These shall complete normally and route through standard retry. [planned]
- R-2.10.16: Every `WorkerResult` shall carry target drive identity (`TargetDriveID`, `ShortcutKey`). Own-drive actions use empty `ShortcutKey`; shortcut actions use `remoteDrive:remoteItem`. [planned]
- R-2.10.17: Scope routing shall use target drive context for 507/403 scope keys. 429/5xx shall always route to account/service scope regardless of target drive. [planned]
- R-2.10.18: Sliding windows shall be independent per scope key. Own-drive 507s shall not count toward shortcut scope windows, and vice versa. [planned]
- R-2.10.19: When 507 occurs on own-drive, scope key `quota:own` shall block own-drive uploads only. Shortcut uploads, downloads, deletes, and moves shall continue. [planned]
- R-2.10.20: When 507 occurs on a shortcut, scope key `quota:shortcut:$remoteDrive:$remoteItem` shall block that shortcut's uploads only. Own-drive and other shortcut operations shall continue. [planned]
- R-2.10.21: Trial actions for `quota:own` shall select own-drive uploads. Trial actions for `quota:shortcut:*` shall select uploads targeting that shortcut. [planned]
- R-2.10.22: The `issues` display shall identify shortcut-scoped 507 by local path name (e.g., "Shared folder 'Team Docs'"), not opaque drive IDs. [planned]
- R-2.10.23: When 403 occurs on a shortcut, the scope boundary shall be that shortcut's `RemoteDriveID`. Permission boundary walking shall use the shortcut's drive, not the primary drive. [planned]
- R-2.10.24: When 403 occurs on shortcut A, it shall not affect shortcut B. Each shortcut has independent permissions from an independent owner. [planned]
- R-2.10.25: When a shortcut root itself is read-only, the system shall record a scope block at shortcut root level without walking above the shortcut boundary. [planned]
- R-2.10.26: When 429 occurs, the system shall block all action types on all drives including shortcuts (`throttle:account`), since the same OAuth token shares rate limits. [planned]
- R-2.10.27: When the 429 scope clears, all held actions (own-drive and shortcuts) shall be released simultaneously. [planned]
- R-2.10.28: When 5xx scope blocks are active, they shall affect all drives including shortcuts, since the Graph API is shared infrastructure. [planned]
- R-2.10.29: The service-scope sliding window shall accept 5xx from any target drive. Five consecutive 5xx from different drives within 30s shall trigger a block. [planned]
- R-2.10.30: During `throttle:account` or `service` scope blocks, the system shall suppress shortcut observation polling to avoid wasting API calls. [planned]
- R-2.10.31: During `quota:shortcut:*` scope blocks, observation of that shortcut shall continue (read-only). Other observations shall be unaffected. [planned]
- R-2.10.32: The `status` command shall show per-scope block status as separate entries per drive/shortcut. [planned]
- R-2.10.33: The `sync_failures` table shall store a `scope_key` column for scope-level failures, enabling `issues` display grouping without re-deriving scope. [planned]
- R-2.10.34: The `scope_key` format shall be: `quota:own`, `quota:shortcut:{localPath}`, `perm:remote:{localPath}`. The sync_failures table (persistent, used for issues display) stores human-readable local paths (e.g., `quota:shortcut:Team Docs`). ScopeState (in-memory, used for scope matching in the tracker) uses stable internal identifiers (e.g., `quota:shortcut:$remoteDrive:$remoteItem`). Translation happens once at recording time — the engine knows both formats. [planned]
- R-2.10.35: Engines shall not coordinate scope blocks across engine boundaries. Each engine shall discover scope conditions independently. [planned]
- R-2.10.36: When 429 is discovered independently per engine (same token), no shared state shall be required. [planned]
- R-2.10.37: Shortcut scope blocks shall be engine-internal. A shortcut in Engine A shall have no effect on Engine B's shortcuts. [planned]
- R-2.10.38: When a shortcut is removed while a scope block exists for it, the system shall clear the block and discard held actions. [planned]
- R-2.10.39: Two shortcuts to the same sharer's drive shall have independent scope keys. A 507 on one shall not auto-block the other. [planned]
- R-2.10.40: Permission boundary walking on shortcuts shall not walk above the shortcut root. The shortcut root is the natural boundary. [planned]
- R-2.10.41: When a download, delete, or move action succeeds, the system shall clear any corresponding `sync_failures` entry for that path. [planned]
- R-2.10.42: The scope detection sliding window shall accept results from concurrent workers. A success from any path in the scope shall reset the unique-path failure counter, preventing false scope blocks from interleaved results. [planned]
- R-2.10.43: When available disk space falls below `min_free_space`, the system shall set a `disk:local` scope block suppressing all downloads. Trial timing shall start at 5 minutes, double on failure, with a maximum of 1 hour. [planned]
- R-2.10.44: When available disk space is above `min_free_space` but below file size plus `min_free_space`, the system shall record a per-file failure without scope escalation. Smaller files that fit within available space may still download. [planned]

## R-2.11 Filename Validation [implemented]

The system shall validate filenames against OneDrive naming restrictions before upload and during remote observation.

- R-2.11.1: The system shall reject local files with characters invalid on OneDrive (`"`, `*`, `:`, `<`, `>`, `?`, `/`, `\`, `|`) before upload. [implemented]
- R-2.11.2: The system shall reject local files with OneDrive reserved names (case-insensitive): `.lock`, `desktop.ini`, `CON`, `PRN`, `AUX`, `NUL`, `COM0`-`COM9`, `LPT0`-`LPT9`. [implemented]
- R-2.11.3: The system shall reject local files matching OneDrive reserved patterns: names starting with `~$`, names containing `_vti_`, and `forms` at root level on SharePoint drives. [implemented]
- R-2.11.4: The system shall reject local files with trailing dots or leading/trailing whitespace. [implemented]
- R-2.11.5: When the scanner filters a file due to naming restrictions, the system shall record it as an actionable issue (not silently skip with only a DEBUG log). [planned]

## R-2.12 Case Collision Handling [planned]

- R-2.12.1: Before uploading, the system shall detect local case-insensitive filename collisions (e.g., `file.txt` vs `File.txt`) and flag them as conflicts rather than attempting upload. [planned]
- R-2.12.2: When a case collision is detected, neither colliding file shall be uploaded until the collision is resolved. [planned]

## R-2.13 Unicode Normalization [verified]

- R-2.13.1: The system shall normalize filenames to NFC form before comparison, to handle macOS NFD paths correctly. [verified]

## R-2.14 Read-Only Shared Items [verified]

- R-2.14.1: When a write to a shared item returns HTTP 403, the system shall query the Graph API to confirm the denial is permanent (not transient), then walk up the folder hierarchy to find the permission boundary and record it as read-only. [verified]
- R-2.14.2: When a subtree is recorded as read-only, the planner shall switch it to download-only mode — suppressing uploads, remote moves, and remote deletes while still allowing downloads. [verified]
- R-2.14.3: At the start of each sync pass, the system shall recheck all permission-denied records against the Graph API and clear any where write access has been restored. [verified]
- R-2.14.4: When the Graph API is unavailable during a permission check, the system shall fail open — not suppressing writes based on inconclusive evidence. [verified]

## R-2.15 Delta Checkpoint Integrity [verified]

- R-2.15.1: The system shall track individual item failures independently of the delta token, since the delta checkpoint only appears on the final page and cannot be partially committed. [verified]

## R-2.16 Eventual Consistency [planned]

- R-2.16.1: The system shall not re-query file metadata immediately after upload, as OneDrive properties may be temporarily in flux during server-side processing (thumbnails, indexing). [planned]
