# Drive Transfers

GOVERNS: internal/cli/get.go, internal/cli/put.go, internal/driveops/cleanup.go, internal/driveops/disk_unix.go, internal/driveops/doc.go, internal/driveops/errors.go, internal/driveops/hash.go, internal/driveops/interfaces.go, internal/driveops/local_errors.go, internal/driveops/session.go, internal/driveops/session_store.go, internal/driveops/stale_partials.go, internal/driveops/transfer_download.go, internal/driveops/transfer_manager.go, internal/driveops/transfer_session.go, internal/driveops/transfer_target.go, internal/driveops/transfer_upload.go, internal/driveops/transfer_verify.go, pkg/quickxorhash/quickxorhash.go

Implements: R-5.1 [verified], R-5.2 [verified], R-5.3 [verified], R-5.5 [verified], R-5.5.2 [verified], R-1.2 [verified], R-1.2.5 [verified], R-1.3 [verified], R-1.3.5 [verified], R-1.3.6 [verified], R-1.4.4 [verified], R-2.8.10 [verified], R-5.6 [verified], R-5.7 [verified], R-5.8 [verified], R-5.8.2 [verified], R-5.10 [verified], R-6.7.14 [verified], R-6.8.3 [verified], R-6.2.6 [verified], R-6.4.7 [verified], R-6.2.10 [verified], R-6.10.6 [verified]

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
| Uploads keep simple-upload and upload-session mechanics inside the transfer boundary, and persisted sessions carry mount scope so lifecycle owners can purge child-owned sessions and resume after restart. | `internal/driveops/upload_test.go`, `internal/graph/upload_test.go`, `internal/graph/upload_session_test.go`, `TestSessionStore_DeleteForScope_RemovesMatchingMountOrRoot`, `TestSessionStore_ReopenPreservesUploadSession` |
| Sync-owned live preconditions can be injected without making `driveops` import sync policy. | `TestDownloadToFile_TargetPreconditionBeforeRenamePreservesPartial`, `TestUploadFile_SourcePreconditionRunsBeforeUpload`, `TestUploadFile_SourcePreconditionRunsDuringSessionRead` |
| Sync execution reuses the same transfer boundary instead of inventing a second transfer path. | `internal/sync/executor_test.go`, `internal/cli/sync_helpers_test.go` |

### Download

Implements: R-6.2.3 [verified]

1. Create a namespaced `.onedrive-go.<target-name>.partial` file in the target directory
2. If that owned partial exists with content, resume via HTTP Range request
3. Stream response body, computing QuickXorHash incrementally
4. Verify hash and size against API metadata
5. Set the partial's mtime, give it its final mode and fsync it, atomically
   rename it over the target, then fsync the directory (best effort where the
   filesystem answers EINVAL, ENOTSUP or EOPNOTSUPP: FUSE, SMB)

Every step goes through a `TargetFS` (`transfer_target.go`): `LocalTarget`
for CLI `get` (absolute paths; missing parents created with 0777 minus the
umask) or sync's rooted target, which never creates a parent. The final mode
is the replaced file's permission bits, or 0666 minus the umask for a new
file; the umask is read once per `TransferManager` from `/proc/self/status`
on Linux, or by creating a probe file elsewhere, since reading it with
`umask(2)` would change it for every other goroutine (D-044, P-8). ACLs, extended attributes and Finder tags of a replaced file are not
carried over. The fsync before the rename and the directory sync after it
make the download durable before success is reported (D-042). Evidence:
`TestDownloadToFile_FsyncsBeforeRenameAndSyncsDirAfter`,
`TestDownloadToFile_PreservesExistingTargetMode`,
`TestDownloadToFile_NewFileAndParentHonourUmask`,
`TestDownloadToFile_ParentRefusalStopsBeforeAnyWrite`,
`TestDownloadToFile_DirSyncUnsupportedIsBestEffort`, `TestParseProcUmask`.

`DownloadOpts.ValidateTargetBeforeRename` is an optional sync-supplied
callback. `driveops` calls it after content/hash/mtime preparation and before
the final rename. The callback owns sync stale-work policy; `driveops` only
propagates the error and preserves the partial file like any other pre-rename
failure.

### Upload

