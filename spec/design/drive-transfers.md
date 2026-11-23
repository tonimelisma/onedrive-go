# Drive Transfers

GOVERNS: internal/cli/folder_transfer.go, internal/cli/get.go, internal/cli/put.go, internal/driveops/cleanup.go, internal/driveops/disk_unix.go, internal/driveops/doc.go, internal/driveops/errors.go, internal/driveops/hash.go, internal/driveops/interfaces.go, internal/driveops/local_errors.go, internal/driveops/session.go, internal/driveops/session_store.go, internal/driveops/stale_partials.go, internal/driveops/transfer_download.go, internal/driveops/transfer_manager.go, internal/driveops/transfer_session.go, internal/driveops/transfer_target.go, internal/driveops/transfer_upload.go, internal/driveops/transfer_verify.go, internal/driveops/tree.go, internal/driveops/tree_download.go, internal/driveops/tree_upload.go, pkg/quickxorhash/quickxorhash.go

Implements: R-5.1 [verified], R-5.2 [verified], R-5.3 [verified], R-5.5 [verified], R-5.5.2 [verified], R-1.2 [verified], R-1.2.5 [verified], R-1.2.7 [verified], R-1.3 [verified], R-1.3.5 [verified], R-1.3.6 [verified], R-1.4.4 [verified], R-2.8.10 [verified], R-5.6 [verified], R-5.7 [verified], R-5.8 [verified], R-5.8.2 [verified], R-5.10 [verified], R-6.7.14 [verified], R-6.8.3 [verified], R-6.2.6 [verified], R-6.4.7 [verified], R-6.2.10 [verified], R-6.10.6 [verified], R-6.7.38 [verified]

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
| Downloads use partial-file resume plus hash verification before the final atomic rename. | `internal/driveops/transfer_manager_test.go` (`TestTransferManager_ResumeDownload_CorruptPartial_RetriesFresh`, `TestTransferManager_StalePartialFromOldVersionIsNotResumed`), `internal/graph/download_test.go`, `internal/driveops/hash_test.go`, `internal/localpath/localpath_test.go` (`TestAtomicWrite`) |
| Uploads keep simple-upload and upload-session mechanics inside the transfer boundary, and persisted sessions carry mount scope so lifecycle owners can purge child-owned sessions and resume after restart. | `internal/driveops/transfer_manager_test.go` (`TestTransferManager_Upload_Success`), `internal/driveops/transfer_create_test.go` (`TestUploadFile_RecordsTheCreatedItemBeforeAnyContent`), `internal/graph/upload_test.go`, `internal/graph/item_create_test.go` (`TestCreateUploadSessionForItem_PreconditionFailureIsNeverRetriedUnconditionally`), `TestSessionStore_DeleteForScope_RemovesMatchingMountOrRoot`, `TestSessionStore_ReopenPreservesUploadSession` |
| The iOS HEIC quirk is accepted only as a smaller HEIF-signed body; an error page, an empty or truncated body, or a larger or unsigned body is an integrity failure that never reaches the target, and an accepted server transform warns on a personal drive only. | `TestTransferManager_HEICMetadataMismatch_AcceptsFirstDownloadQuietly`, `TestTransferManager_HEICQuirkRequiresASmallerHEIFBody`, `TestTransferManager_HEICSameSizeHashMismatch_StillRetries`, `TestTransferManager_AcceptedTransformWarnsOnlyOnPersonalDrives` |
| Sync-owned live preconditions can be injected without making `driveops` import sync policy. | `TestDownloadToFile_TargetPreconditionBeforeRenamePreservesPartial`, `TestUploadFile_SourcePreconditionRunsBeforeUpload`, `TestUploadFile_SourcePreconditionRunsDuringSessionRead` |
| Sync execution reuses the same transfer boundary instead of inventing a second transfer path. | `internal/sync/internal/syncexec/executor_test.go` (`TestExecutor_Download_Success`, `TestExecutor_Download_PartialFileCleanedOnMidStreamError`) |
| Content hashing honors cancellation between 1 MiB chunks, so a canceled upload stops at the pre-upload hash and stopping the sync worker pool does not wait on a whole-file read (R-6.3.6). | `TestComputeQuickXorHashFromReaderContext_StopsAtCancel`, `TestComputeQuickXorHashContext_CanceledContextDoesNotHashTheFile`, `TestComputeQuickXorHashContext_ChunkedReadMatchesSinglePass`, `TestTransferManager_UploadFile_CanceledContextStopsAtTheHash`, `TestWorkerPool_StopReturnsWhileWorkerHashesBeforeUpload` |

### Download

Implements: R-6.2.3 [verified]

1. Create a namespaced `.onedrive-go.<target-name>.partial` file in the target directory
2. If that owned partial exists with content, resume via HTTP Range request
3. Stream response body, computing QuickXorHash incrementally
4. Verify hash and size against API metadata
5. Set the partial's mtime, give it its final mode and fsync it, atomically
   rename it over the target, then fsync the directory (best effort where the
   filesystem answers EINVAL, ENOTSUP or EOPNOTSUPP: FUSE, SMB). The mtime is
   `DownloadOpts.RemoteMtime`, a `time.Time` the caller decodes (sync's
   stored value through `RemoteModTime`, sync-store.md "Stored times"); its
   zero value sets none, and driveops decodes no stored integer
   (`TestDownloadToFile_SetsTheGivenModificationTimeAndLeavesAZeroOneAlone`)

Every step goes through a `TargetFS` (`transfer_target.go`): `LocalTarget`
for CLI `get` (absolute paths; a missing parent is a folder in the user's
tree, created with `localpath.UserTreeDirPerm`, 0777 minus the umask) or
sync's rooted target, which never creates a parent. The final mode
is the replaced file's permission bits, or 0666 minus the umask for a new
file; the umask is read once per `TransferManager` from `/proc/self/status`
on Linux, or by creating a probe file elsewhere, since reading it with
`umask(2)` would change it for every other goroutine (D-044, R-5.10.2). ACLs, extended attributes and Finder tags of a replaced file are not
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

### Folder downloads and uploads (`get`, `put`)

A folder `get` or `put` is one call of a tree engine in `driveops`:
`DownloadTree` (`tree_download.go`) or `UploadTree` (`tree_upload.go`), on
the pool in `tree.go` that "Recursive Transfer Concurrency" below describes.
The engine reports every outcome as a `TreeEvent` (a folder created, a file
done, a file or folder failed) and returns a `TreeSummary` of them;
`internal/cli` renders the events (`folder_transfer.go`) and keeps target
resolution, the transfer manager it builds, and put's overwrite rule.

