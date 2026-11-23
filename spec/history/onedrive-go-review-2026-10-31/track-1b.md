# Track 1B — Transfer integrity and content equality

Repo `~/src/onedrive-go` at `26b3bc8b`. Read-only review; no tests run (verification already using both cores). Methods: full reads of the driveops transfer files, graph download/upload/session files, executor transfer/precondition files, hash reuse, store/fsroot durability code, sqlite comparison CTEs, plus targeted greps. Files are quoted with line numbers from this commit.

Headline for the go/no-go: the two stale `docs/safety.md` claims about downloads are **fixed in code** (fsync + dir sync before success; a persistent hash mismatch now fails as `download_integrity`, with one documented exception). The invariants mostly hold as written. The one concrete integrity hole I would fix before pointing this at a photo library is **1B-1**: any `.heic` with a hash is accepted on the first attempt whenever its size differs from the metadata, with no hash and no error-document check, so an HTML/JSON error page or a truncated body served with HTTP 200 becomes the photo on disk and the baseline records it as synced. Everything else is caveats and performance.

---

## 1. Per invariant / question

### S3 / R-6.2.3, R-5.10.1 — atomic downloads, fsync, directory sync

Enforcing code (all `internal/driveops/transfer_download.go`):
- Partial name is client-namespaced and version-tagged: `DownloadPartialPath` L484–486, `downloadPartialName` L525–540 (`.onedrive-go.<name>~v<12 hex of sha256(hash|etag)>.partial`), perms 0600 L37, L567/L607.
- Verify before publish: `downloadToVerifiedPartial` L379–457 → `classifyDownload` L143–156.
- Publish: `publishPartial` L286–333: Chtimes L291 → `finalizePartial` L347–371 (`f.Chmod` L358, **`f.Sync()` L362**) → caller precondition L303 → `target.Rename` L310 → **`target.SyncDir(parentDir)` L318**, with EINVAL/ENOTSUP/EOPNOTSUPP tolerated as best-effort L319–329 (`dirSyncUnsupported` L337–339).
- Sync's TargetFS routes every step through the rooted tree: `internal/sync/executor_transfer.go` L288–341 (`syncDownloadTarget`; `SyncDir` → `synctree.Root.SyncDir`, `internal/synctree/synctree.go` L1073–1091 opens the dir and `dir.Sync()`).

Call path: `executor.ExecuteDownload` (executor_transfer.go L28–110) → `TransferManager.DownloadToFile` L208 → `downloadToFile` L219 → `checkDiskSpace` L243 → `PrepareParent` L247 → `downloadToVerifiedPartial` L252 → `publishPartial` L266 → worker commits outcome on a detached context (`internal/sync/worker.go` L211–225, `commitOutcome` L234–239).

Tests: `TestDownloadToFile_FsyncsBeforeRenameAndSyncsDirAfter` (transfer_manager_test.go L2082) asserts the exact step order `open-partial, chtimes, finalize(chmod,fsync), rename, syncdir:folder` through a recording TargetFS — it does exercise the ordering. `TestDownloadToFile_DirSyncUnsupportedIsBestEffort` L2628 covers the EINVAL branch. `TestDownloadToFile_RenameFailure_PreservesPartial` L1520, `TestDownloadToFile_TargetPreconditionBeforeRenamePreservesPartial` L1559.

Stale-doc claims (`docs/safety.md` L167, last touched 2026-10-25; transfer_download.go last touched 2026-10-30):
- "a just-downloaded file is not flushed to disk before it is renamed": **FIXED-IN-CODE** — `finalizePartial` L362 fsyncs the partial after chmod and before rename; `publishPartial` L318 fsyncs the directory after the rename. Spec R-5.10.1 `[verified]` is accurate.
- "If a download keeps failing its hash check, the last attempt is accepted after a few retries": **FIXED-IN-CODE, WITH ONE EXCEPTION STILL TRUE**. After `maxRetries+1` attempts a mismatch now returns `ErrDownloadIntegrity` (L456) and removes the partial (L453) *unless* `acceptsServerTransform` (L170–183) holds: every attempt produced the **same hash and same size**, none was an error document, and the size equals the reported size. In that case the download is accepted with `AcceptedMismatch=true` (L440–451, Info log). So "accepted after a few retries" is still literally true for a deterministic same-length mismatch (P-10, R-5.8.2). And for `.heic` the acceptance happens on the **first** attempt with no retry at all (L149, see 1B-1). The doc should be rewritten rather than deleted.
- "files OneDrive reports without any hash cannot be checked": CONFIRMED-STILL-TRUE (size only, `downloadUnverifiable` L151/L410).

Verdict: **HOLDS** for atomicity and durability. **HOLDS WITH CAVEAT** for "hash verify": the P-10 acceptance and the HEIC first-attempt acceptance are deliberate holes in the hash gate (findings 1B-1, 1B-2).

### S9 / R-6.2.11 — never conclude "same content" without both hashes

Every "same content" decision I could find:

1. **First sync / baseline update / conflict detection** — one SQL definition, `internal/sync/sqlite_compare.go` `current_equal` L390–409: requires `l.item_type = r.item_type`, then `l.size = 0 AND r.size = 0` → equal (D-059b zero-byte exception), then `COALESCE(l.hash,'')='' OR COALESCE(r.hash,'')='' → 0`, then hash and size must both match. Consumers: `create_equal` L525 (no baseline), `equal_again` L528 (remote changed, hashes equal → baseline update instead of download), `baseline_update` L593 and L645 (`diverged` with both changed and `current_equal=1`). Nothing else in planner*.go computes equality (grep found no `CurrentEqual` use outside sqlite_compare.go; it is consumed via `comparison_kind`). Tests `TestPlanCurrentState_HashlessSameSizeIsNotTreatedAsEqual`, `..._HashOnOneSideOnlyIsNotTreatedAsEqual`, `..._HashlessZeroByteBothSidesIsEqual`, `..._HashlessNonEmptyCreateIsStillAConflict` (comparison_hashless_test.go L57–132) exercise exactly the hashless/same-size failure. **HOLDS.**
2. **Change detection vs. baseline** (`local_changed` L285–302, `remote_changed` L308–319, `core_types.go` `remoteContentDiffersFromBaseline` L437–446 / `localFileDiffersFromBaseline` L451–466): these decide "changed", not "equal", and a hash known on one side only falls back to size/mtime/eTag (R-6.7.17). They never produce a converged baseline on their own. **HOLDS.**
3. **Move pairing** (`planner_move_composition.go` L1117–1120): pairs only when both `view.Local.Hash` and `entry.LocalHash` are non-empty and equal. **HOLDS.**
4. **Hash reuse** (`local_hash_reuse.go` L34–54): reuses the baseline's *local* hash only when size, full-precision mtime, ctime, (device, inode, birth time) all match and the file is ≥1 s outside the racily-clean window; a baseline without ctime/identity is re-read. This is a local-to-local identity, not a cross-side equality. `TestCanReuseBaselineHash_*` (4 tests) cover sub-second mtime, ctime/identity and metadata mismatch. **HOLDS.**
5. **Download completion** (`downloadOutcome`, executor_transfer.go L128–167): LocalHash = bytes hashed while writing; RemoteHash = `EffectiveRemoteHash` = OneDrive's reported hash (or "" when hashless). An unverifiable (hashless) download is recorded with `RemoteHash=""`, so future comparisons stay in the R-6.7.17 fallback rather than claiming hash agreement. **HOLDS.**
6. **Upload completion** (`transfer_verify.go` `verifyTransfer` L71–85): `item.QuickXorHash == "" && item.Size == sent.size → transferVerified` (L75). This *is* a "same content" conclusion from size alone, and `uploadOutcome` (executor_transfer.go L386–425) then commits a converged baseline with `RemoteHash=""`. It is explicitly permitted by R-5.5.2 ("otherwise by size") but it is the one place the letter of S9 is weaker than stated. The create path is conditional on the created item's eTag and the overwrite on If-Match, so the bytes at that item are the bytes this client just sent; the residual risk is a Graph response that drops the hash while the content actually stored differs (not observed). Same for `adoptLandedUpload` L313–339: hashless/enriched adoption needs sent-mtime-to-the-second + eTag ≠ planned (L344–355). **HOLDS WITH CAVEAT** (1B-5).
7. **Executor preconditions** (`remoteLiveContentMatches`, executor_preconditions.go L215–233): hashless fallback to size+mtime decides "still what the plan saw", not equality. Fine.

R-6.7.17 — what can happen when the remote has no hash: the item is downloaded (if remote-only), verified by size only, recorded with `RemoteHash=""`; if both sides have it with no baseline and it is non-empty, it is a **conflict** (local renamed to a conflict copy, remote downloaded, conflict copy uploaded) — duplicates, not loss. An hashless remote item that later changes with same size, same mtime and same eTag is invisible — that requires an eTag that did not move, which OneDrive does not do for content changes.

### Upload integrity