1. Stat the local file and reject anything above the 250 GB OneDrive limit before hashing or opening network transfer state
2. Non-empty files of any size: create an upload session whose request carries `fileSystemInfo`, then upload in chunks (320 KiB aligned), so content and mtime land in one version. Sessions of files above 4 MiB are persisted for resume; smaller ones are not worth a record
3. Zero-byte files: simple PUT, then a `fileSystemInfo` PATCH. If the PATCH still fails after its bounded retry, the upload is a success with one plain warning: the content is on OneDrive, and failing the upload re-sent it (one more version per attempt) without ever setting the mtime. The file keeps OneDrive's receipt time; nothing repairs it later (a known limitation, `docs/limitations.md`)
4. Verify the stored item against what was sent (`transfer_verify.go`, R-5.5.2)

`verifyTransfer` is the one decision for "did OneDrive store what we sent".
QuickXorHash is the only comparable digest (the only one computed locally,
and it covers the length); without it the size is the only evidence. It
returns `Verified`, `EnrichedKnown` (Office documents and media on a drive
whose `parentReference.driveType` is `business` or `documentLibrary`, or
whose ID starts `b!` when Graph omits the type, and only when the stored
item is non-empty and at least as large as what was sent: enrichment adds
metadata, it never empties a file) or `Mismatch`. A mismatch is
read back by ID once, because server processing can briefly show pre-commit
facts, and then fails with `ErrUploadVerificationFailed`: it used to be an
Info line and a success, so the baseline recorded different bytes on each
side as converged, forever (D-035). An upload completion without an item
(`graph.ErrUploadCompletedWithoutItem`) is looked up by ID, or by an exact
name in the parent listing for a create, through the optional `ItemReader`
capability, and a persisted session whose upload completed that way is
deleted. Evidence: `TestTransferManager_Upload_PersonalHashMismatchFails`,
`TestTransferManager_Upload_MismatchThatReadsBackVerifiedSucceeds`,
`TestTransferManager_Upload_WarnsOnHashMismatch` (enrichment accepted),
`TestTransferManager_Upload_CompletionWithoutItemIsLookedUp`,
`TestExecuteUpload_ResponseSizeMismatch_DoesNotCommitBaseline`.

Before 2026-10-25 files up to 4 MiB were PUT and then PATCHed, which live
probe 7 showed makes two versions per upload on Personal as well as Business
for the same two requests a session takes (D-034). Evidence:
`TestUpload_SmallFileUsesSessionWithFileSystemInfo`,
`TestUpload_ZeroByteMtimePatchFailureReturnsUploadedItem`,
`TestUploadFile_ZeroByteMtimeNotAppliedStillSucceeds`.

`UploadOpts.ValidateSourceBeforeRead` is an optional sync-supplied callback.
`driveops` runs it before upload work begins and through the `io.ReaderAt`
wrapper used by simple and session uploads. `UploadOpts.ExpectedHash` plus
`HashMismatchError` let the sync executor turn a source hash mismatch into its
own stale-precondition error without teaching transfer code about sync result
classification.

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

A read-only shared folder answers a create's `createUploadSession` with the
correct `403`, and a freshly created parent can briefly answer it with `404
itemNotFound`; the graph boundary retries only that exact `404` with a bounded
budget sized for the live convergence window.

When a zero-byte simple upload succeeds and mtime preservation is required,
the immediate follow-on `UpdateFileSystemInfo` PATCH can also briefly return
`404 itemNotFound` for the returned item ID, `409 resourceModified`, or a
transient 502/503/504 service failure after the content write has already
succeeded. That retry stays
in the graph boundary and only applies to the post-simple-upload finalization
path, with a bounded window sized for the longer mount-root item-ID lag seen
in live E2E; transfer-manager and CLI callers see one success, one success
with the mtime pending, or one failure.

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

