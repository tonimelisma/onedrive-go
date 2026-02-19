# Feature Requests & User Needs (from abraunegg/onedrive issues)

> **Source**: abraunegg/onedrive GitHub repository (issues with `Feature Request` and `Enhancement` labels)
> **Date researched**: 2026-02-16
> **Scope**: ~130 labeled feature requests and enhancements; top ~45 investigated in detail
> **Popularity indicators**: comment count (discussion volume) and reaction count (silent agreement)

---

## 1. Sync Behavior Features

### 1.1 Configurable Sync Modes
**Issue [#356](https://github.com/abraunegg/onedrive/issues/356)** (17 comments, 1 reaction) — closed
- **Need**: Users want to configure sync mode (upload-only, download-only, local-first, no-remote-delete) in the config file rather than only via CLI flags. Docker users especially struggle with CLI-only options.
- **Implemented in reference**: Yes. The reference client eventually added `download_only`, `upload_only`, `no_remote_delete`, and `local_first` as config options.
- **Relevance for our client**: **High** — Config-file-driven sync modes are essential. All sync direction policies should be settable in config, not just CLI.

### 1.2 Upload-Only Without Remote Delete
**Issue [#49](https://github.com/abraunegg/onedrive/issues/49)** (10 comments) — closed
- **Need**: Users want to upload files to OneDrive without the client deleting remote files when local files are removed. This is critical for backup/archive use cases where local disk space is reclaimed after upload.
- **Implemented in reference**: Yes, via `--upload-only --no-remote-delete` combination.
- **Relevance for our client**: **High** — The upload-only + no-remote-delete combination is a core backup workflow.

### 1.3 Delay Upload for Rapid File Changes (Debouncing)
**Issue [#3234](https://github.com/abraunegg/onedrive/issues/3234)** (37 comments) — closed
- **Need**: Users editing files with tools like Obsidian trigger rapid save events, causing multiple unnecessary uploads. Users want a configurable delay/debounce before uploading after changes settle.
- **Implemented in reference**: Partially. The reference client added some delay handling but the discussion was extensive, suggesting it was a recurring pain point.
- **Relevance for our client**: **High** — File change debouncing is critical for any monitor-mode sync engine. Applications like Obsidian, IDEs, and office tools save frequently.

### 1.4 Selective Sync with Negative Patterns
**Issue [#1039](https://github.com/abraunegg/onedrive/issues/1039)** (5 comments) — closed
- **Need**: The `sync_list` file only supports inclusion patterns. Users need exclusion patterns (like `!path/to/skip`) for fine-grained control, similar to `.gitignore` syntax.
- **Implemented in reference**: Partially. The reference client has `skip_dir` and `skip_file` as separate mechanisms but never unified include/exclude in `sync_list` with negation.
- **Relevance for our client**: **High** — A unified `.gitignore`-style filter syntax (include + exclude in one file) would be a major usability improvement.

### 1.5 Override Filters for One-Time Sync
**Issue [#1129](https://github.com/abraunegg/onedrive/issues/1129)** (28 comments) — closed
- **Need**: Users who have directories excluded via `skip_dir` sometimes need to force-sync a specific excluded path without editing config and triggering a full resync.
- **Implemented in reference**: Not directly. The discussion was long, suggesting it remained a pain point.
- **Relevance for our client**: **Medium** — A `--force-path` or `--include-once` flag would be useful but is not essential for MVP.

### 1.6 Ignore Folder When Marker File Present
**Issue [#163](https://github.com/abraunegg/onedrive/issues/163)** (12 comments) — closed
- **Need**: Similar to `.nomedia` on Android, users want to place a marker file (e.g., `.nosync`) in a directory to exclude it from syncing. This avoids editing central config for per-directory exclusions.
- **Implemented in reference**: Yes, via `check_nosync` config option and `.nosync` marker file.
- **Relevance for our client**: **High** — Simple, user-friendly mechanism. Should be supported from the start.

### 1.7 Skip Git Repositories
**Issue [#3235](https://github.com/abraunegg/onedrive/issues/3235)** (6 comments) — closed
- **Need**: Developers syncing home directories or Documents folders want to automatically skip directories containing `.git`, since git repos have huge numbers of small files and are already backed up on GitHub/GitLab.
- **Implemented in reference**: Not as a dedicated feature. Users had to manually add `skip_dir` entries.
- **Relevance for our client**: **Medium** — Could be a nice default or config option. Alternatively, the `.nosync` marker approach covers this generically.

### 1.8 Detect Missing Sync Folder (Safety Guard)
**Issue [#8](https://github.com/abraunegg/onedrive/issues/8)** (19 comments) — closed
- **Need**: If the local sync root is on an external drive or mount that is not present, the client sees "all files deleted" and propagates mass deletions to OneDrive. Users want the client to detect a missing/unmounted sync root and abort.
- **Implemented in reference**: Yes. The reference client checks for sync directory existence and warns.
- **Relevance for our client**: **High** — Critical safety feature. Must detect unmounted/missing sync root before any destructive operation.

### 1.9 Warn on Large Deletes
**Issue [#458](https://github.com/abraunegg/onedrive/issues/458)** (9 comments) — closed
- **Need**: Users accidentally triggered mass deletions by misconfiguring paths. Want the client to warn/confirm before deleting a large number of files (threshold-based safety net).
- **Implemented in reference**: Yes, via `classify_as_big_delete` threshold (default 1000) which pauses and warns.
- **Relevance for our client**: **High** — Essential safety feature. Must implement configurable large-delete threshold with interactive confirmation or abort.

### 1.10 Detect Need for Resync Automatically
**Issue [#280](https://github.com/abraunegg/onedrive/issues/280)** (7 comments) — closed
- **Need**: When users change `skip_dir` or `skip_file` config, the sync state becomes inconsistent. The client should detect config changes and automatically suggest/require a resync rather than silently producing wrong results.
- **Implemented in reference**: Partially. Some automatic detection was added over time.
- **Relevance for our client**: **High** — Our sync engine should hash the filter config and detect changes between runs.

### 1.11 Cleanup Local Files in Download-Only Mode
**Issue [#2092](https://github.com/abraunegg/onedrive/issues/2092)** (15 comments) — closed
- **Need**: In download-only mode, files deleted on OneDrive remain locally. Users want an option to mirror remote deletions locally even in download-only mode (i.e., true one-way mirror from remote to local).
- **Implemented in reference**: Yes, via `--cleanup-local-files` option.
- **Relevance for our client**: **Medium** — Useful for "pull mirror" scenarios. Should be a config option.

### 1.12 Local Recycle Bin / Trash
**Issue [#3142](https://github.com/abraunegg/onedrive/issues/3142)** (14 comments, 2 reactions) — closed
- **Need**: Instead of permanently deleting local files during sync, move them to a configurable local trash folder with optional auto-purge after N days.
- **Implemented in reference**: Not implemented.
- **Relevance for our client**: **Medium** — Good safety feature. Could integrate with OS trash (macOS Trash, XDG Trash on Linux) instead of a custom folder.

### 1.13 Dry-Run Mode
**Issue [#317](https://github.com/abraunegg/onedrive/issues/317)** (10 comments, 2 reactions) — closed
- **Need**: Users want to preview what a sync would do without actually making changes. Especially important when setting up or changing config.
- **Implemented in reference**: Yes, via `--dry-run` option.
- **Relevance for our client**: **High** — Essential for user confidence. Should be available from MVP.

### 1.14 Mark Partially-Downloaded Files
**Issue [#1593](https://github.com/abraunegg/onedrive/issues/1593)** (16 comments) — closed
- **Need**: Interrupted downloads leave corrupt files that look complete. These may then be uploaded back, destroying the good remote copy. Users want partial downloads marked (e.g., `.partial` suffix) or deleted.
- **Implemented in reference**: Yes, download validation was added using hash verification.
- **Relevance for our client**: **High** — Must use `.partial` suffix or temp directory during download, then atomic rename on completion. Hash validation mandatory.

---

## 2. Performance Features

### 2.1 Multi-Threaded / Parallel Uploads and Downloads
**Issue [#232](https://github.com/abraunegg/onedrive/issues/232)** (16 comments, **39 reactions** — most reacted issue) — closed
- **Need**: The Windows OneDrive client uses up to 4 concurrent transfers. The reference client was single-threaded for transfers, making it extremely slow for many small files or initial sync of large datasets.
- **Implemented in reference**: Yes, eventually. The v2.5 rewrite added configurable thread count for operations.
- **Relevance for our client**: **High** — This is the single most popular request by reactions. Go's goroutines make concurrent transfers natural. Must be a core feature with configurable parallelism.

### 2.2 Asynchronous / Pipelined Operations
**Issue [#157](https://github.com/abraunegg/onedrive/issues/157)** (9 comments, 4 reactions) — closed
- **Need**: The reference client processes everything serially: scan all files, calculate hashes, then upload one by one. Users with 300K files reported 2+ hour waits before any upload started. Need to pipeline: hash calculation, database updates, and network transfers should overlap.
- **Implemented in reference**: Partially in v2.5 with threading.
- **Relevance for our client**: **High** — Go makes this straightforward with goroutines and channels. Pipeline architecture: scanner -> hasher -> uploader should run concurrently.

### 2.3 Efficient Delta Sync with sync_list
**Issues [#679](https://github.com/abraunegg/onedrive/issues/679)** (38 comments), **[#690](https://github.com/abraunegg/onedrive/issues/690)** (8 comments), **[#818](https://github.com/abraunegg/onedrive/issues/818)** (11 comments) — all closed
- **Need**: When using `sync_list` (selective sync), the reference client still processes ALL changes from the delta feed, even those outside the sync scope. Users with large OneDrives but small sync_lists experienced multi-minute syncs of "Processing N changes" with no actual work. Initial sync was hours of "Skipping item - excluded by sync_list."
- **Implemented in reference**: Improved over time but remained a sore point.
- **Relevance for our client**: **High** — Delta query returns all changes; we must filter efficiently. Store the delta token and skip non-matching items quickly (in-memory filter, not per-item API calls). Consider per-folder sync for targeted sync_list entries.

### 2.4 Sync Smallest Files First (Prioritization)
**Issue [#2544](https://github.com/abraunegg/onedrive/issues/2544)** (11 comments, 3 reactions) — closed
- **Need**: Users on slow/intermittent connections want to sync small files first so they get maximum file count synced in limited time windows.
- **Implemented in reference**: Not implemented.
- **Relevance for our client**: **Medium** — Configurable transfer priority (smallest-first, newest-first, etc.) would be a differentiator. Low effort if we already have a transfer queue.

### 2.5 Chunked / Resumable Downloads
**Issue [#2576](https://github.com/abraunegg/onedrive/issues/2576)** (4 comments, 1 reaction) — closed
- **Need**: Large file downloads fail on unreliable connections because the reference client does simple GET requests. Users want Range-header-based chunked downloads with resume capability, similar to how uploads use upload sessions.
- **Implemented in reference**: Not implemented for downloads (uploads already had session-based chunking).
- **Relevance for our client**: **Medium** — Resumable downloads with Range headers are straightforward to implement. Good for large files on unreliable connections.

### 2.6 Resource Usage Limits
**Issue [#3461](https://github.com/abraunegg/onedrive/issues/3461)** (9 comments) — closed
- **Need**: The reference client consumed excessive RAM (5-10+ GB for large accounts) and CPU, making it crash in resource-constrained Docker containers. Users want configurable CPU and memory limits.
- **Implemented in reference**: Thread count became configurable, but no explicit memory limits.
- **Relevance for our client**: **Medium** — Good engineering practice. Go's memory management is better than D's, but we should still consider memory-bounded queues and streaming (not loading entire file trees into memory).

---

## 3. Platform & Compatibility Features

### 3.1 On-Demand Files (Files On Demand / Placeholder Files)
**Issue [#757](https://github.com/abraunegg/onedrive/issues/757)** (28 comments, **8 reactions** — 3rd most reacted) — **OPEN**
- **Need**: Like Windows OneDrive's "Files On-Demand," show all cloud files in the local filesystem as placeholders that download on first access. Right-click options for "Always keep on this device" and "Free up space."
- **Implemented in reference**: Not implemented. This is architecturally complex, requiring a virtual filesystem layer (FUSE on Linux, FUSE/macFUSE on macOS).
- **Relevance for our client**: **High (future)** — This is a killer feature for the long term but extremely complex. Requires FUSE or platform-specific virtual filesystem integration. Not MVP material, but should be designed for. macOS has File Provider framework; Linux needs FUSE.

### 3.2 Office 365 Government Cloud Support
**Issue [#937](https://github.com/abraunegg/onedrive/issues/937)** (41 comments) — closed
- **Need**: US Government cloud (GCC, GCC-High, DoD) uses different authentication endpoints (`login.microsoftonline.us`, `graph.microsoft.us`). Users could not authenticate.
- **Implemented in reference**: Yes. The reference client added configurable `azure_ad_endpoint` and `azure_ad_graph_endpoint`.
- **Relevance for our client**: **Medium** — Important for government/enterprise users. Should support configurable cloud endpoints from the start (it is low effort to parameterize URLs).

### 3.3 Intune SSO / D-Bus Authentication
**Issue [#3209](https://github.com/abraunegg/onedrive/issues/3209)** (36 comments) — closed
- **Need**: Enterprise users with Intune-enrolled Linux devices want seamless SSO via the `microsoft-identity-device-broker` D-Bus interface, avoiding manual re-authentication.
- **Implemented in reference**: Discussed extensively but complex to implement.
- **Relevance for our client**: **Low** — Highly specialized enterprise feature. Worth noting for future but not a priority.

### 3.4 OAuth2 Device Authorization Flow
**Issue [#2693](https://github.com/abraunegg/onedrive/issues/2693)** (18 comments, 1 reaction) — closed
- **Need**: The traditional browser-redirect OAuth flow is cumbersome on headless servers and containers. Device code flow (user visits a URL and enters a code) is much smoother.
- **Implemented in reference**: Not implemented in the reference (it used browser redirect). The reference author was resistant.
- **Relevance for our client**: **High** — We already use device code flow! This is a competitive advantage we have over the reference client.

### 3.5 Docker / Container Support
**Issues [#282](https://github.com/abraunegg/onedrive/issues/282)** (14 comments), **[#669](https://github.com/abraunegg/onedrive/issues/669)** (13 comments), multiple Docker env var issues
- **Need**: The reference client had extensive Docker support but users constantly struggled with: large image size, limited env var configuration, resync in containers, and entrypoint script limitations.
- **Implemented in reference**: Yes, with ongoing friction.
- **Relevance for our client**: **Medium** — Go produces a single static binary, making Docker images trivially small (scratch or Alpine base). Our architecture inherently solves the image size problem.

### 3.6 Unix File Permissions
**Issues [#1100](https://github.com/abraunegg/onedrive/issues/1100)** (9 comments), **[#2971](https://github.com/abraunegg/onedrive/issues/2971)** (6 comments) — closed
- **Need**: Downloaded files get default permissions. Users want configurable file/folder permissions (umask-style). Developers with git repos in OneDrive suffer from permission bit changes causing spurious diffs.
- **Implemented in reference**: Partially. Configurable file/folder permissions were added.
- **Relevance for our client**: **Medium** — Should support configurable default permissions. Extended attribute storage of original permissions is interesting but niche.

---

## 4. UI/UX Features

### 4.1 Upload/Download Progress Indication
**Issues [#12](https://github.com/abraunegg/onedrive/issues/12)** (17 comments), **[#2512](https://github.com/abraunegg/onedrive/issues/2512)** (5 comments), **[#185](https://github.com/abraunegg/onedrive/issues/185)** (2 comments, 2 reactions)
- **Need**: Large file uploads showed no progress in normal log mode (only verbose). Users want progress bars with percentage, speed, and ETA for large transfers.
- **Implemented in reference**: Yes, progress display was added and improved over time.
- **Relevance for our client**: **High** — Essential UX. Progress bars for large transfers, overall sync progress indicator, and transfer speed display should be standard.

### 4.2 Sync Status Command
**Issue [#112](https://github.com/abraunegg/onedrive/issues/112)** (9 comments) — closed
- **Need**: When running in monitor/daemon mode, users want a command to check current sync status ("syncing", "up-to-date", "error") — like `dropbox-cli status`.
- **Implemented in reference**: Yes, via `--display-sync-status`.
- **Relevance for our client**: **High** — A `status` subcommand is essential for daemon mode. Should report: sync state, last sync time, pending changes count, any errors.

### 4.3 Desktop Notifications
**Issues [#267](https://github.com/abraunegg/onedrive/issues/267)** (9 comments), **[#342](https://github.com/abraunegg/onedrive/issues/342)** (7 comments), **[#3250](https://github.com/abraunegg/onedrive/issues/3250)** (16 comments)
- **Need**: Users want desktop notifications for sync events (file synced, errors, auth expiry). Also want granular control over which notifications are shown.
- **Implemented in reference**: Yes, via libnotify. Granular notification control was added later.
- **Relevance for our client**: **Medium** — Nice to have. On macOS can use native notification center; on Linux use D-Bus/libnotify. Should be optional and granular.

### 4.4 Notification on Auth Expiry
**Issue [#1042](https://github.com/abraunegg/onedrive/issues/1042)** (18 comments) — closed
- **Need**: Enterprise users with short-lived tokens (corporate policy forces re-auth every 2 weeks) need clear notification that authentication has expired, rather than silent failures in logs.
- **Implemented in reference**: Improved over time with better error messaging.
- **Relevance for our client**: **High** — Auth expiry must be clearly surfaced: desktop notification, status command, and log entry. Ideally auto-refresh tokens before expiry.

### 4.5 Quota / Storage Status Display
**Issue [#2359](https://github.com/abraunegg/onedrive/issues/2359)** (9 comments) — closed
- **Need**: Users want a quick command to see OneDrive storage usage (used, remaining, total, deleted) in human-readable format.
- **Implemented in reference**: Yes, added as a CLI option.
- **Relevance for our client**: **High** — Simple to implement (one API call to `/me/drive`). Should be a `quota` or `status` subcommand. Already partially covered by our existing CLI.

### 4.6 Get Shareable Link for Local File
**Issues [#611](https://github.com/abraunegg/onedrive/issues/611)** (15 comments), **[#1040](https://github.com/abraunegg/onedrive/issues/1040)** (8 comments)
- **Need**: Given a local file path, output the OneDrive web URL or create a shareable link. Avoids navigating the web interface.
- **Implemented in reference**: Yes, via `--get-file-link` and `--create-share-link`.
- **Relevance for our client**: **Medium** — Useful utility feature. We already have permissions/sharing in our API client.

### 4.7 Configurable Monitor Interval
**Issue [#31](https://github.com/abraunegg/onedrive/issues/31)** (24 comments, 2 reactions) — closed
- **Need**: The monitor mode polling interval was hardcoded at 45 seconds. Users wanted it configurable for different use cases (more frequent for active work, less frequent for background sync to reduce API calls).
- **Implemented in reference**: Yes, via `monitor_interval` config option.
- **Relevance for our client**: **High** — Must be configurable from the start. Also consider adaptive intervals (more frequent during active use, back off when idle).

---

## 5. Enterprise / Business Features

### 5.1 Sync Shared Folders (Business)
**Issues [#459](https://github.com/abraunegg/onedrive/issues/459)** (73 comments — **most commented issue**), **[#382](https://github.com/abraunegg/onedrive/issues/382)** (10 comments), **[#1986](https://github.com/abraunegg/onedrive/issues/1986)** (12 comments), **[#2824](https://github.com/abraunegg/onedrive/issues/2824)** (16 comments, 2 reactions)
- **Need**: Syncing OneDrive Business shared folders is the most discussed feature by far. Users want folders shared by coworkers to sync locally, including efficient delta sync (not full re-scan). Moving shared folder shortcuts to subfolders (like Windows client allows) is also wanted.
- **Implemented in reference**: Yes, via `sync_business_shared_folders` config option. But it had persistent issues: full rescan instead of delta for shared folders, problems with moved shortcuts, and shared folders with same basename getting merged.
- **Relevance for our client**: **High** — This is the #1 most discussed feature. Must support shared folder sync with proper delta tracking per shared drive.

### 5.2 SharePoint / Teams Document Libraries
**Issues [#5](https://github.com/abraunegg/onedrive/issues/5)** (15 comments, 1 reaction), **[#748](https://github.com/abraunegg/onedrive/issues/748)** (7 comments)
- **Need**: Teams channels store files in SharePoint document libraries. Users want to sync these just like OneDrive folders. Separate from the shared folder feature — this is about syncing entire SharePoint site document libraries.
- **Implemented in reference**: Yes, via `--get-O365-drive-id` to discover SharePoint drive IDs and separate config directories to sync them.
- **Relevance for our client**: **High** — SharePoint document libraries use the same Graph API (Drives endpoint). Must support specifying arbitrary drive IDs.

### 5.3 SharePoint Shortcuts ("Add Shortcut to My Files")
**Issue [#1224](https://github.com/abraunegg/onedrive/issues/1224)** (10 comments, 4 reactions) — closed
- **Need**: Modern OneDrive lets users add SharePoint library folders as shortcuts in their OneDrive root. These appear as `remoteItem` references in the Graph API. The reference client initially could not follow these shortcuts.
- **Implemented in reference**: Eventually supported.
- **Relevance for our client**: **High** — Must handle `remoteItem` references in delta feeds. These are increasingly common in enterprise environments.

### 5.4 Sync Individual Shared Files (Business)
**Issue [#1300](https://github.com/abraunegg/onedrive/issues/1300)** (11 comments) — closed
- **Need**: Beyond shared folders, users receive individual shared files (Excel sheets, documents) that they want synced locally.
- **Implemented in reference**: Not fully. Individual shared files were explicitly skipped with a warning.
- **Relevance for our client**: **Medium** — Complex because individual shared files live in someone else's drive. Could be handled as symbolic references or separate download targets.

### 5.5 Multiple Drive / Config Support
**Issue [#2310](https://github.com/abraunegg/onedrive/issues/2310)** (4 comments) — closed
- **Need**: Users syncing multiple SharePoint libraries or multiple OneDrive accounts need separate config directories with separate auth. They want a single config managing multiple sync targets.
- **Implemented in reference**: Not in a single config. Users had to run multiple instances with `--confdir`.
- **Relevance for our client**: **Medium** — Multi-account/multi-drive support in a single config would be a differentiator. Consider TOML/YAML config with array of sync targets.

### 5.6 File Creator / Editor Metadata
**Issue [#2719](https://github.com/abraunegg/onedrive/issues/2719)** (12 comments, 1 reaction) — closed
- **Need**: In shared/team environments, users want to see who last edited a file. Proposal was to store this in extended file attributes (xattr).
- **Implemented in reference**: Not implemented.
- **Relevance for our client**: **Low** — Niche feature. Could store in xattr or in our local database for query purposes.

---

## 6. Integration Features

### 6.1 Client-Side Encryption
**Issue [#1023](https://github.com/abraunegg/onedrive/issues/1023)** (8 comments, 1 reaction) — **OPEN**
- **Need**: Encrypt files before upload so Microsoft cannot read them. Decrypt after download. Privacy-conscious users, especially in enterprise.
- **Implemented in reference**: Not implemented.
- **Relevance for our client**: **Low** — Complex feature with many UX implications (filename encryption? directory structure? sharing?). Better served by tools like Cryptomator or rclone's crypt backend. Consider as a far-future feature.

### 6.2 Permanent Delete on OneDrive
**Issue [#2803](https://github.com/abraunegg/onedrive/issues/2803)** (10 comments) — closed
- **Need**: When deleting files, they go to OneDrive recycle bin. Some users want permanent deletion (skip recycle bin) to truly free up space immediately.
- **Implemented in reference**: Not at time of request. Graph API supports `permanentDelete` endpoint.
- **Relevance for our client**: **Low** — Niche feature. Easy to implement (one API endpoint) but dangerous. Should require explicit opt-in config.

### 6.3 Backup Mode
**Issue [#3559](https://github.com/abraunegg/onedrive/issues/3559)** (4 comments) — closed
- **Need**: Users want a "backup before sync" safety net: snapshot current state before applying changes, so accidental data loss from resync or misconfiguration is recoverable.
- **Implemented in reference**: Not implemented.
- **Relevance for our client**: **Low** — Better solved by the local trash/recycle bin feature (#3142) and the large-delete warning (#458). Full backup mode is overkill.

---

## 7. Cross-Cutting Themes

Several themes emerge across multiple issues:

| Theme | Issues | Core Insight |
|-------|--------|-------------|
| **Safety** | #8, #458, #280, #1593, #3142, #3559 | Users have lost data. Every destructive operation needs guardrails: missing-root detection, big-delete threshold, atomic downloads, local trash, config-change detection. |
| **Performance** | #232, #157, #679, #690, #818, #2544, #3461 | The reference client was painfully slow. Parallelism, efficient delta processing, and pipelined operations are non-negotiable. |
| **Shared content** | #459, #382, #5, #1224, #1300, #1986, #2824 | Business users desperately need shared folder and SharePoint sync. This is the #1 discussion topic. |
| **Flexibility** | #356, #49, #1129, #163, #1039, #2092, #2310 | Users have diverse workflows (backup, mirror, selective sync). The sync engine must support multiple modes and flexible filtering. |
| **Observability** | #12, #112, #267, #1042, #2359, #2512 | Users need to know what the client is doing: progress, status, notifications, quota. Silent operation breeds distrust. |

---

## Feature Prioritization for Our Client

### MVP (Minimum Viable Sync)
These are table-stakes features without which users will not switch from the reference client:

| Feature | Source Issues | Rationale |
|---------|--------------|-----------|
| Bidirectional sync with delta tracking | #679, #690, #818 | Core function; must be efficient |
| Parallel transfers (configurable threads) | #232, #157 | Most reacted feature; Go makes this easy |
| Configurable sync modes (upload-only, download-only, no-remote-delete) | #356, #49, #2092 | Essential for backup and mirror workflows |
| `.nosync` marker file support | #163 | Simple, high value |
| Sync filter rules (skip_dir, skip_file, sync_list) | #1039, #1129 | Users need selective sync |
| Missing sync root detection | #8 | Critical safety |
| Large-delete threshold and warning | #458 | Critical safety |
| Atomic downloads (temp file + rename + hash verify) | #1593 | Data integrity |
| Dry-run mode | #317 | User confidence |
| Progress display for transfers | #12, #2512 | Basic UX |
| Sync status command | #112 | Basic UX |
| Quota display | #2359 | One API call, high value |
| Device code flow auth | #2693 | Already implemented |
| Configurable monitor interval | #31 | Simple config option |
| File change debouncing | #3234 | Prevents upload storms |

### v1.0 (Competitive Feature Set)
These differentiate us and capture the business user segment:

| Feature | Source Issues | Rationale |
|---------|--------------|-----------|
| Business shared folder sync (with delta) | #459, #1986, #2824 | #1 most discussed feature |
| SharePoint document library sync | #5, #748 | Enterprise essential |
| SharePoint shortcut (remoteItem) handling | #1224 | Increasingly common |
| Unified `.gitignore`-style filter syntax | #1039 | Major UX improvement over reference |
| Government cloud support | #937 | Parameterize endpoints |
| Multiple sync targets in one config | #2310 | Differentiator |
| Desktop notifications (opt-in, granular) | #267, #3250 | Modern UX |
| Auth expiry notification and proactive refresh | #1042 | Enterprise reliability |
| Configurable file/folder permissions | #1100, #2971 | Linux users need this |
| Transfer prioritization (smallest-first option) | #2544 | Low effort, high value |
| Resumable downloads (Range headers) | #2576 | Reliability |
| Local trash / recycle bin | #3142 | Safety net |
| Config-change-triggered resync detection | #280 | Prevent stale state |

### Future (v2.0+)
These are high-complexity or niche features for later consideration:

| Feature | Source Issues | Rationale |
|---------|--------------|-----------|
| On-demand / placeholder files (FUSE) | #757 | Killer feature but architecturally complex |
| Client-side encryption | #1023 | Complex UX; better via external tools |
| Individual shared file sync | #1300 | Complex cross-drive mechanics |
| Intune SSO / D-Bus auth | #3209 | Highly specialized |
| File metadata in extended attributes | #2719 | Niche |
| Permanent delete option | #2803 | Dangerous; niche use case |

### Key Design Principles (derived from user pain)

1. **Safety first**: Every destructive operation (delete, overwrite) must have a guardrail. The reference client's biggest source of user horror stories was accidental data loss.
2. **Performance is a feature**: Parallel transfers and efficient delta processing are not optimizations — they are requirements. Users with 50K+ files abandoned the reference client due to speed.
3. **Shared content is not optional**: Business users represent a large portion of the user base, and shared folder sync is their primary need.
4. **Observability builds trust**: Users who cannot see what the client is doing assume it is broken. Progress, status, and notifications are essential.
5. **Flexible filtering**: Users have wildly diverse directory structures. The filter system must be expressive (include, exclude, glob, regex, marker files) and coherent (one mental model, not three separate mechanisms).
