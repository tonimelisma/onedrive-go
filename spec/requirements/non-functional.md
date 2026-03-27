# R-6 Non-Functional

Performance targets, data integrity, process model, safety, observability, and packaging.

## R-6.1 Performance Targets [target]

- R-6.1.1: Memory usage shall stay below 100 MB for 100K synced files. [target]
- R-6.1.2: CPU usage in idle watch mode shall stay below 1%. [target]
- R-6.1.3: Initial sync of 10K files shall complete in under 10 minutes. [target]
- R-6.1.4: Startup time shall be under 1 second. [verified]
- R-6.1.5: Binary size shall be under 20 MB (single static binary, no runtime dependencies). [verified]
- R-6.1.6: When operating in production, the system shall meet CPU, memory, and I/O performance targets validated via profiling. [planned]

## R-6.2 Data Integrity [implemented]

The system shall never silently lose or corrupt user data. This umbrella principle is enforced by the following specific safety invariants:

- R-6.2.1: (S1) The system shall never delete a remote item based on local absence unless a synced baseline entry exists. [verified]
- R-6.2.2: (S2) The system shall never process deletions from an incomplete enumeration (partial delta fetch or unmounted volume). [verified]
- R-6.2.3: (S3) Downloads shall use atomic file writes (`.partial` + hash verify + rename). [verified]
- R-6.2.4: (S4) Local deletions shall verify the file hash against baseline before deleting; on mismatch, a conflict copy is preserved. [verified]
- R-6.2.5: (S5) Big-delete protection shall prevent mass accidental deletions: one-shot mode aborts when planned deletions exceed `big_delete_threshold`; watch mode holds deletes via a rolling window counter and surfaces them as actionable issues. [verified]
- R-6.2.6: (S6) Before each download, the system shall verify available disk space. When below `min_free_space`: set a `disk:local` scope block on all downloads. When above `min_free_space` but below file size plus `min_free_space`: record a per-file failure. [verified]
- R-6.2.7: (S7) The system shall never upload partial or temporary files (filter cascade excludes temp patterns). [verified]
- R-6.2.8: File operations (ls, get, put, rm, mkdir, stat, mv, cp) shall work independently of sync state — no sync database involved. [verified]
- R-6.2.9: The system shall support configurable file permissions (`sync_file_permissions`) and directory permissions (`sync_dir_permissions`) for synced content. [verified]
- R-6.2.10: When a transfer connection stalls without context cancellation, the system shall enforce a per-transfer timeout or connection-level deadline to prevent indefinite hangs. [implemented]

## R-6.3 Process Model [verified]

- R-6.3.1: Only one sync process per configuration shall run at a time. [verified]
- R-6.3.2: Status and query commands shall be concurrent-reader safe while sync is running. [verified]
- R-6.3.3: The system shall enforce single-instance via PID file with advisory lock. [verified]

## R-6.4 Safety [implemented]

- R-6.4.1: When a one-shot sync would delete more items than `big_delete_threshold` (default: 1000), the system shall abort and require `--force`. Single absolute count threshold — no percentage or per-folder checks. [verified]
- R-6.4.2: In watch mode, when more than `big_delete_threshold` delete actions accumulate within a rolling 5-minute window, the system shall hold all pending delete actions while continuing non-delete operations. Held deletes shall be surfaced via `onedrive-go issues` and released when the user clears all big-delete-held entries via `issues clear`. [verified]
- R-6.4.3: The `big_delete_threshold` config setting shall be threaded from user configuration to the sync engine; the system shall not silently use hardcoded defaults. [verified]
- R-6.4.4: Remote deletions shall go to the OneDrive recycle bin by default. [verified]
- R-6.4.5: Local deletions triggered by remote changes shall go to OS trash on macOS (`use_local_trash`). [verified]
- R-6.4.6: On Linux, local trash shall be opt-in (default off; servers/NAS typically lack XDG trash). [verified]
- R-6.4.7: The system shall support configurable disk space reservation (`min_free_space`, default 1 GB). When available space falls below this threshold, downloads shall be scope-blocked. Set to 0 to disable. [verified]
- R-6.4.8: All local filesystem writes shall be confined to the sync root directory. The executor validates resolved paths via `containedPath()` to prevent escape from path reconstruction bugs. [verified]