`driveops.MountSession` is the single owner of post-success path convergence
and path-authoritative delete reconciliation for one interactive mounted root.
Graph can acknowledge folder creation, upload, or move before an immediate
follow-on path lookup stops returning `itemNotFound`, and it can briefly
return `DELETE .../items/{id} = 404 itemNotFound` even though the same
remote path just resolved successfully. During repeated sibling deletes, the
exact path route itself can also lie with `GET ...root:/path: = 404
itemNotFound` even though the parent collection still lists the leaf, while
that same parent collection can lag positively after a successful delete. The
package boundary therefore exposes the `PathConvergence` capability plus the
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
- mount-root path creation still starts from the configured subtree root
  item, not the backing drive root, so recursive `mkdir` walks create
  descendants in the same subtree that later `ResolveItem()` /
  `WaitPathVisible()` calls read
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
or sleep loop. Ordinary sync execution now always probes relative to the
executor's mounted drive/root context. Direct shared-item CLI flows keep any
explicit target-scoped path work above sync via `SharedTargetClients(...)` and
`MountSession.ResolveItem(...)`.

`SessionRuntime.InteractiveSession(...)` and
`SessionRuntime.SyncSession(...)` are mount-shaped rather than resolved-drive
shaped. File-op commands translate the selected `ResolvedDrive` into explicit
`MountSessionConfig` only at the CLI boundary, while sync workers receive the
same mount-owned identity from `mountSpec`:

- token owner canonical ID
- mounted remote drive ID
- mounted remote root item ID
- account email for auth-proof attribution

That keeps auth ownership aligned with runtime mounts, including managed child
projections, instead of carrying `ResolvedDrive` through transfer/runtime
construction as a second source of truth.

The resulting `driveops.MountSession` carries mount-root path vocabulary.
Interactive path operations anchor themselves at `RemoteRootItemID` when one
is configured. Generic `driveops.Session` stays an authenticated drive-client
shape, so sync engine construction receives mount-root scope from
`EngineMountConfig` rather than session state.

## Session Lifecycle

`persistedSessionUpload` (`transfer_session.go`) owns one resumable upload
for create and overwrite alike. A record for other content is canceled on
the server before it is dropped, and so is one whose resume fails for any
reason but shutdown (D-037, R-5.6.13); records carry the session's
`ExpiresAt`. A failed final fragment (`graph.FinalFragmentError`) or a resume
whose session is gone looks the item up first and adopts it only when it is
provably the upload's own commit, so a commit that outlasted its response is
never uploaded again (D-036, R-5.6.12). A matching QuickXor proves it. A
hashless item, or a Business item whose bytes enrichment rewrote, is adopted
only on positive evidence: it carries the modification time the upload sent
(to the second) and an eTag other than the one the upload was planned
against (`UploadOpts.PlannedRemoteETag`); the same size proves nothing,
since the old version can have it. A lookup that fails is a retryable error
and the record is kept: nothing is concluded from it. A fragment 404, final
or not, on a fresh or resumed upload is an invalidation unless the item is
adopted. Names are matched case-insensitively after NFC normalization until
the WS2 namespace key replaces it; the encapsulated small-file session gets
the same adoption through `settleUploadedItem`. A session gone before its
expiry while the item holds other bytes, or a fragment answered 404, is
`ErrUploadSessionInvalidated`: another writer changed the item (live probe
L-02), and sync turns it into a stale precondition. Evidence:
`TestSessionUpload_FinalFragmentFailureAdoptsCommittedItem`,
`TestSessionUpload_GoneSessionWithLandedItemIsAdopted`,
`TestSessionUpload_GoneSessionBeforeExpiryIsInvalidation`,
`TestSessionUpload_DroppedSessionsAreCancelled`,
`TestUploadFile_SmallFileFinalFragmentFailureAdoptsCommittedItem`,
`TestUploadFromSession_OnlyFinalFragmentFailureIsUncertain`,
`TestUploadFromSession_RecoveredOffsetPastEndIsRefused`,
`TestUploadTransferError_InvalidatedSessionIsAStalePlan`.

Not yet done: `drive reset-sync-state` and managed-child cleanup delete
session records without cancelling their server sessions (they hold no
Graph client), the session's `expirationDateTime` is not watched while
fragments upload, the final fragment's header timeout does not scale with
the file size, and `item.fileSize` is not sent (unprobed on Personal).

### New files and their placeholder