- **Hash compared after upload:** yes. `UploadFile` hashes the file first (`transfer_upload.go` L259), builds `sent{size, quickXorHash, mtime, planETag}` L282–284, and `settleUploadedItem` (transfer_verify.go L164–228) compares the server item's `QuickXorHash` to it; a mismatch is read back once by ID (L213) and then fails with `ErrUploadVerificationFailed` (L226). Sync records `upload_verification_failed` and commits no baseline (`engine_result_classify.go` L267; `TestExecuteUpload_ResponseSizeMismatch_DoesNotCommitBaseline`, executor_test.go L1351). Caveat: the hash compared is computed by a **separate read pass before** the upload, not from the bytes actually streamed; the two are tied together only by the mid-upload stat checks below plus the server's hash. Acceptable: a change that evaded the stat checks still fails the server hash compare on Personal.
- **Enrichment class** (L77–84, `isEnrichedExtension` L102–115): on a `business`/`documentLibrary` drive (or an ID starting `b!`), any hash with `item.Size >= sent.size` is accepted for Office docs **and all common photo/video extensions** (jpg, png, heic, mp4, mov …). On a Business drive, photo and video uploads are effectively never hash-verified (1B-6). Personal is unaffected.
- **File changes during upload:** before: `validateUploadSourcePrecondition` (executor_preconditions.go L351–395) checks regular file, type, size, full-precision mtime, ctime (`plannedCtimeChanged` L567–574) and (dev, inode, birth) identity against the planned local state; `validateUploadExpectedHash` (transfer_upload.go L132–138) requires the fresh hash to equal the planned hash. During: `validatingReaderAt.ReadAt` (L63–76) runs the same stat check **before every ReadAt** (`TestUploadFile_SourcePreconditionRunsDuringSessionRead`). After: no explicit re-stat, but the server hash is compared with the pre-upload hash. Net: a mid-upload edit is caught by ctime/mtime/size/identity or by the server hash. **HOLDS** (cost: one `Lstat` per 32 KiB transport read on a 12.9 GB file; perf only).
- **Remote edit during a long upload:** `ExecuteUpload` does a live `GetItem` preflight and passes its eTag as `opts.IfMatch` (executor_transfer.go L180, L212–219); `CreateUploadSessionForItem` sends `If-Match` (`upload_session.go` L47 `ifMatchHeaders`), zero-byte PUT sends it too (`upload_transfer.go` L187–189). A 412 → `graph.ErrPreconditionFailed` → `uploadTransferError` (L364–376) → `errActionPreconditionChanged` → engine replans from fresh truth (conflict → both kept). A change after the session is open: Graph answers the next fragment 404 (L-02) → `ErrUploadSessionInvalidated` (transfer_session.go L227–231, L263–266) → same stale path (`TestUploadTransferError_InvalidatedSessionIsAStalePlan`, `TestSessionUpload_ResumeFragment404IsInvalidation`, `TestUploadFileToItem_PassesTheCallersIfMatchToTheSession`). A 412 is never retried unconditionally. **HOLDS** on Personal (live probe L-02); documented as unprobed on Business. Note: the `createUploadSession` for a **create** is conditional on the created empty item's eTag (`item_create.go` L172/L194), so a concurrent remote create of the same name is a 409 handled by `resolveCreateConflict` L516–550.

### Upload sessions for the 4.6–12.9 GB files

- Chunk: fixed 10 MiB, 320 KiB aligned (`graph/upload.go` L10–13), loop `uploadChunksFrom` (`upload_transfer.go` L358–420); each chunk is a fresh `SectionReader` per attempt; 416 → `recoverUploadChunkOffset` L422–463 (queries `nextExpectedRanges`, refuses non-advancing or past-end offsets).
- Persisted (`persistedSessionMinSize` 4 MiB, transfer_upload.go L56): `SessionRecord` (`session_store.go` L43–62) = session URL, **file QuickXorHash**, size, created, `ExpiresAt`, mount scope, and for creates `ItemID`+`CreatedETag`; keyed by sha256(driveID, localPath) (overwrites: `item::<id>::<path>`, transfer_session.go L70–72) under `<dataDir>/upload-sessions`; written with `fsroot.AtomicWrite` (temp, chmod, **fsync, close, rename, dir fsync** — `fsroot.go` L432–475, L500–518). Save failure only warns (L196) — resume is lost, correctness is not.
- Resume: `loadUsableSessionRecord` L103–126 recomputes the local hash (every `UploadFile` call hashes first) and **drops + cancels** a record whose `FileHash` differs (L114–116) → local change between session creation and resume is handled. `ResumeUpload` (`upload_session.go` L181–213) queries status; 404 → `ErrUploadSessionExpired` → `adoptLandedUpload` lookup; if the item already holds the bytes it is adopted (D-036); otherwise, if the record's `ExpiresAt` is still in the future, it is treated as invalidated by a concurrent writer (L296–298), else a fresh session (L300–302). Local record TTL 7 days (`StaleSessionAge`).
- Gaps acknowledged in the spec (drive-transfers.md L255–259): session `expirationDateTime` is not watched during fragments; the final fragment's 2-minute header timeout does not scale with size. See 1B-7 for the resume race this leaves for very large files.
- Verdict: **HOLDS WITH CAVEAT** (1B-7).

### Download resume

`downloadToPartial` L560–587: resume only on attempt 0, only when the downloader is a `RangeDownloader`, and only when the item has a hash or eTag to bind to; opens the partial O_APPEND and stats it (TOCTOU-safe). `graph.DownloadRange` (`download.go` L122–184) re-fetches metadata, refuses if `version.matches` fails (L144–146, L166–168: hash, else eTag), sends `Range: bytes=N-` and accepts only `206` with `Content-Range: bytes N-(size-1)/size` (`validateContentRange` L285–305; also applied on the `/content` redirect route L455). After the append the **entire partial is re-hashed from byte 0** (`hashTargetFile`, transfer_download.go L698; transfer_target.go L194–207), so a locally corrupted prefix (e.g. zero-filled tail after a power cut, since in-flight partial bytes are never fsynced) is caught by the hash and the partial is discarded and re-fetched (`TestTransferManager_ResumeDownload_CorruptPartial_RetriesFresh`, `..._RemoteChangedDuringResume_RetriesFresh`, `..._OversizedPartial_RetriesFresh`, `TestDownloadRange_RejectsUnboundOrMisalignedAnswers`). Hashless items resume bound to eTag and are verified by size only; `If-Range` deliberately not sent. **HOLDS.**

