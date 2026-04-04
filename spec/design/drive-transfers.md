# Drive Transfers

GOVERNS: internal/driveops/cleanup.go, internal/driveops/disk_unix.go, internal/driveops/doc.go, internal/driveops/errors.go, internal/driveops/hash.go, internal/driveops/interfaces.go, internal/driveops/session.go, internal/driveops/session_store.go, internal/driveops/stale_partials.go, internal/driveops/transfer_manager.go, pkg/quickxorhash/quickxorhash.go, get.go, put.go

Implements: R-5.1 [verified], R-5.2 [verified], R-5.3 [implemented], R-5.5 [verified], R-1.2 [verified], R-1.2.5 [verified], R-1.3 [verified], R-1.3.5 [verified], R-5.6 [implemented], R-5.7 [verified], R-5.8 [planned], R-6.8.3 [verified], R-6.2.6 [verified], R-6.4.7 [verified], R-6.2.10 [implemented], R-6.10.6 [verified]

## TransferManager

Unified download/upload manager shared by both CLI file operations and the sync engine. Handles resume, hash verification, and cleanup.

## Ownership Contract

- Owns: Download/upload session mechanics, partial-file lifecycle, resumable upload bookkeeping, content-hash verification, and disk-space pre-check mechanics.
- Does Not Own: Graph auth/token lifecycle, sync planning/classification, or durable sync-failure persistence.
- Source of Truth: Local file content, remote metadata, and managed upload-session files.
- Allowed Side Effects: HTTP transfer calls plus local filesystem mutation through `synctree`, `localpath`, and `fsroot` according to the path trust boundary.
- Mutable Runtime Owner: Each `TransferManager` instance owns only request-scoped transfer state and managed session cleanup work. The package has no package-level mutable state or long-lived goroutines.
- Error Boundary: `driveops` translates transfer-specific failures into domain sentinels such as disk-space and hash errors. Retry scheduling and user-facing remediation are owned by higher layers.

### Download

Implements: R-6.2.3 [verified]

1. Create `.partial` file in target directory
2. If `.partial` exists with content, resume via HTTP Range request
3. Stream response body, computing QuickXorHash incrementally
4. Verify hash and size against API metadata
5. Atomic rename `.partial` → final path

### Upload

1. Stat the local file and reject anything above the 250 GB OneDrive limit before hashing or opening network transfer state
2. Files ≤ 4 MiB: simple PUT (single request)
3. Files > 4 MiB: create resumable upload session, upload in chunks (320 KiB aligned)
4. Verify server-reported hash matches local file after upload

If a non-zero-size create-by-parent simple upload returns `404 itemNotFound`,
the graph boundary retries that same create once through `createUploadSession`
before surfacing the error. This preserves the fast path for ordinary small
uploads while correcting the observed OneDrive quirk where read-only shared
folders misreport create denial as 404 on the simple-upload route but return
the correct 403 on the upload-session route.

Shared-file `put` reuses the same transfer machinery, but targets an existing
item by `(driveID, itemID)` instead of resolving a destination parent path.
The transfer manager therefore exposes both parent-path upload and existing-item
overwrite entry points while keeping session persistence, chunk sizing, and
post-upload verification in one owner.

## SessionStore

File-based upload session persistence. Each session is a JSON file in the data directory containing the upload URL, byte offset, file hash, and expiry. Managed-state access goes through `internal/fsroot` so directory creation, temp files, chmod, fsync, and rename stay under one root capability. On resume, local file hash is recomputed — if it differs from the stored hash, the session is discarded.

## Transfer Interfaces

Two required interfaces (`Downloader`, `Uploader`) and two optional interfaces (`RangeDownloader`, `SessionUploader`) type-asserted at runtime. This allows the graph client to support resume without requiring all implementations to.

Shared-item CLI flows still use the same downloader/uploader capabilities. The
only difference is how the target item identity is resolved before the transfer
starts:

- ordinary CLI commands resolve a configured drive plus a remote path
- shared CLI commands resolve a recipient account plus `(remoteDriveID, remoteItemID)`

## Hash Utilities