A create's content step knows its item: `UploadOpts.OnCreated` runs after the
empty item exists and before any content (via `CreatingUploader`, or the
create's persisted session, whose `SessionRecord` keeps `ItemID` and
`CreatedETag`), and every failure after the create is a
`graph.CreatedItemError`, so the outcome is looked up by that ID, never by a
name a variant spelling could also match. A retry of an interrupted create
with a session record opens its fresh session on the created item instead of
creating the file again (`TestSessionUpload_RetryOfAnInterruptedCreateReusesItsItem`).
When a final fragment's lookup shows the content did not land, the empty item
is deleted at once with If-Match on its creation eTag
(`TestUploadFile_UncertainFinalFragmentThatDidNotLandRemovesThePlaceholder`).
CLI `put` onto an existing name overwrites that file by ID with If-Match on
the eTag just listed, and refuses an existing folder (R-1.3.8,
`TestPutFile_ExistingFileIsOverwrittenByIDConditionally`).

## SessionStore

File-based upload session persistence. Each session is a JSON file in the data
directory containing the upload URL, byte offset, file hash, expiry, and the
mount scope that created it. Sync engine construction tags transfer managers
with `(mount_id, local_root)` so managed shortcut child destruction can purge
only sessions owned by that child without deleting sessions for an explicit
mount that happens to target the same remote drive. Managed-state access goes
through `internal/fsroot` so directory creation, temp files, chmod, fsync, and
rename stay under one root capability. On resume, local file hash is recomputed
— if it differs from the stored hash, the session is discarded.

## Transfer Interfaces

Two required interfaces (`Downloader`, `Uploader`) and two optional interfaces (`RangeDownloader`, `SessionUploader`) type-asserted at runtime. This allows the graph client to support resume without requiring all implementations to.

Shared-item CLI flows still use the same downloader/uploader capabilities. The
only difference is how the target item identity is resolved before the transfer
starts:

- ordinary CLI commands resolve a configured drive plus a remote path
- shared CLI commands resolve a recipient account plus `(remoteDriveID, remoteItemID)`

## Hash Utilities

QuickXorHash computation for local files (`hash.go`). The `pkg/quickxorhash/` package implements the algorithm (vendored from rclone, BSD-0 license). `SelectHash` returns only an item's QuickXorHash: SHA-256 and SHA-1 can never equal a local QuickXor digest, so the old fallback made such items fail every verification and never compare equal (D-041). An item with SHA digests only is hashless, and its size is the evidence (`TestSelectHash_ReturnsOnlyQuickXor`).

## Download Verification

`downloadToVerifiedPartial` judges every attempt with `classifyDownload`:
verified (QuickXor match), unverifiable but size-consistent (no hash),
the known HEIC quirk (served smaller than the metadata, accepted on the
first attempt and logged at Debug), or a mismatch. A mismatch, an empty or
short body, or an HTML/JSON error document (`graph.ErrUnexpectedDownloadBody`)
discards the partial and downloads again. After the last attempt only a
deterministic server transform is accepted (`acceptsServerTransform`: the
same bytes at the reported size every time, with a hash to differ from);
anything else fails with `ErrDownloadIntegrity`, which sync reports as
`download_integrity`, and the existing local file is never replaced (D-038,
D-039, R-5.5.1, R-5.8.2). An accepted transform sets
`DownloadResult.AcceptedMismatch` and keeps OneDrive's reported hash as
`EffectiveRemoteHash`: recording the local hash there made every later pass
see a remote change and download the file again. Evidence:
`TestTransferManager_IntegrityFailureKeepsCanonicalFile`,
`TestTransferManager_DeterministicMismatchAtReportedSize_AcceptedOnce`,
`TestTransferManager_HEICMetadataMismatch_AcceptsFirstDownloadQuietly`,
`TestTransferManager_HEICSameSizeHashMismatch_StillRetries`,
`TestTransferManager_AcceptedTransformDoesNotWarn`,
`TestRunOnce_AcceptedServerTransformIsNotDownloadedAgain`.

A partial file's name carries the remote version (`~v` plus 12 hex digits
of the QuickXorHash, or of the eTag for a hashless item), so a resume only
ever appends to bytes of the same version; the graph boundary also checks
that the item is still that version and that the answer is a 206 whose
Content-Range starts at the partial's length (D-040, R-5.2.2). `If-Range` is
deliberately not sent: the eTag format the download CDN compares is
unprobed, and a mismatch would turn every resume into a full download; the
version check plus the Content-Range check give the same protection. A partial
with no version to bind is downloaded afresh. Evidence:
`TestTransferManager_StalePartialFromOldVersionIsNotResumed`,
`TestTransferManager_PartialWithoutVersionIsNotResumed`,
`TestDownloadRange_RejectsUnboundOrMisalignedAnswers`. `DownloadOptsForItem`
builds the options from a Graph item for CLI `get`.

