# Drive Transfers

GOVERNS: internal/driveops/cleanup.go, internal/driveops/disk_unix.go, internal/driveops/doc.go, internal/driveops/errors.go, internal/driveops/hash.go, internal/driveops/interfaces.go, internal/driveops/session.go, internal/driveops/session_store.go, internal/driveops/stale_partials.go, internal/driveops/transfer_manager.go, pkg/quickxorhash/quickxorhash.go, get.go, put.go

Implements: R-5.1 [verified], R-5.2 [verified], R-5.3 [verified], R-5.5 [verified], R-1.2 [verified], R-1.2.5 [verified], R-1.3 [verified], R-1.3.5 [verified], R-1.3.6 [verified], R-1.4.4 [verified], R-5.6 [verified], R-5.7 [verified], R-5.8 [verified], R-6.7.14 [verified], R-6.8.3 [verified], R-6.2.6 [verified], R-6.4.7 [verified], R-6.2.10 [verified], R-6.10.6 [verified]

## TransferManager

Unified download/upload manager shared by both CLI file operations and the sync engine. Handles resume, hash verification, and cleanup.

## Ownership Contract

- Owns: Download/upload session mechanics, partial-file lifecycle, resumable upload bookkeeping, content-hash verification, and disk-space pre-check mechanics.
- Does Not Own: Graph auth/token lifecycle, sync planning/classification, or durable sync-failure persistence.
- Source of Truth: Local file content, remote metadata, and managed upload-session files.
- Allowed Side Effects: HTTP transfer calls plus local filesystem mutation through `synctree`, `localpath`, and `fsroot` according to the path trust boundary.
- Mutable Runtime Owner: Each `TransferManager` instance owns only request-scoped transfer state and managed session cleanup work. The package has no package-level mutable state or long-lived goroutines.
- Error Boundary: `driveops` translates transfer-specific failures into domain sentinels such as disk-space and hash errors. Retry scheduling and user-facing remediation are owned by higher layers.

## Verified By

| Behavior | Evidence |
| --- | --- |
| Downloads use partial-file resume plus hash verification before the final atomic rename. | `internal/driveops/download_test.go`, `internal/driveops/hash_test.go`, `internal/localpath/localpath_test.go` (`TestAtomicWrite`) |
| Uploads keep simple-upload and upload-session mechanics inside the transfer boundary. | `internal/driveops/upload_test.go`, `internal/graph/upload_test.go`, `internal/graph/upload_session_test.go` |
| Sync execution reuses the same transfer boundary instead of inventing a second transfer path. | `internal/sync/executor_test.go`, `internal/cli/sync_helpers_test.go` |

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

Resumable uploads use a fixed 10 MiB fragment size in the graph boundary.
The config surface intentionally does not expose a user-tunable chunk-size
knob: 10 MiB is the documented Microsoft-recommended stable high-speed size,
and keeping fragment sizing graph-owned avoids a dead or misleading config
surface.

If a fragment PUT returns HTTP 416, the graph boundary immediately queries the
session status endpoint, adopts the returned upload URL when Graph rotates it,
parses the authoritative `nextExpectedRanges` start offset, and resumes from
that offset. Empty, malformed, or non-forward session replies are treated as
hard errors instead of guessing.

If a non-zero-size create-by-parent simple upload returns `404 itemNotFound`,
the graph boundary retries that same create once through `createUploadSession`
before surfacing the error. This preserves the fast path for ordinary small
uploads while correcting the observed OneDrive quirk where read-only shared
folders misreport create denial as 404 on the simple-upload route but return
the correct 403 on the upload-session route.

If that same fresh-parent create family still exhausts the bounded
`createUploadSession` retry budget on exact `itemNotFound`, the graph boundary
replays the original simple upload under a second, slightly longer bounded
create-convergence policy before giving up. That keeps the session route fast
as the permission-oracle fallback while extending robustness for ordinary
own-drive creates whose parent path is readable slightly before the child
create routes converge.

If the create-by-parent simple upload falls back to the upload-session route
and that session succeeds, the session result is already final. The graph
boundary must not run the simple-upload-only mtime finalization PATCH on that
session-created item, because upload-session writes already own timestamp
authority for that path and an extra simple-finalization PATCH only adds a
second false-negative point.

When simple upload succeeds and mtime preservation is required, the immediate
follow-on `UpdateFileSystemInfo` PATCH can also briefly return `404
itemNotFound` for the returned item ID. That retry stays in the graph boundary
and only applies to the post-simple-upload finalization path; transfer-manager
and CLI callers still see one success or one failure outcome.

Shared-file `put` reuses the same transfer machinery, but targets an existing
item by `(driveID, itemID)` instead of resolving a destination parent path.
The transfer manager therefore exposes both parent-path upload and existing-item
overwrite entry points while keeping session persistence, chunk sizing, and
post-upload verification in one owner.

Sync execution uses the same split deliberately: when planning already knows
the authoritative remote `itemID`, uploads overwrite that item by ID instead of
recreating the file through the parent-path route. Parent-based uploads remain
for true creates where no remote item identity exists yet. This keeps ordinary
edits and local-wins edit/delete conflict recovery on the narrower overwrite
boundary and avoids teaching parent-creation consistency gaps to flows that
already have stable remote identity.

`driveops` is the single owner of post-success path convergence and
path-authoritative delete reconciliation for one resolved drive session.
Graph can acknowledge folder creation, upload, or move before an immediate
follow-on path lookup stops returning `itemNotFound`, and it can briefly
return `DELETE .../items/{id} = 404 itemNotFound` even though the same
remote path just resolved successfully. During repeated sibling deletes, the
exact path route itself can also lie with `GET ...root:/path: = 404
itemNotFound` even though the parent collection still lists the leaf, while
that same parent collection can lag positively after a successful delete. The
package boundary therefore exposes the `PathConvergence` capability plus the
`PathConvergenceFactory`, both satisfied by `driveops.Session`, and keeps
delete-target path recovery in the same owner:

