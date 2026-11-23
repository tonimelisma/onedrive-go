# 01b — Make the test signals trustworthy: product and e2e half

Repository `~/src/onedrive-go`, branch `main`, commit `70301c0e`. Read-only analysis: no test, build or devtool command was run, nothing in the repository was changed. Paths are relative to the repository root unless they start with `/`.

Marking: **VERIFIED** means I read the code or text that proves the statement. **SUSPECTED** means I did not, and the sentence says what would settle it. Two companion files hold the per-line audits made by two sub-agents of this analysis; where a row was read only by the sub-agent, this report says so:

- `~/agent/notes/onedrive-go-program-2026-11-10/analysis/01c-e2e-single-pass-syncs.md` (Item 3)
- `~/agent/notes/onedrive-go-program-2026-11-10/analysis/01d-e2e-name-only-waits.md` (Item 2)

The 2026-11-10 timeline quoted in Item 1 comes from the task brief (CI log excerpts). I did not read the CI log itself.

## Results

1. **The 2026-11-10 failure is a test bug that exposed three product defects.** The test stops the daemon as soon as the remote *name* exists (`e2e/sync_watch_e2e_test.go:261-264`), which is 155 ms after the create and before any content. The product then (a) gives up removing its empty placeholder after a single 412 (`internal/graph/item_create.go:233-235`), (b) on the large-file path can clear the only record of an item it created (`internal/sync/executor_transfer.go:500-502` with `internal/driveops/transfer_session.go:206-209`), and (c) prints a WARN during a clean shutdown (`internal/sync/engine_housekeeping.go:53-54`), against R-6.6.21. In the incident itself the leftover was recorded and the next pass would have uploaded into it; no user data was at risk.
2. **Shutdown cancels in-flight work by design; it never drains.** One root context is cancelled on the first signal and every request descends from it. This matches R-6.3.6. Three other texts say "drain" and are wrong about the code (Item 1a).
3. **80 name-only remote waits in `e2e/`; 18 are racy against a live daemon**, one of them in the fast (PR) tier. No local-side wait is racy (Item 2).
4. **104 one-shot `sync` calls run once and require success; 66 should use a converge helper**, 37 must stay single-pass, 1 is unclear. Two helper variants are missing (per-drive, all-drives) (Item 3).
5. **There is no shared fake Graph.** Five separate fake layers exist. None can inject a 400 on child create, a 412 on placeholder cleanup, or a cancelled session-create that lands (Item 4).
6. **`status --perf` shows zero HTTP, DB and transfer figures for a running owner**, in watch and one-shot mode, because those three metric families are recorded on the command collector and the status command reads the per-mount collectors (Item 5). This is a bug against R-6.6.15.

---

## Item 1 — The 0-byte upload leftover

### 1.0 The incident mapped to code

`TestE2E_SyncWatch_PauseResume` (`e2e/sync_watch_e2e_test.go:195-270`) runs `sync --watch --upload-only` (:210), pauses, writes a 21-byte file (:239-243), resumes (:258), waits with `stat` for the remote name (:261), sends SIGTERM (:264), then reads the remote content and compares it (:268-269).

| Time | Event (task brief) | Code |
|---|---|---|
| before | intent row written | `internal/sync/executor_transfer.go:493-495` |
| 18:00:48.058 | `POST .../items/<parent>/children` answers 201 | `CreateFile`, `internal/graph/item_create.go:59-94` (route at :71), called at :156 |
| after 201 | create hook writes the item row; `created = true` | `item_create.go:166`; `executor_transfer.go:545-552` |
| in flight | `POST .../items/<id>/createUploadSession` | `item_create.go:172`; `internal/graph/upload_transfer.go:322-325`; `internal/graph/upload_session.go:36, 47` |
| 18:00:48.213 | test's `stat` answers 200 on the empty item; test sends SIGTERM | `e2e/sync_watch_e2e_test.go:261, 264` |
| 18:00:48.218 | root context cancelled; "sealing new work admission in_flight=1"; the session POST fails "context canceled" | `internal/cli/signal.go:45-47`; `internal/sync/engine_watch_drain.go:15`; `upload_session.go:47` |
| 18:00:48.487 | cleanup GET answers 200 | `item_create.go:221` |
| 18:00:48.805 | conditional DELETE answers 412; Debug "failed to remove the empty file a failed upload created" | `item_create.go:233-234` |
| then | "action failed action=upload" | `item_create.go:175`; `internal/driveops/transfer_verify.go:186-188` |
| then | WARN "Could not refresh the shared-folder list; skipping transfer-leftover cleanup until it can be read" | `internal/sync/engine_housekeeping.go:53-54` |
| after exit | test downloads empty content; assertion fails | `e2e/sync_watch_e2e_test.go:268-269` |

VERIFIED: the hook had run and returned nil before the cancel, because `chunkedUploadToExistingItem` is reached only after a nil hook result (`item_create.go:165-172`). So `created` was true and the intent was not cleared (`executor_transfer.go:500-502`). The item row existed when the daemon exited.

VERIFIED: `getRemoteFile` cannot notice a placeholder. It waits for visibility, polls `get` until it succeeds, and reads once (`e2e/remote_read_helpers_test.go:438-471`). A `get` of a 0-byte item succeeds and yields "".

SUSPECTED: the 412 was caused by the cancelled `createUploadSession` landing on the server between the GET (48.487) and the DELETE (48.805) and moving the eTag. This fits the recorded quirk (`spec/reference/graph-api-quirks.md:1137-1139`), but server timing cannot be read from code. A live probe settles it: cancel `createUploadSession` client-side on a fresh empty item and read the item's eTag before and after.

### 1a. Why shutdown cancels the upload in the same millisecond

VERIFIED call chain, no step adds a deadline or a detached context:

1. `shutdownContext` cancels on the first signal: `ctx, cancel := context.WithCancel(parent)` (`internal/cli/signal.go:24`), `cancel()` (:47). Installed at `internal/cli/root.go:713`.
2. The sync command passes that context to `orch.RunWatch(ctx, mode, opts)` (`internal/cli/sync_runtime.go:229`).
3. Each mount derives `mountCtx, mountCancel := context.WithCancel(ctx)` (`internal/multisync/orchestrator_watch.go:415`) and calls `engine.RunWatch(ctx, mode, opts)` (:508).
4. The engine starts its workers with `pool.Start(ctx, ...)` (`internal/sync/engine_watch.go:196`), which derives `ctx, wp.cancel = context.WithCancel(ctx)` (`internal/sync/worker.go:103`).
5. Each action derives `actionCtx, cancel := context.WithCancel(ctx)` (`worker.go:185`) and runs `dispatchAction(actionCtx, ...)` (:209), which reaches `c.doRequest(ctx, ...)` in the graph client (`upload_session.go:47`).

So the "sealing new work admission" line (`engine_watch_drain.go:15`) and the aborted request are two effects of the same `cancel()`.

VERIFIED: there is no drain period. What the code calls draining is consuming the completions of actions that were just cancelled:

- `runDrainStep` selects only on completions, refresh results and observer errors (`engine_watch_drain.go:56-75`). Its comment: "Draining phase: no new work admission remains live. Only action completions, refresh result cleanup, and terminal observer exit/error handling may run." (:60-62).
- `pipe.cleanup` logs "graceful shutdown: draining in-flight actions" (`engine_watch.go:235-240`) and then calls `pool.Stop()` (:245). `Stop` is documented as "Stop cancels all in-flight work, waits for goroutines to exit" (`worker.go:121-130`).

VERIFIED: only two kinds of work outlive the cancel. A finished action's baseline commit runs on `context.WithTimeout(context.WithoutCancel(ctx), outcomeCommitTimeout)` (`worker.go:240-245`; 30 s, :37). Best-effort cleanups run detached for 5 s in graph (`item_create.go:218`, `upload_transfer.go:329`) and 10 s in driveops (`internal/driveops/transfer_session.go:74-75, 431, 458, 480`).

What the specification says (all VERIFIED by reading the cited line):