Errors and warnings at default verbosity name the file the caller asked for,
never the internal `.onedrive-go.<name>.partial` temp file, and give sizes in
human units (`TestTransferManager_DownloadError_NamesTargetNotPartial`).
Housekeeping (expired or corrupt upload-session files, unreadable directories
met by the partial-file cleanup walk) logs at Debug or Info: there is nothing
for the user to do, and the walk runs after every sync pass.

## Cleanup

`cleanup.go` removes stale client-owned partial files and expired upload sessions on startup. `stale_partials.go` detects orphaned `.onedrive-go.*.partial` files from interrupted downloads. Arbitrary user-created `*.partial` files are not transfer artifacts; sync housekeeping only treats them as removable junk when `ignore_junk_files` enables junk cleanup. Session files live under the managed-state boundary (`internal/fsroot` via `SessionStore`). Orphaned owned partial files live in the sync tree and are cleaned through `internal/synctree`. Both rooted boundaries carry unexported injectable ops so cleanup paths can be covered by deterministic create/write/rename/walk/remove failure tests.

## Disk Space Pre-Check

Implements: R-6.2.6 [verified], R-6.4.7 [verified], R-2.10.43 [verified], R-2.10.44 [verified]

`TransferManager.DownloadToFile` runs a disk space pre-check before every download — both sync engine and CLI `get` benefit automatically. Configured via `WithDiskCheck(minFreeSpace, diskAvailableFunc)` functional option at construction time. Two-tier check:

- **Critical**: available space < `min_free_space` → `ErrDiskFull`. In the sync engine, this triggers a `disk:local` block scope. In CLI `get`, it simply fails the download.
- **Per-file**: available space ≥ `min_free_space` but < file_size + `min_free_space` → `ErrFileTooLargeForSpace`. Per-file skip; other smaller files can still download.

Design properties:
- **Zero/nil disables**: `WithDiskCheck(0, fn)` or omitting the option entirely skips the check (R-6.4.7).
- **Fail-open**: statfs errors are logged and the download proceeds — a transient syscall failure should not block all downloads. The first failure per `TransferManager` warns; later ones log at Debug, so a persistently failing filesystem does not warn on every download (`TestTransferManager_DiskCheckFailure_WarnsOnce`).
- **Plain errors**: `ErrDiskFull` and `ErrFileTooLargeForSpace` wrap text in human units with the action ("100.0 MB free, min_free_space requires 1.0 GB; free up space or lower min_free_space in the config").
- **Path accuracy**: checks `filepath.Dir(targetPath)` rather than the sync root, correctly handling cross-filesystem mounts.
- **Error sentinels** (`errors.go`): `ErrDiskFull` and `ErrFileTooLargeForSpace` are in the `driveops` package. The sync engine matches them via `errors.Is` in `classifyResult` and `issueTypeForHTTPStatus`.
- **Mid-write exhaustion**: the pre-check cannot see an exhausted inode table (ENOSPC with bytes free), a user quota (EDQUOT), or space consumed by other writers during the transfer. `DownloadToFile` therefore wraps any ENOSPC/EDQUOT from its own steps in `ErrDiskFull`, naming inodes when statfs reports a non-zero inode total with none free (`TestDownloadToFile_WriteOutOfSpaceReturnsErrDiskFull`, `TestWrapLocalWriteError_NamesQuotaAndLeavesOthersAlone`).
- **DiskAvailable** (`disk_unix.go`): exported function using `syscall.Statfs` — `f_bavail * f_bsize` (blocks available to unprivileged users). Build-tagged `darwin || linux`.

## Local Errno Table

`local_errors.go` owns the one errno-to-domain table for local filesystem
failures, `ClassifyLocalError`. The sync result classifier and CLI error
presentation both consume it, so a full disk or a read-only mount means the
same thing whichever call (download, mkdir, rename, conflict copy) hit it
(`TestClassifyLocalError_Table`):

