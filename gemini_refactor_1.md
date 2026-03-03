# Analysis: Remote State Separation & Refactor Plan

I have thoroughly reviewed `@docs/design/remote-state-separation.md` alongside the Tier 1 research and the existing architectural guidelines.

The pivot to decouple the Delta API cursor from sync success is absolutely correct and addresses the root cause of the data loss bug. Making the Delta token a pure "observation cursor" while using a durable queue for pending changes aligns with best practices seen in mature tools like Dropbox Nucleus and Syncthing.

However, the specific implementation proposed in the document contains **several critical structural flaws and race conditions** that will cause data loss, infinite loops, and database thrashing. Furthermore, it deviates significantly from the Tier 1 research regarding identity models and symmetric sync state.

Here is an exhaustive enumeration of the risks, flaws, and a "blank slate" recommendation on how to build this correctly.

---

## Critical Flaws and Race Conditions

### 1. The Primary Key Flaw: Move Collisions and Data Loss
The proposed `remote_state` schema uses `path TEXT PRIMARY KEY`. This directly violates the Tier 1 research finding: *"Pattern 3: Use server-provided item IDs as the primary identity key."* (`survey-sync-state-models.md`).
By keying on `path` instead of `item_id`, the design breaks down during complex file operations:
*   **Move Chains / Swaps:** If a user renames `FileA` to `FileB`, and `FileB` to `FileC` in quick succession, the delta response contains multiple move events. If `CommitObservation` executes `INSERT OR REPLACE` with `path` as the primary key, a swap (`A->B` and `B->A`) results in both paths being overwritten with unpredictable state. 
*   **Path-less Deletions:** The API analysis notes: *"On Business accounts, deleted items in delta responses may lack the `name` property."* If the `RemoteObserver` receives a deletion for an ID without a path, how does it write to a table where `path` is the primary key?
*   **Orphaned Unsynced Items:** If a file fails to sync, it exists in `remote_state`. If the user renames this file remotely, the `RemoteObserver` receives the new path but might not know the old path (especially if the item wasn't in `baseline` yet). It writes the new path to `remote_state`. The old path remains in `remote_state` forever, causing an infinite retry loop for a path that no longer exists remotely.

### 2. The `NULL IS NULL` Deletion Race Condition
The proposed cleanup mechanism in `CommitOutcome` is:
`DELETE FROM remote_state WHERE path = ? AND hash IS ?`
Because folders do not have hashes, their hash is `NULL`. SQLite evaluates `NULL IS NULL` as `TRUE`.
*   **The Race:** 
    1. Remote deletes `FolderA`. `remote_state` stores `{path: 'FolderA', is_deleted: 1, hash: NULL}`. 
    2. A worker begins executing the local folder deletion.
    3. Meanwhile, the remote user creates a *new* `FolderA`. Delta polling adds `{path: 'FolderA', is_deleted: 0, hash: NULL}` to `remote_state`.
    4. The worker finishes the deletion and calls `CommitOutcome`. The query `WHERE path = 'FolderA' AND hash IS NULL` matches the *new* folder creation row and deletes it! 
    5. The knowledge of the new folder is permanently lost.

### 3. Database Thrashing in the Reconciler
The Reconciler is designed to be level-triggered, running `reconcile()` on every `Kick()`, which is fired after *every single action completion* by `drainWorkerResults`.
*   **The Risk:** `reconcile()` executes a `SELECT rs.* FROM remote_state LEFT JOIN baseline...`. If a user does a massive initial sync or goes offline, 50,000 items might fail. Every time a worker completes *any* action, the reconciler will perform a massive `LEFT JOIN` on SQLite. Even with the 1-buffered channel coalescing rapid kicks, high worker concurrency will trigger continuous heavy JOINs, thrashing the CPU and database.

### 4. The Conflict Escalation Illusion (Type System Gap)
The document states that a non-empty directory deletion will retry, and *"After failure_count reaches a threshold... the reconciler escalates the non-empty directory to a conflict."*
*   **The Flaw:** The Planner is explicitly defined as a pure function of `([]PathChanges, *Baseline)`. The Reconciler only feeds ephemeral `ChangeEvent` structs into the buffer. A standard `ChangeEvent` does not carry a `failure_count` or an `escalate` flag. Therefore, the Planner has no structural way of knowing the threshold was reached. It will simply plan `ActionLocalDelete` again, the executor will fail again, and the system will enter an infinite loop.

---

## Inconsistencies with Tier 1 Research

### 1. Asymmetric Failure Visibility (Uploads Ignored)
The document explicitly defers handling upload failures: *"What this design does NOT solve... No upload failure visibility."*
The Tier 1 research (`survey-sync-cli-tools.md` and `event-driven-rationale.md`) emphasizes a symmetric three-tree model. By persisting remote failures but leaving local upload failures to infinite, un-backed-off planner loops because the "local filesystem IS the durable record", the system is fundamentally unbalanced.
*   **The Problem:** The filesystem stores the *file content*, not the *failure state*. If a user tries to upload an invalid SharePoint filename, or a locked file, the worker fails. Next cycle, the planner sees the file on disk, schedules an upload, and it fails again. No backoff. No visibility. If a user runs `onedrive-go failures`, they will only see download issues. This is unacceptable for a mission-critical app.

### 2. DriveId Truncation and API Quirks
The Tier 1 research (`issues-api-inconsistencies.md`) stresses that `driveId` can be inconsistently formatted (e.g., dropping leading zeros, mixed casing). The proposed `remote_state` schema uses `drive_id` as a `NOT NULL` column. If the `CommitObservation` doesn't strictly normalize this, `remote_state` queries will fail to match `baseline` queries, breaking the `LEFT JOIN` logic.

---

## The Blank Slate Recommendation

If effort is not a criteria and we must get this completely right for a mission-critical app, we must adopt an **ID-Keyed Unified Sync Journal**. We need to stop treating remote failures and local failures as two completely different problems.

Here is the architecture I would choose:

### 1. Unified Pending State (Symmetric Visibility)
Create a single `pending_operations` (or `sync_journal`) table instead of just `remote_state`.
```sql
CREATE TABLE pending_operations (
    item_id         TEXT    PRIMARY KEY, -- Server ID is the ultimate source of truth
    drive_id        TEXT    NOT NULL,
    direction       TEXT    NOT NULL, -- 'upload' or 'download'
    path            TEXT    NOT NULL, -- Current known path
    item_type       TEXT    NOT NULL,
    hash            TEXT,
    is_deleted      INTEGER NOT NULL DEFAULT 0,
    failure_count   INTEGER NOT NULL DEFAULT 0,
    next_retry_at   INTEGER,
    last_error      TEXT
);
```
When a download fails, write to `pending_operations`. When an upload fails, *also* write to `pending_operations`. This gives us a single source of truth for the `failures` CLI command and enables exponential backoff for uploads.

### 2. ID as the Primary Key
By keying on `item_id`, moves become trivial `UPDATE` statements. `CommitOutcome` deletes based on `item_id`, completely eliminating the `NULL IS NULL` folder deletion race condition. For items that have never been synced (new uploads), generate a temporary client-side ID until the server assigns one, or use a composite key for local-only items.

### 3. Timer-Driven Reconciler (No `Kick()`)
Remove the edge/level-triggered `Kick()` logic tied to worker completions entirely. The database thrashing is unnecessary.
Instead, the Reconciler simply runs every 5 seconds (or whatever interval) and executes:
`SELECT * FROM pending_operations WHERE next_retry_at <= NOW()`
It injects those items into the buffer. If an item succeeds, `CommitOutcome` deletes the row. The Reconciler naturally won't see it anymore. This is infinitely more scalable than a `LEFT JOIN` triggered by worker completions.

### 4. Pass Metadata through the Buffer
To fix the conflict escalation gap, the `ChangeEvent` and `PathView` structs *must* be extended to carry sync metadata (e.g., `FailureCount`). 
When the Reconciler injects a retry event, it includes `FailureCount`. The pure-function Planner can then legitimately implement the logic:
`if Remote.IsDeleted && Local.IsFolder && Remote.FailureCount >= 10 -> ActionConflict`

### 5. Safe Delta Observation
The Delta token *should* advance, but the `RemoteObserver` must process the entire page of changes, write them to the `pending_operations` table, and *then* update the `delta_tokens` table in a single atomic SQLite transaction. This ensures zero data loss on crash, while matching Dropbox Nucleus's exact pattern.