| Text | Wording | Matches the code |
|---|---|---|
| R-6.3.6, `spec/requirements/non-functional.md:44` | "cancels the command context so in-flight work unwinds through its own cleanup" | yes |
| `spec/design/cli.md:91-96` | "The first SIGINT/SIGTERM cancels the command context and the command unwinds through its own cleanup" | yes |
| `spec/design/sync-control-plane.md:597-602` | "cancels the shared watch contexts ... No timer escalates the first signal" | yes |
| repo `CLAUDE.md:187` | "In-flight operations are interruptible." | yes |
| R-2.8.3, `spec/requirements/sync.md:92` | "lets already-admitted work follow the normal shutdown path" | ambiguous |
| `internal/cli/signal.go:12-17` | "work in flight follows its normal shutdown path" | ambiguous |
| repo `CLAUDE.md:186` | "stop accepting new work, drain in-flight operations, flush state" | no |
| `spec/design/sync-engine.md:567` | "graceful drain on shutdown" | no |
| log text, `engine_watch.go:237` | "graceful shutdown: draining in-flight actions" | no |

I found no statement about shutdown in the parts of `spec/design/sync-execution.md` (168-181) and `spec/design/drive-transfers.md` (273-311) that I read. Neither file was read in full (see "Not read").

Conclusion: cancelling the upload is not a bug against the requirements. The three "drain" texts are a documentation defect.

### 1b. The placeholder life cycle

**Two upload paths for a new file** (VERIFIED, `internal/driveops/transfer_upload.go:140-188`):

- Size above 4 MiB with a session store: `persistedSessionUpload` (:156-176). The create and the session are one graph call, `CreateUploadSession` (`item_create.go:186-202`).
- Otherwise, with a create hook: graph `UploadCreating` (:178-182; `item_create.go:152-179`).

An overwrite of a known remote item goes by item ID with `If-Match` and makes no placeholder (`internal/sync/executor_transfer.go:213, 218-220, 462-472`).

**What is recorded when** (VERIFIED):

| Step | Small path (at most 4 MiB) | Large path (above 4 MiB) |
|---|---|---|
| Before any request | intent row: path, `item_id=''` (`executor_transfer.go:493-495`; `internal/sync/store_pending_creates.go:76-95`) | same |
| Empty item exists | hook writes the item row, ID and creation eTag (`item_create.go:166`; `executor_transfer.go:537-554`) | nothing yet |
| Session opened | no record | hook writes the item row (`transfer_session.go:214-217`), session record saved (:245-254) |
| Session create fails after the item exists | cleanup (`item_create.go:174`); item row stays | cleanup (`item_create.go:196`); **hook never runs**, because `openSessionForUpload` returns at `transfer_session.go:207-209` |
| Attempt ends | `if !created { e.clearPendingCreateIntent(ctx, action) }` (`executor_transfer.go:500-502`) | same; here `created` is still false |

The hook and the clear both use the action's context: `e.pendingCreates.RecordPendingCreate(ctx, pending)` (`executor_transfer.go:552`), store writes through `s.db.ExecContext(ctx, ...)` (`store_pending_creates.go:103`).

**The cleanup** (VERIFIED). Both sites do one read and one conditional delete, and log a failure at Debug only:

```go
// internal/graph/item_create.go:221-235
current, err := c.GetItem(cleanupCtx, driveID, created.ID)
...
if current.Size != 0 || current.ETag == "" {
	return
}
if err := c.DeleteItemIfMatch(cleanupCtx, driveID, created.ID, current.ETag); err != nil {
	c.logger.Debug("failed to remove the empty file a failed upload created", ...)
}
```

```go
// internal/driveops/transfer_session.go:458-463
removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelSessionTimeout)
defer cancel()
if err := remover.DeleteItemIfMatch(removeCtx, driveID, item.ID, item.ETag); err != nil {
	tm.logger.Debug("couldn't remove the empty file a failed upload created", ...)
}
```

The graph cleanup borrows an auth constant for its timeout: `shutdownTimeout = 5 * time.Second` is documented as the wait for the OAuth callback server (`internal/graph/auth.go:60-61`) and is used at `item_create.go:218`.

When `CreateUploadSessionForItem` itself fails on the client, the client has no session URL, so a session that landed on the server cannot be cancelled (`upload_transfer.go:322-325`).

**The "transfer-leftover cleanup" that was skipped is unrelated to the remote item.** VERIFIED: it deletes local `.partial` files and local session files (`engine_housekeeping.go:11-31`). It has only two production call sites, `engine_watch.go:326` and `engine_run_once.go:99`.

Why it ran during shutdown (VERIFIED except the last step):

1. `resume` restarts the mount's watch runner, so the upload was a bootstrap action (`orchestrator_watch.go:991-1000, 1181-1230`).
2. Cancelled workers exit (`worker.go:139-140`); when all have exited the completions channel is closed (`worker.go:111-114`).
3. `drainLoopDone` turns true (`engine_watch_drain.go:77-79`) and a bootstrap loop that ended by draining returns nil (`internal/sync/engine_watch_loop.go:63-74`).
4. `bootstrapSync` therefore goes on to `finishBootstrapAfterActions(ctx)` (`engine_watch.go:299-303`), which calls `rt.postSyncHousekeeping(ctx)` with the cancelled context (:326).
5. `refreshProtectedRootsFromStore(ctx)` fails with "context canceled" and `refreshProtectedRootsForHousekeeping` logs the WARN; it never looks at `ctx.Err()` (`engine_housekeeping.go:39-58`).
6. SUSPECTED: `armFullRefreshTimer(ctx)` (`engine_watch.go:327`) then fails on the same context and `RunWatch` maps that to a nil return through `isWatchShutdownError` (`engine_watch.go:88-91, 136-142`). I did not read `armFullRefreshTimer` (`internal/sync/engine_watch_maintenance.go:94`).

**What a later run does with the leftover.** Observation classifies each observed item against `pending_creates` (`store_pending_creates.go:224-254`, called from `internal/sync/store_write_observation.go:334-362`). There are two cases.

Case A — a record exists: an item row, or an intent younger than 72 h that claims the empty file at its path (`store_pending_creates.go:291-316`; `pendingCreateIntentTTL`, :57). This was the state after the 2026-11-10 incident. VERIFIED:

1. No baseline row yet, so the decision is `placeholderAdopt` (:244-245).
2. `adoptPlaceholderTx` writes a baseline row "as synced empty content" (:256-276): `LocalHash: item.Hash, RemoteHash: item.Hash, LocalSize: 0, RemoteSize: 0, ETag: item.ETag`.
3. The planner sees `local_changed = 1` (hash differs, `internal/sync/sqlite_compare.go:297-298`) and `remote_changed = 0` (hashes equal, :312-313), so `comparison_kind = 'diverged'` (:531) and `reconciliation_kind = 'upload'` (:643).
4. The action is `makeAction(ActionUpload, view)` (`internal/sync/planner_actions.go:394-398`) for a known item, executed as an overwrite by ID with `If-Match`.

Case B — no record: the item row was never written and the intent was cleared, expired, or the state database was reset. VERIFIED: no baseline, both sides present and different gives `'create_conflict'` (`sqlite_compare.go:526`), then `'conflict_create_create'` (:624), then a conflict copy plus a download (`planner_actions.go:419-426`).

| Mode | Case A: record exists | Case B: no record |
|---|---|---|
| Bidirectional | Content uploaded into the placeholder by ID. Never downloaded, never a conflict. | The user's file is renamed to a conflict copy and the empty file is downloaded in its place. |
| Upload-only | Same as bidirectional. | Nothing happens: download and conflict copy are filtered (`internal/sync/planner.go:28-29, 48-52`). The user's file is not delivered while the leftover exists. |
| Download-only | Upload is deferred (`planner.go:30-31`). The remote stays empty; the local file is not touched, because `remote_changed = 0` plans no download. | Same as bidirectional: conflict copy plus download of the empty file. |
| Local file deleted meanwhile | `'remote_delete'` (`sqlite_compare.go:635`): the placeholder is deleted by ID. Deferred in download-only (`planner.go:34-35`). | Ordinary remote-only file: downloaded. |

Case B is the failure the store's own header describes: "Seen by the next pass without memory of who made it, it was a create_conflict: the user's file was renamed to a conflict copy and the empty file downloaded in its place." (`store_pending_creates.go:16-18`).