- `WaitPathVisible()` so command handlers can require destination-path
  readability before they print a successful `mkdir`, `put`, or `mv`, and so
  `put` can re-resolve an already-created parent path through the same bounded
  visibility gate instead of trusting a one-shot path lookup; when the exact
  path route still lies with `itemNotFound`, the visibility gate confirms the
  path by exact-name parent/ancestor listing before it spends more retry
  budget. The deterministic wait now keeps the `32s` cap for three capped
  sleeps, which yields about `95.75s` of scheduled wait before request
  overhead and roughly a two-minute wall-clock budget, because live Graph
  evidence has shown a path can become readable, regress to `404 itemNotFound`,
  and only recover again well after the older ~32-second budget
- `ResolveDeleteTarget()` so path-oriented deletes can fall back from an exact
  path `itemNotFound` to the parent collection before they decide the target
  is already gone; when the parent-path listing itself is in a transient
  `itemNotFound` gap, delete intent recurses up one level, resolves the parent
  folder through ancestor collections, and then lists that parent by item ID
- `DeleteResolvedPath()` / `PermanentDeleteResolvedPath()` so path-oriented
  deletes keep authority on the remote path instead of trusting one stale
  item ID forever; after any delete-by-ID `itemNotFound`, they re-resolve the
  path through the same exact-path plus parent-collection fallback used by
  `ResolveDeleteTarget()`, treat the target as gone only when both routes miss,
  and downgrade a final parent-list-only stale hit that still deletes as
  `itemNotFound` to success only after the bounded convergence schedule
  exhausts

`WaitPathVisible()` returns a typed `PathNotVisibleError` when the bounded
schedule exhausts on exact `itemNotFound` visibility lag alone. `mkdir`,
single-file `put`, and `mv` still treat that as a hard degraded-success
failure because their contract is "destination path is readable now." `rm`
uses the same typed error only for its post-delete parent confirmation and
downgrades it to a warning once `DeleteResolvedPath()` has already proved the
target path is gone.

Sync execution consumes the same capability for post-success visibility
confirmation after remote folder create, upload, and move. Those sync probes
stay best-effort and warn-only, but they no longer own a second retry budget
or sleep loop. For same-drive actions the executor reuses its current session.
For cross-drive shared-root actions it asks the factory for a target-scoped
session and probes the target-drive-relative path rooted at that configured
shared root's
remote root item. If that root metadata is missing, sync skips the probe
instead of guessing and touching the wrong remote boundary.

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

For iOS `.heic` downloads, the transfer manager keeps the normal integrity
mechanism: retry hash mismatch, warn, then accept after exhaustion so the sync
engine does not loop forever on a server metadata bug. When the accepted
download is a `.heic` whose API metadata still disagrees with the downloaded
bytes, the warning explicitly names the known OneDrive/iOS metadata mismatch
instead of looking like an unexplained integrity bypass.

## Cleanup

`cleanup.go` removes stale `.partial` files and expired upload sessions on startup. `stale_partials.go` detects orphaned partial files from interrupted downloads. Session files live under the managed-state boundary (`internal/fsroot` via `SessionStore`). Orphaned `.partial` files live in the sync tree and are cleaned through `internal/synctree`. Both rooted boundaries carry unexported injectable ops so cleanup paths can be covered by deterministic create/write/rename/walk/remove failure tests.

## Disk Space Pre-Check

Implements: R-6.2.6 [verified], R-6.4.7 [verified], R-2.10.43 [verified], R-2.10.44 [verified]

`TransferManager.DownloadToFile` runs a disk space pre-check before every download — both sync engine and CLI `get` benefit automatically. Configured via `WithDiskCheck(minFreeSpace, diskAvailableFunc)` functional option at construction time. Two-tier check:

- **Critical**: available space < `min_free_space` → `ErrDiskFull`. In the sync engine, this triggers a `disk:local` block scope. In CLI `get`, it simply fails the download.
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
- `driveops.SessionRuntime` owns the reused Graph HTTP clients together with token-source caching. It chooses target-scoped interactive or sync HTTP profiles by composing the stateless builders in `internal/graphtransport`; callers no longer inject one cache owner into another.
- `driveops.Session` may install a proof hook onto its authenticated Graph clients so successful live file operations can clear stale catalog auth requirements. Pre-authenticated upload and download URLs bypass that hook and do not count as auth proof.
- Guard `.partial` file cleanup with `ctx.Err() == nil`: a 3.9 GB partial of a 4 GB download should survive Ctrl-C for resume. Only intentional deletions (hash mismatch) should remove partials.
- **Connection-level deadlines** (`internal/graphtransport` transfer profiles): both interactive and sync transfer clients use the shared transfer transport with `ResponseHeaderTimeout: 2m` (detects servers that accept but never respond) and TCP keepalives (30s idle, 10s interval, 3 probes — detects dead connections within ~60s). No `http.Client.Timeout` — transfer duration varies with file size and bandwidth. [verified]
- Transfer manager resume edge case tests cover corrupt partial file bytes, changed remote content during resume, and oversized partial state. [verified]

### Rationale: Per-Side Hashes

SharePoint enrichment silently modifies files after upload (hash/size change). Per-side hash baselines (`local_hash`, `remote_hash`) handle this natively: the planner compares new hashes against the correct side's baseline. No special code paths, no false conflicts from enrichment.
