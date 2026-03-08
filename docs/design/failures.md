# Failure Analysis: Sync Engine Error Handling

## Overview

This document analyzes every failure path in the sync engine, what the existing retry layers handle, what survives them, and the delta token advancement bug that causes permanent item loss in watch mode.

> **Note:** This document describes the failure analysis that motivated the current design.
> The delta token advancement bug (§4) and in-memory failure tracker (§5) have been
> replaced by durable per-item failure state with exponential backoff in `remote_state`.
> Function names like `watchCycleCompletion` and `failureTracker` refer to removed code.

## Existing Retry Layers

### Layer 1: Graph Client (`internal/graph/client.go`)

- **Max retries**: 5 attempts (1s base, 2x factor, 60s cap, +/-25% jitter)
- **Retries**: network errors, HTTP 408, 429, 500, 502, 503, 504, 509
- **Does NOT retry**: 401, 403, 404, 409, 410, 412, 423, 507, or any other 4xx

### Layer 2: Executor (`internal/sync/executor.go`)

- **Max retries**: 3 additional attempts (1s base, 2x factor, +/-25% jitter)
- **Retryable**: 404, 408, 412, 429, 509, all >= 500
- **Fatal** (immediate stop): 401, 507, context cancellation
- **Skip** (no retry): 423, 403, 409, and any other 4xx

### Combined Behavior

| Status Code | Graph Client (L1) | Executor (L2) | Total Attempts |
|-------------|-------------------|----------------|----------------|
| Network error | Retry 5x | Skip | 6 |
| 401 Unauthorized | No retry | Fatal | 1 |
| 403 Forbidden | No retry | Skip | 1 |
| 404 Not Found | No retry | Retryable | 4 |
| 408 Request Timeout | Retry 5x | Retryable | Up to 24 |
| 409 Conflict | No retry | Skip | 1 |
| 412 Precondition Failed | No retry | Retryable | 4 |
| 423 Locked | No retry | Skip | 1 |
| 429 Throttled | Retry 5x | Retryable | Up to 24 |
| 500-504, 509 | Retry 5x | Retryable | Up to 24 |
| 507 Insufficient Storage | No retry | Fatal | 1 |
| FS permission error | N/A | Skip | 1 |

## Failures That Survive Both Layers

### Downloads (delta-driven)

These failures originate from remote changes delivered via the delta endpoint. The delta cursor determines whether these items appear in future polls.

