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
- R-6.2.5: (S5) Big-delete protection shall abort when planned deletions exceed configured thresholds. [verified]
- R-6.2.6: (S6) The system shall check available disk space before downloading. [planned]
- R-6.2.7: (S7) The system shall never upload partial or temporary files (filter cascade excludes temp patterns). [verified]
- R-6.2.8: File operations (ls, get, put, rm, mkdir, stat, mv, cp) shall work independently of sync state — no sync database involved. [verified]
- R-6.2.9: The system shall support configurable file permissions (`sync_file_permissions`) and directory permissions (`sync_dir_permissions`) for synced content. [verified]
- R-6.2.10: When a transfer connection stalls without context cancellation, the system shall enforce a per-transfer timeout or connection-level deadline to prevent indefinite hangs. [planned]
- R-6.2.11: When enumerating delta items, the system shall enforce a total item cap to bound memory growth from unbounded API responses. [planned]

## R-6.3 Process Model [verified]

- R-6.3.1: Only one sync process per configuration shall run at a time. [verified]
- R-6.3.2: Status and query commands shall be concurrent-reader safe while sync is running. [verified]
- R-6.3.3: The system shall enforce single-instance via PID file with advisory lock. [verified]

## R-6.4 Safety [implemented]

- R-6.4.1: When a sync would delete more items than `big_delete_threshold` (default: 1000), the system shall abort and require `--force`. [verified]
- R-6.4.2: When a sync would delete more than `big_delete_percentage` (default: 50%) of baseline items, the system shall abort. [verified]
- R-6.4.3: Big-delete protection shall apply both globally and per-folder. [verified]
- R-6.4.4: Remote deletions shall go to the OneDrive recycle bin by default (`use_recycle_bin`). [verified]
- R-6.4.5: Local deletions triggered by remote changes shall go to OS trash on macOS (`use_local_trash`). [verified]
- R-6.4.6: On Linux, local trash shall be opt-in (default off; servers/NAS typically lack XDG trash). [verified]
- R-6.4.7: The system shall support configurable disk space reservation (`min_free_space`). [planned]
- R-6.4.8: When receiving filenames from the delta API, the system shall validate them against path traversal characters (`..`, `/`, `\`, null bytes, control chars) and reject invalid names. [planned]
- R-6.4.9: When a requested permanent deletion fails (e.g., National Clouds that do not support it), the system shall fall back to recycle bin deletion. [planned]

## R-6.5 Crash Recovery [verified]

- R-6.5.1: The sync state store shall provide durable, transactional writes that survive process kill. [verified]
- R-6.5.2: Every sync operation shall be atomic — incomplete operations shall not corrupt state. [verified]
- R-6.5.3: On startup, the system shall detect items stuck in `syncing` state and reset them. [verified]

## R-6.6 Observability [implemented]

- R-6.6.1: The system shall support dual-channel logging: console (stderr) and log file. [verified]
- R-6.6.2: Console verbosity shall be controlled by `--quiet`, `--verbose`, and `--debug` flags. [verified]
- R-6.6.3: Log file level shall be controlled independently by `log_level` in config. [verified]
- R-6.6.4: The log file shall use structured JSON format. [verified]
- R-6.6.5: The system shall support progress bars and color-coded transfer output. [future]
- R-6.6.6: The system shall support a TUI (terminal UI) for real-time status. [future]

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
- R-6.7.12: When a transient HTTP 404 occurs on a valid resource (cross-datacenter load balancer timeout), the system shall classify it as transient and retry with backoff. [planned]
- R-6.7.13: When an HTTP 403 occurs on `/me/drives` shortly after token refresh (eventual consistency), the system shall classify it as transient and retry with backoff. [planned]
- R-6.7.14: When the Graph API returns HTTP 400 `invalidRequest` / `ObjectHandle is Invalid` during a multi-hour server outage, the system shall classify it as non-retryable and report the error rather than retrying indefinitely. [planned]
- R-6.7.15: The system shall truncate local timestamps to zero fractional seconds before comparing with OneDrive's whole-second precision. [planned]
- R-6.7.16: The system shall safely handle missing or invalid timestamps (`0001-01-01T00:00:00Z`, absent `lastModifiedDateTime` on deletions) without panicking. [planned]
- R-6.7.17: When a file completely lacks hashes (zero-byte files, certain Business/SharePoint files), the system shall use a fallback comparison of size + mtime + eTag. [implemented]
- R-6.7.18: When extracting identity data for shared items, the system shall use a four-level fallback chain: `remoteItem.shared.sharedBy` → `.owner` → `remoteItem.createdBy` → top-level `shared.owner`. [planned]
- R-6.7.19: The system shall not advance the delta token when a delta response contains zero events, to prevent permanently missing ephemeral deletion events. [planned]
- R-6.7.20: The system shall adapt observation methods based on drive type: folder-scoped delta for Personal shared folders, standard enumeration for Business/SharePoint (which do not support folder-scoped delta). [verified]
- R-6.7.21: The system shall handle shared folder items whose parent IDs reference a different drive (the sharer's drive), including driveId truncation and casing normalization on cross-drive references. [planned]
- R-6.7.22: When the API's path-based query returns an item from an incorrect path (fuzzy matching bug), the system shall post-validate that the returned item's name matches the requested path. [planned]
- R-6.7.23: The system shall URL-decode `parentReference.path` in non-delta responses before using it for path reconstruction. [planned]
- R-6.7.24: When a folder is renamed, the system shall infer and recalculate path changes for all descendants, since only the renamed folder appears in the delta response. [verified]
- R-6.7.25: When re-uploading a modified file to Business/SharePoint, the system shall accept the unavoidable extra version created by the API (unfixed Microsoft bug) without attempting futile workarounds. [planned]
- R-6.7.26: The system shall handle absent `lastModifiedDateTime` (null) on API-initiated deletions without error. [planned]

## R-6.8 Network Resilience [verified]

- R-6.8.1: The system shall respect 429 (Too Many Requests) with Retry-After headers. [verified]
- R-6.8.2: The system shall use exponential backoff with jitter for transient failures. [verified]
- R-6.8.3: All transfers shall be resumable after network interruption. [verified]

## R-6.9 Packaging [future]

- R-6.9.1: The system shall be distributable as a single static Go binary. [verified]
- R-6.9.2: The system shall provide a Homebrew formula (macOS). [future]
- R-6.9.3: The system shall provide deb/rpm packages. [future]
- R-6.9.4: The system shall provide an AUR package. [future]
- R-6.9.5: The system shall provide a Docker image (Alpine-based, multi-arch). [future]
