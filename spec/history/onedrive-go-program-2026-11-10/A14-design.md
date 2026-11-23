# A14 design: a create whose answer is lost keeps its claim

Found live on 2026-11-11 (A5's BasicRoundTrip during a 75 s network outage).
Worktree to make after A3 merges: `fix/create-answer-lost`. Same files as A3
(`internal/sync/executor_transfer.go`, `store_pending_creates.go`), same chaos
file as A7 (`internal/sync/chaos_remote_test.go`): A3, then A14, then A7.

## Defect

`uploadNewFile` records an intent (path, no item ID), then creates. When the
create lands on OneDrive but its answer is lost (transport error, 5xx, 408,
deadline), the `OnCreated` hook never runs, and `uploadNewFile` clears the
intent because the attempt was not canceled. Every retry then gets 409, and
`resolveCreateConflict` adopts only a row that has an item ID. The next
observation sees a stranger's empty file: a create conflict.

## Fix

1. `graph.CreateFile` marks a failure that does not say whether the create
   landed with a sentinel `ErrCreateOutcomeUnknown`, wrapped with `%w` so it
   survives the driveops wrapping. Unknown: no response, 5xx, 408, context
   cancel or deadline. Not unknown: a `GraphError` 4xx other than 408
   (409 and 429 included).
2. `uploadNewFile` reads the pending row before it records this attempt's
   intent (`prior` = found, `ItemID == ""`, within `pendingCreateIntentTTL`).
   After an unknown outcome it keeps the intent.
3. `resolveCreateConflict`, on 409 with a prior intent and no item ID: in the
   `ListChildren` it already makes, a child with the same spelling (NFC-equal
   name), live, a file, size 0 is claimed: `RecordPendingCreate` with its ID
   and eTag, then the existing adopt path (GetItem, `Size == 0`,
   `opts.IfMatch = item.ETag`, `UploadFileToItem`). A zero-size local file
   needs no content upload (trace how size-0 creates flow: `UploadCreating`
   calls `onCreated` only for a non-zero size, the intent is recorded for
   every size).
   - Same-spelling child with content: not ours. Clear the intent, return the
     conflict as today.
   - No same-key child in the listing yet (eventual consistency): keep the
     intent, return the conflict (the retry comes later).
   - Differently spelled same-key sibling: `remoteNameTakenError` and clear,
     as today.
   - 409 without a prior intent: exactly today's behavior. Never claim a
     stranger's empty file.
4. If a retry layer inside `CreateFile` (`doDocumentedGraphQuirkRetry`,
   `internal/graph/quirks.go:38`; the sync engine's transport itself does not
   retry, R-6.8.21) can replay after an unknown outcome and return 409 within
   one call, that 409 must also let the executor claim.
5. Safety: the claim is guarded by If-Match and the size-0 check, the same
   exposure `claimIntendedPlaceholderTx` already accepts on the observation
   side. No creation-time comparison.

## Tests first

- Executor: the create lands, the answer is lost, the retry gets 409, claims
  and uploads; one remote file, no conflict copy.
- A `TestRunOnce_` scenario (listed in `runOnceScenarios()`): answer lost, the
  next pass observes the empty item and uploads into it.
- 409 without a prior intent does not claim a stranger's empty file.
- Chaos fault `create:response-lost` in `chaos_remote_test.go`
  (`uploadCreating`: the real create runs, the real `onCreated` hook is not
  called, a lost-response error comes back). Today's only create fault,
  `upload-session:timeout-after-create`, returns after the real hook ran.

## Docs

R-5.6.15, `sync-execution.md`, `drive-transfers.md`, `sync-store.md`, the
header comment of `store_pending_creates.go`.

## Live proof

A lost answer cannot be forced live (no network fault injection on this
server). Proof is the unit and chaos tests plus a regression run of
`TestE2E_SyncWatch_BasicRoundTrip` and the watch client.

Writer: the session model (omit `model`).