- **Download.** A drive path (`get.go`) and a shared folder (`get_shared.go`)
  download the same way: every folder, the root included, is listed by item
  ID through a `ChildLister` (`*graph.Client`), and every file goes to a
  `FileDownloader` (the `TransferManager`, built by `newDownloadManager`,
  the one place every `get` reads `min_free_space`, reporting a value it
  cannot read instead of turning the disk-space check off). The walk reads
  a folder a page at a time (`ListChildrenPages`) and creates it once the
  first page is in: a folder whose first page cannot be read is reported and
  not created, in one line that names the remote folder by its path below
  `TreeDownload.RemoteRoot` (the drive path or shared selector the user
  gave), the local folder it would have filled, and the cause. A page that
  fails after the first is the same failure of that folder: what the pages
  before it held downloads, and the walk goes on, as after an upload's
  failed read. Every folder it creates is a folder in the user's tree: it
  asks for `localpath.UserTreeDirPerm`, 0777 minus the umask, like the files
  in it (R-5.10.2). A folder that is already there is used as it is, keeps
  its mode and is not counted in `folders_created` (R-1.2.4).
- **Refused names.** A remote child whose name is empty, `.` or `..`, holds
  `/` or `\`, or fails `filepath.IsLocal` is refused before any local path is
  built from it: a failed event whose error is a `RefusedNameError` naming
  the reason, reported in the folder it would have gone into, nothing
  written, a refused folder not listed, its siblings downloaded (R-1.2.7).
  OneDrive refuses `/` and `\` in names and Linux cannot hold `.` or `..`, so
  no name OneDrive serves and Linux can hold is refused (`RefuseRemoteName`).
  A `get` that takes a remote name as a local one refuses it the same way
  before anything is written: a folder `get` given no local path, whose
  local folder would take the name OneDrive reports (a shared folder, a
  drive's root, which for a mounted folder is the name its owner gave it),
  asks for a local folder (`localFolderTarget`); a single-file `get` given
  no local path or a local folder to download into (R-1.2.6) asks for a
  local file (`localFileTarget`). A local file the user names is the user's
  choice and is not checked. For a drive path the file's name is OneDrive's
  spelling of the path's last element: the path lookup
  (`graph.GetItemByPath`), and the walk from a mount root, refuse any other
  name. A shared file's name is its owner's choice.
- **Upload.** `put` resolves the remote parent and hands `UploadTree` its ID
  and the root's name. `UploadTree` opens the local root first and only then
  makes sure of the remote root through a `FolderEnsurer` (`*Session`), the
  run's first event, so a local folder that cannot be opened, missing or not
  a folder, creates nothing on OneDrive. The walk reads each local folder a
  batch of `uploadDirBatch` entries at a time through `localpath.OpenDir`,
  so a folder of 100,000 entries is never one slice, and opens each
  subfolder before it makes sure of it on OneDrive and reads it: a folder
  that cannot be opened is reported and not created there, and a remote
  folder exists before anything is uploaded into it. A read that fails
  partway through a folder is one failure of that folder; what was read
  before it uploads, and the walk goes on. A symbolic link the user names
  as the folder to upload is followed. Inside the tree the walk does not
  follow one and queues it as a file: a link to a regular file uploads that
  file under the link's name, and a link to a folder is one failed file
  (`not a regular file`). After a 409, `EnsureFolder` takes
  the folder OneDrive holds under a name it treats as the same
  (`namekey.Equal`), as `putFile` does for a file, and says so when a file
  holds the name. Both find that item through `FindExistingChild`
  (`session.go`): one read of the name under its parent
  (`graph.GetChildByName`, `items/{parent}:/{name}:`), whose answer counts
  only when it sits in that parent under an equal name. It lists the
  parent only when the read cannot settle the name: a name the path
  route cannot carry (escaped longer than `maxPathAddressedLength`, or
  holding `:`), a read answered 400 (`graph.ErrBadRequest`; measured for a
  child named `$value`, see `graph-api-quirks.md`), or a read that finds
  nothing equal, which the listing can still hold while the path route lags
  behind it. The listing is read a page at a time and stops at the page that
  holds the name as spelled; a page holding only another spelling OneDrive
  treats as the same does not stop it, as a later page could hold the exact
  one, which is then the item (`graph.NameSeek`,
  `TestFindExistingChild_TheListingPrefersTheNameAsSpelled`). Failing that
  spelling, the item is the one child of a name OneDrive treats as the same:
  two such children are not found and none is taken, and a child the pages
  repeat counts once (`namekey.Find`, R-2.12.3,
  `TestFindExistingChild_TwoFoldedCandidatesIsNotFound`,
  `TestLookupUploadTarget_PrefersExactSpelling`); `put` then answers the 409
  instead of overwriting one of them
  (`TestPutFile_PrefersExactSpellingOverCaseVariant`). Any other failed
  read is the error. If the listing finds nothing either, the 409 stands for
  a file; a folder reports that it was not found in its parent. An item of
  the name that carries no ID, read or listed, is an error
  (`TestEnsureFolder_AFolderWithoutAnIDIsAnError`). The same lookup finds
  the item of an upload whose answer did not carry it, where a folder of the
  name is not the upload's file
  (`TestLookupUploadTarget_AFolderOfTheNameIsNotTheUpload`), and each name of a
  path below a mounted folder (`MountSession.resolveItemFromMountRoot`), so
  neither lists a folder whose name one read answers
  (`TestLookupUploadTarget_ReadsTheNameAndListsOnlyWhenTheReadCannotSettleIt`,
  `TestMountSession_ResolveItem_MountRootReadsEachNameOnce`). A path below
  a drive's root is resolved by one rule (`MountSession.resolveDrivePath`),
  which `ResolveItem` and so every command, the delete target and
  `WaitPathVisible` use, and which `ListChildrenPages` falls back to for a
  folder the listing by path cannot reach: a path the route can resolve is
  read by path, one request, and a genuine miss is not found; a path holding
  a name the route is known not to resolve (`graph.NameIsPathAddressable`: a
  colon, a name starting with `v1.0`, `$value`, measured in
  `graph-api-quirks.md`) has the folder above the first such name resolved
  by the same rule and the rest walked by name from it (`walkByName`, shared
  with the walk below a mounted folder), so that name is found in its
  folder's listing; a path read answered 400 for a name no rule knows is
  walked by name from the drive root (R-6.7.38,
  `TestMountSession_ANameThePathRouteDoesNotResolveIsFoundThroughItsParent`,
  `TestMountSession_AGenuineMissCostsOneRequest`). Every path a
  session is given is first cleaned by `CleanRemotePath`, which drops the
  slashes at its ends and the empty names between doubled slashes (OneDrive
  holds no empty name), so `a//b` is `a/b` below a mounted folder as on a
  drive, and `mkdir` and `put` print it so
  (`TestMountSession_APathWithEmptyNamesIsThePathWithoutThem`,
  `TestFileCommands_OnASharedFolderAPathWithEmptyNamesIsThePathWithoutThem`).
  The fallback of a delete or a visibility wait to the parent's listing
  stops at the page that holds the name, and takes no item that the
  listing's own answers place in another folder than the one at the parent's
  path (`TestMountSession_ParentListingStopsAtThePageThatHoldsTheName`,
  `TestMountSession_AFallbackTakesNoItemFromAListingThatNamesAnotherFolder`).
  Where the answers for a path disagree (a read and the read of its folder,
  or a listing and the read of its folder, do not agree on the folder at the
  path: `graph.ErrPathAnswersDisagree`, `graph-client.md` rule (2)), the read
  or the listing ends with that error, which is not "not found". A delete's
  target is then refused at once, and no folder is listed because of it: a
  delete, `rm -r` included, deletes nothing
  (`TestMountSession_ADeleteTargetIsNotTakenWhereAnswersNameDifferentFolders`,
  `TestMountSession_DeleteResolvedPathStopsWhereAnswersNameDifferentFolders`,
  `TestRunRm_DeletesNothingWhereAnswersNameDifferentFolders`). A visibility
  wait reads the path again on it, as on "not found", because such answers can
  settle once OneDrive converges after a change, but it lists no folder
  because of it; answers that still disagree when the wait runs out end it as
  a path not visible yet (`PathNotVisibleError`), with the disagreement as
  its cause. So `put` uploads nothing into a parent whose
  answers disagree, and `rm` whose delete landed gives its note for a parent
  not visible yet instead of failing
  (`TestMountSession_AVisibilityWaitReadsAgainWhileAnswersNameDifferentFolders`,
  `TestRunPut_UploadsNothingWhereAnswersNameDifferentFolders`,
  `TestRunRm_NotesAParentWhoseAnswersStillDisagreeAfterTheDelete`). On a drive
  whose answers carry no parent path, answers that agree with one another on
  a folder of the right name in another subtree two or more levels up are not
  caught: the read, and the fallback after it, take that folder's item
  (`graph-client.md`, rule (2)). A second
  upload of a tree of n files in f folders therefore costs n + f one-item
  reads and no listing, apart from such names. Every
  file goes through the caller's `PutFile`, which is `putFile`: a file
  already there is overwritten by ID, conditionally, as a single `put`
  overwrites it (R-1.3.8).