Existing unit coverage of Case A (VERIFIED, `internal/sync/pending_creates_test.go`): `TestRunOnce_InterruptedCreateFinishesIntoItsOwnPlaceholder` (:224), `TestRunOnce_UploadOnlyCreateFinishesIntoItsPlaceholderAfterItsETagMoved` (:295), `TestRunOnce_PlaceholderOfACreateWhoseFileWasDeletedIsRemoved` (:328), `TestCommitObservation_IntentClaimsTheEmptyFileACrashLeftUnrecorded` (:441).

SUSPECTED: no code other than the observation commit and the 409 resolver (`executor_transfer.go:476-481`, `PendingCreateAt`) consumes `pending_creates`; in particular there is no sweep at process start. A grep for `pending_creates` and `PendingCreate` across `internal/sync` settles it.

**Three gaps in the life cycle.**

G1 — the large-file path can lose the record. Code order VERIFIED, end-to-end effect SUSPECTED. When `CreateFile` succeeds and the session create fails, graph returns `&CreatedItemError{Item: created, Err: err}` (`item_create.go:194-197`), `openSessionForUpload` returns before the hook (`transfer_session.go:206-209`), `created` stays false, and `uploadNewFile` clears the intent (`executor_transfer.go:500-502`). If the cleanup also failed (the 412 of this incident), the leftover is Case B. This contradicts R-5.6.15 ("an attempt that ends without creating anything clears its intent", `spec/requirements/transfers.md:44`) and the comment at `store_pending_creates.go:124-127` ("the attempt ended, by success or failure, without the create hook firing, so nothing on OneDrive is ours to recognize there"). On a cancelled context the clear itself probably fails, which keeps the intent by accident and logs WARN "Could not clear a pending create intent" (`executor_transfer.go:563`). What settles it: a driveops unit test whose `createUploadSessionFn` returns a `*graph.CreatedItemError`, asserting whether the hook ran. Also unread: the wrapper `persistedSessionUpload` around `persistedSessionUploadOnce` (`transfer_session.go:227-294`), so I have not proven that the `%w` chain reaches the executor intact.

G2 — a hashless placeholder whose eTag moves again after adoption. SQL consequence VERIFIED, live occurrence SUSPECTED. `remote_changed` compares hashes first; without hashes on both sides it falls through to mtime and eTag (`sqlite_compare.go:303-317`; the comment names "zero-byte files on Business"). An adopted placeholder is observed like any item (`store_pending_creates.go:250-252`). A new eTag then gives `remote_changed = 1` with `local_changed = 1`, which is `'conflict_edit_edit'` (:646): conflict copy plus download of the empty file. `TestCommitObservation_PlaceholderStateSeenAfterTheCommitIsNotARemoteChange` (`pending_creates_test.go:174-203`) does not cover this. What settles it: a store test with a hashless adopted placeholder and a second observation under a new eTag, and a Business live probe showing whether empty items carry a hash.

G3 — the record write can be lost to the cancel. VERIFIED: the hook writes on the action context (`executor_transfer.go:552`). A cancel between the 201 and the hook fails the write; the hook error then triggers cleanup (`item_create.go:167`). The intent row still covers the leftover for 72 h, so this is a narrowing, not a loss.

### 1c. Why two requests

VERIFIED from the code comment `item_create.go:18-44`, R-5.6.14 (`spec/requirements/transfers.md:43`) and `graph-api-quirks.md:1141-1143`:

- "The name never travels in a URL. Path routes (items/{parent}:/{name}:/...) hand the name to Graph's router: a file named `v1.0` uploaded that way created a folder `_api` holding a file `v2.0` (live probe 4), and a path route caps names near 291 characters through the URL length limit, where the body allows OneDrive's own 400 (L-05)" (`item_create.go:18-44`).
- `conflictBehavior=fail`, not `replace`: "replace on create silently overwrote whatever already held the name, including a variant OneDrive treats as the same name".
- `fail` makes the create safe to resend, so `CreateFile` marks its request `retry.WithReplaySafe`.
- Cost measured by probe L-01: the create plus a session on the item commit one version in total.
- One request by path would not avoid the placeholder: "Path-Routed Upload Sessions Create A Visible Placeholder (L-34, L-35) ... any interrupted session upload, not only the create-by-body path, can leave an empty item behind" (`graph-api-quirks.md:1141-1143`).

