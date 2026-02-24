# Next: Concurrent Execution Redesign

## Problem

`RunOnce` is fully sequential: observe → plan → execute (downloads → uploads → deletes) → commit. A slow transfer blocks everything else. This causes user-facing issues even in one-shot `sync`: interrupted syncs lose all progress, downloads block uploads, large files starve small ones, and Ctrl-C means restarting multi-GB transfers from byte 0.

## Alternatives

### A: Concurrent download + upload phases

Minimal change. Run downloads and uploads as separate `errgroup`s in parallel instead of sequentially. Two pools of 8 workers each, draining simultaneously. Folder creates and deletes stay sequential (ordering matters).

*Fixes*: #1 (downloads don't block uploads)
*Doesn't fix*: #2, #3, #4, #5
*Effort*: Small — restructure `Execute()` phases 3-4 into a parent errgroup
*Risk*: Low — no new data structures, same commit model

### B: Incremental per-transfer commits

After each successful transfer, immediately commit that single outcome to the baseline. If sync is interrupted, all completed work is persisted. Next `sync` re-observes but the planner sees the updated baseline and skips already-synced files.

*Fixes*: #3 (interrupted sync preserves progress)
*Doesn't fix*: #1, #2, #4, #5
*Effort*: Medium — `BaselineManager` needs a `CommitOne(outcome)` method, must handle concurrent writers if combined with A (WAL mode helps but needs careful locking)
*Risk*: Medium — current `Commit()` is atomic (all outcomes + delta token in one tx). Per-transfer commits mean the delta token can't be saved until all transfers complete, creating a split between "baseline updated" and "delta token saved"

### C: Micro-batch execution with re-observation

Split the action plan into batches of N actions (e.g., 50). Execute a batch, commit, re-observe (delta + local scan), re-plan, execute next batch. Each cycle picks up new changes that arrived during the previous batch.

*Fixes*: #3 (batch commits), #4 (periodic re-observation)
*Doesn't fix*: #1 (still sequential within batch), #2, #5
*Effort*: Medium — loop around `RunOnce` with batch-size limit in the planner
*Risk*: High — repeated delta fetches hit the API hard, repeated full local scans are expensive for large trees, re-planning can produce inconsistent decisions if remote state changed between batches

### D: Priority queue with small-files-first scheduling

Replace flat `[]Action` with a priority queue. Small files first (quick wins, fast progress), large files last. Combined with B (incremental commits), this maximizes the number of files synced before any interruption. Priority could also consider: recently modified > old, user-opened files > background.

*Fixes*: #2 (large files don't starve small ones), #3 (with B)
*Doesn't fix*: #1, #4, #5
*Effort*: Small — sort actions by size before feeding to `executeParallel`, or use a channel-based worker pool instead of index-based errgroup
*Risk*: Low — purely a scheduling optimization, same execution model

### E: Persistent transfer ledger with resume

Before execution, write the entire action plan to a `transfer_queue` table in SQLite. Each row: action type, path, item ID, status (pending/in_progress/done/failed), bytes_transferred, upload_session_url. Workers claim rows, update progress, mark done. `Commit` only touches done rows. If the process crashes: next `sync` reads the ledger, resumes in-progress transfers (chunked uploads via `QueryUploadSession`, downloads via `Range` header from `.partial` file size). Planner merges new changes with existing ledger entries.

*Fixes*: #1 (with concurrent phases), #2 (progress visible per-transfer), #3 (ledger survives crash), #5 (resume from last byte)
*Partially fixes*: #4 (next sync picks up new changes and merges with remaining ledger)
*Effort*: Large — new DB table, worker-claims model, upload session persistence, download resume (B-085), ledger-aware planner
*Risk*: Medium — more moving parts, but each piece is well-understood (DB queue, Range headers, upload session query). This is essentially the "transfer queue" from the Phase 5 architectural note, pulled forward.

## Ranking

**E > A+B+D combined > C**

E is the most work but it's the foundation Phase 5 needs anyway. Building it now means watch mode inherits a working transfer layer instead of requiring a ground-up rewrite.

The pragmatic middle ground is **A+B+D** combined — concurrent phases, incremental commits, size-based priority. Three small-to-medium changes that compose well, fix problems #1/#2/#3, and don't require new persistence.

C is the weakest — high cost, high risk, and the re-observation overhead defeats the purpose.

## Issue Reference

1. Downloads block uploads — sequential phases
2. One slow transfer blocks the phase — 7 idle workers waiting
3. Interrupted sync loses all progress — batch commit at the end
4. No new changes detected during execution — observe phase is done
5. No transfer resume — crash = restart from byte 0