### S6 / R-6.2.6 — free space

`checkDiskSpace` (transfer_download.go L732–778) runs at the top of every `downloadToFile` (L243) before parent prep or any write; path = `filepath.Dir(target.Abs(targetPath))` (L738–742), i.e. statfs of the **target's parent**, so a mount inside the sync root is measured correctly; `DiskAvailable` = `f_bavail*f_bsize` (`disk_unix.go` L24–30). Thresholds: `available < minFreeSpace → ErrDiskFull` (L763–767, scope-wide), `available < fileSize+minFreeSpace → ErrFileTooLargeForSpace` (L770–775, per file). Fails open on statfs error (warn once). Engine: `ErrDiskFull`/ENOSPC/EDQUOT → `LocalErrorNoSpace` → `issueDiskFull` (`engine_result_classify.go` L273–275) → `ScopeDiskLocal` block scope (`scope_semantics.go` L79–84), one Warn line (`engine_runtime_lifecycle.go` L316–322); released by the generic trial mechanism (`releaseScope` L335–349) when a trial action succeeds. Mid-write ENOSPC is wrapped into the same sentinel (`DownloadToFile` L213 `wrapLocalWriteError`). Config: `min_free_space` default 1GB (`config/defaults.go` L13), wired via `WithDiskCheck` (`engine.go` L173). Tests: `TestTransferManager_DiskSpaceCheck` L1836, `TestTransferManager_DiskCheckFailure_WarnsOnce`, `TestDownloadToFile_WriteOutOfSpaceReturnsErrDiskFull`. Caveat: 8 concurrent workers each pass the per-file check independently against the same free space (1B-8). **HOLDS WITH CAVEAT.**

### S7 / R-6.2.7 — partials never upload; cleanup scope