History (`git log -- internal/graph/item_create.go`): `88f6d317` 2026-10-27 (#804, create by body and own placeholders), `2891ad55` 2026-10-30 (#818, placeholder recognised after its eTag moved), `1e07455a` 2026-10-30 (#820, create resent after a transient failure).

Recommendation: keep the two requests.

### 1d. The fix

**(1) and (2): retry the conditional delete on 412. Recommended, as one shared method.**

R-5.6.14 already requires the delete to be "conditional on the eTag just read". A 412 means the eTag just read is stale, so the same rule calls for another read.

End state:

```go
// internal/graph/item_create.go
const (
	placeholderCleanupTimeout  = 10 * time.Second
	placeholderCleanupAttempts = 3
)

// RemoveEmptyItem deletes itemID while it is still an empty file. Each attempt
// reads the item and deletes it conditional on the eTag just read; a 412 is
// answered by reading again, at most placeholderCleanupAttempts times.
// removed is true when the item is gone (deleted here, or already 404).
func (c *Client) RemoveEmptyItem(ctx context.Context, driveID driveid.ID, itemID string) (removed bool, err error)
```

Loop body: `GetItem`; `ErrNotFound` returns `true, nil`; any other read error returns it; `Size != 0` or empty eTag returns `false, nil`; `DeleteItemIfMatch`; nil or `ErrNotFound` returns `true, nil`; `ErrPreconditionFailed` continues; any other error returns it. After the last attempt: `false, fmt.Errorf("removing empty item %s: %w", itemID, err)`.

- `removeCreatedPlaceholder` (`item_create.go:213-239`) keeps its guard (:214-216) and its detached context, switches the timeout to `placeholderCleanupTimeout`, and calls `RemoveEmptyItem`. Lines 221-235 are deleted. One Debug line when the item remains.
- `driveops.PlaceholderRemover` (`internal/driveops/interfaces.go:50-52`) changes from `DeleteItemIfMatch` to `RemoveEmptyItem`.
- `removePlaceholder` (`transfer_session.go:426-444`) and `removeEmptyPlaceholder` (:452-467) collapse into one function that detaches the context once and calls `RemoveEmptyItem(ctx, driveID, itemID)`. The separate `lookupUploadTarget` read (:431-443) and the single-shot delete (:458-463) are deleted.
- `settleUncertainFinalFragment` (`transfer_verify.go:159-161`) passes `landed.ID`. This costs one extra GET and adds the re-read that guards against content landing in between.

Tests first:

- Graph, next to `TestUpload_FailedContentStepRemovesItsEmptyItemByItsCurrentETag` (`internal/graph/item_create_test.go:166-194`), on the extended fake of Item 4:
  - first DELETE 412, second read returns a newer eTag, second DELETE 204: asserts `rec.deleteIfMatch` holds both eTags in order;
  - second read shows size 7: asserts exactly one DELETE;
  - 412 every time: asserts exactly three DELETEs and that `UploadCreating` still returns `*CreatedItemError`;
  - second read answers 404: asserts no further DELETE.
- Driveops: `tmCreatingUploader` (`internal/driveops/transfer_create_test.go:22-52`) replaces `DeleteItemIfMatch`/`deletedIfMatch` with `RemoveEmptyItem`/`removedEmpty []string`. `TestUploadFile_UncertainFinalFragmentThatDidNotLandRemovesThePlaceholder` (:83) and `..._RemovesThePlaceholderByItsCurrentETag` (:118) assert the item ID instead of `id@etag`.

Risks:

- Shutdown can take up to 10 s longer in the worst case. The incident's read plus delete took about 0.6 s, so three rounds are about 2 s in practice.
- SUSPECTED: a delete with the current eTag succeeds while an orphan session is open on the item. The existing path deletes only after cancelling its session. Settle with the live probe named in 1.0, extended by a delete.

**(3) Should shutdown let an in-flight upload finish? Not recommended.**

- The leftover is recoverable in every mode once the record is reliable (Case A).
- A grace period for small creates contradicts "No timer escalates the first signal" (`sync-control-plane.md:601-602`), R-6.3.6 and repo `CLAUDE.md:187`. It needs an owner decision on the requirement, not a code change.
- Instead, align the three "drain" texts with R-6.3.6: repo `CLAUDE.md:186`, `sync-engine.md:567`, and the log text at `engine_watch.go:237` (for example "graceful shutdown: waiting for cancelled actions to stop").

**(4) Make the created-item record survive the cancel. Recommended.**

- Hook write detached. In `recordPendingCreate` (`executor_transfer.go:537-554`):

  ```go
  recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeCommitTimeout)
  defer cancel()
  return e.pendingCreates.RecordPendingCreate(recordCtx, pending)
  ```

  This follows `commitOutcome` (`worker.go:240-245`).
- Large path calls the hook for an item it learns from an error. In `openSessionForUpload` (`transfer_session.go:206-209`), before returning the wrapped error: when `errors.As(err, &createdErr)` holds, `createdErr.Item.ID != ""`, `up.onCreated != nil` and `up.createdItem == nil`, call `up.onCreated(createdErr.Item.ID, createdErr.Item.ETag)` and log a hook failure at Debug. Do not set `up.createdItem`: the cleanup may have removed the item, and a retry by ID would then meet 404.
- `clearPendingCreateIntent` (`executor_transfer.go:558-565`): when `ctx.Err() != nil`, skip the write and log at Debug. The intent expiring is the documented fallback (:556-557).
- Rewrite the comment at `store_pending_creates.go:124-127` to state the real precondition: the hook did not fire *and* no created item was reported.

Open point, SUSPECTED: an item row for a placeholder that the cleanup then removed may linger if no tombstone is ever observed for it. The small path has the same property today (hook at `item_create.go:166`, cleanup at :174). I did not read where a row is cleared after a successful upload. Read `ClearPendingCreate` callers (`store_pending_creates.go:120-122`) before implementing.

Tests first:

- Driveops, new test in `transfer_create_test.go` beside `TestSessionUpload_RetryOfAnInterruptedCreateReusesItsItem` (:159-192), using `tmItemSessionUploader`, `NewSessionStore`, `sessionTestFile` and `newTestTM`: `createUploadSessionFn` (`internal/driveops/transfer_manager_test.go:60-68`) returns `&graph.CreatedItemError{...}`; assert `OnCreated` received the item ID and creation eTag exactly once and `UploadFile` still fails.
- Sync, in `pending_creates_test.go` beside `TestPendingCreateIntent_ClearedWhenNothingWasCreatedAndKeepsAKnownItem` (:510-533): a cancelled action context still leaves the item row; a cancelled context on the clear path produces no WARN record.
- Sync, G2: `TestCommitObservation_AdoptedHashlessPlaceholderWithMovedETagIsNotAConflict` beside :268-287. Write it first; if it fails, the fix is to treat an adopted, still-empty placeholder as unchanged remotely whatever its eTag. That fix needs its own design note in `spec/design/sync-store.md`.

**Quiet shutdown. Recommended.**

In `refreshProtectedRootsForHousekeeping` (`engine_housekeeping.go:39-58`), before either `Swap`:

```go
if err != nil && ctx.Err() != nil {
	e.logger.Debug("skipping transfer cleanup: shutting down", slog.Any("error", err))
	return false
}
```

The failure-streak flag stays untouched, so a later real failure still warns once (R-6.6.30).

Test first: `TestPostSyncHousekeeping_CanceledContextIsQuiet` in `internal/sync/engine_housekeeping_test.go`, built like `TestPostSyncHousekeeping_SkipsTransferCleanupWhenProtectedRootsCannotBeRead` (:142-191) with `newDebugLogBuffer` (:117) and a working `newTestStore(t)`. Assert: no `level=WARN`, the partial file still exists, and a following call on a live context removes it without logging "transfer cleanup resumes".

SUSPECTED: the one-shot path reaches `postSyncHousekeeping` on a cancelled context too (`engine_run_once.go:98-100`). I did not trace `RunOnce` from the cancel to :99. The proposed check covers it either way.

**Spec and docs to update with these changes.**

| File | Change |
|---|---|
| `spec/requirements/transfers.md:43` (R-5.6.14) | add: the read-back and conditional delete are repeated while the delete answers 412 and the item is still empty, at most three times |
| `spec/requirements/transfers.md:44` (R-5.6.15) | add: an attempt that reports a created item through its error records that item; the intent is cleared only when no item was created; the record is written on a context detached from shutdown |
| `spec/requirements/sync.md:92` (R-2.8.3) | replace "follow the normal shutdown path" with the R-6.3.6 wording |
| `spec/design/drive-transfers.md:279-311` | describe `RemoveEmptyItem`, its bound, and the large-path hook call |
| `spec/design/sync-execution.md:168-181` | "an attempt whose hook never ran clears the intent" becomes "an attempt that created nothing" |
| `spec/design/sync-engine.md:567, 1322-1324` | "graceful drain" wording; add housekeeping and the intent clear to the list of quiet shutdown paths |
| repo `CLAUDE.md:186` | "drain in-flight operations" becomes cancel and unwind |
| `spec/reference/graph-api-quirks.md:1137-1139` | add: a session create cancelled by the client can still land and move the eTag after the cleanup read (mark as unconfirmed until probed) |
| `spec/reference/live-incidents.md` | new entry `LI-20261110-01` (index at :41-86, newest is `LI-20261029-01`): area sync/e2e, test `TestE2E_SyncWatch_PauseResume`, classification test bug plus product hardening; record the 2026-10-30 `TestE2E_SyncWatch_FileModification` occurrence in the same entry, since no entry exists for it |
| `spec/reference/live-recurring-families.md` | new family: "Name-only wait stops the daemon mid-create and leaves an empty placeholder" |

**PR split for Item 1:** PR-D (cleanup loop), PR-E (record durability), PR-F (quiet shutdown and wording). See the breakdown at the end.

---

## Item 2 — e2e waits that check names, not content

Full per-line tables: `01d-e2e-name-only-waits.md`. All 18 racy rows below were also read by me (VERIFIED). The 62 safe rows are VERIFIED by the sub-agent; I read the ones marked in the note under the count table.

### Helpers involved (VERIFIED)

- Name-only: `waitForRemoteReadContains` — ready when `err == nil && strings.Contains(stdout, expected)` (`e2e/remote_read_helpers_test.go:185-209`); `waitForRemoteExactStatVisible` (:247-269); `waitForRemoteParentListingContains` (:273-292); `waitForRemoteFixtureSeedVisible` (:325-391); `pollForDrive2File` (`e2e/sync_watch_e2e_test.go:384-406`).
- Size: `waitForRemoteStatSize` reads `stat --json` and compares `size` (`remote_read_helpers_test.go:214-243`). It hard-codes the default drive (`""` at :228).
- Local content: `pollLocalFileContent` (`e2e/sync_e2e_test.go:722-748`).

A size wait is sufficient for a *new* file: the item is 0 bytes until the final fragment commits. It is not sufficient for a modification to the same length.

### The 18 racy waits

Only six e2e files start a long-lived process (`cmd.Start`): `sync_watch_e2e_test.go`, `sync_watch_full_test.go`, `orchestrator_e2e_test.go`, `socketio_e2e_test.go`, `sync_shortcut_full_e2e_test.go`, `sync_kill_mid_transfer_full_e2e_test.go`.

"SS" is `waitForRemoteStatSize(t, <cfg>, <env>, <path>, int64(len(<content>)), <timeout>)`. "What follows" is the first thing the test does after the wait.

| # | File:line | Test | What follows | Replacement |
|---|---|---|---|---|
| 1 | `e2e/sync_watch_e2e_test.go:103` | `TestE2E_SyncWatch_BasicRoundTrip` | SIGTERM :106, content assert :120-121 | SS with `"created during watch mode\n"` |
| 2 | `e2e/sync_watch_e2e_test.go:261` | `TestE2E_SyncWatch_PauseResume` | SIGTERM :264, content assert :268-269 | SS with `"created while paused\n"` |
| 3 | `e2e/sync_watch_e2e_test.go:348` | `TestE2E_SyncWatch_ControlSocketReload` | reload :353-358, later SIGTERM :374, content assert :378-379 | SS with `"before reload\n"` |
| 4 | `e2e/sync_watch_e2e_test.go:371` | same (second drive) | SIGTERM :374 | per-drive size wait, `"after reload\n"` |
| 5 | `e2e/sync_watch_full_test.go:142` | `TestE2E_SyncWatch_Bidirectional` | remote put :145, SIGTERM :151 | SS with `"from local"` |
| 6 | `e2e/sync_watch_full_test.go:185` | `TestE2E_SyncWatch_ConflictDuringWatch` | remote put to the same path :190 | SS with `"baseline"` |
| 7 | `e2e/sync_watch_full_test.go:254` | `TestE2E_SyncWatch_FileModification` | local modify :257 | SS with `"version 1"` |
| 8 | `e2e/sync_watch_full_test.go:310` | `TestE2E_SyncWatch_FileDeletion` | `os.Remove` :313 | SS with `"will be deleted"` |
| 9 | `e2e/sync_watch_full_test.go:347` | `TestE2E_SyncWatch_FolderCreation` | SIGTERM :350 | SS on `/a/b/c/deep.txt`, `"deeply nested content"` |
| 10 | `e2e/sync_watch_full_test.go:384` | `TestE2E_SyncWatch_MultipleFiles` (loop of 5) | SIGTERM :388 | SS per file, `fmt.Sprintf("multi content %d", i)` |
| 11 | `e2e/sync_watch_full_test.go:508` | `TestE2E_SyncWatch_GracefulShutdown` | SIGTERM :511, then a one-shot sync asserting "Uploaded " :525-528 | SS with `"synced by daemon"` |
| 12 | `e2e/sync_watch_full_test.go:583` | `TestE2E_SyncWatch_TimedPauseExpiry` | SIGTERM :586 | SS with `"created during pause"` |
| 13 | `e2e/orchestrator_e2e_test.go:304` | `TestE2E_Orchestrator_WatchSimultaneous` | drive-2 wait :307, SIGTERM :310 | SS with `"watch drive1"` |
| 14 | `e2e/orchestrator_e2e_test.go:307` | same (second drive) | SIGTERM :310 | per-drive size wait, `"watch drive2"` |
| 15 | `e2e/orchestrator_e2e_test.go:359` | `TestE2E_Orchestrator_WatchDriveIsolation` | SIGTERM :368 | SS with `"drive1 only"` |
| 16 | `e2e/orchestrator_e2e_test.go:448` | `TestE2E_Orchestrator_WatchPausedDrive` | SIGTERM :469 | SS with `"active drive"` |
| 17 | `e2e/socketio_e2e_test.go:84` | `TestE2E_SyncWatch_WebsocketDisabledLongPollRegression` (**fast tier**, R-2.8.5) | SIGTERM :86 | SS with `"websocket disabled regression\n"` |
| 18 | `e2e/sync_shortcut_full_e2e_test.go:271` | `TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget` | test ends; cleanup stops the daemon :381 | per-drive size wait, `"watch local shortcut upload\n"` |

Grouping: rows 1-3 stop and then assert content (the 2026-11-10 shape). Rows 6, 7, 8, 11 act on the assumption that the upload finished; row 7 is the test whose 2026-10-30 run produced the eTag quirk. The other 11 stop the daemon and assert no content, so they pass today while leaving placeholders on the test drive.

### End state

In `e2e/remote_read_helpers_test.go` (already used by the fast tier, see row 17):

```go
func waitForRemoteStatSizeForDrive(
	t *testing.T, cfgPath string, env map[string]string,
	driveID, remotePath string, size int64, timeout time.Duration,
)

func waitForRemoteFileContent(
	t *testing.T, cfgPath string, env map[string]string,
	driveID, remotePath, want string, timeout time.Duration,
)
```

- `waitForRemoteStatSize` keeps its signature and delegates with `driveID == ""`.
- `waitForRemoteFileContent` polls `get` and compares the bytes. Build it from `eventuallySharedContentEquals` (`e2e/shared_items_helpers_test.go:1433-1459`). It replaces the two inline loops at `e2e/sync_watch_full_test.go:260-273` and :463-476.
- Rows 4, 14 and 18 use the per-drive variant. `pollForDrive2File` then has no caller among the sites listed in `01d` and is deleted. SUSPECTED that rows 4 and 14 are its only callers; grep before deleting.

Tests first: none new. These are test helpers; the proof is the 18 call sites. Row 2 must be confirmed by a `workflow_dispatch` run of the full tier.

Optional strengthening among the safe sites (`01d` §A2): `e2e/sync_full_test.go:593, 647` (a name wait on a file that existed before the modification proves nothing), `e2e/cli_commands_e2e_test.go:606-607, 625-626` (content assert after `--force` over an existing file, with no wait at all).

Count per file (racy / safe): `sync_watch_e2e` 4/0; `sync_watch_full` 8/3; `orchestrator_e2e` 4/3; `socketio_e2e` 1/0; `sync_shortcut_full_e2e` 1/3; `sync_e2e` 0/1; `sync_full` 0/15; `sync_hardening_full_e2e` 0/7; `sync_demoted_full_e2e` 0/1; `sync_safety_full_e2e` 0/1; `cli_commands_e2e` 0/11; `edge_cases_e2e` 0/4; `sync_recovery_e2e` 0/3; `sync_edge_cases_full` 0/3; `fileops_full_e2e` 0/5; `fileops_smoke_e2e` 0/1; `drive_list_full` 0/1. Total 18 / 62. Safe rows I read myself: `sync_watch_full_test.go:460, 531, 532`; `orchestrator_e2e_test.go:70`; `sync_shortcut_full_e2e_test.go:190, 244, 458`; `sync_e2e_test.go:676`; `sync_full_test.go:281`.

### Local-side waits

VERIFIED: a download never exposes a partial file at the final path. `publishPartial` (`internal/driveops/transfer_download.go:323-370`) works on `.onedrive-go.<name>~v<tag>.partial` in the target's directory:

1. bytes are written and the hash verified (`downloadToVerifiedPartial`, :410-505);
2. mtime is set on the partial (:328);
3. mode, fsync and close (`finalizePartial`, :336);
4. the caller's last precondition (:340);
5. atomic rename (:347) and directory sync (:355).

Nothing is written to the final path after the rename (`internal/sync/executor_transfer.go:105, 129-168`).

Consequence: "exists, then read" is safe for a new file. For a modified file, existence proves nothing, because the old content stays until the rename.

Result (`01d` §B2): every polling wait on a file a live daemon downloads compares content — for example `e2e/sync_watch_full_test.go:106, 148, 199-216` and `e2e/sync_shortcut_full_e2e_test.go:300-303` (read by me). Three existence-only waits follow a completed one-shot command (`e2e/sync_shortcut_full_e2e_test.go:325`, `e2e/sync_hardening_full_e2e_test.go:141-143`, `e2e/shared_items_helpers_test.go:473`). **No local wait is racy. No change proposed.**

---

## Item 3 — single-pass sync calls in the e2e suite

Full per-line tables with reasons: `01c-e2e-single-pass-syncs.md`. Rows I read myself are named under the table; the rest are VERIFIED by the sub-agent.

**Count: 104 call sites** run a one-shot `sync` exactly once, require exit 0, and are not behind a converge helper. **66 should converge, 37 must stay single-pass (17 of these are the idempotence wrapper `assertSyncLeavesLocalTreeStable`), 1 is unclear.** Two are in the fast tier: `e2e/sync_e2e_test.go:642` and `e2e/sync_shortcut_e2e_test.go:23`.

### Helper behaviour that decides the verdicts (VERIFIED, `e2e/e2e_test.go`)

- `runSyncThroughTransientFailures` (:571-610): returns the first pass when it logged no `sync retry work`; otherwise re-runs while stdout matches `\d+ failed|will be retried on the next sync` (:555). Every pass must exit 0. It does nothing for delta lag.
- `requireSyncEventuallyConverges` (:491-551): repeats until a caller predicate holds. This is the one for delta lag.
- Both run on the default drive only.

A site must stay single-pass when it asserts the first pass's own output (for example "Uploaded ...", "Up to date", a conflict line), is a `--dry-run`, or is the idempotence check itself. A pass meant to end in a permanent per-item failure must not be wrapped in `runSyncThroughTransientFailures`.

### Per file

"C" lines should converge; "SP" lines stay single-pass.

| File | C | SP | Lines |
|---|---|---|---|
| `sync_e2e_test.go` | 0 | 1 | SP 642 (asserts "Uploaded ..." :644) |
| `sync_full_test.go` | 19 | 8 | C 278, 294, 336, 340, 408, 412, 549, 591, 615, 699, 751, 752, 885, 906, 949, 975, 1007, 1011, 1065; SP 316, 487, 705, 836, 932, 957, 1024, 1040 |
| `sync_demoted_full_e2e_test.go` | 4 | 3 | C 46, 68, 121, 135; SP 27, 73, 80 |
| `sync_edge_cases_full_test.go` | 5 | 2 | C 34, 42, 112, 171, 207; SP 180, 211 |
| `sync_recovery_e2e_test.go` | 6 | 4 | C 51, 121, 138, 168, 173, 266; SP 56, 141, 240, 284 |
| `sync_kill_mid_transfer_full_e2e_test.go` | 0 | 3 | SP 88, 96, 140 |
| `sync_safety_full_e2e_test.go` | 2 | 1 | C 37, 109; SP 114 |
| `sync_hardening_full_e2e_test.go` | 6 | 8 | C 267, 276, 284, 323, 328, 335; SP 137, 150, 177, 247, 248, 295, 296, 375 |
| `sync_shortcut_full_e2e_test.go` | 9 | 2 | C 30, 72, 91, 130, 173, 187, 201, 255, 282 (all per-drive); SP 74, 208 |
| `sync_shortcut_e2e_test.go` | 1 | 0 | C 23 |
| `sync_shared_e2e_test.go` | 0 | 1 | SP 116 |
| `cli_commands_e2e_test.go` | 8 | 0 | C 42, 191, 216, 243, 275, 300, 745, 758 (all `--upload-only`) |
| `drive_list_full_test.go` | 1 | 0 | C 126 |
| `output_validation_e2e_test.go` | 1 | 2 | C 40; SP 108, 155 |
| `orchestrator_e2e_test.go` | 3 | 0 | C 66, 120 (all-drives), 230 (per-drive) |
| `shared_items_e2e_test.go` | 1 | 1 | C 195; SP 202; unclear 212 |
| `sync_watch_full_test.go` | 0 | 1 | SP 525 (asserts "Uploaded " :528; depends on Item 2 row 11) |
| **Total** | **66** | **37** | plus 1 unclear = 104 |

Read by me: `sync_full_test.go:278, 294`; `orchestrator_e2e_test.go:66, 120`; `cli_commands_e2e_test.go:191`; `sync_shortcut_full_e2e_test.go:173, 187, 201, 208, 255, 282`; `sync_watch_full_test.go:525`.

Strongest flake candidates:

- `e2e/sync_full_test.go:294` — a full sync straight after `mkdirRemoteFolder` (:290) and `putRemoteFile` (:291), then an unpolled read of the downloaded file (:302-303). Delta lag fails it. Lines 906 and 975 have the same shape.
- `e2e/orchestrator_e2e_test.go:120` — `sync --download-only` straight after a remote put (:115), then `os.Stat` plus `require.NoError` (:124-125).
- `e2e/orchestrator_e2e_test.go:66` — all-drives upload, then an unpolled `stat` on the second drive (:74).
- `e2e/cli_commands_e2e_test.go:191` — one `--upload-only` pass, then `Zero(status.Retrying)` (:193-196). This is the recurring family "Upload-Only Live Sync Convergence Can Leave Retryable Work On First Pass" (`spec/reference/live-recurring-families.md`, `LI-20260422-05`, last seen 2026-11-09). Line 758 is the same.
- `e2e/sync_hardening_full_e2e_test.go:328`, `e2e/sync_recovery_e2e_test.go:138`.

### End state

In `e2e/e2e_test.go`, parameterise the two helpers by a runner and add the missing variants. Existing names keep their signatures and delegate.

```go
type syncRunner func(args ...string) (stdout, stderr string, err error)

func requireSyncEventuallyConvergesForDrive(t *testing.T, cfgPath string, env map[string]string,
	driveID string, timeout time.Duration, description string,
	ready func(syncAttemptResult) bool, args ...string) syncAttemptResult
func requireSyncEventuallyConvergesAllDrives(/* same without driveID */) syncAttemptResult
func runSyncThroughTransientFailuresForDrive(t *testing.T, cfgPath string, env map[string]string,
	driveID string, args ...string) string
func runSyncThroughTransientFailuresAllDrives(t *testing.T, cfgPath string, env map[string]string,
	args ...string) string
```

Runners already exist: `runCLIWithConfigAllowErrorForDrive` (`e2e/cli_runner_test.go:127`), `runCLIWithConfigAllDrivesAllowError` (:171).

Related findings in `01c`:

- 10 single runs tolerate or expect an error; one of them (`e2e/sync_hardening_full_e2e_test.go:235`) is followed by unpolled content assertions (:236-245).
- 4 hand-rolled retry loops. Three use `require.Eventually` around a runner that requires success (`e2e/sync_safety_full_e2e_test.go:137`, `e2e/sync_shared_e2e_test.go:79, 111`), so one transient non-zero exit inside the loop still fails the test. Convert them to the helper.
- `shared_items_e2e_test.go:212` is unclear: the sub-agent could not show whether the expected 403 reaches an action result.

Risk: a converge helper hides a product regression where a pass that should finish in one go needs two. The 37 single-pass sites, and the 17 idempotence checks among them, are what keeps that visible. Do not convert them.

---

## Item 4 — the fake Graph

### Inventory (VERIFIED)

There is no shared fake-Graph package. Five layers exist:

| Layer | Where | Level | Fault model |
|---|---|---|---|
| A. Per-test HTTP handlers | `internal/graph/test_helpers_test.go:17-49` (`newGraphServer`), :71-78 (`newGraphErrorServer`); `internal/graph/upload_test.go:806-889` (`sessionUploadRecorder`, `newSessionUploadServer`) | HTTP | scripted: one status per server, or one status for the session create (`sessionStatus`, :856-865); read-back eTag and size fixed before the test (:822-823) |
| B. In-memory OneDrive | `internal/sync/scenario_remote_test.go:23-88` (`moveScenarioRemote`), wired into `engineMockClient` at :475-521 | Go method | behavioural: 409 on a taken name (:570-572), 412 on a stale `If-Match` (:576-578, :651-664, :738-757), 400 on a move into its own subtree (:599-601); knobs `interruptCreates` and `churnInterruptedCreates` (:79-84, :718-725) |
| C. Chaos layer (D-092) | `internal/sync/chaos_remote_test.go:27-46, 58-78`, wraps B (`wrap`, :217-282) | Go method | seeded random, transient only: 429 and 503 with Retry-After, 500, 502, 504, 408, connection reset (:172-194); lost response after the mutation took effect (:198-205, :296-307); upload-session timeout after the empty item exists (:311-334); truncated download (:337-361); delta 410, failed page, split pages (:365-413); a concurrent writer (:163). Run by `TestModel_ChaosGraphConverges` (:476-492) |
| D. CLI drive | `internal/cli/json_contract_fake_graph_test.go:21-35` (`fakeGraphDrive`) | HTTP | two knobs: `deleteRefused` (403 on every DELETE, :30-31) and `hangRenameTo` (:32-34); fails the test on an unknown endpoint |
| E. Driveops mocks | `internal/driveops/transfer_manager_test.go:60-68` (`tmMockUploader`), `internal/driveops/transfer_create_test.go:22-52` (`tmCreatingUploader`) | Go method | function fields per call; `DeleteItemIfMatch` always returns nil (:49-52) |

Non-Graph seams, for completeness: SQL fault driver (`internal/sync/store_fault_seam_test.go`), crash points (`internal/sync/crash_points.go:3-6`, D-091), filesystem shim (`internal/synctest/fsshim.go`, D-094).

Two structural limits (VERIFIED):

- In layer B a create is one method call covering create, hook and content (`uploadCreating`, `scenario_remote_test.go:691-735`). Graph's own `removeCreatedPlaceholder` never runs there. An interrupted create always leaves the placeholder, so a failed cleanup cannot be told apart from a successful one.
- `engineMockClient` does not implement `SessionUploader` or `ItemSessionUploader` (`internal/sync/engine_test_helpers_test.go:47-58`). The path for files above 4 MiB is never exercised by scenario, chaos or model tests. Gap G1 lives exactly there.

### Adding the three live faults

**1. HTTP 400 on child create.** Live evidence: a 373-character path refused by OneDrive and retried for nine hours (`~/agent/CHANGELOG.md:217`, also :162). There is no entry for it in `live-incidents.md` or `graph-api-quirks.md`; add one.

- Layer A: add `createStatus int` to `sessionUploadRecorder`; when non-zero the `POST .../children` branch (`upload_test.go:842-845`) answers that status with an `invalidRequest` body. Test `TestUpload_RefusedCreateSendsNoContentAndDeletesNothing` in `item_create_test.go`: `UploadCreating` returns `ErrBadRequest`, the hook is not called, no DELETE and no session request is recorded.
- Layer B: add `refuseCreate func(parentID, name string) error` to `moveScenarioRemote`, consulted at the top of `uploadCreating`, `upload` and `createFolder` before `addFile`. Tests return `&graph.GraphError{StatusCode: 400, Code: "invalidRequest", Err: graph.ErrBadRequest}` with and without the path-limit message.
- Engine test in `pending_creates_test.go`: the refusal is classified by `classifyServerAnswer` (`internal/sync/domain_graph_error.go:135-140` for the limit messages, :166-167 otherwise), waits on the durable curve (R-2.10.71, `spec/requirements/sync.md:187`), and the intent row is cleared.
- Keep it out of the default chaos storm. The chaos contract is "every fault is transient" (`chaos_remote_test.go:27-46`); a permanent refusal breaks the convergence assertion.

**2. HTTP 412 on the conditional delete of a placeholder.**

- Layer A: add `deleteStatuses []int` (one consumed per DELETE, default 204) and `readBacks []struct{ etag string; size int }` (one consumed per GET, falling back to `currentETag`/`currentSize`). The DELETE branch is `upload_test.go:846-848`, the GET branch :849-855. These carry the four tests listed in 1d.
- Layer E: `tmCreatingUploader` gets `removeResults []error` for the new `RemoveEmptyItem`.
- Layer B: not expressible, and not needed. The end state "placeholder left behind" is what `interruptCreates` already produces.

**3. A session create cancelled by the client that lands on the server.**

- Layer A: add `sessionLandsThenDrops bool`. On `createUploadSession` (:856-868) the handler sets `currentETag` to a new value and then closes the connection without a response (`http.Hijacker`). The first cleanup GET is scripted through `readBacks` to still return the creation eTag, and the first DELETE answers 412. This reproduces the 2026-11-10 sequence in a unit test.
- Layer C: add a variant of `upload-session:timeout-after-create` (`chaos_remote_test.go:311-334`) that also moves the placeholder's eTag. It needs a callback `churnPlaceholder func(itemID string)` on `chaosGraph`, set by the model test the way `concurrentWrite` is set (`internal/sync/engine_model_based_test.go:307`). This puts the eTag-moved case, today reachable only through the hand-set knob `churnInterruptedCreates`, into the seeded storm.

### Coverage of the live families

Recurring families from `spec/reference/live-recurring-families.md` (six) and the incident families I traced.

| Family | Injectable today | Evidence |
|---|---|---|
| Transient 429/5xx/408/reset before a call | yes, layer C | `chaos_remote_test.go:172-194` |
| Response lost after a mutation took effect | yes, layer C | :198-205, :296-307 |
| Interrupted create leaves a placeholder; eTag moved | yes, layer B knobs; eTag move not in the storm | `scenario_remote_test.go:79-84` |
| Delta 410, failed page, split pages with repeats | yes, layer C | `chaos_remote_test.go:365-413` |
| Another client writes between two calls; 409; 412 through `If-Match` | yes, layers B and C | `scenario_remote_test.go:570-578`; `chaos_remote_test.go:163` |
| Upload-only first pass leaves retry work (`LI-20260422-05`) | partly: transient faults do produce retry work under layer C. SUSPECTED that no storm runs in upload-only mode; read `engine_model_based_test.go:462-527` to settle | — |
| **400 on child create** | **no** | create branches return only 409: `scenario_remote_test.go:638-649, 669-735`; `upload_test.go:842-845` always succeeds |
| **412 on placeholder cleanup** | **no** | `upload_test.go:846-848` always 204; `transfer_create_test.go:49-52` always nil |
| **Cancelled session create that lands** | **no** | read-back values are fixed before the test, `upload_test.go:822-823` |
| Mount-root children-route lag (`LI-20260405-09`, `LI-20260422-02`) | no at engine level: layer B is read-your-writes and path convergence is a stub (`engine_test_helpers_test.go:296-308`). SUSPECTED covered at graph level by retry-policy tests on the quirk at `item_create.go:73-79`, which I did not read | — |
| Post-mutation destination-path visibility lag (`LI-20260405-07`, `LI-20260410-01`) | no at engine level, same reason | — |
| Strict auth preflight projection lag (`LI-20260405-06`) | SUSPECTED no; CLI tests with their own servers were not read | — |
| Mtime PATCH transient after a simple upload (`LI-20260408-01`) | no at engine level: an upload is one call (`uploadFn`). SUSPECTED at graph level: the shared server only counts PATCH requests (`upload_test.go:878`); other handlers in that file were not read | — |
| Shared-folder search visibility in `drive list` (`LI-20260422-04`) | SUSPECTED no: layer D knows only file and recycle-bin endpoints (`json_contract_fake_graph_test.go:21-35`); other CLI test servers were not read | — |
| Create answered success but the item is not visible (`live-incidents.md:1408-1417`) | no at engine level (read-your-writes) | — |
| Large-file session create path | no in layers B and C | `engine_test_helpers_test.go:47-58` |
| Hashless zero-byte items (Business) | SUSPECTED no; I did not read how layer B assigns hashes (`addFile`) | — |
| 401 mid-upload, revoked sign-in (proxy round of 2026-11-03, `~/agent/CHANGELOG.md:169`) | not in layer C's fault list (:172-194). SUSPECTED absent elsewhere in `internal/sync` tests | — |
| Retry-After of 120 s and 86,400 s (same round) | no in layer C: capped at `chaosMaxRetryAfter = 30s` (:48-56) | — |

Proposed order of additions: the three faults above first (they have a live failure behind them), then a visibility-lag knob for layer B (`invisibleFor map[string]int`, counted in calls, consulted by `getItemByPathFn` and `listChildrenFn`), then `SessionUploader` on `engineMockClient` so layer B covers large files.

---

## Item 5 — `status --perf` reads the wrong collector

### Wiring (VERIFIED)

Three collector levels, child to parent:

1. Per mount: `Runtime.RegisterMount` creates `NewCollector(r.collector)` (`internal/perf/runtime.go:34-46`). Called at `internal/multisync/orchestrator_watch.go:402` and `internal/multisync/orchestrator_run_once.go:217`, passed to the engine as `PerfCollector`.
2. Runtime: `perf.NewRuntime(cfg.PerfParent)` (`internal/multisync/orchestrator.go:170`), with `PerfParent: perf.FromContext(ctx)` (`internal/cli/sync_runtime.go:223`).
3. Command: `perf.NewSession(...)` creates the collector and attaches it to the context (`internal/perf/logging.go:36, 52`), from `internal/cli/root.go:744-761`. This is the only production call of `perf.WithCollector`.

`Collector.apply` propagates from child to parent only (`internal/perf/collector.go:337-351`):

```go
if c.parent != nil {
	c.parent.apply(update)
}
```

Who records where:

| Metric family | Recorded on | Evidence |
|---|---|---|
| Engine phases (observe, plan, execute, refresh, watch batches) | the mount collector, through `e.collector()` | `internal/sync/engine.go:409-415`; `internal/sync/engine_run_once.go:273-276`; `internal/sync/worker.go:267-269, 346-348` |
| HTTP requests | `perf.FromContext(req.Context())` | `internal/perf/http.go:25-27` |
| HTTP retries | `perf.FromContext(req.Context())` | `internal/retry/transport.go:210-212, 278-280` |
| DB transactions | `perf.FromContext(ctx)` | `internal/sync/tx.go:38, 96` |
| Transfers | `perf.FromContext(ctx)` | `internal/driveops/transfer_upload.go:213-218, 401-402`; `internal/driveops/transfer_download.go:311-312` |

The mount context never carries the mount collector. Watch: `mountCtx, mountCancel := context.WithCancel(ctx)` (`orchestrator_watch.go:415`). One-shot: the work function passes its context straight to `engine.RunOnce(c, runMode, runOpts)` (`orchestrator_run_once.go:240-248`). So `perf.FromContext` inside the engine returns the *command* collector.

What `status --perf` reads: `overlay.snapshots = response.Mounts` (`internal/cli/status_perf.go:59`), filled by `o.perfRuntime.SnapshotByMount()` (`internal/multisync/control_socket.go:502`). `response.Aggregate` (the runtime collector, :501) is not used by the status command.

### The bug (VERIFIED)

For a running owner, watch or one-shot, the HTTP, DB and transfer figures are recorded on the command collector. They never reach the mount collectors, because propagation only goes upward, and never reach the runtime collector either. Therefore:

- the `HTTP`, `DB` and `Transfers` lines of `status --perf` (`status_perf.go:204-213`) and the same JSON fields are always zero;
- `Aggregate` on the control socket is zero for those families too;
- only the daemon's own "performance update" and "performance summary" log lines (`logging.go:84, 97`) carry them, merged across all mounts.

R-6.6.15 promises "live sync-owner performance snapshots through `status --perf`" (`spec/requirements/non-functional.md:81`) and is marked verified.

Why the tests pass: `TestOrchestrator_OneShotControlSocket_PerfStatusAndCapture` records by hand on the runtime and mount collectors (`internal/multisync/perf_control_test.go:27-29`), and `TestStatusPerf_SummaryJSON_WithLivePerf` serves a canned response with non-zero counts (`internal/cli/status_perf_test.go:37-51`). No test runs an engine and then reads the mount snapshot.

### End state

Attach the mount collector to the context the engine runs under. Totals in the command log do not change, because mount to runtime to command propagation already exists and each record still enters at exactly one leaf.

```go
// internal/multisync/orchestrator_watch.go, after :415
mountCtx = perf.WithCollector(mountCtx, mountCollector)

// internal/multisync/orchestrator_run_once.go, first line of fn at :240
c = perf.WithCollector(c, mountCollector)
```

Open detail: the engine factory runs before the wrap (`orchestrator_watch.go:403-409`, `orchestrator_run_once.go:218-224`), so the drive verification requests of `NewMountEngine` stay on the command collector. Wrap the context passed to `o.engineFactory` as well if those requests should count for the mount.

Tests first:

- `internal/multisync/perf_control_test.go`, new `TestOrchestrator_MountPerfIncludesContextRecordedMetrics` (`// Validates: R-6.6.15`): an engine factory whose `RunOnce` calls `perf.FromContext(ctx).RecordHTTPRequest(...)`, `RecordDBTransaction(...)` and `RecordTransfer(...)`. Assert `SnapshotByMount()[id]` shows them, `AggregateSnapshot()` shows them, and a command-level parent collector shows the same totals once (no double count). The same for the watch path.
- `internal/sync`: one engine pass with `PerfCollector` set and the context wrapped as above; assert `Snapshot().DBTransactionCount > 0` on the mount collector.

Docs: R-6.6.15 text stays. Add one sentence to `spec/design/sync-control-plane.md` (performance section; the file implements R-6.6.15 to R-6.6.17, :5) and to `spec/design/system.md:222-226`: metrics recorded through the context belong to the mount whose engine runs under that context.

Risk: low. One behaviour change for users: `status --perf` starts showing non-zero HTTP, DB and transfer figures.

---

## Not read

Stated so nothing below is taken as checked.

- The CI log of the 2026-11-10 nightly. The timeline is the task brief's.
- `internal/driveops/transfer_session.go` before line 120 and the wrapper `persistedSessionUpload` around `persistedSessionUploadOnce`.
- `internal/sync/engine_watch_maintenance.go:94` (`armFullRefreshTimer`), `internal/sync/protected_roots.go:16` (`refreshProtectedRootsFromStore`), `internal/sync/engine_runtime_shutdown.go`.
- The one-shot cancel path from the signal to `internal/sync/engine_run_once.go:99`.
- `internal/sync/sqlite_compare.go:390-409` (`current_equal`); `resolveCreateConflict` in `internal/sync/executor_transfer.go` (about :580-614); the callers of `ClearPendingCreate`.
- Whether anything besides observation and the 409 resolver reads `pending_creates`.
- `internal/sync/fs_shim_scenario_test.go`, `engine_model_ops_test.go`, `crash_points_test.go`, `internal/synctest/*`, `testutil/testenv.go`, `internal/graph/integration_testenv_test.go`; the storm loop at `engine_model_based_test.go:462-527`; how `moveScenarioRemote.addFile` assigns hashes.
- The other `httptest` servers: 21 files in `internal/graph`, 16 in `internal/cli`, plus `internal/sync/socketio_test.go` and `internal/driveops/session_test.go`. Only the helpers named in Item 4 were read.
- `spec/design/sync-execution.md` and `spec/design/drive-transfers.md` outside the cited ranges; `spec/design/graph-client.md`; `spec/design/sync-store.md:390-432`; `spec/reference/rearchitecture-candidates.md:153`.
- The entry at `spec/reference/live-incidents.md:170`, which mentions the "sealing new work admission" line.
- Rows of `01c` and `01d` not marked as read by me.
- devtool's repository checks. No guard is proposed here against new name-only waits for that reason.

---

## Dependency-ordered PR breakdown

Each PR is one increment in its own worktree, tests first, with its spec and ledger edits in the same PR. The host has two cores and the live CI jobs share one concurrency group, so push one or two at a time.

| # | PR | Content | Depends on |
|---|---|---|---|
| 1 | e2e: size and content waits per drive; converge helpers per drive and all-drives | `waitForRemoteStatSizeForDrive`, `waitForRemoteFileContent` in `e2e/remote_read_helpers_test.go`; the four helper variants in `e2e/e2e_test.go`. No call-site change. | — |
| 2 | e2e: replace the 18 racy name-only waits | Item 2 table, row 17 (fast tier) and row 2 (the failing test) first; delete `pollForDrive2File` and the two inline content loops. New `LI-20261110-01` and the recurring-family entry. Confirm with a `workflow_dispatch` run of the full tier. | 1 |
| 3 | e2e: converge the flake candidates | `sync_full_test.go:294, 906, 975`; `orchestrator_e2e_test.go:66, 120`; `cli_commands_e2e_test.go` (all 8); `sync_hardening_full_e2e_test.go:328`; `sync_recovery_e2e_test.go:138`. | 1 |
| 4 | e2e: converge the remaining sites | the other 48 "C" lines of Item 3 by file; the three `require.Eventually` loops; settle `shared_items_e2e_test.go:212`. | 1 |
| 5 | graph + driveops: remove a placeholder while it is empty, retrying on 412 | Fake knobs `createStatus`, `deleteStatuses`, `readBacks`, `sessionLandsThenDrops`; `RemoveEmptyItem`; `PlaceholderRemover` change; named timeout. R-5.6.14, `drive-transfers.md`, quirk addition. | — |
| 6 | driveops + sync: the created-item record survives failure and shutdown | Hook call from `openSessionForUpload` on `CreatedItemError`; detached record write; quiet intent clear; comment at `store_pending_creates.go:124-127`; the G2 test and, if it fails, its fix. R-5.6.15, `sync-execution.md`. | 5 (shares the fakes) |
| 7 | sync: a clean shutdown logs no warning; wording says cancel, not drain | `refreshProtectedRootsForHousekeeping` check and test; log text at `engine_watch.go:237`; R-2.8.3, `sync-engine.md:567, 1322-1324`, repo `CLAUDE.md:186`. | — |
| 8 | sync tests: refusals and placeholder churn in the fakes | `refuseCreate` in `moveScenarioRemote` with the engine test for R-2.10.71; the chaos variant with `churnPlaceholder`; ledger entry for the 400 on create. | 6 (the new storm variant must not trip over G1/G2 first) |
| 9 | multisync: mount collectors receive context-recorded metrics | The two `perf.WithCollector` lines, the tests of Item 5, the two doc sentences. | — |
| 10 | sync tests: visibility lag and large-file path in the scenario fake | `invisibleFor` knob; `SessionUploader` on `engineMockClient`. | 8 |

PRs 1-4 touch only `e2e/`. PRs 5, 7 and 9 are independent of each other and of the e2e work. One decision is the owner's and blocks nothing: whether the first signal should ever wait for small in-flight creates (Item 1d (3)); this report recommends not.