QuickXorHash computation for local files (`hash.go`). The `pkg/quickxorhash/` package implements the algorithm (vendored from rclone, BSD-0 license). When a remote file lacks a hash (common on Business/SharePoint), a fallback chain is attempted: QuickXorHash → SHA256 → SHA1. `HashVerified` is set to false when the remote hash is empty.

## Cleanup

`cleanup.go` removes stale `.partial` files and expired upload sessions on startup. `stale_partials.go` detects orphaned partial files from interrupted downloads. Session files live under the managed-state boundary (`internal/fsroot` via `SessionStore`). Orphaned `.partial` files live in the sync tree and are cleaned through `internal/synctree`. Both rooted boundaries carry unexported injectable ops so cleanup paths can be covered by deterministic create/write/rename/walk/remove failure tests.

## Disk Space Pre-Check

Implements: R-6.2.6 [verified], R-6.4.7 [verified], R-2.10.43 [verified], R-2.10.44 [verified]

`TransferManager.DownloadToFile` runs a disk space pre-check before every download — both sync engine and CLI `get` benefit automatically. Configured via `WithDiskCheck(minFreeSpace, diskAvailableFunc)` functional option at construction time. Two-tier check:

- **Critical**: available space < `min_free_space` → `ErrDiskFull`. In the sync engine, this triggers a `disk:local` scope block. In CLI `get`, it simply fails the download.
- **Per-file**: available space ≥ `min_free_space` but < file_size + `min_free_space` → `ErrFileTooLargeForSpace`. Per-file skip; other smaller files can still download.

Design properties:
- **Zero/nil disables**: `WithDiskCheck(0, fn)` or omitting the option entirely skips the check (R-6.4.7).
- **Fail-open**: statfs errors are logged and the download proceeds — a transient syscall failure should not block all downloads.
- **Path accuracy**: checks `filepath.Dir(targetPath)` rather than the sync root, correctly handling cross-filesystem mounts.
- **Error sentinels** (`errors.go`): `ErrDiskFull` and `ErrFileTooLargeForSpace` are in the `driveops` package. The sync engine matches them via `errors.Is` in `classifyResult` and `issueTypeForHTTPStatus`.
- **DiskAvailable** (`disk_unix.go`): exported function using `syscall.Statfs` — `f_bavail * f_bsize` (blocks available to unprivileged users). Build-tagged `darwin || linux`.

## Design Constraints

- `Upload()` accepts `io.ReaderAt` (not `io.Reader`): enables retry-safe uploads without re-opening the file. `io.NewSectionReader` creates independent readers for each chunk.
- Managed session files use `internal/fsroot`.
- Sync-engine runtime cleanup of `.partial` files under one configured sync root uses `internal/synctree`.
- Arbitrary local source/target paths use `internal/localpath`, making the three filesystem trust boundaries explicit instead of routing them through one helper package.
- `driveops.Session` may install a proof hook onto its authenticated Graph clients so successful live file operations can clear stale `auth:account` scope blocks. Pre-authenticated upload and download URLs bypass that hook and do not count as auth proof.
- Guard `.partial` file cleanup with `ctx.Err() == nil`: a 3.9 GB partial of a 4 GB download should survive Ctrl-C for resume. Only intentional deletions (hash mismatch) should remove partials.
- **Connection-level deadlines** (`transferTransport()`): `transferHTTPClient()` and `syncTransferHTTPClient()` use a shared `transferTransport()` with `ResponseHeaderTimeout: 2m` (detects servers that accept but never respond) and TCP keepalives (30s idle, 10s interval, 3 probes — detects dead connections within ~60s). No `http.Client.Timeout` — transfer duration varies with file size and bandwidth. [implemented]
- Transfer manager resume edge case tests cover corrupt partial file bytes, changed remote content during resume, and oversized partial state. [verified]

### Rationale: Per-Side Hashes

SharePoint enrichment silently modifies files after upload (hash/size change). Per-side hash baselines (`local_hash`, `remote_hash`) handle this natively: the planner compares new hashes against the correct side's baseline. No special code paths, no false conflicts from enrichment.
