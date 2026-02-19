# SharePoint Enrichment: Implementation Guide

## 1. What Is Enrichment?

SharePoint has a server-side "feature" (Microsoft calls it enrichment) that **silently modifies files after upload**. When a file is uploaded to a SharePoint document library (including OneDrive for Business drives backed by SharePoint), SharePoint may inject metadata from the library's column schema directly into the file's binary content. This changes the file's bytes, hash, and size on the server relative to what was uploaded.

This is documented as Microsoft API issue [onedrive-api-docs#935](https://github.com/OneDrive/onedrive-api-docs/issues/935) and is acknowledged as intended behavior by Microsoft. There is no API to disable it.

### 1.1 Affected File Types

SharePoint enriches files that contain structured metadata containers:

| File type | How SharePoint modifies it |
|-----------|---------------------------|
| **PDF** (.pdf) | Injects XML metadata into the PDF's XMP metadata stream |
| **MS Office** (.docx, .xlsx, .pptx, etc.) | Injects custom XML parts into the Office Open XML package |
| **HTML** (.html, .htm) | May inject metadata tags or modify existing meta elements |

Other file types (images, plain text, archives, source code, etc.) are **not** enriched. Enrichment only occurs on **SharePoint document libraries** (`driveType == "documentLibrary"`). Personal OneDrive drives and standard Business drives that are not backed by SharePoint libraries are not affected.

### 1.2 What Changes

After enrichment, the following properties differ between what was uploaded and what the server now holds:

- **QuickXorHash**: Different because file bytes changed
- **Size**: Different (usually larger — metadata was added)
- **cTag**: Different (content changed)
- **eTag**: Different (item was modified)
- **lastModifiedDateTime**: May change (server records the modification)

The **itemId**, **name**, and **parentReference** remain unchanged.

### 1.3 When It Happens

Enrichment occurs during upload processing. The upload API response already reflects the enriched state — the response contains the post-enrichment hash and size, not the pre-enrichment values. This is the key detection signal: `response.hash != localHash` after a successful upload to a SharePoint library.

### 1.4 Enrichment Is Not Idempotent

Evidence from [abraunegg/onedrive#3070](https://github.com/abraunegg/onedrive/issues/3070) shows that uploading an already-enriched file produces a different hash than the input. Each upload triggers a fresh round of metadata injection. This means any approach that downloads the enriched version and later re-uploads it will produce yet another enriched variant. This has critical implications for approach selection (see section 3).

---

## 2. The Infinite Loop Bug

Without enrichment handling, the following catastrophic loop occurs:

```
1. Client uploads file.pdf (hash=AAA, size=1000)
2. Upload response returns (hash=BBB, size=1050)        ← enriched
3. Client records SyncedHash=AAA in DB (used local hash as single baseline)
4. Next delta query: server reports hash=BBB for file.pdf
5. Client: BBB != SyncedHash(AAA) → remote changed → download
6. Client downloads enriched file (hash=BBB, size=1050)
7. Local file now has hash BBB. BBB != SyncedHash(AAA) → local changed → upload
8. Upload response returns (hash=CCC, size=1100)         ← re-enriched!
9. GOTO step 4. Loop forever.
```

This is reference implementation bug [abraunegg/onedrive#3070](https://github.com/abraunegg/onedrive/issues/3070). The root cause is using a **single synced hash as the baseline for both sides**. When local and remote content legitimately diverge (due to server-side modification), no single baseline value can satisfy both comparisons without triggering a spurious change detection.

---

## 3. Alternative Approaches Analyzed

### 3.1 Option A: Download Enriched Version

After upload, detect hash mismatch on SharePoint, immediately download the enriched file to replace the local copy. All three states (local, remote, synced) become identical.

This is what the reference implementation does by default (when `create_new_file_version = false`), and was our original Tier 2 design choice.

**Pros:**
- Simple mental model: after sync, local == remote, always
- `verify` command is trivial — just compare local hash against remote hash
- Single `SyncedHash` works (all three states agree)
- Loop prevention is inherent (all hashes converge)

**Cons:**
- **Silently modifies the user's local file.** The PDF they saved is replaced with a SharePoint-modified version.
- Extra download for every enriched file on every upload (bandwidth + latency). For initial sync of a large SharePoint library with many Office files, this roughly doubles the transfer count for affected files.
- If the user has the file open in an editor, they see "file changed on disk" warnings.
- **Editor-fighting loop risk:** If the editor reacts by saving back the original content, it triggers re-upload → re-enrichment → re-download. Because enrichment is not idempotent (section 1.4), this creates an infinite loop that is difficult to break.
- Interacts poorly with Linux file indexers (e.g., `tracker3`) that may update timestamps on the downloaded file, triggering spurious change detection.

**Verdict:** Works, but the editor-fighting risk and silent local file modification are significant downsides. The non-idempotent enrichment behavior makes the download approach more fragile than it appears.

### 3.2 Option B: Per-Side Change Baselines (Recommended)

Instead of a single synced base, use **two separate baselines for change detection** — one per side. Compare local changes against the last known local hash. Compare remote changes against the last known remote hash.

Current merge logic (single baseline):
```
remoteChanged = (currentRemoteHash != SyncedHash)
localChanged  = (currentLocalHash  != SyncedHash)
```

Proposed merge logic (per-side baselines):
```
remoteChanged = (currentRemoteHash != item.QuickXorHash)  // last known remote
localChanged  = (currentLocalHash  != item.LocalHash)      // last known local
```

After uploading to SharePoint with enrichment:
- `LocalHash = AAA` (pre-enrichment, what is actually on disk)
- `QuickXorHash = BBB` (post-enrichment, what the server holds)

Next cycle: `AAA == LocalHash` → no local change. `BBB == QuickXorHash` → no remote change. **No action. No loop. No download. No local file modification.**

**Pros:**
- No extra download (saves bandwidth + latency)
- **Never modifies the user's local file** — their content is preserved exactly as they saved it
- No data model changes needed (we already have `QuickXorHash` and `LocalHash` as separate columns)
- No special enrichment detection code — the algorithm handles it naturally because it uses separate baselines
- No configuration options needed (no `disable_upload_validation` flag)
- Eliminates the editor-fighting loop entirely (file is never changed on disk)
- Works correctly in all sync modes (bidirectional, push-only, pull-only)
- **Future-proof:** handles any server-side content modification, not just SharePoint enrichment

**Cons:**
- Local file content != remote file content for enriched files (permanently, until the user next modifies the file)
- `verify` command is more nuanced (see section 5)
- `SyncedHash` role changes — it is no longer the sole baseline for the three-way merge
- Developers must understand that the merge uses per-side baselines, not a common ancestor

**Verdict:** Most robust approach. Correct by construction rather than by detection. See section 4 for full design.

### 3.3 Option C: Dual Synced Hashes (Explicit New Columns)

Add two explicit columns: `SyncedLocalHash` and `SyncedRemoteHash`. Same logic as Option B but with dedicated columns instead of repurposing `LocalHash` and `QuickXorHash`.

**Pros:** Same as Option B, plus clearer schema intent.

**Cons:** Adds a column to the data model purely for an edge case. Every query that touches synced state now touches two columns. Option B achieves identical results with existing columns.

**Verdict:** Unnecessary. Option B is strictly better — same behavior, fewer columns.

### 3.4 Option D: Create New File Version

After upload with hash mismatch, PATCH the file's `lastModifiedDateTime` to create a new version on SharePoint. This is the reference implementation's alternative approach (when `create_new_file_version = true`).

**Pros:** Local file untouched. No extra download.

**Cons:**
- Consumes the user's storage quota (each version counts against their allocation)
- Creates confusing version history (metadata-only versions with no user-visible changes)
- Does not change the server hash — the enriched content is still the current version's content
- **Incompatible with our hash-based sync algorithm.** The reference implementation uses timestamps for primary change detection, so updating the timestamp is sufficient. We use hashes. After creating a new version, `SyncedHash` still cannot match both the local hash and the server hash. The fundamental single-baseline problem remains unsolved.

**Verdict:** Not viable with our sync algorithm. Only works for timestamp-based sync engines.

### 3.5 Option E: Validation Bypass Flags

Provide `disable_upload_validation` and `disable_download_validation` config flags. When enabled, skip hash verification entirely. This was the reference implementation's original approach before explicit enrichment handling.

**Pros:** Simple to implement.

**Cons:**
- Disables ALL hash verification, not just for enriched files — silent data corruption becomes undetectable
- Requires manual user configuration per SharePoint library
- The user must diagnose the problem themselves, understand it, and know to enable the flag
- Even with validation disabled, the three-way merge still has the single-baseline problem — bypassing verification does not prevent the merge from detecting a spurious change

**Verdict:** A blunt instrument that disables safety checks without solving the root cause. We retain these as last-resort escape hatches for unknown future edge cases, but they are not a solution.

### 3.6 Comparison Matrix

| Criterion | A: Download | **B: Per-Side** | C: Dual Cols | D: New Version | E: Bypass |
|-----------|:-----------:|:---------------:|:------------:|:--------------:|:---------:|
| Prevents infinite loop | Yes | **Yes** | Yes | No* | No |
| Local file untouched | No | **Yes** | Yes | Yes | N/A |
| No extra network I/O | No | **Yes** | Yes | No (PATCH) | N/A |
| No data model changes | Yes | **Yes** | No | Yes | Yes |
| No enrichment-specific code | No | **Yes** | No | No | No |
| Hash-based merge compatible | Yes | **Yes** | Yes | No | No |
| `verify` is straightforward | Yes | Nuanced | Nuanced | No | No |
| Editor-fighting safe | No | **Yes** | Yes | Yes | N/A |
| Future-proof to other quirks | No | **Yes** | Yes | No | No |

*Option D prevents the loop in the reference implementation only because it uses timestamp-based change detection.

---

## 4. Recommended Design: Per-Side Change Baselines

### 4.1 Core Algorithm Change

The three-way merge's change detection is modified to use per-side baselines:

```
// Was (single baseline):
remoteChanged = (currentRemoteHash != item.SyncedHash)
localChanged  = (currentLocalHash  != item.SyncedHash)

// Now (per-side baselines):
remoteChanged = (currentRemoteHash != item.QuickXorHash)
localChanged  = (currentLocalHash  != item.LocalHash)
```

`QuickXorHash` stores the last known server hash (recorded from the API response after upload or from the delta query after download). `LocalHash` stores the last known local file hash (computed during upload or after download).

### 4.2 State After Each Operation

#### After upload (no enrichment)

```
LocalHash     = AAA  (computed during upload)
QuickXorHash  = AAA  (from upload response — matches local)
SyncedHash    = AAA  (all agree)
```

#### After upload (with enrichment)

```
LocalHash     = AAA  (computed during upload — what's on disk)
QuickXorHash  = BBB  (from upload response — enriched by SharePoint)
SyncedHash    = BBB  (set to server response hash for DB consistency)
```

The local file is **not modified**. The DB records what is actually on disk (`LocalHash`) and what the server holds (`QuickXorHash`) separately.

#### After download

```
LocalHash     = BBB  (hash of downloaded content — matches server)
QuickXorHash  = BBB  (from server metadata)
SyncedHash    = BBB  (all agree)
```

Downloads always produce agreement because the downloaded content matches the server hash.

### 4.3 Upload Flow Pseudocode

```
function executeUpload(ctx, action, syncRoot):
    // 1. Read local file and compute hash during upload
    file = openFile(localPath)
    stat = file.Stat()
    hasher = quickxorhash.New()
    reader = io.TeeReader(file, hasher)

    // 2. Upload via simple PUT (<=4MB) or session (>4MB)
    if stat.Size <= 4MB:
        response = api.SimpleUpload(ctx, driveID, parentID, name, reader)
    else:
        session = api.CreateUploadSession(ctx, driveID, parentID, name)
        store.SaveUploadSession(session)
        response = api.UploadSessionFragments(ctx, session, reader, stat.Size)
        store.DeleteUploadSession(session.ID)

    file.Close()
    localHash = base64.Encode(hasher.Sum(nil))
    serverHash = response.Item.File.Hashes.QuickXorHash

    if response.Error != nil:
        return UploadResult{OK: false, Error: response.Error}

    // 3. Log enrichment if detected (informational only — no corrective action)
    if serverHash != "" AND serverHash != localHash:
        if isSharePointLibrary(item.DriveID):
            log.Info("SharePoint enrichment detected; local file preserved",
                "path", item.Path,
                "localHash", localHash,
                "serverHash", serverHash,
                "localSize", stat.Size,
                "serverSize", response.Item.Size)
        else:
            log.Warn("upload hash mismatch on non-SharePoint drive",
                "path", item.Path,
                "local", localHash,
                "server", serverHash)

    // 4. Update state DB — store per-side truth
    item.ItemID       = response.Item.ID
    item.QuickXorHash = serverHash                // Remote truth (may be enriched)
    item.Size         = response.Item.Size         // Remote size (may be enriched)
    item.ETag         = response.Item.ETag
    item.RemoteMtime  = parseTimestamp(response.Item.FileSystemInfo.LastModifiedDateTime)
    item.LocalHash    = localHash                  // Local truth (what's on disk)
    item.LocalSize    = stat.Size                  // Local size (what's on disk)
    item.LocalMtime   = toUnixNano(stat.ModTime)
    item.SyncedHash   = serverHash                 // Record server state as reference
    item.SyncedSize   = response.Item.Size
    item.SyncedMtime  = item.RemoteMtime
    item.LastSyncedAt = NowNano()
    store.UpsertItem(item)

    return UploadResult{OK: true, Size: stat.Size}
```

Note the key difference from the original Tier 2 design: **no `executeDownload` call after enrichment detection.** We log the enrichment and store the divergent hashes. The per-side merge baselines prevent any spurious action on the next cycle.

### 4.4 Three-Way Merge Pseudocode

```
function classifyChange(item, currentLocalHash, currentRemoteHash):
    // Per-side baseline comparison
    localChanged  = (currentLocalHash  != item.LocalHash)
    remoteChanged = (currentRemoteHash != item.QuickXorHash)

    if !localChanged AND !remoteChanged:
        return NoChange

    if localChanged AND !remoteChanged:
        return LocalChange    // Upload needed

    if !localChanged AND remoteChanged:
        return RemoteChange   // Download needed

    // Both changed — determine if it's a real conflict
    if currentLocalHash == currentRemoteHash:
        // Both sides converged to the same content independently
        return Converged      // Just update DB, no transfer

    return Conflict           // Genuine conflict — keep both
```

### 4.5 Edge Case Verification

| # | Scenario | LocalHash | QuickXorHash | Next local | Next remote | Detection | Action | Correct? |
|---|----------|-----------|--------------|------------|-------------|-----------|--------|----------|
| 1 | Normal upload, no enrichment | AAA | AAA | AAA | AAA | No change | None | Yes |
| 2 | Upload with enrichment, no further changes | AAA | BBB | AAA | BBB | No change | None | Yes |
| 3 | Enrichment, then user modifies file | AAA | BBB | CCC | BBB | Local changed | Upload | Yes |
| 4 | Enrichment, then remote change by other user | AAA | BBB | AAA | CCC | Remote changed | Download | Yes |
| 5 | Enrichment, then both sides change | AAA | BBB | CCC | DDD | Conflict | Keep both | Yes |
| 6 | Enrichment, user saves same content | AAA | BBB | AAA | BBB | No change | None | Yes |
| 7 | User moves enriched file locally | AAA (tombstone) | BBB | AAA (new path) | — | Move detected (AAA==AAA) | Remote move | Yes |
| 8 | Initial download from SharePoint | BBB | BBB | BBB | BBB | No change | None | Yes |
| 9 | Upload-only mode with enrichment | AAA | BBB | AAA | — | No local change | None | Yes |
| 10 | Enrichment, user replaces file with web-downloaded copy | AAA | BBB | BBB | BBB | Local changed (BBB!=AAA) | Upload | Yes* |

*Case 10: User downloads the enriched version from the web UI and replaces their local file. `BBB != LocalHash(AAA)` → local change → upload. After upload, SharePoint re-enriches (hash CCC). Store `LocalHash=BBB, QuickXorHash=CCC`. Stable on next cycle. This is correct behavior — the user made a deliberate local change.

### 4.6 Role of SyncedHash

With per-side baselines, `SyncedHash` is no longer the primary merge baseline. It serves two secondary purposes:

1. **Enrichment indicator:** When `SyncedHash == QuickXorHash` but `SyncedHash != LocalHash`, we know enrichment occurred at the last sync. This can be used for `verify` reporting and diagnostics.

2. **Conflict resolution context:** During conflict resolution, knowing the common ancestor hash (the last value where both sides agreed, if one exists) helps generate meaningful conflict descriptions. `SyncedHash` tracks the server's response hash, which serves this purpose.

If desired, `SyncedHash` could be removed from the schema in a future simplification. The merge algorithm does not depend on it.

---

## 5. Verify Command

The `verify` command checks that the sync state is consistent:

```
function verify(item, syncRoot):
    localPath = join(syncRoot, item.Path)
    currentLocalHash = computeHash(localPath)
    currentRemoteHash = fetchRemoteHash(item.DriveID, item.ItemID)

    localConsistent  = (currentLocalHash  == item.LocalHash)
    remoteConsistent = (currentRemoteHash == item.QuickXorHash)
    enriched         = (item.LocalHash != item.QuickXorHash)

    if localConsistent AND remoteConsistent:
        if enriched:
            report(item.Path, "OK (enriched by SharePoint — local content preserved)")
        else:
            report(item.Path, "OK")
    else if !localConsistent:
        report(item.Path, "LOCAL CHANGED since last sync")
    else if !remoteConsistent:
        report(item.Path, "REMOTE CHANGED since last sync")
```

A `--strict` flag could optionally report enriched files as warnings for users who want local == remote:

```
verify --strict: "WARN: Documents/report.pdf — enriched by SharePoint, local differs from remote"
```

---

## 6. Upload-Only Mode

In `--push-only` (upload-only) mode, the per-side baseline approach works without any special handling:

1. Upload file → server enriches → store `LocalHash=AAA, QuickXorHash=BBB`
2. Next cycle: local hash `AAA == LocalHash` → no local change → no re-upload
3. Remote is not checked in push-only mode

No warnings needed. No special code paths. The algorithm is mode-agnostic.

---

## 7. Download-Side Hash Mismatches

Enrichment can also cause hash mismatches during downloads from SharePoint, though this is rarer. The typical case: the API reports the post-enrichment metadata, the download delivers the post-enrichment content, and the hashes match. However, edge cases exist where the **reported size** does not match the downloaded size (see [ref-edge-cases #6.3](docs/tier1-research/ref-edge-cases.md)).

Download verification handles this with a known-exception list:

```
function verifyDownload(item, computedHash):
    if item.QuickXorHash != "" AND computedHash != item.QuickXorHash:
        if isKnownHashMismatchType(item):
            // SharePoint library files, iOS .heic files
            log.Warn("hash mismatch for known-buggy file type",
                "path", item.Path,
                "expected", item.QuickXorHash,
                "got", computedHash)
            // Accept the download — this is a known API inconsistency
        else:
            // Genuine corruption — delete partial and fail
            removeFile(partialPath)
            return DownloadResult{OK: false, Error: HashMismatchError{...}}
```

`isKnownHashMismatchType` returns true when:
- The drive is a SharePoint document library (`driveType == "documentLibrary"`)
- OR the file is an iOS `.heic` file (separate API bug)

---

## 8. Related: Azure Information Protection (AIP)

AIP-protected files exhibit similar symptoms through a different mechanism:

- **Upload**: File may be encrypted/modified server-side. Hash and size change.
- **Download**: File may be decrypted on download. Downloaded bytes differ from server-reported metadata.
- **Both hash and size differ** (unlike enrichment where size only increases slightly).

The per-side baseline approach handles AIP naturally: after upload, `LocalHash` reflects the local (unencrypted) content and `QuickXorHash` reflects the server (encrypted) content. No spurious change detection occurs.

For downloads where both hash and size differ from API-reported values, the download verification's known-exception list handles this case. The actual on-disk hash and size are stored in `LocalHash` and `LocalSize`.

---

## 9. Escape Hatches

Despite automatic handling via per-side baselines, we retain two config options for unknown future edge cases:

| Option | Default | Purpose |
|--------|---------|---------|
| `disable_download_validation` | `false` | Skip QuickXorHash verification after downloads. Workaround for SharePoint libraries where even the download hash is wrong. |
| `disable_upload_validation` | `false` | Skip hash comparison after uploads. Workaround for extreme cases. |

These should **never** be enabled in normal operation. When enabled, a warning is logged on every sync cycle:

```
WARN: Download validation disabled. Data integrity cannot be guaranteed.
WARN: Upload validation disabled. Data integrity cannot be guaranteed.
```

---

## 10. Testing Strategy

### 10.1 Unit Tests

| Test name | What it proves |
|-----------|----------------|
| `TestMerge_EnrichmentNoAction` | After upload with enrichment (LocalHash != QuickXorHash), next cycle produces no action |
| `TestMerge_EnrichmentThenLocalChange` | User modifies enriched file → local change detected → upload |
| `TestMerge_EnrichmentThenRemoteChange` | Remote change on enriched file → download |
| `TestMerge_EnrichmentThenBothChange` | Both sides change → conflict correctly detected |
| `TestMerge_EnrichmentConverged` | User replaces local file with enriched content → both hashes now agree → update DB only |
| `TestUpload_EnrichmentLogsInfo` | Post-upload hash mismatch on SharePoint logs INFO, does not trigger download |
| `TestUpload_NonSharePointHashMismatch` | Hash mismatch on non-SharePoint drive logs WARN |
| `TestVerify_EnrichedFileReportsOK` | Enriched file reports "OK (enriched by SharePoint)" |
| `TestVerify_EnrichedFileStrictMode` | `--strict` flag reports enriched files as warnings |

### 10.2 Regression Tests

| Test name | Bug ref | What it proves |
|-----------|---------|----------------|
| `TestRegression_EnrichmentNoLoop` | issues-common-bugs #1.2 | Upload to SharePoint → enrichment → next cycle produces no upload, no download. Run 5 sync cycles to prove stability. |
| `TestRegression_EditorFightingImpossible` | (preventive) | After upload with enrichment, local file is never modified by the sync engine. Verify file mtime and content are unchanged. |

### 10.3 How to Simulate in Tests

Mock the upload API to return a response with a different `QuickXorHash` than what was computed locally. Set the drive type to `"documentLibrary"`. Verify that:

1. `executeDownload` is **never** called after upload
2. `item.LocalHash` equals the pre-enrichment hash (what's on disk)
3. `item.QuickXorHash` equals the server response hash (enriched)
4. A second sync cycle with the same server state produces **zero actions**
5. A third, fourth, and fifth cycle also produce zero actions (loop-free)

---

## 11. Logging

All enrichment events are logged at **INFO** level (not debug) because they indicate a material server-side modification:

```
INFO: SharePoint enrichment detected; local file preserved
      path=Documents/report.pdf localHash=abc123 serverHash=def456
      localSize=10240 serverSize=10752

WARN: Upload hash mismatch on non-SharePoint drive
      path=Documents/report.pdf local=abc123 server=def456
```

No warning for enrichment on SharePoint — it is expected, documented behavior. Only non-SharePoint hash mismatches warrant a warning.

---

## 12. Changes from Original Tier 2 Design

This document supersedes the enrichment handling described in the following Tier 2 documents. The changes are:

| Document | Section | Original design | New design |
|----------|---------|-----------------|------------|
| [sync-algorithm.md](docs/tier2-design/sync-algorithm.md) | §9.3 (Upload Execution) | Download enriched version after upload | Log enrichment, store per-side hashes, no download |
| [sync-algorithm.md](docs/tier2-design/sync-algorithm.md) | §5 (Three-Way Merge) | Single `SyncedHash` baseline for both sides | Per-side baselines: `LocalHash` for local, `QuickXorHash` for remote |
| [sync-algorithm.md](docs/tier2-design/sync-algorithm.md) | Appendix C, D11 | "Download enriched version after SharePoint upload" | "Store per-side hashes; algorithm naturally handles divergence" |
| [sync-algorithm.md](docs/tier2-design/sync-algorithm.md) | Appendix B comparison table | "Detect enrichment, download enriched version" | "Per-side baselines; no enrichment-specific code" |
| [architecture.md](docs/tier2-design/architecture.md) | §7.4 (API Quirks) | "Detect server-side hash change after upload, accept without re-upload" | "Per-side baselines handle naturally; no detection code needed" |
| [architecture.md](docs/tier2-design/architecture.md) | §9.3 (Transfer Verification) | "Exception: SharePoint libraries where server-side enrichment is expected" | Upload verification logs INFO for SharePoint enrichment; no exception needed in merge |
| [configuration.md](docs/tier2-design/configuration.md) | §8.4 (Validation Bypass) | Primary workaround for enrichment | Escape hatch only; per-side baselines are the primary mechanism |
| [test-strategy.md](docs/tier2-design/test-strategy.md) | §3, §10 | `TestTransfer_EnrichmentDetection` (download-based) | `TestMerge_EnrichmentNoAction` and related (baseline-based) |
| [prd.md](docs/tier2-design/prd.md) | §15 (Handled with Warnings) | "Detect server-side modification, don't re-upload" | "Per-side baselines prevent spurious re-upload and re-download" |

These Tier 2 documents should be updated to reflect the per-side baseline approach before Tier 3 implementation begins.

---

## 13. References

- [onedrive-api-docs#935](https://github.com/OneDrive/onedrive-api-docs/issues/935) — Microsoft's acknowledgment of the enrichment behavior
- [abraunegg/onedrive#3070](https://github.com/abraunegg/onedrive/issues/3070) — Infinite loop bug report
- [issues-common-bugs.md §1.2](docs/tier1-research/issues-common-bugs.md) — Our research on the loop bug
- [ref-edge-cases.md §6.2](docs/tier1-research/ref-edge-cases.md) — SharePoint post-upload file modification
- [ref-edge-cases.md §3.1](docs/tier1-research/ref-edge-cases.md) — SharePoint library quirks
- [issues-api-inconsistencies.md §8.1](docs/tier1-research/issues-api-inconsistencies.md) — SharePoint data loss scenario
- [api-item-field-matrix.md §3.9](docs/tier1-research/api-item-field-matrix.md) — SharePoint enrichment field matrix entry
- [ref-conflict-scenarios.md §6.4](docs/tier1-research/ref-conflict-scenarios.md) — AIP file handling (related)
- [sync-algorithm.md §9.3](docs/tier2-design/sync-algorithm.md) — Original upload execution design (to be updated)
- [configuration.md §8.4](docs/tier2-design/configuration.md) — Validation bypass escape hatches