Observation excludes any path with a component matching `IsOwnedTransferArtifactName` (`content_filter.go` L137–143), so partials are never in `local_state` and never planned for upload; delete safety ignores them too (`planner_delete_safety.go` L277). The namespaced prefix+suffix match (transfer_download.go L546–548) does not match user `*.partial` files. Cleanup (`engine_housekeeping.go` L14–31) passes `SkipDirs = IgnoredDirs + protected roots` (L60–69), `IncludedDirs = contentFilter.IncludedDirs`, and deletes arbitrary `*.partial` only with `ignore_junk_files` (`stale_partials.go` L71); `shouldSkipPartialCleanupDir` L140–167 prunes skipped/out-of-scope directories. **HOLDS.** (Caveat: `IgnoredPaths` glob patterns are not passed to cleanup — only `IgnoredDirs` — so an owned partial under a glob-ignored path would be removed; it is this client's own artifact, so no user data is at stake.)

### Hash algorithms

`SelectHash` returns only `QuickXorHash` (`hash.go` L25–31); SHA-1/SHA-256 are parsed (`graph/items.go` L215–219) but never compared (D-041), so an algorithm mismatch cannot occur: a SHA-only item is hashless. Personal and Business both report quickXorHash. `pkg/quickxorhash` is rclone's implementation; `vectors_test.go` pins the Microsoft known vectors (`""`→`AAAA…=`, `"hello"`, `"hello world"`) and checks the optimized digest against an independent bit-by-bit reference at 0/20/21/159/160/161/64K±1/1M+7 bytes, whole and in odd-sized pieces. I re-derived `"hello"` = `aCgDG9jwBgAAAAAABQAAAAAAAAA=` by hand from the algorithm (bytes at bit offsets 0,11,22,33,44 → `68 28 03 1b d8 f0 06`, length 5 XORed into the last 8 bytes → `05` at byte 12): matches. **HOLDS.** Residual: a bug affecting only lengths the vectors do not cover would present as a *deterministic* mismatch and be silently accepted by P-10 (see 1B-2) rather than detected.

### Modification time

- Upload: `fileSystemInfo.lastModifiedDateTime = mtime.UTC().Format(time.RFC3339)` (`graph/upload.go` L45) — **whole seconds, UTC**; pre-1900 clamped to 1900-01-01 with one Warn (`sendableMtime` L50–64, L-26). Zero-byte: PATCH after PUT, bounded retry; failure → `MtimeNotAppliedError` accepted as success (`upload_transfer.go` L289–314; transfer_upload.go L194–210).
- Download: `Chtimes(partial, remoteMtime)` **before** fsync (transfer_download.go L289–297, best-effort Warn); the baseline's `LocalMtime` is what `Stat` reports afterwards, not what was asked for (executor_transfer.go L156–162, D-046).
- Per-side baseline (`LocalMtime` ns from the local stat, `RemoteMtime` whole seconds from Graph; `uploadOutcome` L412–413). Change detection compares local-to-local (`local_changed` on `b.local_mtime` vs `l.mtime`) and remote-to-remote (`b.remote_mtime` vs `r.mtime`), never across sides, and `metadata_drift` (L435–458) refreshes both without a transfer. So the second-truncation cannot cause a re-upload loop or a spurious conflict. **HOLDS.**
- Pre-1970: `parseTimestamp` rejects `year < 1970` as unknown (`graph/items.go` L22, L344), so an item whose mtime is 1900–1969 has `RemoteMtime=0`; a download then leaves the local mtime at download time (`RemoteMtime 0 = don't set`), and `committedByThisUpload` cannot adopt such an item (needs non-zero `ModifiedAt`). Low (1B-10).

### Zero-byte and hashless items

Zero-byte download: Business may omit `downloadUrl`; `freshDownload` treats `ErrNoDownloadURL` with `RemoteSize==0` as an empty file (L616–621; `TestTransferManager_EmptyItemWithoutDownloadURLCreatesEmptyFile`); a known-empty hashed item never accepts a non-empty "deterministic" body (`TestTransferManager_EmptyHashedItemNeverAcceptsIdenticalWrongBodies`, `TestExecutor_Download_KnownEmptyItemRejectsNonEmptyBody`). Two empty sides compare equal without hashes (sqlite_compare.go L397). Hashless non-empty: size-only download verification plus `checkDownloadBodyType` (download.go L313–332) refusing HTML/JSON whose length is not the item's size; both-present-no-baseline is a conflict. **HOLDS.**

### R-6.2.10 — timeouts and stalls

No whole-transfer deadline by design. Transports: `ResponseHeaderTimeout` 2 min, TCP keepalive, HTTP/2 pings (`graphtransport/profiles.go` L14–15, L118–132). Body stalls: `stallWatchdog` (`graph/transfer_watchdog.go`) cancels after 2 min without progress on request or response bodies (disarmed during retry waits), returning `ErrTransferStalled`; downloads keep the partial for resume (transfer_download.go L628–632, L676–684; `TestDownloadToFile_StalledTransferKeepsPartialForResume`, `TestTransferManager_StalledResumeKeepsPartial`), a persisted upload keeps its session record (transfer_session.go L233–234). Engine classifies it retryable (`TestClassifyResult_StalledTransferIsRetryableNotShutdown`). **HOLDS WITH CAVEAT**: the final-fragment commit of a multi-GB upload is bounded by the fixed 2-minute header timeout (1B-7).

### R-6.5 — store durability; SIGKILL between transfer and commit

- SQLite: DSN pragmas `journal_mode(WAL)`, `synchronous(FULL)`, `foreign_keys(ON)`, `busy_timeout(5000)`, `journal_size_limit(64 MiB)` on every pooled connection (`store.go` L98–103), single writer (`SetMaxOpenConns(1)` L111); `wal_checkpoint(TRUNCATE)` on `Close` (L139–153). FULL in WAL mode fsyncs the WAL on every commit. **HOLDS.**
- Managed files: tokens (`tokenfile.go` L91), upload-session records (`session_store.go` L202) go through `fsroot.AtomicWrite` (temp in target dir, chmod, `Sync`, close, rename, **directory fsync**, error returned if the dir sync fails — `fsroot.go` L456–474, L509). Tests `fsroot_test.go` L401/L431 (`Validates: R-6.5.4`). I did not trace the catalog writer (`internal/cli/drive_catalog.go`) in this track. **HOLDS** for tokens and sessions.
- SIGKILL between transfer and baseline commit: the outcome commit is one transaction on a detached context (`worker.go` L205–225). Crash points `download.renamed` and `upload.remote_done` (`crash_points.go` L19–22) are injected right after the transfer (executor_transfer.go L104, L270) and `TestCrashRecovery_EveryCrashPointConverges` (crash_points_test.go L316) reopens the store and tree and requires convergence. By the comparison rules: after a download crash the local file hashes to the remote hash → `equal_again`/`diverged+current_equal` → baseline update, no transfer; after an upload crash the remote item hashes to the local hash → same; an interrupted create is identified by the pending-create record (R-5.6.15). Pending-create recording is itself a store write before any content (`recordPendingCreate`, executor_transfer.go L483–501; `TestUploadFile_RecordsTheCreatedItemBeforeAnyContent`). **HOLDS.**

---

## 2. Findings

### 1B-1 — HIGH — `.heic` downloads accept any body whose size differs from the metadata, on the first attempt, with no hash and no error-document check

- File: `internal/driveops/transfer_download.go` L64–66 (`isKnownHEICServedSmaller`), L149–150 (`classifyDownload` case `downloadKnownQuirk`), L418–424 (accepted and returned); `internal/graph/download.go` L313–316 (`checkDownloadBodyType` returns nil for any item with a QuickXorHash).
- Rule: R-5.5.1 / R-6.2.3 "hash verify", D-038 ("accepting whatever arrived … replaced good local files with error pages"). R-5.8.1 says "a size mismatch identifies the quirk", so the code matches the requirement; the requirement is the hole.
- Scenario: a Personal drive full of iPhone photos (the `.heic`/`.HEIC` extension is matched case-insensitively). A captive portal, corporate proxy, CDN hiccup or Graph edge answers the pre-authenticated download URL with **HTTP 200 and an HTML/JSON error page** (D-038 records this has happened), or with a **truncated or empty body whose Content-Length matches** (Go's client only errors when the body is *shorter than* the declared length). For a hashed `.jpg` this is a `downloadMismatch` → retried 3× → `ErrDownloadIntegrity`, local file untouched. For a hashed `.heic`: `opts.RemoteHash != ""` fails, `sizeDiffers` is true, `remoteSize > 0 && remoteSize != localSize` is true → `downloadKnownQuirk` → accepted at once, Debug log only. `publishPartial` renames the page over `IMG_0001.HEIC`; `downloadOutcome` commits `LocalHash = hash(page)`, `RemoteHash = OneDrive's real hash`, so the next pass sees neither side changed and the file is never fetched again. The previous local version (if any) survives only in the 30-day trash. For a first-time download there is no previous version: the "photo" on disk is an HTML page, silently.
- Evidence: `return strings.EqualFold(filepath.Ext(targetPath), ".heic") && remoteSize > 0 && remoteSize != localSize` (L65) — note there is no `remoteSize > localSize`, no content-type check, no attempt counter, no hash involvement; and `if item.QuickXorHash != "" { return nil }` (download.go L314) disables the HTML/JSON gate precisely for the hashed items this path accepts.
- Existing coverage: `TestTransferManager_HEICMetadataMismatch_AcceptsFirstDownloadQuietly` (L748) asserts the acceptance with a *larger* reported size; `TestTransferManager_HEICSameSizeHashMismatch_StillRetries` (L788). **None** exercises an HTML/JSON body, an empty body, or a body larger than the metadata for `.heic`.
- Proposed regression tests: (a) `.HEIC`, hashed, downloader writes `<html>…</html>` with `Content-Type: text/html` (through the graph layer, or inject `ErrUnexpectedDownloadBody`) → expect `ErrDownloadIntegrity`, no file; (b) `.heic`, hashed, downloader writes 0 bytes while `RemoteSize=3_000_000` → expect integrity failure; (c) `.heic` body larger than reported → not the quirk. A sound fix shape (not applied): require `localSize < remoteSize`, a non-HTML/JSON content type, a plausible HEIF signature (`ftyp` box at offset 4), or accept only after the 3-attempt determinism check like every other transform.
- Confidence: **CONFIRMED by tracing** that the path accepts such bodies; the trigger (200 + error page / truncated body) is PLAUSIBLE and is the exact event D-038 was written for.

### 1B-2 — MEDIUM — a deterministic same-length hash mismatch is accepted and recorded as synced (P-10)

- File: `transfer_download.go` L170–183, L440–451; result consumed by `downloadOutcome` (executor_transfer.go L108, L139) and `TestRunOnce_AcceptedServerTransformIsNotDownloadedAgain`.
- Rule: S3 "hash verify" and the user docs' promise that the file is "checked against the hash OneDrive reports".
- Scenario: a middlebox or local AV that rewrites bytes length-preservingly, a server-side bug serving a different version of the same length, or an undiscovered local `quickxorhash` bug at some input length — all produce the same wrong hash three times at the reported size and are accepted with one Info line ("keeping the download"). The baseline then records `RemoteHash = reported`, `LocalHash = actual`, so the mismatch is permanent and invisible to later passes. This is spec'd (R-5.8.2) for AIP-protected files, but the acceptance criterion cannot tell a legitimate server transform from a consistent corruption, and the log level (Info) means a user running at default verbosity will not notice.
- Existing coverage: `TestTransferManager_DeterministicMismatchAtReportedSize_AcceptedOnce`, `TestTransferManager_AcceptedTransformDoesNotWarn` (asserts it is *not* a Warn).
- Proposed test / change: none for correctness of the implemented rule; recommend at least a Warn with the path and a `status` condition so an operator can audit accepted transforms, and an opt-out config for drives where no AIP/server rewriting is expected (Personal).
- Confidence: CONFIRMED by tracing (behaviour); the trigger is SPECULATIVE.

### 1B-3 — LOW — stale user documentation on download safety

- File: `docs/safety.md` L167 ("a just-downloaded file is not flushed to disk before it is renamed"; "the last attempt is accepted after a few retries").
- Status: the flush claim is **FIXED-IN-CODE** (transfer_download.go L362, L318, 2026-10-30, R-5.10.1); the acceptance claim is now only true for the P-10 transform and `.heic` (1B-1, 1B-2). The row should say so, and should mention the `.heic` first-attempt acceptance explicitly, because it is the one place a hashed download is accepted without a hash.
- Confidence: CONFIRMED.

### 1B-4 — MEDIUM — no post-upload re-stat; upload verification trusts a hash computed in a separate earlier read pass

- File: `transfer_upload.go` L259 (hash pass), L272 (reopen), L285–304 (upload + settle); `transfer_verify.go` L71–85.
- Rule: R-5.5.2 "verify the stored item against the bytes sent".
- Scenario: on Personal this is safe (server hash must equal the pre-upload hash, and the per-ReadAt stat check catches size/mtime/ctime/identity changes). On a **Business** drive with an enriched extension (1B-6), or any hashless response (1B-5), a file rewritten in place between the hash pass and the chunk reads with size preserved is caught only by ctime — which is reliable on Linux/macOS local filesystems but not on all network/FUSE mounts (ctime may be absent → `plannedCtimeChanged` returns false when `planned.LocalCtime == 0`).
- Existing coverage: `TestUploadFile_SourcePreconditionRunsDuringSessionRead`.
- Proposed test: hashless mock uploader + file rewritten with same size and restored mtime between hash and first ReadAt on a shim without ctime → expect a stale precondition, currently expect success.
- Confidence: PLAUSIBLE.

### 1B-5 — MEDIUM — size-only "verified" upload commits a converged baseline (R-5.5.2 vs S9)

- File: `transfer_verify.go` L75 (`item.QuickXorHash == "" && item.Size == sent.size → transferVerified`), `uploadOutcome` executor_transfer.go L392–415.
- Rule: R-6.2.11 says equal size is never evidence of equality; R-5.5.2 says "otherwise by size". The code follows R-5.5.2.
- Scenario: Graph's final-fragment response omits `file.hashes` (R-5.6.8 notes post-processing can show pre-commit facts); the upload is recorded as verified with `RemoteHash=""`. If the stored content actually differs, nothing detects it until the next delta brings a hash — and then `remote_changed` compares `b.remote_hash=''` vs `r.hash` (skipped), size (equal), eTag (changed by processing → `remote_changed=1`), and `equal_again` requires `l.hash == r.hash`, so a *real* divergence does surface as a download/conflict on the next pass; a same-size divergence with an unchanged eTag would not. Low likelihood on Personal.
- Existing coverage: `TestTransferManager_Upload_CompletionWithoutItemIsLookedUp`; none for "item present but hashless after upload on a drive that normally hashes".
- Proposed test: Personal-like mock returning a hashless item with the right size → assert the outcome is read back once by ID (one extra `GetItem`) before being accepted, or at least that `RemoteHash` stays empty so the next delta re-evaluates.
- Confidence: CONFIRMED by tracing (behaviour), SPECULATIVE (trigger).

### 1B-6 — MEDIUM (Business/SharePoint only) — the enrichment allowlist disables hash verification for all photo and video uploads on Business drives

- File: `transfer_verify.go` L77–84, L102–115 (`.jpg .jpeg .png .gif .tif .tiff .heic .heif .mp3 .m4a .mp4 .mov .m4v .wmv .avi` plus Office and `.pdf`), `isBusinessLikeDrive` L125–132 (any drive ID starting `b!` counts).
- Rule: R-5.5.2 "accepted only for the documented enrichment class".
- Scenario: on a Business/SharePoint drive, any stored hash and any size ≥ sent size is `transferEnrichedKnown` for these extensions, so a corrupted or wrong-content upload of a photo/video is recorded as verified with per-side hashes. Not applicable if the production target is a Personal drive (the test account is Personal); flagging because the owner's drive type is not stated.
- Existing coverage: `TestTransferManager_Upload_WarnsOnHashMismatch` (acceptance), `TestTransferManager_Upload_EnrichedButEmptyAnswerIsReadBackAndFails`.
- Proposed test: Business item with enriched extension and a *smaller-but-non-zero* or wildly larger size → currently accepted; consider bounding enrichment growth and reading back once.
- Confidence: CONFIRMED by tracing.

### 1B-7 — MEDIUM — multi-GB final-fragment commit can race the 2-minute header timeout and the resume logic into a full re-upload

- File: `graph/upload_transfer.go` L392–399 (`FinalFragmentError`), `transfer_session.go` L212–231 (immediate adoption lookup), L239–303 (`resumePersistedSession`), `graph/upload_session.go` L187–199 (`ResumeUpload`: empty `nextExpectedRanges` → parse error) and `graphtransport/profiles.go` L15 (2 min).
- Rule: R-5.6.12, R-6.2.10; spec itself lists "final fragment's header timeout does not scale with the file size" as not done (drive-transfers.md L257–258).
- Scenario: the last 10 MiB chunk of a 12.9 GB file is sent; OneDrive's commit outlasts `ResponseHeaderTimeout` → `FinalFragmentError` → `adoptLandedUpload` runs **immediately**, before the server has committed, finds the old item → not adopted, not 404 → session record kept. The engine retries later → `ResumeUpload` → `QueryUploadSession`. If the session has completed, Graph answers 404 → `ErrUploadSessionExpired` → lookup → adopted by hash (good). If Graph instead answers 200 with `nextExpectedRanges: []` for a committing/committed session, `parseNextExpectedRangeStart` fails → `default:` branch **drops and cancels the session** (L267–272) and the next attempt starts a fresh 12.9 GB upload. No data loss (the If-Match preflight and 412 still protect remote edits), but a possible repeat of the whole transfer per attempt for exactly the owner's 8 largest files.
- Existing coverage: `TestSessionUpload_FinalFragmentFailureAdoptsCommittedItem`, `TestSessionUpload_GoneSessionWithLandedItemIsAdopted`, `TestUploadFromSession_OnlyFinalFragmentFailureIsUncertain` — all assume the item is already committed when looked up or the session is already 404; none covers "status 200 with empty ranges".
- Proposed test: mock `QueryUploadSession` returning 200 with `NextExpectedRanges: []` → expect a lookup/adoption attempt (or a bounded wait), not `dropSession`. Also consider a larger header timeout for the final fragment proportional to size.
- Confidence: PLAUSIBLE (Graph's exact answer for a committed session's status is unprobed in the repo's live-probes).

### 1B-8 — LOW — free-space check is per download, not per concurrent set

- File: `transfer_download.go` L732–778; workers `minWorkers` 4, default 8 (`worker.go` L21, R-5.1.1).
- Scenario: 8 workers each see 20 GB free and each start a 10 GB download; the pre-check passes for all; the disk fills mid-write. Mitigated: ENOSPC is wrapped into `ErrDiskFull` → `disk:local` scope, partials are preserved for resume, no corruption (the partial is never renamed until verified and fsynced). Behavioural only.
- Proposed test: two concurrent downloads whose sum exceeds `available - minFree` with a stubbed `diskAvailableFunc` → currently both proceed.
- Confidence: CONFIRMED by tracing.

### 1B-9 — LOW — `TestTransferManager_HEICSameSizeHashMismatch_StillRetries` documents retries but ends in acceptance

- File: transfer_manager_test.go L788–812: after 3 attempts `require.NoError` — the same-size, same-wrong-hash `.heic` is accepted via P-10, so "StillRetries" does not mean "still refused". Combined with 1B-1 this means a hashed `.heic` can never fail hash verification at all: different size → accepted at once; same size → accepted after 3 identical attempts. The design doc wording ("keeps the normal hash retries") reads as if the hash still gates `.heic`; it does not.
- Confidence: CONFIRMED.

### 1B-10 — LOW — timestamps before 1970 become "unknown" on the remote side

- File: `graph/items.go` L22 (`minValidYear = 1970`), L344; `graph/upload.go` L50–64 clamps only pre-1900.
- Scenario: a local file with mtime 1950 uploads with `lastModifiedDateTime=1950-…` (accepted by OneDrive), but every read-back parses it as unknown (zero): `RemoteMtime=0` in the baseline, `committedByThisUpload` can never adopt it, and a download on another machine sets no mtime (file gets download time). No loop (per-side comparisons), no loss. Archives of scanned documents sometimes carry such dates.
- Proposed test: round-trip an item with `fileSystemInfo.lastModifiedDateTime=1950-01-01T00:00:00Z` through `toItem` → currently zero time.
- Confidence: CONFIRMED by tracing.

### 1B-11 — LOW (perf) — every large upload reads the file twice plus one `Lstat` per transport read

- `UploadFile` hashes the whole file (L259), then streams it; `validatingReaderAt` stats before every `ReadAt` (L63–76). For 8 files of 4.6–12.9 GB that is ~2× the I/O and hundreds of thousands of stats; on resume after a crash the full hash pass repeats. Correctness is fine; mention because the owner's first sync is 1.2 TB.

---

## 3. Checked and found sound

- Partial lifecycle: namespaced, 0600, version-tagged, never resumed without a version binding, discarded on mismatch, kept on cancel/stall/rename failure; cleanup excluded from observation and delete safety, scoped by ignored dirs, included dirs and protected roots.
- Download verification ordering: hash → size → error-document gate for hashless; existing local file is never replaced on integrity failure; the replaced version is verified through the open descriptor and kept in the local trash before the rename (`validateDownloadTargetBeforeReplace`, executor_preconditions.go L581–642).
- Target preconditions before and again immediately before the rename: type, size, full-precision mtime, ctime, identity, hash of the planned/synced version; missing parent is a stale plan, never `MkdirAll` in sync.
- Upload preflight + If-Match on both PUT and `createUploadSession`; 412/404-fragment/409 all become stale preconditions, never unconditional retries; creates are by JSON body with `conflictBehavior=fail`, content sent by ID conditional on the created eTag, pending-create recorded durably before content, empty placeholder removed conditionally when content did not land.
- Session persistence: hash-bound records, atomic+fsync'd writes, cancel-on-drop, expiry-aware invalidation vs. fresh-session decision, 416 recovery with monotonic offset checks.
- `current_equal` is the single SQL definition of equality and requires both hashes (zero-byte exception only); `equal_again` prevents re-downloading a file whose hash already matches; move pairing requires both hashes; hash reuse requires size+mtime+ctime+identity and the 1 s settle window.
- Hash algorithm: QuickXorHash only, known vectors plus independent reference; SHA digests never compared.
- mtime: per-side storage, whole-second wire format, mtime set before fsync, observed mtime recorded (no FAT/SMB loops), pre-1900 clamp with warning.
- Stall watchdog on request and response bodies, header timeout, keepalives; stalled transfers resume.
- Store: WAL + synchronous=FULL, single writer, checkpoint on close; outcome commit detached from cancellation; crash-point convergence test covers download-renamed and upload-remote-done; token and session files fsync file and directory.
