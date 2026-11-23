# Track 4 — Silent failures (discarded, downgraded, defaulted errors)

Repository: ~/src/onedrive-go at `26b3bc8b` (clean tree, `go build ./...` OK).
Scope: 393 non-test Go files (internal/, pkg/, cmd/, main.go; internal/devtool excluded).
Read-only; no tests run. Inventories saved under
`/tmp/scratch/scratchpad/review/inv/`.

Headline: no critical finding. The codebase has `errcheck` (`check-blank: true`), `nilerr`,
`rowserrcheck`, `sqlclosecheck`, `bodyclose` and `wrapcheck` enforced, every blank discard is
annotated, all 25 SQLite transactions use `finalizeTxRollback`/`errors.Join`, and every write path
checks `Write`/`Sync`/`Close`/`Rename`. The real exposure is category 2: ~30 "handle and go on"
branches, of which one is a user-visible mis-report (high) and six are rule violations with
bounded effect (medium). Everything else examined is justified in a comment or converted into a
visible condition.

## 1. Inventory

### Grep commands

```
F=inv/files.txt   # find internal pkg cmd -name '*.go' ! -name '*_test.go' ! -path '*/devtool/*' ; + main.go
# 1 discards / nolint / naked calls
xargs grep -nE '^\s*_\s*=\s|^\s*_,\s*_\s*=|,\s*_\s*=\s|\b_\s*=\s*[a-zA-Z]' < $F
xargs grep -n 'nolint' < $F
xargs grep -nE '^\s*(defer\s+)?[A-Za-z_][A-Za-z0-9_.()]*\.(Close|Write|WriteString|Flush|Sync|Commit|Rollback|Remove|RemoveAll|Rename|Set|Exec|Scan|Seek|Truncate|Chmod|Chtimes|Unlock|Release)\([^=]*$' < $F
xargs grep -nE '^\s*(defer\s+)?(os|json|strconv|time|io|filepath)\.(Remove|RemoveAll|Rename|Unmarshal|Marshal|Atoi|ParseInt|ParseBool|Parse|Copy|WriteFile|MkdirAll|Mkdir|Chmod|Chtimes|Symlink|Walk|WalkDir)\(' < $F   # 0 hits
# 2 non-returning err branches: inv/branches.py (brace-matched body of every `if … err != nil {`,
#   classified fallthrough / continue-break / return-of-zero)
# 3 benign tests
xargs grep -nE 'errors\.(Is|As)\(|os\.IsNotExist|fs\.ErrNotExist|os\.IsExist|fs\.ErrExist|syscall\.E[A-Z]+|IsNotExist\(' < $F
# 4 defaults / type switches
xargs grep -nE '^\s*default:' < $F ; xargs grep -nE '\.\(type\)' < $F
# 6 recover
xargs grep -n 'recover()' < $F
# 7 write paths and transactions
xargs grep -nE 'CreateTemp|\.Rename\(|\.Sync\(\)|SyncDir\(|\.Commit\(\)|\.Rollback\(\)|BeginTx|\.Begin\(' < $F
grep -rn 'defer.*Rollback\|finalizeTxRollback(' internal
# 8 Debug/Info calls carrying an error: inv/slogscan.py (paren-matched multi-line slog calls)
xargs grep -nE '\.Warn(Context)?\(' < $F | wc -l
```

### Counts

| Category | Total | Notes |
|---|---|---|
| 1a `_ =` / `_, _ =` discards | 32 | 14 are non-error (unused params/vars, `(entry, bool)` lookups, bool returns); 3 are `strings.Builder`/`bytes.Buffer` writes; **15 are annotated `//nolint:errcheck` error discards** — all read below |
| 1b `//nolint` directives | 205 | errcheck 84 (29 `defer resp.Body.Close()`, 25 `defer rows.Close()`, 3 `stmt.Close()`, 12 read-only `f.Close()`/`dir.Close()`, 15 the discards above); wrapcheck ~95; nilerr 5; gosec/gocritic/funlen/etc. the rest. `nolintlint` requires an explanation on each |
| 1c naked calls without nolint | 221 lines | all `mu.Unlock()`, `Header.Set`, `url.Values.Set`, `strings.Builder.WriteString` — none return an error |
| 2 `if err != nil` branches that do not return | 206 | 74 fall-through, 31 `continue`/`break`, 101 return a zero value. Per package: sync 72, cli 47, config 31, driveops 20, multisync 16, graph 7, synctree 6, localtrash 4, localpath/retry/tokenfile 1 each. All 206 read in context (the scanner's 6 "empty body" flags were `else if` chains, false positives) |
| 3 `errors.Is/As`, not-exist, errno tests | 378 | sync 145, cli 73, driveops 54, graph 32, config 18, multisync 17, synctree 15; 97 are `ErrNotExist`/`ErrExist`/errno tests, each read |
| 4 `default:` branches | 222 | sync 114, cli 45, config 15, graph 10, driveops 9, multisync 7, rest ≤4; 2 type switches (`graph/redaction.go:48` log redaction, `driveid/id.go:96` `sql.Scanner`) |
| 5 bulk loops | 11 | executor/worker pool, remote observer window, local scanner, session-store scans, stale-partial walk, trash purge, shortcut child cleanup, multisync mount start, email reconcile, `put` tree walk, config validation — see §2.5 |
| 6 `recover()` | 4 | scanner.go:277, engine_watch_panic_recovery.go:29, worker.go:153, multisync/mount_runner.go:38 |
| 7 write/tx sites | 98 lines | 25 `BeginTx`/`beginPerfTx`, every one paired with `finalizeTxRollback` or `errors.Join(err, tx.Rollback())`; atomic writers `localpath.AtomicWrite`, `fsroot.AtomicWrite`, download `finalizePartial`/`publishPartial`, `detach_unix` copy, `localtrash.copyToSibling` all check chmod→write→sync→close→rename→dirsync |
| 8 Debug/Info calls that carry an error | 161 (+25 single-line) | sync 74, cli 27, graph 23, multisync 12, driveops 11, config 8; Warn calls total 101 |

## 2. Findings (ranked)

### 4-1 HIGH — a permission/IO error on the data directory is reported as "no accounts" / "nothing synced"
- **Where:** `internal/cli/cleanup.go:33-41` (`managedPathExists` returns `false` on *any* error), `internal/config/discovery.go:30-33` and `:98-102` (`readManagedDir` error → `return nil` at Debug).
- **Consumers:** `internal/cli/status_sync_state.go:18` (`if !managedPathExists(statePath) { return DriveStatusSnapshot{} }`), `internal/cli/auth_health.go:139` (`!managedPathExists(tokenPath)` → `authReasonMissingLogin`), `internal/cli/drive_add.go:168,391`, `internal/cli/status_snapshot.go:176,190` ("No accounts configured. Run 'onedrive-go login'" when `DiscoverTokens` returns nothing).
- **Scenario:** `~/.local/share/onedrive-go` loses read/search permission (a `sudo onedrive-go` run that chowned a subdirectory, a restored home with wrong ACLs, a stalled NFS/FUSE home). `status` prints "No accounts configured… login", the drive card shows zero files and no issues, and `auth` health says the login is missing. The user is steered to log in again rather than to fix permissions. The cause is in the log only at Debug.
- **Evidence:** `status_sync_state.go:22-24` already carries the comment *"Without this, an unreadable state database looked like a drive with nothing to report: zero files and no issues"* — the `managedPathExists` guard six lines above produces exactly that outcome for a stat error. `discovery.go` distinguishes nothing: `ErrNotExist` (fresh install, benign) and `EACCES` both become `nil`.
- **Classification:** 2(d) silently swallowed / 3 mis-bucketing (EACCES, EIO, ELOOP read as "not exist").
- **Tests:** `internal/config/discovery_test.go:45 TestDiscoverCIDFiles_ReadDirError` asserts the **nil** result for a generic error, i.e. it locks the behaviour in. No test for `managedPathExists` on EACCES.
- **Regression test:** `discoverCIDFilesWithIO` with `readManagedDir` returning `fs.ErrPermission` must not return an empty list silently (return an error, or Warn and mark the discovery degraded, as `shared_discovery.go:152` already does for remote discovery); `readDriveStatusSnapshot` with a stat error must surface a read error in the snapshot, matching its own comment.
- **Confidence:** CONFIRMED by tracing.

### 4-2 MEDIUM — protected-root refresh failure is Debug-only and housekeeping then walks the children
- **Where:** `internal/sync/engine_housekeeping.go:15-18`.
- **Code:** `if err := flow.engine.refreshProtectedRootsFromStore(ctx); err != nil … logger.Debug("refresh protected roots before housekeeping", …)` then `CleanTransferArtifactsWithOptions(... SkipDirs: transferArtifactCleanupSkipDirs(filter, flow.engine.protectedRoots) ...)`.
- **Why it matters:** `CleanStalePartialsWithOptions` (`driveops/stale_partials.go:46-100`) deletes **every** owned `.partial` it finds, with no age check. On a failed refresh `protectedRoots` keeps the previous list (`protected_roots.go:12-25` assigns only on success), so a shortcut root bound since the last successful refresh is unprotected and the parent pass unlinks the child mount's in-flight download partials. The child then fails at `renaming partial to …` and restarts from zero on the next pass. No data loss; wasted transfer, repeated while the read keeps failing, invisible above Debug.
- **Tests:** `internal/sync/engine_housekeeping_test.go` covers the skip when records load; none for the refresh error.
- **Regression test:** store whose `listShortcutRoots` returns an error; assert housekeeping either skips the partial walk or logs Warn once per streak, and that a partial under the newly added child root survives.
- **Confidence:** CONFIRMED (code path), PLAUSIBLE (trigger needs a SQLite read failure).

### 4-3 MEDIUM — trash purge failure is Debug-only; nothing in `status` covers it
- **Where:** `internal/sync/engine_housekeeping.go:48-53` (`purgeExpiredTrash`): `removed, err := e.trash.Purge(); if err != nil { e.logger.Debug("could not purge expired trash entries", …) }`.
- **Scenario:** trash directory (`<datadir>/trash/<state>`) on a filesystem that refuses `RemoveAll` (ACL, immutable attr, EROFS after an error remount). Purge fails every hour in watch mode and after every one-shot pass; trash grows without bound; disk fills; no Warn, no status condition (the condition catalog in `internal/sync/conditions.go` has nothing for housekeeping; `grep -il trash internal/cli/status*.go` is empty).
- **Tests:** `localtrash_test.go` covers `Purge` itself; none for the engine's handling of its error.
- **Regression test:** `Purge` returning an error → one Warn per failure streak (the pattern already used for `safetyScanFailing`) and/or a status condition.
- **Confidence:** CONFIRMED.

### 4-4 MEDIUM — a facet-less new remote item whose single re-read fails transiently is dropped for good
- **Where:** `internal/sync/item_converter.go:188-208` (enrich failure recorded only in `sparseEnrichFailed`), `:722-724` (`if !c.normalizeRemoteFacts(...) { return nil }`), `:808-815` (`applyBaselineKind`: `existing == nil` → Debug "skipping remote item that does not say whether it is a file or a folder" → `return false`).
- **Inconsistency:** the parentless path (`:896-906`) checks `sparseEnrichFailed` and calls `markIncompleteObservation()`, which fails the window (`observer_remote.go:251-254` → `errRemoteObservationIncomplete`) so the cursor is not advanced and the window is replayed. The facet-less path does not: the item is skipped, the window completes, the delta token is persisted, and the item is never seen again until a full resync.
- **Scenario:** Graph returns a sparse delta entry (no `file`/`folder` facet) for an item not in the baseline; the one `GetItem` re-read gets a 503/timeout.
- **Tests:** `item_converter_kind_test.go:66 TestFullDelta_FacetlessNewItemIsReadOnceOrSkipped` covers "re-read also facet-less → skip" only; the re-read *error* branch is untested.
- **Regression test:** same fixture with `getItem` returning a transient error for the facet-less item → expect `errRemoteObservationIncomplete` (window replayed), not a committed window missing the item.
- **Confidence:** CONFIRMED (code), PLAUSIBLE (how often Graph emits facet-less *new* items is unknown).

### 4-5 MEDIUM — `scanRemoteStateRow` maps every SQL error to "no row"
- **Where:** `internal/sync/store_write_observation.go:613-625`: `row, err := scanRemoteStateRowWithQuerier(...); if err != nil { return nil }` — no `sql.ErrNoRows` distinction, no doc comment justifying it. Callers: `processObservedItem` (`:382-389`: `existing == nil` → insert, or for a delete `forgetBaselineForReuploadTx` **without** `deleteObservedRemoteStateTree`), `parkMovingRemoteRowsTx` (`:231,:241` → `continue`), `storedPathForObservedItem` (`store_remote_twins.go:72`).
- **Effect:** bounded by the enclosing write transaction — a canceled context or poisoned connection fails the next `Exec` and the tx rolls back. A read error that does **not** poison the tx (SQLITE_IOERR_READ, interrupted read) makes a remote delete leave its `remote_state` row behind with the baseline dropped, which the planner reads as a new remote file; or makes a move insert a duplicate (PK conflict → rollback, benign).
- **Tests:** none reference `scanRemoteStateRow`.
- **Regression test:** querier failing once with a non-`ErrNoRows` error → the observation write must return that error, not proceed down the insert/forget path.
- **Confidence:** CONFIRMED (mapping), SPECULATIVE (data effect needs a non-poisoning read error).

### 4-6 MEDIUM — retry_work not cleared on success is Debug-only and leaks into `status`
- **Where:** `internal/sync/engine_retry_trial.go:72-86` (`clearRetryWorkOnActionSuccess`: resolve failure → `logger.Debug("failed to clear retry_work on success", …)`).
- **Effect:** the row stays; `store_retry_work.go` `sqlListRetryWorkRetrying` (`attempt_count >= 3`) feeds the "Retrying" count in `status`, so a file that just synced keeps showing as retrying, and the row can schedule a redundant retry. Spec says engine-returned errors are not double-logged, but this one is not returned either.
- **Tests:** none reference `clearRetryWorkOnActionSuccess`/`retryResolutionSourceActionSuccess`.
- **Regression test:** store whose `resolveRetryWork` fails → expect the failure to be returned/retried or a Warn, and `status` not to list the path.
- **Confidence:** CONFIRMED.

### 4-7 MEDIUM — block-scope read failure silently empties the remote write guard
- **Where:** `internal/sync/permission_probe_remote.go:23-30`: `ListBlockScopes` error → Debug → `return nil` ("no blocked boundaries"). The file-level comment justifies Debug for the *handle403 probes*, not for this read.
- **Effect:** writes under a read-only remote subtree are admitted again, fail with 403, get re-probed and re-recorded — churn and duplicate 403 traffic, no data loss. The function's doc comment does not claim the discard.
- **Tests:** none reference `ActiveRemoteBlockedBoundaries`.
- **Regression test:** failing store → return the error (callers already handle an error from admission) or Warn.
- **Confidence:** CONFIRMED.

### 4-8 LOW/MEDIUM — upload-session load failure Debug claims "the store already reported"
- **Where:** `internal/driveops/transfer_session.go:103-111` comment: *"the store already reported what it did with the file"*. `SessionStore.Load` (`session_store.go:109-176`) logs only for expired and corrupt files; a `stat`/`ReadFile` failure (EACCES, EIO) is returned unlogged. The upload starts a fresh session every time; a multi-GB upload over a flaky link never resumes. `Save`'s Warn (`:196`) fires only if the write also fails.
- **Regression test:** session file present but unreadable → expect a Warn naming the path once.
- **Confidence:** CONFIRMED (comment is wrong), PLAUSIBLE (trigger).

### 4-9 LOW — catalog load failure during drive resolution hides the cause of a later "drive ID is required"
- **Where:** `internal/config/drive.go:206-210` Debug on `LoadCatalog()` error; `DriveID` stays zero; `engine_config.go:61` / `engine.go:121` reject it ("sync: drive ID is required") without naming the catalog. Also `:311`, `:372` (org name/display name silently blank).
- **Mitigation present:** `validated_state.go:49-52` fails hard on a catalog error, so `sync` sees the real message first; other `ResolveDrive` callers do not.
- **Confidence:** CONFIRMED, low effect.

### 4-10 LOW — `refuseLiveDatabaseTarget` treats any sidecar lstat error as "no sidecar"
- **Where:** `internal/sync/executor_preconditions.go:981-990`: `if err != nil { continue }` for `path-wal`/`path-journal`. EACCES/EIO on the sidecar (perm bits differ from the main file) lets a download replace a live SQLite database. `executor_database_guard_test.go` covers only the present-sidecar path.
- **Confidence:** CONFIRMED mapping, SPECULATIVE trigger.

### 4-11 LOW — recovered mount panic logged at Info (rule says Error)
- **Where:** `internal/multisync/mount_runner.go:50-58` `dr.logger.Info("recovered panic while syncing a drive", …stack…)`. The report error is user-facing, so the failure is visible; the stack is not at the level CLAUDE.md requires. Hygiene.

### 4-12 LOW — trash `reservedAt` zero-time fallback purges an unstat-able foreign entry immediately
- **Where:** `internal/localtrash/localtrash.go:541-551` (`return time.Time{}`) with `Purge` `:528` (`now.Sub(found.reservedAt) > incompletePlacementGrace`). Only for entry names this package did not create and whose `Info()` fails. Hygiene.

### 4-13 LOW — systemd watchdog keepalive failure is Debug-only
- `internal/cli/sync_notify.go:103-107`. A persistently failing `WATCHDOG=1` gets the service killed by systemd with no log above Debug. Realistically only when systemd itself is gone.

### 4-14 LOW — unparseable `paused_until` pauses forever with no log at all
- `internal/config/config.go:122-126` (no logger in scope). Conservative and visible as "paused" in status, but the reason (corrupt value) is never shown.

### 4-15 LOW — leftover empty placeholder after a failed `put`
- `internal/graph/item_create.go:231-237`, `driveops/transfer_session.go:397-402` log the placeholder-delete failure at Debug. For the sync engine `pending_creates` (`store_pending_creates.go` header) adopts the item, so this is justified there; for the stateless `put` command the empty remote file with the user's name stays and nothing says so.

### Not findings, but worth the next reviewer's attention
- `internal/sync/engine_watch.go:538-543` "Instant change notifications … stopped" at Info: `socketIOWakeSource.Run` only returns on context end (`socketio.go:151-180`, retries are internal with one Warn per episode), so this line is effectively shutdown-only. Fine.
- `internal/driveops/transfer_download.go:745-757` statfs fail-open: Warn once per `TransferManager`, Debug after; documented. Fine.
- `internal/sync/engine_watch_observation.go:34-43` `knownObservationIssues` read failure → `nil` → every finding re-warned. Documented fail-open. Fine.

## 2.5 Bulk loops (category 5)
| Loop | Per-item failure handling | Surfaced at end of pass |
|---|---|---|
| Worker pool / executor (`worker.go`, `executor.go:788-800`) | each action → `failedOutcome` at Debug, completion carries the error | `logFailureSummary` (`engine_results.go:116-178`): Info per condition type when >10, Debug per item; console activity line is the per-file report (spec sync-engine.md:1088-1091). Meets R-6.6.12 as written |
| Remote observer window (`observer_remote.go:249-255`, `item_converter.go`) | orphan/parent failures → `markIncompleteObservation` → whole window fails and replays (conservative) | yes — **except** the facet-less path (4-4) |
| Local scanner (`scanner.go:296-325`, `:518-540`, `scanner_unavailable.go:224-236`) | `skippedItem` with issue type; EACCES vs IO distinguished (D-005) | persisted observation issues → `status` |
| Session store `CleanStale`/`DeleteForScope` (`session_store.go:245-275`, `:331-345`) | `errs` joined / Warn per file | caller logs Warn |
| Stale partials (`stale_partials.go`) | Warn per unremovable file, Debug per unreadable path (scanner owns that report) | count logged |
| Trash `removeWhere` (`localtrash.go:555-576`) | errors joined | **caller drops it at Debug (4-3)** |
| Shortcut child cleanup (`multisync/shortcut_child_artifacts.go:255-275`, `:355-420`) | classified errors collected | recorded as cleanup diagnostics |
| Multisync mount start (`orchestrator_run_once.go:168-178`, `orchestrator_watch.go:220-230`) | per-mount `MountStartupResult` | startup results printed once |
| Email reconcile (`config/email_reconcile.go:180-190`, `:338-345`) | Debug/Warn + continue | config-level, not sync truth |
| `put` tree walk (`cli/put.go:303-306`) | `onError` callback per unreadable dir | command output |

## 2.6 Logging level audit (category 8)
- Retries: `retry/transport.go:199-230` Debug per attempt, one Warn per outage episode (`notices`), `announceLongWait` once per throttle episode (`:361-376`); socket.io `logRetry` one line per episode; multisync `warnOnChange` dedupes by error text. No per-attempt WARN noise found.
- Debug-while-user-visible: 4-1, 4-2, 4-3, 4-6, 4-7, 4-8, 4-13 above. All other 150+ Debug/Info-with-error sites either return the error (graph/driveops transport layers: "Returned, not logged above Debug"), convert it to a skipped item/condition, or are shutdown/cleanup paths with a comment.

## 3. Discards examined and judged correctly justified
(file:line — reason confirmed)

- `internal/sync/executor_delete.go:602` `known, _ = e.baseline.GetByID` — second value is `bool`, not error (`core_types.go:146`).
- `internal/sync/engine_watch_loop.go:128` `_ = rt.beginWatchDrainIfContextCanceled` — returns bool.
- `internal/config/drive.go:389-391`, `sync/engine_runtime_completion.go:426`, `permission_probe_local.go:103,119`, `permission_probe_remote.go:183`, `engine_primary_root_watch.go:72`, `multisync/control_socket.go:477`, `cli/status_render_sections.go:9,115`, `cli/status_snapshot.go:818`, `cli/status_render.go:510` — unused-parameter silencers, no error involved.
- `internal/cli/drive_add.go:555,562,565` — `bytes.Buffer`/`fmt.Fprintf` into an in-memory buffer; infallible.
- `internal/localtrash/localtrash.go:417` "leftover copy is harmless" — true: the temp name carries `restoreArtifactPrefix` and `content_filter.go:140` excludes `IsRestoreArtifactName`, so sync never uploads it.
- `internal/localtrash/localtrash.go:451-452`, `localpath/rename_noreplace.go:108-109`, `synctree/detach_unix.go:415-427` — error-path cleanup; the primary error is returned.
- `internal/synctree/detach_unix.go:193-194,216`, `root_identity_unix.go:115,183`, `rmdir_unix.go:34`, `synctree.go:769,860,896,1099`, `spelling.go:49`, `driveops/hash.go:40`, `transfer_upload.go:276,370`, `transfer_target.go:199`, `localpath/rename_noreplace.go:95`, `localtrash.go:442` — read-only descriptors; nothing buffered.
- `internal/driveops/transfer_target.go:166-167` umask probe temp file — the mode read from `Stat` is the answer; the file is empty and in `os.TempDir()`.
- All 29 `defer resp.Body.Close() //nolint:errcheck` in `internal/graph` and `sync/socketio_conn.go:37` — response bodies; `bodyclose` lint also enforces the close.
- All 25 `defer rows.Close() //nolint:errcheck` and 3 `defer stmt.Close()` — `rowserrcheck`/`sqlclosecheck` on; `rows.Err()` checked.
- `internal/cli/console_log_bootstrap.go:85` — replaying buffered log records into a handler whose own error is a log-write failure; justified in the comment above it.
- `internal/sync/observer_local.go:910` `defer watcher.Close()` — fsnotify shutdown.
- nilerr sites: `observer_local_handlers.go:236` (cancellation = clean stop), `observation_root_identity.go:233` (`CountSyncFolderFiles` is a display count for `rebind-root`, the doc says so), `inotify_linux.go:138` (unknown FUSE flavor → conservative "remote"), `localtrash.go:483` (target is the sync folder itself).
- Transactions: all 25 `BeginTx` sites (`store_*.go`, `shortcut_root_store.go:132-197`, `schema.go:303-325`, `engine_current_plan.go:487-497` read-only) use `finalizeTxRollback`, which suppresses only `sql.ErrTxDone` after a successful commit and joins any other rollback error onto the returned error.
- Write paths: `localpath.AtomicWrite` (`localpath.go:374-422`), `fsroot.AtomicWrite` (`fsroot.go:440-520`), `finalizePartial`/`publishPartial` (`transfer_download.go:283-370`, dir-sync failure returned unless `EINVAL/ENOTSUP/EOPNOTSUPP`), `synctree.SyncDir` (`:1073-1090`), `copyToSibling` (`localtrash.go:440-475`), `detach_unix.go:408-435` — chmod, write, fsync, close, rename, parent fsync all checked.
- Not-exist tests reviewed and correct: `synctree.go:1134-1150` (`normalizeNotExist` maps ENOTDIR to not-exist only after an lstat confirms absence), `spelling.go:63` (`isMissing` ENOENT/ENOTDIR only; EACCES leaves spelling alone), `scanner_unavailable.go:226-235` (only ENOENT is absence, D-005), `localtrash.go:277-280` (anything but ENOENT keeps `hasItem`), `executor_delete.go:80-90,221-230`, `executor_transfer.go:297-301`, `executor_preconditions.go:426-440,652-700,761,782,923` (ENOENT → stale precondition, other errors returned), `config/resolved_validator.go:53-58,80-86`, `shortcut_root_planner_local.go:245-253`, `engine_mount_root.go:235-243` and `engine_shortcut_topology.go:139-146` (only `graph.ErrNotFound` confirms a deletion; any other error keeps the item), `item_converter.go:420-426` (only 404/403 mark a parent unreachable; transient errors replay the window).
- `default:` branches: every enum switch in `internal/sync` returns an error for an unknown value (`planner_actions.go:436`, `store_write_baseline.go:544`, `enums.go:148`, `store_retry_work.go:226`); `classifyItemType` default=file is correct because packages/bundles are filtered first (`item_converter.go:532,566`); graph/driveops defaults (`transfer_verify.go:82` → mismatch, `transfer_download.go:153` → mismatch, `errors.go:471` → `ErrServerError` for ≥500) fail closed.
- `recover()` sites: all four log the panic with a stack and turn it into a failed completion / fatal observer error / mount report (`mount_runner` level noted in 4-11).