## R-6.5 Crash Recovery [verified]

- R-6.5.1: The sync state store shall provide durable, transactional writes that survive process kill. [verified]
- R-6.5.2: Every sync operation shall be atomic — incomplete operations shall not corrupt state. [verified]
- R-6.5.3: On startup, the system shall detect items stuck in `syncing` state and reset them. [verified]

## R-6.6 Observability [verified]

- R-6.6.1: The system shall support dual-channel logging: console (stderr) and log file. [verified]
- R-6.6.2: Console verbosity shall be controlled by `--quiet`, `--verbose`, and `--debug` flags. [verified]
- R-6.6.3: Log file level shall be controlled independently by `log_level` in config. [verified]
- R-6.6.4: The log file shall use structured JSON format. [verified]
- R-6.6.5: The system shall support progress bars and color-coded transfer output. [future]
- R-6.6.6: The system shall support a TUI (terminal UI) for real-time status. [future]
- R-6.6.7: When more than 10 items share the same warning category in a sync pass, the system shall log one WARN summary with count and individual items at DEBUG. [verified]
- R-6.6.8: Individual retry attempts for transient errors shall be logged at DEBUG, not WARN. Only the final outcome shall be logged at WARN or higher. [verified]
- R-6.6.9: When transient errors resolve within the retry budget, the system shall log at INFO with attempt count (not WARN). [planned — deferred: per-path resolution logging adds per-success DB query; scope block/release INFO provides equivalent visibility]
- R-6.6.10: When retries are exhausted, the system shall log a single WARN with final error, attempt count, and next retry time. [verified]
- R-6.6.11: Every failure shown to the user shall include a plain-language reason and a concrete user action. Per-error-type reason and action text shall cover all failure categories (quota, permissions, disk space, service outage, rate limiting, naming violations, case collisions, network errors, auth failures, unknown errors), with scope-owner-specific variants for shortcut-scoped failures. [verified]
- R-6.6.12: When more than 10 transient failures of the same issue_type exhaust their retry budget within a single sync pass, the system shall aggregate them into a single summary WARN log line with count, logging individual paths at DEBUG. This extends the scanner-skipped aggregation pattern (R-6.6.7) to execution-time transient failures. [verified]

## R-6.7 Technical Requirements [implemented]

Constraints derived from the OneDrive API that the system must satisfy for correctness. See [graph-api-quirks.md](../reference/graph-api-quirks.md) for the underlying API behaviors.