| Failure | Frequency | Self-heals? | Typical Duration |
|---------|-----------|-------------|------------------|
| **423 Locked** (SharePoint co-authoring) | Common in enterprises | Yes, when user closes file | Minutes to hours |
| **403 Forbidden** (permission revoked) | Rare | Only if re-granted | Indefinite |
| **Persistent 5xx** (extended outage) | Extremely rare (survives 24 attempts) | Yes, when outage ends | Minutes |
| **FS permission** (can't write locally) | Rare (misconfiguration) | No, user must fix | Indefinite |

### Uploads (local-change-driven)

These failures originate from local filesystem changes detected by the watcher or scanner. The delta cursor is **not involved** — the planner regenerates upload actions every run as long as the local file differs from baseline.

| Failure | Frequency | Self-heals? | Typical Duration |
|---------|-----------|-------------|------------------|
| **400 Bad Request** (invalid SharePoint filename) | Occasional | No, user must rename | Indefinite |
| **423 Locked** (remote file locked) | Common in enterprises | Yes, when lock released | Minutes to hours |
| **403 Forbidden** (no write permission) | Rare | Only if re-granted | Indefinite |
| **FS read errors** (can't read local file) | Rare | Only if permissions fixed | Indefinite |
| **Parent not in baseline** | Rare (ordering issue) | Yes, next run | Seconds |

### Local-only operations (no graph calls)

These are filesystem operations triggered by remote changes. No graph retry layers apply.

| Failure | Frequency | Self-heals? | Typical Duration |
|---------|-----------|-------------|------------------|
| **Non-empty directory delete** | Occasional | No, structural mismatch | Indefinite |
| **FS permission** (can't delete/move locally) | Rare | No, user must fix | Indefinite |
| **Cross-device rename** | Very rare | No, filesystem layout issue | Indefinite |

## Recovery Mechanisms by Action Source

### Uploads: natural infinite retry

Uploads do not need a recovery mechanism beyond what already exists. The planner compares local files against baseline every run. If an upload failed, the file still differs from baseline, and the planner regenerates the upload action. This provides infinite retry with no additional mechanism.

The only gaps are:
- **No backoff**: a permanently-failing upload (bad filename) is retried every run forever, wasting one worker's time per run
- **No visibility**: the user has no way to know a file can't be uploaded

### Downloads: the delta token advancement bug

Downloads rely on the delta cursor to know what changed remotely. Once the cursor advances past a failed item, that item is permanently lost from the delta stream. See the next section.

### Local operations: depends on trigger source

Local-only operations (LocalDelete, LocalMove) are triggered by remote changes delivered via delta. They share the same delta token loss problem as downloads — if the local operation fails and the token advances, the remote change that triggered it is lost.

## Delta Token Advancement Bug

### The mechanism

Watch mode (`engine.go:RunWatch`) polls the remote delta endpoint periodically. Each poll returns changes since the last token and a new token.

Two tokens exist:
1. **In-memory token** in `RemoteObserver` — updated after every successful API call (`observer_remote.go:199`), regardless of action success
2. **Database token** — committed only when a run has zero failures (`engine.go:989`)

The intended safety: if a run fails, the database token stays stale. On daemon restart, the stale token causes re-observation of the failed items.

### The bug

`watchCycleCompletion` (`engine.go:1006`) commits `remoteObs.CurrentDeltaToken()` — the **latest** in-memory token, not a per-run token. This means:

```
Poll 1: token T → changes [A, B, C, D, E] → new token T'
  Observer stores T' in memory
  D fails with 423
  cycleFailures > 0 → T' NOT committed to DB ← intended safety

Poll 2: token T' → changes [F, G] → new token T''
  Observer stores T'' in memory
  F, G succeed
  cycleFailures == 0 → commits T'' to DB ← safety defeated

D is permanently lost. Even on restart, DB has T'' which is past D.
```

The "don't commit on failure" protection is defeated by the next successful run, which commits the latest token (past everything, including previous failures). This happens in one poll interval (typically 5 minutes).

### What is lost

The specific item version that failed to download. If the file is later modified on the remote, a new delta event appears and the new version is downloaded. If the file is never modified again, the local copy remains stale permanently.

### The failure tracker's role

The in-memory failure tracker (`failure_tracker.go`) suppresses items after 3 failures. This accelerates the bug — suppressed items don't count as failures, so the run succeeds and commits the token. But the bug exists independently of the tracker: even without suppression, the in-memory token advances past failed items, and the next run (with different items) commits it.

## Current State of the In-Memory Failure Tracker

The failure tracker (`failure_tracker.go`) provides watch-mode suppression:
- Threshold: 3 failures within 30 minutes
- After threshold: `shouldSkip(path)` returns true, item excluded from plan
- Recovery: `recordSuccess(path)` clears the record
- Cooldown: records older than 30 minutes are reset

The tracker is in-memory only — lost on daemon restart. It was introduced to prevent "delta token starvation" (a permanently-failing item blocking the token forever). In practice, it accelerates the delta token bug by ensuring runs containing the suppressed item succeed.

## Problem Decomposition

Three distinct problems exist:

### 1. Delta token advancement bug (data integrity)

Items lost permanently from the delta stream after one failed run followed by one successful run. Root cause: `watchCycleCompletion` commits the latest in-memory token rather than a per-run token. Affects downloads and local operations triggered by remote changes.

### 2. Silent permanent failures (user experience)

Permanently-failing items (bad filenames, permission errors, non-empty directories) retry silently every run forever. The user has no visibility into what can't sync. Affects all action types.

### 3. Wasted work on permanent failures (efficiency)

Items that will never succeed (bad filename, revoked permissions) consume a worker slot every run. With 4+ workers and a 5-minute poll interval, this is ~12 seconds of wasted work per item per run — negligible unless many items are permanently broken.

## Delta Token Bug: Detailed Mechanism

### Two tokens, two lifetimes

The `RemoteObserver` holds an **in-memory token** (`observer_remote.go:60`) protected by a mutex. It is updated after every successful delta API call (`observer_remote.go:199`), regardless of whether the resulting sync actions succeed or fail. This token drives what the next poll returns.

The **database token** (`delta_tokens` table) is committed by `watchCycleCompletion` (`engine.go:1007`) only when a run has zero failures. This token is read on daemon startup to determine where to resume. It is the crash-recovery token.

### Why the safety is illusory

The design intent: "if a run fails, don't commit the token, so on restart we re-observe the failed items." This works if and only if no subsequent run ever commits a later token. But `watchCycleCompletion` always reads `remoteObs.CurrentDeltaToken()` — the latest in-memory value, not the token that was current when the run's batch was created.

Timeline:

```
Time 0: DB token = T. Poll with T → items [A, B, C, D, E], new token T'.
         Observer stores T' in memory.

Time 1: Planner creates cycle-1 for [A, B, C, D, E].
         Workers execute. A, B, C, E succeed. D fails (423 locked).
         cycleFailures[cycle-1] = 1.
         watchCycleCompletion: failed > 0 → don't commit. DB still has T.

Time 2: Poll with T' (in-memory) → items [F, G], new token T''.
         Observer stores T'' in memory.

Time 3: Planner creates cycle-2 for [F, G].
         Workers execute. F, G succeed.
         cycleFailures[cycle-2] = 0.
         watchCycleCompletion: failed == 0 → commit CurrentDeltaToken() = T''.
         DB now has T''.

Time 4: D is permanently lost. T'' is past D. Even on restart,
         the delta response for T'' won't include D.
```

The gap between "don't commit on failure" (cycle-1) and "commit latest on success" (cycle-2) is typically one poll interval (5 minutes). In practice, the bug fires within 5-10 minutes of any failed download.

### The failure tracker makes it worse, not better

Without the failure tracker, D appears in every delta poll (because the in-memory token has already moved past it... wait, no — D only appeared in the T→T' range). Actually, once the in-memory token passes T', D never appears in delta again regardless of the failure tracker. The tracker's suppression only matters if D somehow reappears — which it does NOT via delta once the in-memory token advances.

The failure tracker's actual effect: it suppresses D within runs that DO contain it (before the in-memory token advances past it). In the scenario where D keeps failing and appears in the same run as other items that succeed, the tracker causes the run to succeed (by suppressing D), committing the token past D even in the SAME run. This is faster item loss than the base bug.

### What the local safety scan does and doesn't catch

The local safety scan (`observer_local_handlers.go:469`) runs every 5 minutes. It walks the local filesystem and compares against baseline. It can detect:

- **Local files not in baseline** → upload events
- **Baseline entries not on local disk** → delete events
- **Local files differing from baseline** → modify events

It **cannot** detect "a file exists on the remote that should be downloaded but isn't local." The safety scan only sees the local filesystem. A failed download leaves no local trace — the file simply doesn't exist locally, and the baseline has no entry for it (because `CommitOutcome` is a no-op for failures). The safety scan sees nothing wrong.

### When the lost item is eventually recovered

The item is recovered only if:

1. **The file is modified on the remote again** — a new delta event appears in a future poll. The new version is downloaded. The intermediate version that failed is lost, but the latest version arrives. This is the most common recovery path.

2. **The daemon restarts and the DB token is still stale** — this only works if no successful run has committed a later token, which (per the bug above) typically happens within 5 minutes. In practice, this recovery path almost never works.

3. **The delta token expires** (Microsoft's ~90-day TTL) — the daemon gets a 410 Gone response, resets the token, and does a full resync. This catches everything but takes 90 days.

4. **The user runs a manual one-shot sync** — `onedrive-go sync` does a fresh delta from the DB token. If the token is stale (pre-bug), this works. If the token has already been committed past D (post-bug), this doesn't help.

### Realistic impact assessment

The worst case: a SharePoint file is locked, someone edits it, the lock is released, and nobody ever edits it again. The local copy is stale. The user has no indication anything is wrong.

How likely is this? In an enterprise SharePoint environment with co-authoring:
- File locks during editing: very common (minutes to hours)
- Files that are edited exactly once and never again: uncommon for actively-used documents, but possible for archives, templates, reference docs
- The user noticing the stale copy: unlikely unless they check timestamps

For personal OneDrive accounts, 423 locks are rare. The most realistic failure is a transient 5xx that persists through 24 retries — extremely unlikely.
