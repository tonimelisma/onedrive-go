# Exhaustive Blank-Slate Analysis: Remote State Separation

This analysis evaluates the `remote-state-separation.md` design document from a strict blank-slate perspective, cross-referenced against the *complete* corpus of Tier 1 research, design documents, and known API quirks.

While the fundamental pivot to a persistent "Remote Tree" (identical to Dropbox Nucleus) is absolutely correct for solving the starvation bug, the current design contains **four fatal logical flaws**, severe **performance bottlenecks**, and **schema contradictions** that will cause infinite loops, data loss, and database crashes if implemented as-is.

---

## 1. Critical Risks & Fatal Flaws

### FATAL FLAW 1: The "Upload Echo" Infinite Loop (Aggravated by SharePoint Enrichment)
The design states that `computeNewStatus()` is a pure function operating *only* on the current `remote_state` row and the incoming delta item. This breaks when the incoming delta is an "echo" of our own successful upload.
* **The Mechanism:** You upload a file. `baseline` updates with the new `RemoteHash = X`. The execution layer does *not* update `remote_state` (per the doc). The next Delta poll returns the file with hash X. `CommitObservation` sees hash X differs from the old hash in `remote_state`. `computeNewStatus` returns `pending_download`. The Reconciler injects it. The Planner sees `RemoteState` (hash X) == `Baseline` (hash X), outputs *no action*. `CommitOutcome` is never called. `remote_state` remains `pending_download` forever.
* **Tier 1 Validation (`issues-common-bugs.md` 1.2):** This is drastically worsened by SharePoint's "file enrichment", where the server modifies the file post-upload, changing its hash. If uploads don't update `remote_state` with the authoritative server response, the engine will enter an infinite upload-download loop.

### FATAL FLAW 2: The Reconciler Data Loss Bug
The design assumes upload success only needs to clear `local_issues` and update `baseline`. Failing to update `remote_state` creates a data loss vector.
* **The Mechanism:** A remote file fails to download (`download_failed` with Hash A). The user edits the local file and uploads it successfully (Hash B). `baseline` gets Hash B. `remote_state` still says `download_failed` with Hash A. The Reconciler wakes up and injects Hash A. The Planner sees `baseline` has Hash B but `remote_state` has Hash A, concludes the remote changed *back* to A, and generates an `ActionDownload`, overwriting the user's newly uploaded edit.