- **Failures and the returned error.** The failure of one file or folder is
  an event, counted, and the walk goes on with its siblings; the event names
  the path and its error does not (Where a Local Path Enters an Error). The
  returned error is a `TreeRootError` when the root's
  first page cannot be read (download) or the root cannot be opened or made
  on OneDrive (upload), before anything is reported, which the command
  returns as one error that names the remote path (`listing "<remote>": …`
  for `get`, `uploading "<remote>": …` for `put`); the end of the context,
  once the run reported every file it queued and did not finish as failed
  with the context's error and every folder whose walk it cut short (one
  event for what the walk had not reached in it); or a panic of the walk or
  of an event's report, which fails the command. A panic in one file's
  transfer is that file's failure (R-6.3.10). Nothing is downloaded or
  created when the root cannot be listed, which holds for the root's first
  page; a later page of the root that fails is the root folder's failure
  like any other folder's, after the pages before it were walked, and `get`
  exits 1 whatever the page's error, a sign-in failure included, unless the
  context has ended.

Evidence: `TestDownloadTree_RefusesUnsafeRemoteNames`,
`TestDownloadTree_ListFailureIsReportedAndSiblingsContinue`,
`TestDownloadTree_TransferFailureIsReportedAndOthersContinue`,
`TestDownloadTree_UnlistableRootIsTheReturnedError`,
`TestDownloadTree_AFailedLaterPageFailsOnlyItsFolder`,
`TestDownloadTree_FolderExistsBeforeItsFilesDownload`,
`TestDownloadTree_CreatedFolderFollowsTheUmask`,
`TestDownloadTree_CountsOnlyTheFoldersItCreates`,
`TestDownloadTree_FileWhereAFolderBelongsIsReported`,
`TestDownloadTree_APanickingTransferFailsOnlyThatFile`,
`TestDownloadTree_APanickingReportFailsTheRun`,
`TestDownloadTree_APanickingWalkFailsTheRun`,
`TestUploadTree_CreatesParentBeforeChildren`,
`TestUploadTree_UploadsAFolderLargerThanOneBatch`,
`TestUploadTree_ReadDirFailureReported`,
`TestUploadTree_ReadFailurePartwayThroughAFolderIsReported`,
`TestUploadTree_FolderThatCannotBeCreatedIsReported`,
`TestUploadTree_TransferFailureIsReportedAndOthersContinue`,
`TestUploadTree_UnopenableRootCreatesNothingOnOneDrive`,
`TestUploadTree_RootThatCannotBeCreatedIsTheReturnedError`,
`TestEnsureFolder_Conflict`, `TestEnsureFolder_ConflictFindsTheFolderInAnotherCase`,
`TestEnsureFolder_ConflictListsOnceWhenTheReadCannotFindTheName`,
`TestEnsureFolder_ConflictNotFound`, `TestEnsureFolder_AReadThatFailsIsTheError`,
`TestEnsureFolder_AFileHoldingTheNameIsSaidSo`,
`TestRunPut_FolderUploadedAgainListsNoFolder`,
`TestOpenDir_ReadsInBatches`, `TestOpenDir_RefusesAFIFOWithoutBlocking`,
`TestRunGet_FolderReportsEveryFileAsTextAndJSON`,
`TestRunGet_FolderRefusesAnUnsafeRemoteName`,
`TestRunGet_DriveRootWithNoLocalPathTakesTheRootsName`,
`TestRunGet_SharedFolderWithAnUnsafeNameIsRefused`,
`TestRunGet_SharedFileWithAnUnsafeNameIsRefused`,
`TestRunGet_FileReportedUnderAnUnsafeNameIsNotWritten`,
`TestLocalFileTarget_RefusesAnUnsafeRemoteName`,
`TestRunPut_FolderReportsEveryFileAsTextAndJSON`,
`TestRunPut_FolderThatCannotBeOpenedCreatesNothing`,
`TestRunPut_FolderWithAFailingFileUploadsTheOthers`,
`TestRunPut_SymbolicLinksInAFolder`,
`TestRunGet_SharedFolderDownloadsItsTree`, `TestConfiguredMinFreeSpace`.

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
(`graph.ErrUploadCompletedWithoutItem`) is looked up by ID, or for a create
by its name in the parent (`FindExistingChild`: one read of the name, the
parent's listing only when the read cannot settle it; a folder holding the
name is not the file), through the optional `ItemReader`
capability, and a persisted session whose upload completed that way is
deleted. A lookup that fails leaves the outcome unknown
(`uploadOutcomeUnknownError`, the lookup's failure in its chain), and after a
create the failure still names the created item (`graph.CreatedItemError`):
returned plain, a 404 from looking up the created item reached sync as the
create's own 404, read as the parent folder gone, and the folder and its
subtree were forgotten from remote truth
(`TestSettleUploadedItem_LookupFailureKeepsTheCreateMark`; in sync,
`TestRunOnce_LookupFailureAfterACreateKeepsItsParentInRemoteTruth`).
Evidence: `TestTransferManager_Upload_PersonalHashMismatchFails`,
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
for create and overwrite alike.

An interrupted upload keeps its record so that its next attempt resumes.
When sync gives the upload up instead (a conflict takes the path, the file
moves or is deleted), the record has nothing left to resume, so sync ends it
in the step in which it ends the upload's retry work: the prune of retry rows
no current action names returns the keys it deleted
(`SyncStore.PruneRetryWorkToCurrentActions`), and after that transaction, never
under it, the engine hands the paths of the pruned uploads to
`TransferManager.DropUploadSessions` (through `ExecutorConfig`, which tries
the stored path, its spelling on disk and its parent's spelling). That reads
every record of the path under either key an upload holds, the create key
(the absolute path) and the overwrite key (`item::<id>::<path>`), and drops
each with `dropSession`: the record goes first, then the server session is
canceled best-effort on a detached, bounded context, so a failed cancel still
drops the record (`TestTransferManager_DropUploadSessions_DropsBothKeysAndCancelsEachOnce`,
`TestTransferManager_DropUploadSessions_AFailedCancelStillDropsTheRecord`,
`TestRunOnce_ConflictOverAnInterruptedLargeUploadDropsItsSession`). An upload
still in the plan keeps its row and its record
(`TestChaosGraph_FragmentFaultLeavesAPartUploadedSessionToResume`), and a
plan that deferred uploads by its mode (download-only) drops nothing, since
the next run in both directions still wants them. A replan runs only while no
action runs, so no record of an upload in flight is dropped. Before
2026-11-23 such a record stayed until `CleanStale` removed it after 7 days,
and its server session was never canceled. A record for other content is canceled on
the server before it is dropped, and so is one whose resume fails for any
reason but shutdown (D-037, R-5.6.13); records carry the session's
`ExpiresAt`. A failed final fragment (`graph.FinalFragmentError`), a resume
whose session is gone, or a resume whose session status lists no range left
to upload (`graph.ErrUploadSessionComplete`: every byte arrived and the item
is committed or committing) looks the item up first and adopts it only when
it is provably the upload's own commit, so a commit that outlasted its
response is never uploaded again (D-036, R-5.6.12). The empty status used to
parse as an error and drop the session, so a multi-GB file whose final
fragment outlasted its response and was looked up before the server had
committed was uploaded a second time. A matching QuickXor proves it. A
hashless item, or a Business item whose bytes enrichment rewrote, is adopted
only on positive evidence: it carries the modification time the upload sent
(to the second) and an eTag other than the one the upload was planned
against (`UploadOpts.PlannedRemoteETag`); the same size proves nothing,
since the old version can have it. A lookup that fails is a retryable error
and the record is kept: nothing is concluded from it. The error says the
outcome is unknown and carries only the lookup's failure
(`uploadOutcomeUnknownError`); the answer it followed is named in its text,
not in its chain. Joined to a final fragment's 409 or 412, the lookup's
outage or throttle was hidden behind the answer's status, sync read the
upload as a stale plan, and the upload was looked up a second time
(`TestUploadFile_FailedLookupAfterAnUncertainFinalFragmentIsAnUnknownOutcome`,
`TestSessionUpload_FailedLookupAfterAnUncertainFinalFragmentIsAnUnknownOutcome`,
`TestSessionUpload_FailedLookupAfterAnUncertainResumeIsAnUnknownOutcome`).
A failure after a create made its item still names that item. A fragment 404, final
or not, on a fresh or resumed upload is an invalidation unless the item is
adopted. The item is found by name as every lookup among siblings finds it
(`lookupUploadTarget`, `namekey.Find`, R-2.12.3); the encapsulated small-file session gets
the same adoption through `settleUploadedItem`. A session gone before its
expiry while the item holds other bytes, or a fragment answered 404, is
`ErrUploadSessionInvalidated`: another writer changed the item (live probe
L-02), and sync turns it into a stale precondition. Evidence:
`TestSessionUpload_FinalFragmentFailureAdoptsCommittedItem`,
`TestSessionUpload_GoneSessionWithLandedItemIsAdopted`,
`TestSessionUpload_CompletedSessionWithLandedItemIsAdopted`,
`TestSessionUpload_CompletedSessionWithoutLandedItemStartsFresh`,
`TestResumeUpload_StatusWithNoRangesLeftIsComplete`,
`TestSessionUpload_GoneSessionBeforeExpiryIsInvalidation`,
`TestSessionUpload_DroppedSessionsAreCancelled`,
`TestUploadFile_SmallFileFinalFragmentFailureAdoptsCommittedItem`,
`TestUploadFromSession_OnlyFinalFragmentFailureIsUncertain`,
`TestUploadFromSession_RecoveredOffsetPastEndIsRefused`,
`TestUploadTransferError_InvalidatedSessionIsAStalePlan`.

A session record that cannot be loaded does not stop the upload: it starts
over from a fresh session. The store reads only the format `Save` writes
(R-5.2.3): one JSON object of known fields at `currentSessionVersion`. A file
that is not JSON, is cut short or empty, carries a field the record does not
have or data after it, or holds another version (a record an older binary
wrote included) is invalid: `SessionStore.Load` removes it and warns once,
naming the file, the upload that starts over and the reason and never the
upload URL, and returns `ErrInvalidSessionRecord`, which the caller logs at
Debug only. Nothing is read from such a file, its URL included, so its server
session is not cancelled; Graph expires it (R-5.6.13). `DeleteForScope`
removes such a file too, with one warning naming the file and the reason,
instead of failing the scope cleanup over it; one it cannot remove is
returned with its other failures for its caller to report once. It reads only
record files, so the temporary file of a `Save` in progress
(`sessionTempPattern`) is left alone
(`TestSessionStore_Load_RemovesARecordInAnyOtherFormat`,
`TestSessionStore_Load_ReportsAnInvalidRecordItCannotRemove`,
`TestSessionStore_DeleteForScope_RemovesAnUnreadableRecord`,
`TestSessionStore_DeleteForScope_ReturnsAnInvalidRecordItCannotRemove`,
`TestSessionStore_DeleteForScope_LeavesASaveInProgressAlone`,
`TestTransferManager_SessionUpload_RecordInAnOlderFormatStartsOver`,
`TestSessionStore_SaveAndLoadRoundTripTheCurrentRecord`). A stat or read
failure comes back unreported and `loadUsableSessionRecord` warns once per
upload attempt, naming the file, since for a multi-GB file "did not resume" is
otherwise invisible
(`TestTransferManager_SessionUpload_UnreadableRecordWarnsAndStartsOver`).

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
After a crash the retry is no longer a create: the next pass adopts the
placeholder as a known item (R-5.6.15) and plans an overwrite by ID, whose
session key names the item (`item::<id>::<path>`) while the create recorded
its session under the local path alone. `loadCreateRecordForOverwrite` looks
under the create's key when the overwrite's own is empty and takes the record
only when its `ItemID` is the overwrite's target and its hash is the content
in hand; it re-keys the record under the overwrite's key, without the
creation eTag, so a failed overwrite leaves the item rather than removing it
as its own placeholder (R-5.2.4,
`TestUploadFileToItem_ResumesTheSessionACrashedCreateLeft`,
`TestUploadFileToItem_LeavesAnotherItemsCreateRecordAlone`,
`TestUploadFileToItem_DropsACreateRecordForOtherContent`). The soak of
2026-11-02 found this: a 2 GiB create killed at 1.2 GiB was sent again from
byte 0 after the restart, with the create's record left beside the new one.
The transfer manager never deletes a placeholder itself. It hands the item's
ID to `PlaceholderRemover.RemoveEmptyItem`, the Graph client's bounded
read-back and conditional delete ([graph-client.md](graph-client.md),
R-5.6.14), on a context detached from the upload's and bounded by
`placeholderRemovalTimeout` (10 s); a dropped session is canceled under
`graph.SessionCancelTimeout`, the one bound for that request. A failure, or an item left in place, is logged at
Debug and the upload's own error is what the caller sees.
There are two such places. When a final fragment's lookup shows the content
did not land and the created item still empty, the item is removed at once;
the creation eTag no longer matches by then, because opening and canceling
the session moved it, which is why the removal reads the eTag it deletes by
(`TestUploadFile_UncertainFinalFragmentThatDidNotLandRemovesThePlaceholder`,
`TestUploadFile_UncertainFinalFragmentRemovesThePlaceholderByItsCurrentETag`).
When recording the item a persisted session created fails (`OnCreated`
returns an error), the session is canceled and the item removed the same way,
before any content is sent
(`TestSessionUpload_FailedCreatedHookRemovesTheEmptyItem`).
In both places the failure is the one `graph.CreatedItemError` naming the
item, never one wrapped inside another, and its `Removed` says whether the
removal worked, as Graph's own removal does, so sync ends its record of an
item that can no longer receive the content (R-5.6.15,
`TestUploadFile_RemovedPlaceholderIsReportedRemoved`).
A large create makes its item and opens its session in one Graph call
(`CreateUploadSession`). When the session request fails after the item
exists, Graph names the item in a `graph.CreatedItemError` and has already
tried to remove it; `recordItemOfFailedSession` still runs `OnCreated` for
that item, once, because the removal is best effort and an empty file nobody
recorded reads on the next pass as someone else's (R-5.6.15). Before, the
hook ran only once a session was open, so the caller cleared its intent and a
failed removal left a stranger's empty file (live, e2e
`TestE2E_SyncWatch_PauseResume`). An item the error reports removed is not
recorded: it is gone, and a record would name an item no upload can use
(`TestRecordItemOfFailedSession_SkipsARemovedItem`). The item is not adopted
for a retry by ID, since it may be gone; a hook failure there is Debug, and
the session's error is what the caller sees
(`TestSessionUpload_FailedSessionRecordsTheItemTheCreateMade`).
The hook cannot run for a create whose answer never arrived: there is no item
to name. The Graph client marks that failure `graph.ErrCreateOutcomeUnknown`
([graph-client.md](graph-client.md)), and the transfer manager's wrapping
keeps the mark (`%w`), so the caller can tell "nothing was created" from "not
known" and keep its intent for the second (R-5.6.15,
[sync-execution.md](sync-execution.md),
`TestTransferManager_Upload_CreateOutcomeUnknownKeepsItsMark`). A create that
succeeded without naming its item, and that its folder does not list yet, is
that failure and not `graph.ErrUploadCompletedWithoutItem`: no content was
sent, and looking the name up to check it against the content failed the
empty file as a bad upload and dropped the mark. A zero-byte create never runs
the hook either: the created item is the whole file.
CLI `put` onto an existing name overwrites that file by ID with If-Match on
the eTag just read, the file found by one read of its name
(`FindExistingChild`, under "Folder downloads and uploads"), and refuses an
existing folder (R-1.3.8,
`TestPutFile_ExistingFileIsOverwrittenByIDConditionally`,
`TestPutFile_ExistingFolderIsAnError`).

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