| Kind | Errors | Sync meaning |
| --- | --- | --- |
| `LocalErrorNoSpace` | `ErrDiskFull`, `ENOSPC`, `EDQUOT` | one `disk:local` block scope (R-2.10.60) |
| `LocalErrorReadOnly` | `EROFS` | one `local:read-only:<path>` scope at the read-only mount (R-2.10.61) |
| `LocalErrorFileTooLarge` | `EFBIG` | per-item `local_file_too_large` (R-2.10.62) |
| `LocalErrorBusy` | `EBUSY`, `ETXTBSY`, `ESTALE` | retryable `local_busy` (R-2.10.62) |
| `LocalErrorCrossDevice` | `EXDEV` | per-item `local_cross_device` (R-2.10.62) |
| `LocalErrorNameTooLong` | `ENAMETOOLONG` | per-item `local_name_too_long` (R-2.11.8) |

`EACCES`/`EPERM` are deliberately not in the table: `os.ErrPermission` already
names them and the sync permission probe owns their boundary decision.

## Recursive Download Concurrency

`get -r` bounds concurrency at exactly one place: a shared semaphore over file
transfers, sized for the whole tree rather than per directory.

Directory descent is deliberately not concurrent. Spawning a goroutine per
subdirectory made the goroutine count scale with the number of directories in
the tree -- the unbounded fan-out the semaphore exists to prevent, on the one
path the semaphore never covered. A 200-directory tree left 202 goroutines
live.

Walking directories inline costs nothing here. The counting pass has already
fetched every listing into `childCache`, so descent performs no network I/O at
all -- only `MkdirAll` and a map lookup. Transfers still begin as soon as each
directory is reached and still overlap with one another, so the concurrency
that matters is unchanged; only the bookkeeping that was never bounded is
removed.

This also removes the last concurrent reader of `childCache`: the counting
pass fills it before any download starts, and the inline walk reads it from a
single goroutine.

## Design Constraints

- `Upload()` accepts `io.ReaderAt` (not `io.Reader`): enables retry-safe uploads without re-opening the file. `io.NewSectionReader` creates independent readers for each chunk.
- Managed session files use `internal/fsroot`.
- Sync-engine runtime cleanup of owned partial files under one configured sync root uses `internal/synctree`.
- Arbitrary local source/target paths use `internal/localpath`, making the three filesystem trust boundaries explicit instead of routing them through one helper package.
- `driveops.SessionRuntime` owns the reused Graph HTTP clients together with token-source caching. It chooses target-scoped interactive or sync HTTP profiles by composing the stateless builders in `internal/graphtransport`; callers no longer inject one cache owner into another. Interactive and sync session creation take explicit mount-owned identity instead of a generic resolved-drive wrapper.
- `driveops.Session` may install a proof hook onto its authenticated Graph clients so successful live file operations can clear stale catalog auth requirements. Pre-authenticated upload and download URLs bypass that hook and do not count as auth proof.
- Guard owned partial file cleanup with `ctx.Err() == nil`: a 3.9 GB partial of a 4 GB download should survive Ctrl-C for resume. Only intentional deletions (hash mismatch) should remove partials.
- **Connection-level deadlines** (`internal/graphtransport` transfer profiles): both interactive and sync transfer clients use the shared transfer transport with `ResponseHeaderTimeout: 2m` (detects servers that accept but never respond), TCP keepalives (30s idle, 10s interval, 3 probes — detects dead connections within ~60s) and HTTP/2 pings. No `http.Client.Timeout` — transfer duration varies with file size and bandwidth; instead the graph boundary abandons a body that moves no bytes for 2 minutes (`graph.ErrTransferStalled`, R-6.8.20), and a stalled download keeps its partial so the retry resumes (`TestDownloadToFile_StalledTransferKeepsPartialForResume`). [verified]
- Transfer manager resume edge case tests cover corrupt partial file bytes, changed remote content during resume, and oversized partial state. [verified]

### Rationale: Per-Side Hashes

SharePoint enrichment silently modifies files after upload (hash/size change). Per-side hash baselines (`local_hash`, `remote_hash`) handle this natively once `verifyTransfer` has classified the difference as enrichment: the planner compares new hashes against the correct side's baseline, with no false conflicts. A difference outside that class is not recorded at all.