### FATAL FLAW 3: The `path UNIQUE` Delta Ordering Crash
Section 12 schema defines `path TEXT NOT NULL UNIQUE`.
* **The Mechanism:** As explicitly documented in `docs/tier1-research/issues-api-inconsistencies.md` (Issue #154), the Graph API does *not* guarantee delta ordering. It frequently returns new file creations *before* the deletion of the old file at the same path.
* **The Crash:** If `CommitObservation` processes a new file at `/Documents/Report.pdf` before the deletion entry for the old `/Documents/Report.pdf`, the SQLite `UNIQUE` constraint on `path` will instantly fail, crashing the engine.
* **Secondary Contradiction:** Section 6 claims folder renames are O(1) ("children's parent_id unchanged"). But if `path` is materialized and `UNIQUE`, a parent folder rename requires an O(N) recursive cascade update of every child's path, otherwise subsequent lookups fail. 

### FATAL FLAW 4: Primary Key Vulnerability to API ID Truncation
Section 12 uses `PRIMARY KEY (drive_id, item_id)`.
* **The Mechanism:** `docs/tier1-research/issues-api-inconsistencies.md` (Issues #3072, #3336) proves the Graph API randomly truncates leading zeros from `drive_id`s (15 chars instead of 16) and alters casing across different endpoints. If `CommitObservation` writes the ID exactly as received from the Delta payload without a strict, engine-wide normalization barrier, the primary key will fragment. The same item will have two rows, breaking the state machine completely.

---

## 2. Deviations from Best Practices & Performance Gotchas

### GOTCHA 1: The `DepTracker.Add()` N+1 DB Write Bottleneck
Section 8.2 states: `DepTracker.Add() sets downloading in DB AND adds to in-memory map.`
* **The Problem:** The `DepTracker` is populated in a tight loop during Planner dispatch. On an initial sync of 100,000 files, `DepTracker.Add()` triggers 100,000 synchronous `UPDATE remote_state SET sync_status = 'downloading'` queries in milliseconds. This will lock the SQLite WAL and freeze the engine before workers even start.
* **Best Practice:** Keep `downloading` and `deleting` states *purely in-memory*. The in-memory `DepTracker` already protects files from the Reconciler (`HasInFlight` check). The database state machine only needs to track durable states: `pending_download` -> `synced` or `download_failed`.

### GOTCHA 2: Conflating "Observation" with "Execution"
By storing `downloading` and `download_failed` in `remote_state`, the design breaks the blank-slate principle of isolating the "Remote Tree" (what the server says) from the "Action Queue" (what we are doing). This mixing of application state into the observation table is the root cause of Flaws 1 and 2.

### GOTCHA 3: Missing Hash Fallbacks
The design assumes `hash` is always present. `issues-api-inconsistencies.md` (Section 2.1) proves zero-byte files and many SharePoint files lack hashes in Delta responses. `computeNewStatus()` must have a documented fallback (Size + eTag) when `hash` is `NULL`, otherwise items will endlessly toggle statuses.

---

## 3. Inconsistencies with Existing Architecture

1. **Axiom Broken:** `data-model.md` proudly states: *"The baseline is the ONLY durable per-item state in the system."* This pivot shatters that axiom. You now have two durable per-item states. The documentation must acknowledge this shift from a "pure functional event pipeline" to a "state synchronization" model.
2. **Delta Token Commit Boundary:** Architecture decision `E3` explicitly delayed committing the delta token until *all* cycle actions completed. The new design says: *"Token committed at observation time."* This means if the engine crashes after observation but before execution, the token is permanently advanced. Crash recovery now relies 100% on the Reconciler. This is fine, but needs explicit acknowledgment that `E3` is overturned.

---

## 4. The Blank-Slate Verdict & Required Fixes

The architecture in `remote-state-separation.md` is the right paradigm, but requires these 6 surgical fixes before implementation:

### Fix 1: `CommitOutcome` MUST update `remote_state` on Uploads
When an upload or local move succeeds, the Executor must update `remote_state` to `synced` using the *new* hash and metadata returned by the server (crucial for SharePoint enriched files). This prevents the Reconciler Data Loss Bug.

### Fix 2: `CommitObservation` MUST read `baseline`
`computeNewStatus()` cannot be purely functional against just the delta item. It must check `baseline.RemoteHash`. If an incoming delta matches `baseline.RemoteHash`, it is an echo of our own upload. It must immediately set `sync_status = 'synced'` to prevent the infinite loop.

### Fix 3: Remove `downloading` / `deleting` from the Database
Drop the database writes in `DepTracker.Add()`. In-flight tracking remains purely in-memory. The database state machine simplifies to `pending` -> `synced` / `failed`. This completely eliminates the N+1 performance bottleneck.

### Fix 4: Drop the `path UNIQUE` Constraint
Remove the `UNIQUE` constraint on the `path` column in `remote_state` (or drop the materialized column entirely in favor of lookup). This is the only way to survive the Graph API's out-of-order delta responses (Issue #154) and achieve true O(1) folder renames.

### Fix 5: Enforce ID Normalization Before DB Insertion
Before any data touches `remote_state`, `drive_id` and `item_id` must pass through a strict normalization function (lowercased, 16-character left-padded for drive IDs) to protect the composite Primary Key from API truncation bugs.

### Fix 6: Unify the Reconciler with the Change Buffer
When the Reconciler injects a `download_failed` item into the buffer, it must attach a flag (e.g., `IsReconciliation: true`) to the `ChangeEvent` so the Planner and telemetry know it is dealing with a retried item.