- R-6.7.1: The system shall handle delta operation reordering (deletions arriving after creations at the same path). [verified]
- R-6.7.2: The system shall normalize driveId values across all endpoints to a canonical form. [verified]
- R-6.7.3: The system shall track items by ID and reconstruct paths from parent chains. [verified]
- R-6.7.4: The system shall detect server-side file modification after upload (SharePoint enrichment) and not re-upload. [verified]
- R-6.7.5: The system shall handle HTTP 410 (delta token expiry) with full re-enumeration. [verified]
- R-6.7.6: The system shall enforce upload chunk alignment to 320 KiB boundaries. [verified]
- R-6.7.7: The system shall not compare hashes for deleted items. [verified]
- R-6.7.8: The system shall unconditionally URL-decode item names in all API responses. [verified]
- R-6.7.9: The system shall filter out OneNote package items (`type: "oneNote"`) that lack standard file/folder facets. [verified]
- R-6.7.10: The system shall deduplicate identical items appearing multiple times within a single delta response, keeping the last occurrence per item ID. [verified]
- R-6.7.11: The system shall filter out phantom system drives provisioned by Microsoft for Personal accounts (face crops, albums) that return HTTP 400 on access. For Personal accounts, the system shall use `GET /me/drive` (singular) to discover the primary drive. [planned]
- R-6.7.12: When `GET /drives/{driveID}/items/root/children` returns transient HTTP 404 with Graph code `itemNotFound` for a valid resource (cross-datacenter load balancer timeout), the system shall classify it as transient and retry with backoff. [verified]
- R-6.7.13: When `GET /me/drives` returns HTTP 403 with Graph code `accessDenied` shortly after token refresh (eventual consistency), the system shall classify it as transient and retry with backoff. [verified]
- R-6.7.14: When the Graph API returns HTTP 400 with a Graph error code chain containing `invalidRequest` and known outage message patterns (e.g., "ObjectHandle is Invalid"), the system shall classify it as transient (service outage) and route through scope-based retry, not generic skip. [verified]
- R-6.7.15: The system shall truncate local timestamps to zero fractional seconds before comparing with OneDrive's whole-second precision. [planned]
- R-6.7.16: The system shall safely handle missing or invalid timestamps (`0001-01-01T00:00:00Z`, absent `lastModifiedDateTime` on deletions) without panicking. [planned]
- R-6.7.17: When a file completely lacks hashes (zero-byte files, certain Business/SharePoint files), the system shall use a fallback comparison of size + mtime + eTag. [implemented]
- R-6.7.18: When extracting identity data for shared items, the system shall use a four-level fallback chain: `remoteItem.shared.sharedBy` → `.owner` → `remoteItem.createdBy` → top-level `shared.owner`. [planned]
- R-6.7.19: The system shall not advance the delta token when a delta response contains zero events, to prevent permanently missing ephemeral deletion events. [verified]
- R-6.7.20: The system shall adapt observation methods based on drive type: folder-scoped delta for Personal shared folders, standard enumeration for Business/SharePoint (which do not support folder-scoped delta). [verified]
- R-6.7.21: The system shall handle shared folder items whose parent IDs reference a different drive (the sharer's drive), including driveId truncation and casing normalization on cross-drive references. The planner detects and decomposes cross-drive moves into separate delete + upload actions since MoveItem is a single-drive API. [verified]
- R-6.7.22: When the API's path-based query returns an item from an incorrect path (fuzzy matching bug), the system shall post-validate that the returned item's name matches the requested path. [planned]
- R-6.7.23: The system shall URL-decode `parentReference.path` in non-delta responses before using it for path reconstruction. [planned]
- R-6.7.24: When a folder is renamed, the system shall infer and recalculate path changes for all descendants, since only the renamed folder appears in the delta response. [verified]
- R-6.7.25: When re-uploading a modified file to Business/SharePoint, the system shall accept the unavoidable extra version created by the API (unfixed Microsoft bug) without attempting futile workarounds. [planned]
- R-6.7.26: The system shall handle absent `lastModifiedDateTime` (null) on API-initiated deletions without error. [planned]
- R-6.7.27: When classifying errors, the engine shall handle empty `TargetDriveID` (local-only operations like `os.ErrPermission`) by skipping remote scope routing. Only remote API errors shall require drive-aware scope routing. [verified]

## R-6.8 Network Resilience [verified]

- R-6.8.1: The system shall respect 429 (Too Many Requests) with Retry-After headers. [verified]
- R-6.8.2: The system shall use exponential backoff with jitter for transient failures. [verified]
- R-6.8.3: All transfers shall be resumable after network interruption. [verified]
- R-6.8.4: The system shall treat HTTP 429 as account-scoped: all drives under the same account share the throttle. [verified]
- R-6.8.5: ~~The system shall parse `RateLimit-Remaining` headers and proactively slow when less than 20% remains.~~ [cancelled — headers only cover per-app 1-minute resource units; not reliable for proactive throttling. Reactive 429 scope handling is sufficient.]
- R-6.8.6: The system shall honor `Retry-After` from both 429 and 503 responses, populating `GraphError.RetryAfter`. [implemented]
- R-6.8.7: During sync, workers shall never block on retry backoff. Failed actions are completed and recorded in `sync_failures` with `next_retry_at`. Workers block only for HTTP round-trip time. [verified]
- R-6.8.8: For sync operations, the graph client shall use raw `http.DefaultTransport` (no retry transport). Each dispatch equals one HTTP request. CLI commands shall retain `Transport` policy (`MaxAttempts: 5`). [verified]
- R-6.8.9: The executor shall not contain a retry loop. It shall dispatch actions and return results; the engine shall classify and schedule retries. [verified]
- R-6.8.10: Each sync action failure shall be recorded in `sync_failures` with `next_retry_at` computed by the `retry.Reconcile` backoff policy. The `FailureRetrier` re-injects due items via buffer → planner → tracker. One retry mechanism, one backoff curve. No in-memory retry budget — the tracker is a pure dependency graph and scope-aware dispatch gate. [verified]
- R-6.8.11: The system shall use `retry.Reconcile` backoff for all sync failures: 1s base, 2× multiplier, ±25% jitter, 1h max. Curve: 1s, 2s, 4s, 8s, 16s, 32s, 64s, 128s, 256s, 512s, 1024s, 2048s, 3600s cap. Single persistent mechanism via `sync_failures` + `FailureRetrier`. [verified]
- R-6.8.12: Target drive identity shall flow through the pipeline without lookup: planner to Action to TrackedAction to WorkerResult to engine. No component shall query drive identity from DB or API during failure handling. [verified]
- R-6.8.13: `Action` shall expose `TargetsOwnDrive() bool` and `ShortcutKey() string` as the only drive-identity accessors for scope matching and routing. [verified]
- R-6.8.14: Sync callers use raw `http.DefaultTransport`. The graph client's `doOnce()` still performs transparent 401 token refresh. Auth refresh is lifecycle, not transient retry. When refresh fails, the system shall return `ErrUnauthorized` (fatal). [verified]
- R-6.8.15: The engine shall classify the following HTTP status codes as transient: 5xx (issue_type: server_error), 408 (request_timeout), 412 (transient_conflict), 404 (transient_not_found), 423 (resource_locked). Transient errors are recorded in `sync_failures` with `next_retry_at` (backoff via `retry.Reconcile`: 1s-1h). The `FailureRetrier` re-injects due items through the full pipeline (buffer → planner → tracker). In one-shot mode, failed items remain in sync_failures for the next `onedrive sync` invocation to replan. HTTP 423 (SharePoint co-authoring lock) was previously classified as skip — the persistent retry architecture handles multi-hour locks naturally via escalating backoff in sync_failures. [verified]

## R-6.9 Packaging [future]

- R-6.9.1: The system shall be distributable as a single static Go binary. [verified]
- R-6.9.2: The system shall provide a Homebrew formula (macOS). [future]
- R-6.9.3: The system shall provide deb/rpm packages. [future]
- R-6.9.4: The system shall provide an AUR package. [future]
- R-6.9.5: The system shall provide a Docker image (Alpine-based, multi-arch). [future]

## R-6.10 Verification and Static Analysis [verified]

- R-6.10.1: The repository shall provide a single verification entry point that runs lint, build, race tests, coverage, and credential-gated fast E2E checks. [verified]
- R-6.10.2: CI shall enforce the same lint policy as local development and pin the `golangci-lint` version used for verification. [verified]
- R-6.10.3: Inline `//nolint` usage shall require both a specific linter name and a justification, and stale exclusions shall be surfaced automatically. [verified]
- R-6.10.4: Total statement coverage shall not drop below 76.0% in the verification gate. [verified]
