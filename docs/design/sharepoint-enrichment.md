# SharePoint Enrichment: Per-Side Hash Baselines

> This document is the **design rationale** for per-side hash baselines
> (`local_hash` / `remote_hash` in the baseline table). Sections 2-3 analyze
> the problem and alternatives; Section 4 describes the chosen design. See
> [data-model.md](data-model.md) for the baseline schema and
> [sync-algorithm.md](sync-algorithm.md) for how the planner uses per-side
> hashes.

---

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
3. Client records hash=AAA in DB (using a single baseline for both sides)
4. Next delta query: server reports hash=BBB for file.pdf
5. Client: BBB != baseline(AAA) → remote changed → download
6. Client downloads enriched file (hash=BBB, size=1050)
7. Local file now has hash BBB. BBB != baseline(AAA) → local changed → upload
8. Upload response returns (hash=CCC, size=1100)         ← re-enriched!
9. GOTO step 4. Loop forever.
```

This is reference implementation bug [abraunegg/onedrive#3070](https://github.com/abraunegg/onedrive/issues/3070). The root cause is using a **single hash as the baseline for both sides**. When local and remote content legitimately diverge (due to server-side modification), no single baseline value can satisfy both comparisons without triggering spurious change detection.

---

## 3. Alternative Approaches Analyzed

### 3.1 Option A: Download Enriched Version

After upload, detect hash mismatch on SharePoint, immediately download the enriched file to replace the local copy. All three states (local, remote, synced) become identical.

This is what the reference implementation does by default (when `create_new_file_version = false`), and was our original Tier 2 design choice.

**Pros:**
- Simple mental model: after sync, local == remote, always
- `verify` command is trivial — just compare local hash against remote hash
- Single baseline hash works (all states agree)
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

Single-baseline merge logic (the bug):
```
remoteChanged = (currentRemoteHash != singleBaseline)
localChanged  = (currentLocalHash  != singleBaseline)
```

Per-side baseline merge logic (the solution):
```
remoteChanged = (currentRemoteHash != baseline.remote_hash)  // last known remote
localChanged  = (currentLocalHash  != baseline.local_hash)   // last known local
```

After uploading to SharePoint with enrichment:
- `local_hash = AAA` (pre-enrichment, what is actually on disk)
- `remote_hash = BBB` (post-enrichment, what the server holds)

Next cycle: `AAA == local_hash` → no local change. `BBB == remote_hash` → no remote change. **No action. No loop. No download. No local file modification.**

**Pros:**
- No extra download (saves bandwidth + latency)
- **Never modifies the user's local file** — their content is preserved exactly as they saved it
- No special enrichment detection code — the algorithm handles it naturally because it uses separate baselines
- No configuration options needed (no `disable_upload_validation` flag)
- The editor-fighting loop cannot occur (file is never changed on disk)
- Works correctly in all sync modes (bidirectional, push-only, pull-only)
- **Future-proof:** handles any server-side content modification, not just SharePoint enrichment

**Cons:**
- Local file content != remote file content for enriched files (permanently, until the user next modifies the file)
- `verify` command is more nuanced (see section 5)
- Developers must understand that the merge uses per-side baselines, not a common ancestor

**Verdict:** Most robust approach. Correct by construction rather than by detection. See section 4 for full design.

### 3.3 Option C: Dual Synced Hashes (Explicit New Columns)

Add two explicit columns: `synced_local_hash` and `synced_remote_hash`. Same logic as Option B but with dedicated columns instead of repurposing `local_hash` and `remote_hash`.

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
- **Incompatible with our hash-based sync algorithm.** The reference implementation uses timestamps for primary change detection, so updating the timestamp is sufficient. We use hashes. After creating a new version, a single baseline hash still cannot match both the local hash and the server hash. The fundamental single-baseline problem remains unsolved.

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

The three-way merge uses per-side baselines for change detection:

```
remoteChanged = (currentRemoteHash != baseline.remote_hash)
localChanged  = (currentLocalHash  != baseline.local_hash)
```

`remote_hash` stores the last known server hash (recorded from the API response after upload or from the delta query after download). `local_hash` stores the last known local file hash (computed during upload or after download).

### 4.2 State After Each Operation

#### After upload (no enrichment)

```
local_hash    = AAA  (computed during upload)
remote_hash   = AAA  (from upload response — matches local)
```

#### After upload (with enrichment)

```
local_hash    = AAA  (computed during upload — what's on disk)
remote_hash   = BBB  (from upload response — enriched by SharePoint)
```

The local file is **not modified**. The baseline records what is actually on disk (`local_hash`) and what the server holds (`remote_hash`) separately.

#### After download

```
local_hash    = BBB  (hash of downloaded content — matches server)
remote_hash   = BBB  (from server metadata)
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

    // 4. Produce Outcome for BaselineManager — store per-side truth
    outcome.ItemID      = response.Item.ID
    outcome.RemoteHash  = serverHash                // Remote truth (may be enriched)
    outcome.Size        = response.Item.Size         // Remote size (may be enriched)
    outcome.ETag        = response.Item.ETag
    outcome.Mtime       = parseTimestamp(response.Item.FileSystemInfo.LastModifiedDateTime)
    outcome.LocalHash   = localHash                  // Local truth (what's on disk)
    outcome.SyncedAt    = NowNano()
    // BaselineManager commits: local_hash=localHash, remote_hash=serverHash

    return UploadResult{OK: true, Size: stat.Size}
```

The key point: **no download after enrichment detection.** The executor logs the enrichment and produces an Outcome with divergent hashes. The BaselineManager commits `local_hash != remote_hash`. The planner's per-side comparison prevents any spurious action on the next cycle.

### 4.4 Three-Way Merge Pseudocode

```
function classifyChange(view PathView):
    // Per-side baseline comparison
    localChanged  = (view.LocalHash  != view.Baseline.LocalHash)
    remoteChanged = (view.RemoteHash != view.Baseline.RemoteHash)

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

| # | Scenario | local_hash | remote_hash | Next local | Next remote | Detection | Action | Correct? |
|---|----------|------------|-------------|------------|-------------|-----------|--------|----------|
| 1 | Normal upload, no enrichment | AAA | AAA | AAA | AAA | No change | None | Yes |
| 2 | Upload with enrichment, no further changes | AAA | BBB | AAA | BBB | No change | None | Yes |
| 3 | Enrichment, then user modifies file | AAA | BBB | CCC | BBB | Local changed | Upload | Yes |
| 4 | Enrichment, then remote change by other user | AAA | BBB | AAA | CCC | Remote changed | Download | Yes |
| 5 | Enrichment, then both sides change | AAA | BBB | CCC | DDD | Conflict | Keep both | Yes |
| 6 | Enrichment, user saves same content | AAA | BBB | AAA | BBB | No change | None | Yes |
| 7 | User moves enriched file locally | AAA (old path deleted) | BBB | AAA (new path) | — | Move detected (AAA==AAA) | Remote move | Yes |
| 8 | Initial download from SharePoint | BBB | BBB | BBB | BBB | No change | None | Yes |
| 9 | Upload-only mode with enrichment | AAA | BBB | AAA | — | No local change | None | Yes |
| 10 | Enrichment, user replaces file with web-downloaded copy | AAA | BBB | BBB | BBB | Local changed (BBB!=AAA) | Upload | Yes* |

*Case 10: User downloads the enriched version from the web UI and replaces their local file. `BBB != local_hash(AAA)` → local change → upload. After upload, SharePoint re-enriches (hash CCC). Baseline committed: `local_hash=BBB, remote_hash=CCC`. Stable on next cycle. This is correct behavior — the user made a deliberate local change.

### 4.6 Enrichment Detection

Enrichment is detectable from the baseline: when `local_hash != remote_hash` for a file that was last synced via upload, the server modified the content. This can be used for `verify` reporting and diagnostics. The planner does not need special enrichment logic — the per-side comparison handles it naturally.

---

## 5. Verify Command

The `verify` command checks that the sync state is consistent:

```
function verify(entry BaselineEntry, syncRoot):
    localPath = join(syncRoot, entry.Path)
    currentLocalHash = computeHash(localPath)
    currentRemoteHash = fetchRemoteHash(entry.DriveID, entry.ItemID)

    localConsistent  = (currentLocalHash  == entry.LocalHash)
    remoteConsistent = (currentRemoteHash == entry.RemoteHash)
    enriched         = (entry.LocalHash != entry.RemoteHash)

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

1. Upload file → server enriches → baseline committed: `local_hash=AAA, remote_hash=BBB`
2. Next cycle: local hash `AAA == local_hash` → no local change → no re-upload
3. Remote is not checked in push-only mode

No warnings needed. No special code paths. The algorithm is mode-agnostic.

---

## 7. Download-Side Hash Mismatches

Enrichment can also cause hash mismatches during downloads from SharePoint, though this is rarer. The typical case: the API reports the post-enrichment metadata, the download delivers the post-enrichment content, and the hashes match. However, edge cases exist where the **reported size** does not match the downloaded size (see [ref-edge-cases #6.3](docs/tier1-research/ref-edge-cases.md)).

Download verification handles this with a known-exception list:

```
function verifyDownload(remoteHash, computedHash, item):
    if remoteHash != "" AND computedHash != remoteHash:
        if isKnownHashMismatchType(item):
            // SharePoint library files, iOS .heic files
            log.Warn("hash mismatch for known-buggy file type",
                "path", item.Path,
                "expected", remoteHash,
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

The per-side baseline approach handles AIP naturally: after upload, `local_hash` reflects the local (unencrypted) content and `remote_hash` reflects the server (encrypted) content. No spurious change detection occurs.

For downloads where both hash and size differ from API-reported values, the download verification's known-exception list handles this case. The actual on-disk hash is stored in `local_hash`.

---

## 9. Escape Hatches

Despite automatic handling via per-side baselines, we retain two config options for unknown future edge cases:

| Option | Default | Purpose |
|--------|---------|---------|
| `disable_download_validation` | `false` | Skip hash verification after downloads. Workaround for SharePoint libraries where even the download hash is wrong. |
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
| `TestPlanner_EnrichmentNoAction` | After upload with enrichment (local_hash != remote_hash), next cycle produces no action |
| `TestPlanner_EnrichmentThenLocalChange` | User modifies enriched file → local change detected → upload |
| `TestPlanner_EnrichmentThenRemoteChange` | Remote change on enriched file → download |
| `TestPlanner_EnrichmentThenBothChange` | Both sides change → conflict correctly detected |
| `TestPlanner_EnrichmentConverged` | User replaces local file with enriched content → both hashes now agree → update baseline only |
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

Mock the upload API to return a response with a different hash than what was computed locally. Set the drive type to `"documentLibrary"`. Verify that:

1. No download action is produced after upload
2. Outcome's `LocalHash` equals the pre-enrichment hash (what's on disk)
3. Outcome's `RemoteHash` equals the server response hash (enriched)
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

## 12. References

- [onedrive-api-docs#935](https://github.com/OneDrive/onedrive-api-docs/issues/935) — Microsoft's acknowledgment of the enrichment behavior
- [abraunegg/onedrive#3070](https://github.com/abraunegg/onedrive/issues/3070) — Infinite loop bug report
- [issues-common-bugs.md §1.2](docs/tier1-research/issues-common-bugs.md) — Our research on the loop bug
- [ref-edge-cases.md §6.2](docs/tier1-research/ref-edge-cases.md) — SharePoint post-upload file modification
- [ref-edge-cases.md §3.1](docs/tier1-research/ref-edge-cases.md) — SharePoint library quirks
- [issues-api-inconsistencies.md §8.1](docs/tier1-research/issues-api-inconsistencies.md) — SharePoint data loss scenario
- [api-item-field-matrix.md §3.9](docs/tier1-research/api-item-field-matrix.md) — SharePoint enrichment field matrix entry
- [ref-conflict-scenarios.md §6.4](docs/tier1-research/ref-conflict-scenarios.md) — AIP file handling (related)
- [sync-algorithm.md](sync-algorithm.md) — Sync algorithm (§9 Executor covers upload flow)
- [configuration.md](configuration.md) — Validation bypass escape hatches