Two required interfaces (`Downloader`, `Uploader`) and optional ones type-asserted at runtime (`RangeDownloader` on the downloader; `ItemUploader`, `SessionUploader`, `ItemSessionUploader`, `CreatingUploader`, `PlaceholderRemover`, `ItemReader` and `SessionCanceler` on the uploader). This allows the graph client to support resume without requiring all implementations to.

The tree engines take their collaborators as narrow interfaces instead: `ChildLister` (a folder's children by item ID; `*graph.Client`), `FileDownloader` (one file to a local path; `*TransferManager`) and `FolderEnsurer` (a remote folder made sure of; `*Session`), plus the caller's `PutFile` function. Each is what the engine calls and nothing more, which is also what lets the engine's tests run a tree of 10,000 files without writing one.

Shared-item CLI flows still use the same downloader/uploader capabilities. The
only difference is how the target item identity is resolved before the transfer
starts:

- ordinary CLI commands resolve a configured drive plus a remote path
- shared CLI commands resolve a recipient account plus `(remoteDriveID, remoteItemID)`

## Hash Utilities

QuickXorHash computation for local files (`hash.go`). The `pkg/quickxorhash/` package implements the algorithm (vendored from rclone, BSD-0 license).

Hashing is cancellable. `ComputeQuickXorHashContext` and
`ComputeQuickXorHashFromReaderContext` read in `hashReadChunkSize` (1 MiB)
chunks and check the context before each one, so a canceled upload or a
shutdown stops a hash within one chunk instead of at the end of the file
(R-6.3.6). `TransferManager.hashFunc`, the pre-upload hash every upload runs
on a sync worker, takes the upload's context; `WithContentHasher` replaces it,
which the sync engine's tests use to prove that stopping the worker pool does
not wait on a whole-file read. The sync package's local observer and scanner
use the Context form too, and so do syncverify's hasher seam and the
executor's checks of the descriptor a destructive action verifies, which hash
under the action's context. Evidence:
`TestComputeQuickXorHashFromReaderContext_StopsAtCancel`,
`TestComputeQuickXorHashContext_CanceledContextDoesNotHashTheFile`,
`TestComputeQuickXorHashContext_ChunkedReadMatchesSinglePass`,
`TestTransferManager_UploadFile_CanceledContextStopsAtTheHash`,
`TestWorkerPool_StopReturnsWhileWorkerHashesBeforeUpload`,
`TestExecutor_LocalMove_DestinationHashStopsWhenTheContextEnds`. `SelectHash` returns only an item's QuickXorHash: SHA-256 and SHA-1 can never equal a local QuickXor digest, so the old fallback made such items fail every verification and never compare equal (D-041). An item with SHA digests only is hashless, and its size is the evidence (`TestSelectHash_ReturnsOnlyQuickXor`).

## Download Verification

`downloadToVerifiedPartial` judges every attempt with `classifyDownload`:
verified (QuickXor match), unverifiable but size-consistent (no hash),
the known HEIC quirk (accepted on the first attempt and logged at Debug), or
a mismatch. A mismatch, an empty or short body, or an HTML/JSON error
document (`graph.ErrUnexpectedDownloadBody`) discards the partial and
downloads again. After the last attempt only a deterministic server
transform is accepted (`acceptsServerTransform`: the same bytes at the
reported size every time, with a hash to differ from); anything else fails
with `ErrDownloadIntegrity`, which sync reports as `download_integrity`, and
the existing local file is never replaced (D-038, D-039, R-5.5.1, R-5.8.2).
An accepted transform sets `DownloadResult.AcceptedMismatch` and keeps
OneDrive's reported hash as `EffectiveRemoteHash`: recording the local hash
there made every later pass see a remote change and download the file again.
It is reported once per file, at INFO on a Business or SharePoint drive
(`isBusinessLikeDriveID`: those rewrite uploads routinely) and at WARN with
the path on a personal drive, where nothing legitimately rewrites bytes.

The HEIC quirk (R-5.8.1, R-5.8.2) is a smaller post-processed HEIF file, and
only that shape is accepted: a `.heic` whose served size is non-zero and
below the reported size (`isHEICServedSmaller`) **and** whose partial starts
with an ISO BMFF `ftyp` box (`hasISOBMFFSignature`, read through the target
filesystem before the verdict); an error document never reaches the
verdict. The signature matters because an HTML or JSON error page served
with HTTP 200, or an empty or truncated body, is smaller too: accepting any
size difference put the page on disk as the photo and recorded it in the
baseline as synced (local hash of the page, remote hash the real one), never
to be fetched again. Anything else about a `.heic` is an ordinary mismatch
and goes through the retries and `ErrDownloadIntegrity`. The graph
boundary's HTML/JSON gate (`checkDownloadBodyType`) still applies to hashless
items only; for a hashed item the hash, and for the quirk the signature,
decide. Evidence:
`TestTransferManager_IntegrityFailureKeepsCanonicalFile`,
`TestTransferManager_DeterministicMismatchAtReportedSize_AcceptedOnce`,
`TestTransferManager_HEICMetadataMismatch_AcceptsFirstDownloadQuietly`,
`TestTransferManager_HEICQuirkRequiresASmallerHEIFBody`,
`TestTransferManager_HEICSameSizeHashMismatch_StillRetries`,
`TestTransferManager_AcceptedTransformWarnsOnlyOnPersonalDrives`,
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

A download's errors name no local path of their own, so neither the target,
which the caller names, nor the internal `.onedrive-go.<name>.partial` temp
file; only a filesystem call on the partial that failed names it, once,
through `localpath` (Where a Local Path Enters an Error). The plain sentence
of a single `get` names the partial's folder in its place where the sentence
is about the folder (permission denied for a partial that does not exist, a
read-only filesystem, a file too large), names the partial itself when it
exists and denies permission, and gives none for a partial that is missing,
whose folder is there, so the error itself is printed. Sizes are given in
human units (`TestTransferManager_DownloadError_NamesNoLocalPath`,
`TestTransferManager_DownloadIntoAReadOnlyFolderNamesThePartialOnce`,
`TestRunGet_AnUnwritableFileIsNamedOnce`,
`TestDownloadOneFile_AnExistingPartialItCannotWriteIsNamed`,
`TestCommandErrorText_APartialFileIsNamedByItsFolderOnlyWhereThatIsTrue`).
Housekeeping (expired or corrupt upload-session files, unreadable directories
met by the partial-file cleanup walk) logs at Debug or Info: there is nothing
for the user to do, and the walk runs at the end of every sync run that did
work.

## Cleanup

`cleanup.go` removes stale client-owned partial files and expired upload sessions when a one-shot sync that did work ends and when a watch's first sync settles. `stale_partials.go` detects orphaned `.onedrive-go.*.partial` files from interrupted downloads. Arbitrary user-created `*.partial` files are not transfer artifacts; sync housekeeping only treats them as removable junk when `ignore_junk_files` enables junk cleanup. Session files live under the managed-state boundary (`internal/fsroot` via `SessionStore`). `SessionStore.CleanStale` goes on past every session file it cannot check or delete and returns them all, joined, with the count it did delete, as `DeleteForScope` does: a file whose age it cannot read is one of them, and so is a file it cannot delete (`TestSessionStore_CleanStale_ReportsASessionItCannotRemove`, `TestSessionStore_RemoveStaleEntries_ReportsAnEntryItCannotStat`). Orphaned owned partial files live in the sync tree and are cleaned through `internal/synctree`; `CleanStalePartials` answers the same way, every partial it could not remove joined beside the count it did remove, and a partial already gone when the cleanup reaches it is neither (`TestCleanStalePartials_ReturnsEveryPartialItCannotRemove`). `CleanTransferArtifacts` runs both and returns each one's count and failure in a `TransferArtifactCleanup`; it logs no failure itself, because only its caller knows whether a failure is new (`TestCleanTransferArtifacts_ReturnsEachCleanupsFailureAndLogsNone`). The engine reports each cleanup's failures once per failure streak (R-6.6.30, [sync-engine.md](sync-engine.md) → Post-Sync Housekeeping), and an engine that closes reports its session cleanup once, at Warn with the count removed (`TestEngineClose_ReportsStaleSessionCleanupWithTheCount`). Both rooted boundaries carry unexported injectable ops so cleanup paths can be covered by deterministic create/write/rename/walk/remove failure tests.

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
| `LocalErrorBusy` | `EBUSY`, `ETXTBSY`, `ESTALE`, `ETIMEDOUT`, `EAGAIN` | retryable `local_busy` (R-2.10.62) |
| `LocalErrorCrossDevice` | `EXDEV` | per-item `local_cross_device` (R-2.10.62) |
| `LocalErrorNameTooLong` | `ENAMETOOLONG` | per-item `local_name_too_long` (R-2.11.8) |

`EACCES`/`EPERM` are deliberately not in the table: `os.ErrPermission` already
names them and the sync permission probe owns their boundary decision.

`ETIMEDOUT` and `EAGAIN` are local only when a local file operation returned
them: a read on a network mount or a FUSE folder that timed out, or was told
to try again (a regular file on a local disk ignores the `O_NONBLOCK` that
`localpath.OpenRegular` opens with, but a network or FUSE filesystem and a
lock another process holds can still return `EAGAIN`). Both clear on their own
like `ESTALE` from the same mount, so they are retried with the retry work's
backoff rather than at once. A network timeout never reaches this table: the
sync classifier reads a request timeout first, and a chain that holds an
`*fs.PathError` is never one (`syncdomain.IsRequestTimeout`), so a timed-out
local read is per-file retry work and never outage evidence
(`TestClassifyResult_LocalErrnoTable`).

## Where a Local Path Enters an Error

What a user sees names a local path once. The call that touched the
filesystem names it: every failure of `internal/localpath` reads
`<verb> <path as given>: <reason>` (`opening Backup/a.txt: permission
denied`), and the os error beneath it, whose `*fs.PathError` or
`*os.LinkError` holds the absolute path the call reached, is kept whole for
`errors.Is` and `errors.As` but not printed again, so `ClassifyLocalError`,
`localpath.IsMissingName` and the CLI's sentence for a missing or unreadable
file answer as they would on the os error itself
(`TestFailures_NameEachPathOnce`,
`TestFailures_NameARelativePathAsItWasGiven`,
`TestFailures_AnswerAsTheOSErrorTheyHold`,
`TestClassifyLocalError_ReadsALocalpathFailureAsItsOSError`). No wrap in
`driveops` names a local path: it says what the transfer was doing
(`hashing`, `creating the partial file`, `moving the download into place`,
`sending the file`), so a single file's error names the path once, through
the call that failed (`TestComputeQuickXorHashContext_AnUnreadableFileIsNamedOnce`,
`TestTransferManager_UploadOfAnUnreadableFileNamesItOnce`,
`TestTransferManager_DownloadIntoAReadOnlyFolderNamesThePartialOnce`,
`TestDownloadToFile_RenameFailure_PreservesPartial`), and a failure no local
call caused, a Graph answer or an integrity mismatch, names none
(`TestTransferManager_DownloadError_NamesNoLocalPath`,
`TestTransferManager_Upload_ErrorWrapping`). An os error that names another
path than the one the call was given, a parent `MkdirAll` could not create,
is printed whole (`TestFailures_KeepAnOSErrorThatNamesAnotherPath`). The
command names the file it was working on, so every failure names it once: a
single `put` as `uploading "<remote>": …` and a single `get` as
`downloading "<remote>": …`, by the drive path or the shared selector the
user typed, whatever the cause (`TestDownloadOneFile_AFailureThatIsNotLocalNamesTheRemotePathOnce`,
`TestRunGet_ASharedFileThatFailsNamesTheSelectorOnce`). A selector has no
parent folder, so a single `get` of a shared file that OneDrive does not find
suggests no folder to list (`TestRunGet_ASharedFileNotFoundSuggestsNoFolderToList`).
A tree event names its file or
folder by `LocalPath`, and its error names no local path of its own. A line
that starts with the path prints the cause through `localpath.WithoutPath`,
which leaves out every local path the cause's failures and os errors name and
keeps their verbs and reasons (`TestWithoutPath_LeavesOutEveryLocalPath`):
a folder `get` or `put` prints `Failed: <LocalPath>: <cause>`, the path as
the walk found it below the folder the user typed, and its `--json` `errors`
hold the same string (`TestRunPut_FolderNamesAnUnreadableFileOnce`,
`TestRunPut_FolderWithAFailingFileUploadsTheOthers`,
`TestRunGet_FolderNamesAnUnwritableFileOnce`). A single `put` or `get` prints
the error itself, whose `--verbose` details name the path once
(`TestRunPut_AnUnreadableFileIsNamedOnce`,
`TestRunGet_AnUnwritableFileIsNamedOnce`), and sync's activity line names the
path below the sync folder and then only the innermost reason
(`TestSyncConsole_ALocalFileFailureNamesTheFileOnce`). A log line may carry a
`path` attribute beside the error.

## Recursive Transfer Concurrency

A recursive `get` or `put` is bounded in one place, the pool of `tree.go` in
`driveops`, whatever the size of the tree, in both directions:

- **One walker.** The caller's goroutine walks the tree depth first, a folder
  at a time, and is the only goroutine that lists or reads folders or creates
  them. It creates a folder (locally for a download, on OneDrive for an
  upload) before it queues a file into it.
- **A fixed pool.** `TreeWorkers` (4, the number R-1.2.1 and R-1.3.1 state)
  workers take files from a channel of capacity `TreeWorkers`. A walk that
  finds every worker busy and the queue full waits on that channel: at most
  `2×TreeWorkers` files are in flight or
  queued, transfers start while the tree is still being walked, and no file
  waits on a goroutine of its own. `put` runs as parallel as `get`.
- **No whole-tree state.** Nothing counts or lists the tree first, nothing
  caches its listings, and no map of folder IDs exists: a folder's ID
  travels down the walk's own stack. The walk holds one page of a listing
  per level of depth for a download, at most 200 children: the Graph client
  hands a folder on a page at a time (`ListChildrenPages`) and asks for the
  next page only once the walk, a subfolder's included, is done with the one
  before, so no folder is ever one slice. An upload holds one open folder
  and one batch of entries per level.
- **One collector.** A single goroutine counts every event and hands it to
  the caller's `OnEvent`, in the order the events happen, so the CLI's
  rendering needs no lock. A panic in `OnEvent` stops the delivery, not the
  counting or the draining, and fails the run.
- **Owned goroutines.** The engine starts the workers and the collector,
  each behind a `panicguard` boundary, and has waited for every one of them
  when it returns. On cancellation the walk stops at its next step, every
  queued file a worker had not started is failed with the context's error,
  and every folder on the walk's stack is reported as cut short.

Progress is therefore a running count, not a total: a total needs the whole
tree listed before the first byte moves. Evidence:
`TestDownloadTree_InFlightNeverExceedsWorkers` (10,000 files in one folder,
generated in memory, peak transfers equal `TreeWorkers`),
`TestDownloadTree_WalkBlocksWhileWorkersAreBusy`,
`TestDownloadTree_WalksAPageBeforeAskingForTheNext`,
`TestDownloadTree_GoroutinesDoNotScaleWithFilesOrFolders`,
`TestDownloadTree_CancellationReportsEveryUnstartedFile`,
`TestDownloadTree_CancellationAsksForNoFurtherPage`,
`TestDownloadTree_CancellationAtALaterPageCutsItsFolderShort`,
`TestListChildrenPages_HandsEachPageOnBeforeAskingForTheNext`,
`TestUploadTree_InFlightNeverExceedsWorkers`,
`TestUploadTree_CancellationReportsEveryUnstartedFile`,
`TestTreeWorkers_FourFilesTransferAtOnce`,
`TestRunPut_CanceledFolderReportsThenEndsWithTheCancellation`,
`TestFolderTransferReport_ACanceledRunReportsThenEndsWithTheCancellation`.

## Design Constraints

- `Upload()` accepts `io.ReaderAt` (not `io.Reader`): enables retry-safe uploads without re-opening the file. `io.NewSectionReader` creates independent readers for each chunk.
- Managed session files use `internal/fsroot`.
- Sync-engine runtime cleanup of owned partial files under one configured sync root uses `internal/synctree`.
- Arbitrary local source/target paths use `internal/localpath`, making the three filesystem trust boundaries explicit instead of routing them through one helper package.
- `driveops.SessionRuntime` owns the reused Graph HTTP clients together with token-source caching. It chooses target-scoped interactive or sync HTTP profiles by composing the stateless builders in `internal/graphtransport`; callers no longer inject one cache owner into another. Interactive and sync session creation take explicit mount-owned identity instead of a generic resolved-drive wrapper. `BootstrapClient(tokenPath)` serves account-level calls from the same token-source cache, so a drive session and the identity probe before it share one source per sign-in file; the default token sources refresh through the runtime's `graphtransport.AuthClient` (R-6.8.27). No session constructor (`InteractiveSession`, `SyncSession`, `SharedTargetClients`, `BootstrapClient`) and no `TokenSourceFn` takes a context: a token source keeps none and refreshes under the context of each request that needs a token. A sign-in file is loaded once (`getOrCreateTokenSource`): the first caller reads it outside the runtime's lock, so a slow read holds up no other sign-in and none of the other caches, callers for the same file meanwhile wait for that load, and a failed load is not kept, so the next caller reads the file again (`TestSessionRuntime_ATokenLoadDoesNotHoldTheRuntime`, `TestSessionRuntime_ConcurrentCallersOfOneSignInLoadItOnce`, `TestSessionRuntime_BootstrapClientKeepsNotLoggedIn`). A load that panics is a failed load too: the callers waiting for it get an error, and the panic goes on to the caller that ran it (`TestSessionRuntime_ATokenLoadThatPanicsHoldsNoLaterCaller`, `TestSessionRuntime_ATokenLoadThatPanicsReleasesItsWaiters`). Each sign-in file also has its own bootstrap metadata client (`BootstrapMeta(tokenPath)`, used by `BootstrapClient` and by login's account discovery for the pending sign-in file), so the throttle deadline a 429 sets belongs to the sign-in whose request was throttled: it holds for that sign-in's next requests and delays no other account's (`TestSessionRuntime_OneSignInsThrottleDoesNotReachAnother`, `TestSessionRuntime_BootstrapClientsOfOneSignInShareItsThrottle`, `TestStatus_OneAccountsThrottleLeavesTheOtherAlone`). `FlushTokenCache` clears both.
- `driveops.Session` may install a proof hook onto its authenticated Graph clients so successful live file operations can clear stale catalog auth requirements. Pre-authenticated upload and download URLs bypass that hook and do not count as auth proof.
- Guard owned partial file cleanup with `ctx.Err() == nil`: a 3.9 GB partial of a 4 GB download should survive Ctrl-C for resume. Only intentional deletions (hash mismatch) should remove partials.
- **Connection-level deadlines** (`internal/graphtransport` transfer profiles): both interactive and sync transfer clients use the shared transfer transport with `ResponseHeaderTimeout: 2m` (detects servers that accept but never respond), TCP keepalives (30s idle, 10s interval, 3 probes — detects dead connections within ~60s) and HTTP/2 pings. No `http.Client.Timeout` — transfer duration varies with file size and bandwidth; instead the graph boundary abandons a body that moves no bytes for 2 minutes (`graph.ErrTransferStalled`, R-6.8.20), and a stalled download keeps its partial so the retry resumes (`TestDownloadToFile_StalledTransferKeepsPartialForResume`). [verified]
- Transfer manager resume edge case tests cover corrupt partial file bytes, changed remote content during resume, and oversized partial state. [verified]

### Rationale: Per-Side Hashes

SharePoint enrichment silently modifies files after upload (hash/size change). Per-side hash baselines (`local_hash`, `remote_hash`) handle this natively once `verifyTransfer` has classified the difference as enrichment: the planner compares new hashes against the correct side's baseline, with no false conflicts. A difference outside that class is not recorded at all.
