# Sync Observation

GOVERNS: internal/sync/buffer.go, internal/sync/inotify_linux.go, internal/sync/inotify_other.go, internal/sync/item_converter.go, internal/sync/local_hash_reuse.go, internal/sync/local_observation_batch.go, internal/sync/observation_local_rules.go, internal/sync/observation_remote_batch.go, internal/sync/observation_root_identity.go, internal/sync/observed_items.go, internal/sync/observer_local.go, internal/sync/observer_local_collisions.go, internal/sync/observer_local_handlers.go, internal/sync/observer_remote.go, internal/sync/scanner.go, internal/sync/scanner_unavailable.go, internal/sync/socketio.go, internal/sync/socketio_conn.go, internal/sync/socketio_protocol.go, internal/sync/symlink_observation.go

LAYER: 2

Implements: R-2.1.2 [verified], R-2.2.4 [verified], R-2.15.2 [verified], R-2.15.4 [verified], R-2.4.11 [verified], R-2.4.12 [verified], R-2.4.16 [verified], R-2.8.8 [verified], R-2.8.12 [verified], R-2.8.13 [verified], R-2.8.14 [verified], R-2.8.21 [verified], R-2.8.15 [verified], R-2.8.16 [verified], R-2.8.18 [verified], R-2.8.19 [verified], R-2.8.20 [verified], R-2.11 [verified], R-2.12 [verified], R-2.13.1 [verified], R-2.13.2 [verified], R-6.6.17 [verified], R-6.7.1 [verified], R-6.7.3 [verified], R-6.7.5 [verified], R-6.7.19 [verified], R-6.7.20 [verified], R-6.7.21 [verified], R-6.7.24 [verified], R-6.7.28 [verified], R-6.7.29 [verified], R-6.2.12 [verified], R-6.2.13 [verified], R-6.2.14 [verified], R-6.2.15 [verified]

## Overview

Observation turns local filesystem changes and Graph delta/enumeration results
into live observation facts. The low-level observers do not write the sync DB
directly. The engine-owned observation orchestration persists the resulting
observation batch as one coherent durable set. `observation_state` owns the
remote cursor, remote full-refresh cadence, and local-truth confidence; local
watch safety-scan cadence is rebuildable watch runtime state. Observation
produces wake signals, dirty path/scope hints, observation findings, direct
local-snapshot rows for `local_state`, and scoped local observation batches
that the engine applies to `local_state`.

In watch mode, that ownership is explicit: local observers emit engine-applied
`localObservationBatch` values, while remote observers and full-refresh workers
emit one `remoteObservationBatch` value that the watch loop applies durably. No
observer goroutine commits state rows or observation cursors directly. Once the
engine starts applying that batch as durable current truth, failures in local
row commits, remote row commits, cursor commits, or observation-findings
reconciliation are fail-closed for that run/session rather than best-effort
warnings.
Those observer outputs are runtime-owned too: the watch runtime owns the local
batch stream, remote-batch stream, skipped-item stream, observer error stream,
refresh channels, and active-observer count, while the pipeline shell keeps only
the non-runtime dependencies needed by the loop.

The observation stack has four main pieces:

- `remoteObserver`: Graph delta/enumeration and wake-driven watch polling
- `localObserver`: fsnotify-driven watch observation
- `Scanner`: full local walk for one-shot bootstrap and rebuild cases
- `dirtyBuffer`: debounce/coalescing before snapshot refresh and replanning

## Observer Time Is Engine Clock Time

`localObserver` and `remoteObserver` hold the engine's `syncClock` rather than
separate sleep, tick, and timer function fields. So does the Socket.IO wake
source: its endpoint refresh deadline, the refresh timer, and its reconnect
backoff sleeps all read one clock (`socketIOWakeSourceOptions.Clock`). It once
took separate now and sleep hooks while the refresh timer ran on wall time, so
the refresh test waited on a real 400 ms timer and failed on a loaded host
(`TestSocketIOWakeSource_RefreshesEndpointBeforeExpiry` now advances a manual
clock). Observer fields are also
unexported: the type is unexported, so exported fields on it conveyed nothing
except that the type had once been public.

## Ownership Contract

- Owns: local and remote change capture, path normalization, sparse-item recovery, skipped-item reporting, and dirty buffering
- Does Not Own: planning, retry policy, scope persistence, or user-facing issue interpretation
- Source of Truth: live filesystem state, Graph responses, and read-only baseline context supplied by the engine
- Allowed Side Effects: rooted filesystem reads, hashing, Graph observation calls, watch registration, and event emission. Hashing takes the observer's context (`localObserver.hashFunc`, `computeStableHashWith`, the scan's hash phase through its errgroup context): the watch event loop hashes synchronously, so a shutdown must stop a read in progress rather than wait it out (R-6.3.6; `driveops.ComputeQuickXorHashContext`).
- Mutable Runtime Owner: `remoteObserver`, `localObserver`, `Scanner`, and `dirtyBuffer` each own only their own invocation- or run-scoped mutable state, watches, and goroutines. The engine owns lifecycle and composition across those pieces.
- Error Boundary: observation surfaces filesystem, watch, and Graph observation failures as observation results and control signals; once the engine starts durably applying an observation batch, observation-finding reconciliation is fail-closed and execution paths do not invent observation-owned durable rows.

## Verified By

| Behavior | Evidence |
| --- | --- |
| A full scan refuses a sync folder that is not the one the state was built from (replaced, an emptied mount point, a re-pointed root symlink) and a sync folder that cannot be read, rather than reading either as deletions; every engine validates against an expected identity, parent-issued for shortcut children and store-recorded for primary roots. | `TestFullScan_ExpectedSyncRootIdentityMismatchReturnsMountRootUnavailable`, `TestRunOnce_EmptiedMountPointPlansNoRemoteDeletes`, `TestEngine_SymlinkedSyncDirRepointedIsRefused`, `TestRunOnce_SyncRootReplacedWhileEngineRunsIsRefused`, `TestFullScan_RootReadDirEIOReturnsError`, `TestNewEngine_RootTokenDecidesOverInode`, `TestRunOnce_RemoteDeleteAfterRebindingInTheSameEngineSucceeds`, `TestWatchObserver_SeesBindingMadeAfterItWasBuilt`, `TestCommitLocalSnapshot_ConcurrentBindsLeaveOneMarker`, `TestValidateExpectedSyncRootIdentity_NamesTheMarkerAndTheWayOut`, `TestMarkRoot_TokenThatDoesNotReadBackIsUnsupported`, `TestNewEngine_RootRecreatedAtSamePathIsRefusedEvenIfInodeIsReused`, `TestNewEngine_UnboundStateWithSyncedItemsIsRefusedUntilRebind`, `TestSameRoot_TokenDecidesWhenRecorded`, `TestSameRoot_BtrfsSubvolumeRootAlsoNeedsDevice`, `TestMarkRoot_FilesystemWithoutXattrsFallsBack`, `TestSameRoot_BirthTimeMustMatchWhenBothSidesKnowOne`, `TestRootIdentity_LinuxBirthTimeIsStatxBtime`, `TestNewEngine_RootWithoutTokenIsRefusedWhenBirthTimeDiffers` |
| Synced paths the scan cannot represent (replaced by a skipped symlink or special file, renamed to an invalid, too-long or excluded name, unstat-able, unreadable) and mount points of other filesystems are recorded as unavailable truth on the synced path, so nothing is deleted on either side; a watch create of such an entry triggers an immediate safety scan. | `TestRunOnce_SyncedItemsMadeUnrepresentableAreNotDeletedOnline`, `TestFullScan_EntryLstatEIOIsReadBoundaryNotDelete`, `TestFullScan_HashEIOProducesNoUploadAndNoDelete`, `TestIsForeignMountPoint_DoesNotCrossFilesystemsByDefault`, `TestIsForeignMountPoint_CrossingRecordsTheBoundary`, `TestRunOnce_UnmountedNestedDiskWithFewFilesIsNotDeleted`, `TestFlaggedMountIsGone_MountedButEmptiedDiskIsNotABoundary`, `TestClearSyncRootIdentity_ForgetsRecordedMountPoints`, `TestRootDeviceForScan_UnreadableRootAbortsScan` |
| A watch Remove or Rename event becomes a delete only when the exact name is really gone, so a lone MOVED_FROM, an atomic save or a misattributed event never deletes a file that is still there. | `TestHandleDelete_PathThatStillExistsEmitsNoDelete`, `TestHandleFsEvent_DeletePassesFsPath` |
| Whole-drive observation emits normalized observation facts and direct local snapshot rows for `local_state` without writing the sync DB directly, including filesystem identity for files and directories when available. | `TestFullScan_NonexistentSyncRoot_ReturnsError`, `TestFullScan_StoresFilesystemIdentityForFilesAndDirectories`, `TestFullScan_NosyncMarkerDoesNotGuardSync`, `TestResolveDebounce_DefaultIsFiveSeconds` |
| Watch-mode local observation emits scoped engine-applied batches: exact file rows, directory-prefix deletes, full safety-scan snapshots, and suspect-local-truth recovery signals. The watch runtime applies those batches to `local_state` and `observation_state`. | `TestLocalObservationBatchForEvent_FileCreateUpsertsExactRow`, `TestLocalObservationBatchForEvent_DirectoryDeleteRemovesPrefix`, `TestLocalObserver_RunSafetyScanEmitsFullLocalSnapshotBatch`, `TestWatchRuntime_LocalObservationBatchUpsertsScopedRows`, `TestWatchRuntime_LocalObservationBatchDeletesExactPath`, `TestWatchRuntime_LocalObservationBatchDeletesDirectoryPrefix`, `TestWatchRuntime_LocalObservationBatchFullSnapshotReplacesLocalTruth`, `TestWatchRuntime_MaintenanceMarksLocalTruthSuspectAfterDroppedObservation` |
| Normal drive content observation suppresses embedded shared-folder shortcut placeholders from content events, including Graph items whose local placeholder is not a folder but whose `remoteItem.folder` target is a folder. Parent drive engines convert those placeholders into parent-owned shortcut-root state before publishing child work commands to the control plane. | `TestFullDeltaWithShortcutTopology_EmitsShortcutFactsAndSuppressesContent`, `TestClassifyItem_EmbeddedSharedPlaceholdersIgnored`, `internal/sync/remote_state_mirror_test.go` |
| Mount-root runtimes still support remote observation rooted at their configured remote root. Separately configured shared folders and managed shortcut child mounts use this path when their content root is below the backing drive root. | `internal/sync/engine_phase0_test.go` (`TestBootstrapSync_WithChanges`, `TestBootstrapSync_ReconcilesRemoteDeleteDriftWithoutFreshDelta`), `internal/sync/observer_remote_test.go` |
| A watcher that cannot be created or extended because of an operating-system limit (inotify watches or instances, open files, kernel memory) degrades to periodic full scans with one warning naming the limit and its fix; a descriptor-limit failure after startup stops watching so the watcher's descriptors are released. | `TestClassifyWatchAddError_ResourceLimitsAreWatchUnavailable`, `TestClassifyWatcherCreateError_ResourceLimits`, `TestAddWatchesRecursive_EMFILEReturnsWatchUnavailable`, `TestRunWatch_WatcherCreateEMFILEFallsBackToPeriodicScan`, `TestWatch_NewDirectoryEMFILEStopsWatchingForFallback`, `TestRunWatch_WatchFallbackScansAtOnceThenAtSafetyInterval`, `TestRunWatch_WatchFallbackRetriesTheWatcher` |
| Known-missed local changes (kernel queue overflow, watcher errors, dropped batches or hash requests) arm one coalesced recovery rescan instead of waiting for the periodic safety scan. | `TestWatch_EventOverflowRunsRecoveryScan`, `TestWatch_RecoveryRequestsCoalesceIntoOneArmedScan`, `TestRecoveryScanDelay_ScalesWithScanDurationAndBacksOffUnderStorm`, `TestCancelRecoveryScan_StaleTimerFiringIsIgnored`, `TestWatch_DirectoryCreatedDuringOverflowIsWatchedAfterRecoveryScan`, `TestRewatchUnwatchedDirectories_FailedAddIsRetriedAtNextScan`, `TestTrySendLocalObservationBatch_DropRequestsRecoveryScan` |
| A full scan schedules a replan only when it changes committed truth, and a due scan whose snapshot the runtime cannot take yet is deferred, with one recovery scan armed at the base pace until there is room. | `TestWatchRuntime_SafetyScanWithoutNewTruthKeepsAdmittedPlan`, `TestWatchRuntime_SkippedFindingsReplanOnlyWhenTheyChange`, `TestBuildObservationReconcilePlan_ReportsChangeOnlyWhenTheIssueSetDiffers`, `TestWatch_RecoveryScanDefersWhileRuntimeHasNotTakenObservations`, `TestWatch_SafetyTickDefersWhileRuntimeHasNotTakenObservations`, `TestLocalObserver_RunSafetyScanEmitsFullLocalSnapshotBatch` |
| A deleted or renamed-away directory drops the watches of its whole subtree, deepest first, so the re-added tree reports its new paths. | `TestHandleDelete_DirectoryRemovesDescendantWatchesDeepestFirst`, `TestWatch_RenamedDirectoryDescendantEventsUseNewPath`, `TestHandleCreate_ReplacedDirectoryDropsStaleDescendantWatches`, `TestHandleCreate_DuplicateDirectoryCreateDoesNotRescanItsTree`, `TestWatchRegistry_SubtreeLookupFollowsTheIndexAndUnlinksEmptyBranches` |
| The destination half of a rename is observed without waiting on a content read: an identity match reuses the synced hash when size and mtime are unchanged, otherwise the row goes out with an unknown hash and the hash follows; a created directory's row precedes its contents. | `TestHandleCreate_MovedChangedFileRowIsNotHeldBackByHashing`, `TestHandleCreate_RenamedUnchangedFileReusesSyncedHash`, `TestHandleCreate_RenameThatMovedCtimeDefersTheHash`, `TestHandleCreate_ReusedInodeWhileSyncedPathExistsIsHashed`, `TestHandleCreate_DirectoryRowPrecedesItsContents` |
| The hash fast path trusts the baseline hash only when size, full-precision mtime, ctime and identity (device, inode, birth time) all match and the file is outside the racily-clean window; an in-place edit with a restored mtime and a different file renamed over the synced one are re-read, and transfers record ctime and identity so the fast path applies after them (R-2.2.4). | `TestFullScan_ContentChangedWithRestoredMtimeIsDetected`, `TestFullScan_IdentityChangeForcesRehash`, `TestFullScan_SubsecondMtimeDifferenceForcesRehash`, `TestFullScan_UnchangedFileWithMatchingIdentityAndCtimeSkipsHash`, `TestCanReuseBaselineHash_CtimeOrIdentityMismatchRequiresHash`, `TestCanReuseBaselineHash_SubsecondMtimeDifferenceRequiresHash`, `TestRunOnce_InPlaceEditWithRestoredMtimeUploads`, `TestRunOnce_InPlaceEditWithRestoredMtimeConflictsWithRemoteEdit`, `TestRunOnce_TransfersRecordLocalCtimeAndIdentity` |
| Write coalescing hashes a continuously written file within a bounded wait, and backs off exponentially for a file that never settles. | `TestScheduleWriteHash_ContinuousWritesHashWithinMaxWait`, `TestWatch_ContinuouslyWrittenFileBacksOff`, `TestHashAndEmit_StillChangingAtRetryCapStaysPendingWithoutEmptyHash`, `TestPruneWriteChurn_DropsSettledPaths` |
| Observer and scan hashing run through the context-aware hasher, so a canceled run stops a whole-file read within one chunk (R-6.3.6). | `TestComputeQuickXorHashFromReaderContext_StopsAtCancel`, `TestComputeQuickXorHashContext_CanceledContextDoesNotHashTheFile` (driveops), with the observer's injected hasher exercised by `TestHandleWrite_VirtualTime_RapidWritesCoalesceIntoOneHash` and `TestFullScan_HashEIOProducesNoUploadAndNoDelete` |
| On kqueue platforms watch setup projects its descriptor use (watched directories plus their entries) and falls back to periodic scans above half the open-file limit. | `TestAddWatchesRecursive_OverDescriptorBudgetFallsBackBeforeExhaustingFiles`, `TestAddWatchesRecursive_WithinDescriptorBudgetWatches`, `TestAddWatchesRecursive_DescriptorProjectionCountsSubdirectoriesOnce` |
| A network or FUSE sync root is identified with statfs, warned about once, and rescanned every two minutes. | `TestWatch_NetworkRootShortensSafetyScanInterval`, `TestAdaptToRootFilesystem_LocalRootKeepsInterval`, `TestAdaptToRootFilesystem_NonDefaultIntervalIsNeverOverridden`, `TestMountFilesystemType_DistinguishesFuseblkFromNetworkFuse`, `TestLocalFilesystemKind_ReadsTheRealFilesystem` |
| The replan debounce is bounded by a max wait that later observations do not push back. | `TestDirtyBuffer_FlushDebounced_ContinuousSignalsFlushAtMaxWait`, `TestDirtyMaxWait_ScalesWithLongDebounce` |
| A delta cursor that loops, is refused twice with a non-transient 4xx, or fails with 5xx for hours in watch mode is recovered through the full refresh; a single refusal is retried, empty pages with a nextLink continue, and a transient failure keeps the token (R-2.15.2). | `TestFullDelta_RepeatedNextLinkIsCursorUnusable`, `TestFullDelta_RepeatedPageItemsIsCursorUnusable`, `TestFullDelta_EmptyPageWithNextLinkContinues`, `TestFullDelta_SavedTokenRejectedTwiceIsCursorUnusable`, `TestFullDelta_OneRejectionOfASavedTokenIsRetried`, `TestWatch_TransientErrorRetriesAndKeepsToken`, `TestWatch_SustainedServerErrorsOnTheCursorResetIt`, `TestRunOnce_SavedTokenOneDriveKeepsRefusingIsReenumerated` |
| Items delta reports without a type facet keep their baseline kind or are re-read once and otherwise skipped; a re-read that fails for any reason but "not found" marks the window incomplete so it is replayed rather than the item skipped behind a saved cursor; bundles and their children are skipped; a negative size is unknown (R-2.4.16). | `TestFullDelta_FacetlessUpdateOfABaselineFolderKeepsItsType`, `TestFullDelta_FacetlessNewItemIsReadOnceOrSkipped`, `TestFullDelta_FacetlessNewItemReReadFailureReplaysWindow`, `TestFullDelta_FacetlessNewItemGoneOnReReadIsSkipped`, `TestFullDelta_BundleAndItsChildrenAreSkipped`, `TestFullDelta_NegativeSizeIsUnknown`, `TestToItem_BundleAndMissingTypeFacet` |
| Child-before-parent windows resolve from the window itself without reads, and a window needing more ancestor reads than one budget completes on replay without re-reading (R-2.15.4). | `TestFullDelta_CrossPageChildBeforeParentResolvesWithoutFetch`, `TestFullDelta_ManyOrphansCompleteAcrossReplaysWithoutRefetching` |
| Watch re-checks address an NFD-named entry by its spelling on disk on byte-exact filesystems: a delete event for a file that still exists is not a delete, an identity match is not a move while the synced file is still there, a full scan re-adds the watch of an NFD-named folder on its own spelling, and a case-collision peer is tracked and re-emitted under its stored path (R-2.13.2). | `TestWatchRechecks_FindTheNFDNamedLocalFile`, `TestRewatchUnwatchedDirectories_WatchesTheNFDNamedFolder`, `TestHandleDelete_ReemitsCollisionPeerUnderItsStoredPath`, `TestHandleDelete_DeletingTheNFDNamedPeerReemitsTheOther` |
| Local watch prefiltering keeps unknown-kind include-root and include-ancestor events observable until stat/type-aware observation can decide the exact item kind. | `TestContentFilter_ShouldObserveUnknownKindIncludesDirectoryCapablePaths` |
| A failed websocket dial is reported without the URL that was dialed: `net/http` embeds it in `*url.Error` and it carries the notification URL's `snthgk` credential, so `connect` strips it to host, operation and cause before the retry log line and the lifecycle event see it (R-6.8.17). | `TestSocketIOWakeSource_DialFailureNeverLogsTheNotificationURL` |

## Remote Observation

`remoteObserver` supports:

- one-shot `FullDelta`
- watch-mode polling
- optional Socket.IO wakeups that tell the observer to poll sooner. The wake source dials the notification URL Graph returned, rewritten to `wss://…/socket.io/?EIO=4&transport=websocket` with its `snthgk` credential kept in the query. A dial that fails comes back from `coder/websocket` wrapping the HTTP client's `*url.Error`, whose text is the whole URL; `socketIODialError` rebuilds the failure from host, operation and cause so the credential never reaches the first-retry Info line or the lifecycle event (`TestSocketIOWakeSource_DialFailureNeverLogsTheNotificationURL`)

Important properties:

- observation emits wake/dirty signals plus normalized remote facts
- delta windows that carried no items (apart from the root) do **not**
  advance the saved cursor; the old token replays the empty window at no cost
  while a recent delete may still be propagating
  ([graph-api-quirks.md](../reference/graph-api-quirks.md))
- a window that carried items does advance the cursor even when every item
  converted to no event (for example deletes of items never synced here).
  Treating that as an empty window kept the old token, so watch mode replayed
  the same deletes on every poll indefinitely
  (`TestWatch_OnlyUnrecoverableDeletes_AdvancesToken`)
- an incremental window that deletes a folder the baseline holds makes the
  full refresh due within 15 minutes (`deltaWindowDeletesSyncedFolder`,
  `fullRefreshAfterFolderDeleteDelay`; deferred so a burst of folder deletions
  shares one enumeration and refreshes stay that far apart). Delta reports a
  deleted folder alone (R-2.17.2), and that also hides an item moved into the
  folder in the same window: its row stayed at the old path as a ghost and its
  synced local copy was never deleted. The full refresh's orphans are confirmed
  by ID before anything is deleted. The echo of a folder this client deleted
  is no longer in the baseline and costs nothing
  (`TestRunOnce_ItemMovedIntoRemotelyDeletedFolderIsDeletedLocally`,
  `TestHandleRemoteObservationBatch_SyncedFolderTombstonePullsFullRefreshForward`,
  `TestRunOnce_VirtualTime_FolderDeletesSpaceFullRefreshes`)
- a full refresh whose lookup of an orphan failed keeps the item (absence is
  not proof) and brings the next full refresh forward: the delta position
  saved with the refresh never reports that item again, so the next refresh,
  not a day-later one, has to judge it. The delay starts at 15 minutes and
  doubles per consecutive undecided refresh up to the daily cadence
  (`observation_state.undecided_refresh_streak`), so a lookup that keeps
  failing does not re-enumerate the drive every 15 minutes forever; a refresh
  that decides every orphan resets it. A shared-folder mount's complete
  folder delta does the same; a mount listed in full every pass needs nothing,
  as the next listing judges the orphan again
  (`TestFullRefresh_UndecidedOrphanPullsTheNextRescanForward`,
  `TestFullRefresh_PermanentlyUndecidedOrphanBacksOff`,
  `TestRunOnce_MountRootUndecidedOrphanPullsTheNextRescanForward`)
- HTTP 410 delta expiry is surfaced as `deltaExpiredError`, which carries the
  resync flavor from the error's inner code; recovery is the engine's full
  refresh with orphan detection, in watch mode too (`SetDeltaExpiredRecovery`,
  `sync-engine.md` "An Expired Token Is A Full Refresh", R-2.15.3)
- a cursor the client cannot continue takes the same recovery, through
  `newCursorUnusableError` (R-2.15.2): `deltaLoopGuard` stops a window whose
  nextLink returns to a page already fetched or whose non-empty page repeats
  an earlier page's items (followed, such a list ran to the 10,000-page guard
  with every item in memory); `fetchPageRetryingRejection` retries a token
  refused with a non-transient 4xx once and gives up on the second refusal;
  and watch mode's `cursorServerErrorTracker` gives up on a token that has
  failed with 5xx for `cursorServerErrorResetAfter` (6 h). Every other failure
  keeps the token and backs off
- sparse delta items recover omitted name/parent data from baseline when
  possible, and missing parent IDs trigger one best-effort item-by-ID enrich
  before baseline fallback
- deleted-item names may also be recovered from baseline context
- deleted delta items that are absent from baseline require a surviving item
  name plus enough path or parent-chain context before observation may emit a
  delete; missing-name untracked deletes are skipped even when the parent is
  known, because the child path cannot be identified safely. Such a skip is
  logged at DEBUG: there is nothing local to remove and no user action
- explicit remote read denial discovered during observation is emitted as an
  observation-owned finding (`remote_read_denied`) plus
  `perm:remote:read:<boundary>`
- observation-owned read-denial boundary facts are subtree boundaries:
  descendants become unavailable for planning and status through the tagged
  boundary issue rather than through synthetic per-descendant issue rows or
  execution-time permission retries

### Drive-root and mount-root observation

The engine now has two remote observation shapes:

- drive-root observation for ordinary drives
- mount-root observation for runtimes scoped below the remote drive root

There is no nested shared-folder-following runtime inside the content engine. If
a normal drive's delta stream contains an embedded shared-folder link or
shortcut placeholder item, observation suppresses that item from ordinary
content events. The parent drive engine is still the only Graph observer for
that drive, so it converts shortcut placeholders into engine-internal raw
shortcut observation facts before committing remote observation progress. The parent
persists those facts in `shortcut_roots` and publishes parent-declared child
process commands; `internal/multisync` only starts, skips, final-drains, or
stops managed children as separate mount-root engines.

The engine applies topology batches with its internal `shouldApply` gate.
Any batch with facts applies, a complete batch applies even when it contains no
facts, and an empty incremental batch is a non-event. This lets a full parent
enumeration with no shortcut aliases retire old managed children while keeping
ordinary zero-event delta polls from advancing observation progress.

Separately configured shared folders and managed shortcut child mounts use the
mount-root path when their content root is below the backing drive root.
Mount-root observation may use folder delta or recursive enumeration depending
on drive type and Graph support.

Parent local observation rebuilds protected shortcut-root paths from the
parent sync store. A protected path records its boundary `local_state` identity
but suppresses descendants from normal create/move/delete planning. Shortcut
root rename/move/delete policy is then layered on top of the same generic
filesystem identity facts used for ordinary local move detection; a unique
identity match at a new path is a parent shortcut alias move, while ambiguous
matches become parent-owned recovery state. Parent alias mutations are executed
by the parent engine by binding item ID; multisync receives only the resulting
child work commands.

When remote shortcut topology moves or renames an existing binding inside the
same parent namespace, the parent keeps the previous protected path reserved
while it moves the local child projection to the new alias path. The child mount
ID and child content state stay bound to the shortcut binding item ID rather
than the alias path.

Remote read-denied boundaries are observation-owned facts. When drive-root or
mount-root observation proves that remote truth is unreadable, the engine
persists one managed observation batch containing:

- `observation_issues` rows for `remote_read_denied`
- `ScopeKey` tags of `perm:remote:read:<boundary>` on those issue rows

Healthy observation batches reconcile that managed set away again. Worker `403`
results do not create these read-denied findings.

## Item Conversion

`item_converter.go` is the single item-to-observation normalization path.

It owns:

- NFC normalization
- drive/item identity handling
- best-effort item-by-ID enrich for sparse delta items missing `ParentID`
  or known shortcut target fields
- path reconstruction from parent chains
- move detection against baseline
- sync-only suppression of Graph package/OneNote items and of bundles
  (albums), with their descendants (R-2.4.16)
- kind without guessing (R-2.4.16): a non-deleted item with none of the file,
  folder, root, package, bundle or remoteItem facets
  (`graph.Item.TypeFacetMissing`) keeps its baseline kind, and for a file its
  baseline content facts, since a facetless update carries no hashes; without
  a baseline it gets one `GetItem` re-read through the sparse-item enrich and
  is skipped if that still does not say, or if Graph answers not found (the
  item is gone; its deletion arrives in a later window). A re-read that fails
  for any other reason marks the observation incomplete
  (`errRemoteObservationIncomplete`), so the window is replayed and the delta
  cursor is not saved past an item that was never classified: the same rule
  as a parentless item whose re-read fails (`sparseEnrichFailed` records the
  error per item). Read as a file by default, a sparse update of a synced
  folder planned a folder-to-file type change that deleted the local folder
- a negative size is stored as unknown (`SizeUnknown`, a NULL
  `remote_state.size`), never as a size
- sync-only suppression of Personal Vault content
- malformed sparse-item rejection, including names that cannot be one local
  path segment (`.`, `..`, `/`, NUL, invalid UTF-8): the item and its
  descendants are skipped with a warning, never folded into another path
  (`TestFullDelta_RejectsUnmaterializableRemoteNames`). Names are otherwise
  opaque: the Graph client no longer URL-decodes them
- mount-root path materialization relative to the engine's configured root

Observation emits current truth for the mount root only. It does not carry a
separate per-event target-root field for later planning or execution.

Sparse parent recovery is intentionally layered:

- first use the delta item directly when it already carries `ParentID`
- otherwise try one best-effort `GetItem(driveID, itemID)` enrich before
  materializing the path
- if enrich still leaves the parent missing, use the baseline ancestry of a
  synced item or an exact Graph `parentReference.path`; a live item with
  neither is skipped rather than placed at the drive root, and its window is
  replayed only when the enrich read itself failed
  (`TestFullDelta_ItemWithoutParentReferenceIsNotPlacedAtRoot`)
- the parent-ID chain (window, then baseline) places an item before
  `parentReference.path` is consulted: IDs are the authority, and the
  client used to decode that path, which turned a folder literally named
  `a%20b` into `a b`
  (`TestFullDelta_ParentIDChainWinsOverParentReferencePath`)
- if the parent chain still leaves both the window and the baseline (an
  incremental window carries a change inside a folder this client never put
  in its baseline, such as a suppressed OneNote package or Personal Vault
  folder, or a folder whose own download has not succeeded yet), read each
  missing ancestor by ID with `GetItem` and register it as path context only
  (never as an event), bounded per window by `maxAncestorFetchesPerWindow`.
  The whole window is collected before anything is classified, so the budget
  is spent only on ancestors no page of the window carries: resolved page by
  page, a child that arrived pages before its folder spent a read on it, and
  a large window exhausted the budget and replayed identically forever.
  Ancestors read in a window that stays incomplete are carried to its replay
  (`carriedAncestors`, forgotten once a window completes), so a window that
  needs more reads than one budget completes after a few replays (R-2.15.4)
  A fetched ancestor carries its vault and package flags, so a child inside
  the Personal Vault or a OneNote package is still skipped exactly as if that
  ancestor had been in the window (`TestWatch_OrphanUnderFetchedVaultStaysExcluded`,
  `TestWatch_OrphanUnderFetchedPackageStaysExcluded`)
- if Graph answers a missing ancestor as not found or forbidden, no replay can
  ever place the item: skip it with one warning and let the window advance,
  so a permanently orphaned item cannot hold the cursor forever
  (`TestWatch_OrphanWithUnreachableParent_AdvancesToken`)
- otherwise (a transient read failure, or the fetch bound reached) fail the
  batch as `errRemoteObservationIncomplete` rather than fabricating a
  root-level path, and retry it on the next poll
  (`TestWatch_OrphanWithReadableParent_AdvancesToken`,
  `TestFullDelta_OrphanParentReadTransientFailure_StaysIncomplete`)

Observation does not fail merely because the best-effort enrich read returns an
error. The enrich read exists only to reduce sparse-delta blind spots before
path materialization; durable ancestry still lives in `baseline`, not in a
second remote-observation store field. Once materialization still fails after
those sources are exhausted, the batch is incomplete and the cursor must not
advance.

Remote observation persists raw manageable `remote_state` rows. Product content
filters are applied when the engine builds planner-visible local/remote views,
not by deleting raw remote mirror rows. Delta/watch handling may suppress
planner wakeups for batches whose emitted events are entirely outside the
filtered view, but cursor advancement remains tied to successfully committed raw
observation progress.

Known managed shortcut aliases also use persisted protected-root target metadata
as a same-binding fallback when Graph returns a sparse moved/renamed shortcut
placeholder without `remoteItem` target fields. The parent engine still owns the
observation and mutation; this fallback only keeps the same shortcut binding
classified as parent-declared child work commands instead of treating an empty
placeholder delta as ordinary content or an unavailable new child.

## Local Observation

`localObserver` and `Scanner` observe the configured local root through the
drive's compiled `contentFilter`. The filter is product visibility policy only:
`included_dirs`, `ignored_dirs`, `ignored_paths`, `ignore_dotfiles`,
`ignore_junk_files`, and `follow_symlinks`. It is not the owner of provider
structural exclusions or OneDrive admissibility validation.

Full local observation persists the current visible local snapshot to
`local_state`. A filter change causes the watch runtime mount to restart; the
startup/full local observation then rewrites `local_state` under the new
visibility rules. Local observation also uses the filter early to avoid walking,
hashing, watching, and reporting issues for ignored or out-of-include paths.

Observation reads real names (the walk and the event paths) and stores them
in NFC. The few places that go back to the filesystem from a stored path use
the spelling on disk (`synctree.Root.Spelling`, R-2.13.2): the full scan's
re-watch of directories it found unwatched, the "synced path is gone" check
behind move pairing, and the presence check behind a delete event, which
looks up the event's own path instead of the stored one. On a byte-exact
filesystem the stored NFC spelling of an NFD-named entry does not exist, so
those checks used to fail the watch add, read an unchanged file as moved
away, and read an atomic save as a delete.

Watch mode's case-collision peers follow the same rule. A held-back create
records the colliding sibling under its stored (NFC) path, the path that
sibling's own events carry, and when the collision is resolved by a delete
the survivor is re-emitted with its stored path and name and stated at its
spelling on disk (`onDiskSiblingName`). The peer used to be recorded under the
name the directory listing returned, which is NFD for a file copied from a
Mac: its re-emitted create named an item neither the baseline nor OneDrive
knows, and its own delete event, carrying the NFC path, never found the peers
waiting on it.

Watch-mode local observation also maintains `local_state` incrementally from
re-observed scopes. File create/modify handlers stat and hash the exact file and
emit an upsert row. File delete/rename-away handlers emit an exact path delete.
Directory create handlers add watches, scan the new subtree, and emit rows for
the observed directory tree. Directory delete/rename-away handlers emit a prefix
delete for the removed directory. Safety scans emit a full local snapshot
replacement and clear any suspect-local-truth marker after the engine commits
the replacement. The snapshot carries no dirty hint of its own: the runtime
replans when the snapshot changes committed truth (a row, or the truth-complete
marker, R-2.8.17) and when the scan's skipped findings change the persisted
issue set, which the engine reconciles from the separate skipped-items stream
and reports as changed or not. A scan's events are counted against the
baseline, so during a backlog every safety scan re-reports every change the
installed plan already covers; marking the runtime dirty for them retired the
admitted plan at every safety scan, and with a replan slower than the safety
interval starved dispatch entirely (R-2.8.21, the 2026-11-01 livelock on
11,000 entries).

Watch registration is bookkept per directory in `watchedDirs`, a
`watchRegistry` that records each directory's identity (device and inode) at
registration and indexes registrations by parent, so a directory's registered
subtree is found in time proportional to that subtree. A Create for a
directory already registered with the same identity is a duplicate (fsnotify's
event racing the new-directory scan); it emits the directory's row but does
not drop, re-add or re-scan the subtree. When a watched
directory is deleted or renamed away, the observer removes the watch on it and
on every watched directory beneath it, deepest first. inotify watches follow the
inode while fsnotify keys registrations by path, so leaving descendant watches
in place made a renamed tree keep reporting its old paths after it was re-added,
and a tree moved out of the sync root keep reporting changes as if it were
still inside. A directory created where watched descendants are still
registered replaced one that vanished while watched (its delete event is
ignored when the path exists again), so those stale registrations are dropped
before the new tree is registered.

When the operating system cannot provide the watches -- inotify watch limit
(ENOSPC), inotify instance or open-file limit at creation (EMFILE, ENFILE),
kernel memory (ENOMEM) -- the observer returns a `localWatchUnavailableError`
carrying the cause, and the watch runtime falls back to full scans with one
warning per cause that names the limit and its fix. The fallback scans at once
(the stopped watcher's pending hashes and recovery scan are gone with it), then
at the safety-scan interval, and retries the watcher after
`localWatchRetryInitialDelay` (10 minutes), doubling to
`localWatchRetryMaxDelay` (an hour) while the limit persists; a watcher that
then ran for an hour or more before failing again starts a fresh episode.
`Watch` resets its registrations and stop reason on every call, so it can be
re-entered after a fallback period. The same applies to a
descriptor-limit failure on a new folder after startup, because on kqueue every
watched directory holds a descriptor and keeping them starves hashing,
transfers and SQLite. A watch or memory limit hit for a new folder after
startup leaves existing watches healthy: it warns once per cause and the
safety scan covers that folder.

A rename arrives as the delete of the old path, sent at once, and a create of
the new one. File creates are normally hashed in the watch loop before their
row is emitted; for the destination of a rename that read could outlast the
replan debounce, and the replan then saw only the delete and planned a remote
delete instead of a move. So a created file whose filesystem identity matches a
synced file at another path -- and only when that path is confirmed gone; if it
still exists the inode was freed and reused by unrelated content, which is
hashed like any create -- reuses that file's synced hash when size and mtime
are unchanged (no read at all, so a renamed tree is not re-read), and otherwise
is emitted at once with an unknown hash -- move pairing accepts an unknown hash
on equal mtime -- and hashed through the deferred-hash path. "Unchanged" is the
full R-2.2.4 rule, ctime included, and the kernel moves a file's ctime when the
file itself is renamed, so a renamed single file takes the deferred-hash path;
a renamed directory leaves its children's ctimes alone, so its tree is still
not re-read.

### Hash Reuse Trusts Only Exact Local Facts

`canReuseBaselineHash` (`local_hash_reuse.go`) is the one rule the full scan
and the watch create path use to skip reading a file (R-2.2.2, R-2.2.4). The
baseline hash is trusted only when size, mtime at full nanosecond precision,
ctime and (device, inode) all equal the baseline, and both mtime and ctime are
at least a second older than the scan start (the racily-clean guard; Linux
stamps ctime from a coarse clock, so two writes in one tick share it).

- Whole-second truncation (R-6.7.15) is for comparing a local time with
  OneDrive's wire value. Every value compared here is a local observation, and
  truncating them hid sub-second edits.
- ctime is the only timestamp no unprivileged call can set back. `cp -p`,
  `touch -r`, `rsync -t --inplace` and mmap writes followed by `utimes` restore
  size and mtime but move ctime. The stale hash used to be reused, written to
  `local_state`, and made permanent by the `baseline_update` that followed;
  a later remote edit then downloaded over the unsynced edit.
- Identity catches a different file renamed over the synced one with the same
  size and mtime.
- A baseline with no ctime or identity proves nothing and is re-read.
- Rows record ctime for regular files only: a folder's ctime moves with every
  child. The ctime a row carries is always stated before the content is read
  (walk stat, watch stat, `computeStableHashWith` pre-stat, which also rejects a
  read during which ctime moved), so a write racing the read leaves a newer
  ctime and forces the next re-read rather than hiding behind the recorded one.
- A rolling re-verify budget (P-12) is not implemented: content that changes
  without any metadata change (bit rot) is still only caught when something
  else forces a read. A created
directory's own row is emitted before its contents are scanned.

Write coalescing (B-107) restarts a path's quiet-period timer on every write,
but never past `maxWriteCoalesceWait` (30 s) after the first pending write, so a
file written more often than the quiet period is still observed. A path that
emits `unstableWriteEmits` content changes within `unstableWriteWindow` is
unstable: its next hash waits `unstableWriteInitialBackoff`, and every later
emit whose hash differs from the previous emit doubles the wait up to
`unstableWriteMaxBackoff`; writes during the wait do not restart it. A file
still changing after `maxCoalesceRetries` re-hashes is not emitted with an
empty hash (that would only plan an upload of a file in mid-write); it stays
pending with an escalated backoff.
The first time a path becomes unstable the observer logs one Info line that
suggests excluding it. The per-path history is watch-loop state, reset when the
path goes quiet and pruned at safety scans.

fsnotify has no FSEvents backend: on macOS and FreeBSD it uses kqueue, which
holds an open descriptor for every watched directory and every file inside one.
Watch setup therefore counts the entries of each directory it watches (a
watched subdirectory reuses its entry's descriptor, so it is counted once) and
stops with `watchCauseDescriptorBudget` once those entries plus the root exceed
`watchDescriptorBudget()`, half of `RLIMIT_NOFILE` (zero, meaning no budget,
on Linux, where inotify watches are not open files). The runtime then falls
back to periodic full scans. An FSEvents watcher behind `fsWatcher` is the
durable fix and is planned separately (P-15).

At watch start the observer identifies the sync root's filesystem with statfs
(`localFilesystemKind`, in the platform files). Changes that reach a network or
FUSE filesystem without going through this kernel raise no local events, so on
such a root it warns once and, when the safety-scan interval is the default,
shortens it to `networkSafetyScanInterval` (2 minutes); a non-default interval
is left alone. statfs reports one magic for all FUSE mounts, so on Linux the
mount table (`/proc/self/mountinfo`) tells a block-backed `fuseblk` driver
for a local disk (ntfs-3g, exfat-fuse), which is treated as local, from a
network FUSE server such as `fuse.sshfs`.

Raw fsnotify events are not a durable journal. They only choose the observation
scope. The observer performs normal filesystem reads for that scope, emits a
batch, and the watch loop applies that batch to SQLite. If the observer drops a
local observation batch, drops a delayed hash request, sees a watcher error, or
cannot complete a safety scan, the watch runtime marks
`observation_state.local_truth_complete=false` with a recovery reason and
schedules a normal dirty replan/full local refresh. The observer itself
arms one recovery rescan after the first such signal; further signals coalesce
into it and any full scan that starts disarms it (a generation number makes a
timer that fires after Stop harmless). The delay is at least
`recoveryScanDelay` (10 s) and twice the last full scan's duration, doubles
while requests keep arriving before the previous delay has passed since the
last recovery scan ended, and is capped at the safety-scan interval, so a
sustained event storm on a large tree cannot keep the observer rescanning back
to back, and missed changes are still observed well before the safety scan.
A due scan, safety or recovery, whose snapshot could not be handed over because
the runtime has not taken the observations already sent (the batch channel is
full: its loop is inside a long replan) is deferred, not run: it requests a
recovery scan, which stays armed at the base delay (a deferral is not a scan
that ended, so it does not double the pace) until the channel has room, and
the first scan after that covers everything the deferred ones would have.
Before this each deferred scan ran, dropped its snapshot, and re-armed the
next at twice the delay: six full scans in ten minutes that delivered nothing
(R-2.8.21).
Every full scan in a running watch then re-adds watches for scanned
directories missing from `watchedDirs`, parents first and at most
`maxRewatchPerScan` per scan: event-driven registration misses directories
created while events were lost and directories whose `Add` failed, which
otherwise stayed unwatched for the session. A failed `Add` is not remembered,
so the next full scan tries again; a resource limit ends the pass early. Local-state absence is
therefore only trustworthy to later stale-work validation when local truth is
marked complete.

The same engine-owned apply path records production perf counters for scoped
local observation: scoped commit count, upsert count, exact path deletes,
prefix deletes, full snapshot replacements, and suspect-local-truth recovery
reasons. Those counters are aggregate-only; they never expose paths, inode
facts, item IDs, or drive IDs.

Managed shortcut child subtrees are not configured as ordinary skip dirs.
Parent engines derive protected roots from `shortcut_roots` and update their
own observer state internally. Parent shortcut filters are translated into
child-root-relative `contentFilter` values when the parent publishes child work
commands.

Full local scans persist `local_device`, `local_inode`, `local_birth_time` and
`local_has_identity` for files and directories on platforms that expose stable
device/inode identity. Unsupported platforms persist `local_has_identity=false`
and keep hash-based file move fallback behavior for files only.

`local_birth_time` is the entry's creation time in Unix nanoseconds, zero when
unknown (R-2.17.1). On Linux the standard stat carries none, so the sync tree's
handle reads `statx(2)` `STATX_BTIME` for every rooted `Stat`/`Lstat` and for
the `Info` of every walked or listed entry (`synctree.withBirthTime`), and
keeps it only when the mask bit is set and statx names the same device and
inode; ext4, xfs, btrfs, f2fs and tmpfs on current kernels report it. macOS
and the BSDs read `st_birthtimespec` from the stat itself. Watch mode stats by
absolute path (`statObservedPath`, the listing of a new directory) and attaches
the birth time there with `synctree.AttachBirthTime`, so a watch event records
the identity a full scan would.

Built-in local observation policy remains:

- validate OneDrive-invalid names before they become upload work
- count the 400-character path limit in UTF-16 code units, the unit OneDrive
  measures (R-2.11.10, `TestFullScan_LongNonASCIIPathWithinTheCharacterLimitIsObserved`);
  counting UTF-8 bytes skipped valid CJK and accented paths
- report invalid names, path length, file size, case collisions, read-denied
  paths, and hash failures as observation issues for visible paths only
- "the same name" means OneDrive's name, decided by `internal/namekey` (R-2.12.3):
  full case folding (`straße`/`STRASSE`), canonical equivalence (an NFD and an
  NFC twin), and supplementary-plane characters ignored (`a.txt` collides with
  `a😀.txt`; the issue detail names both files and says why). Full scans,
  watch-mode collision caches and the baseline sibling index all group by that
  key (`TestDetectCaseCollisions_FullCaseFoldingCollides`,
  `TestDetectCaseCollisions_EmojiOnlyDifferenceCollidesAndNamesBothFiles`,
  `TestHasCaseCollisionCached_NFDOnDiskNameDoesNotCollideWithItself`)
- a case collision needs both names on disk: a baseline name the scan no
  longer finds is the old half of a case-only rename (or a local delete), not
  a collider; the planner orders the remote delete or move of the old name
  before the create of the new one
- skip symlinks by default; when `follow_symlinks=true`, observe at the alias
  path with cycle protection and executor no-follow delete safety; observed
  facts are the followed target's, and the executor's upload precondition
  re-reads them the same followed way (`sync-execution.md`, Symlink Boundary
  Policy), so a file beneath a directory alias to another disk uploads
- never let unreadable content look deleted (R-2.4.12): any walk error other
  than a vanished entry, an unreadable directory reached through a followed
  symlink, and a followed symlink whose target is missing or unreadable all
  record a `local_read_denied` read boundary at that path; the planner then
  suppresses remote deletes beneath it, exactly as for a permission-denied
  directory in the main walk
- keep scanner/watch behavior aligned for hashing and case-collision detection

Ignore invariants:

- product ignore/include policy is symmetric at the planner-visible view
- ignored local items do not enter `local_state`
- remote observation persists raw manageable `remote_state`; planner loading
  filters the remote current-state view before comparison
- remote delta wake optimization is driven by whether the emitted path or old
  path is visible under the current `contentFilter`; hidden-to-hidden changes
  advance raw observation state without waking planning
- if a baseline path is absent from both filtered current views, reconciliation
  removes only the baseline row

Observation may emit `skippedItem`s for invalid or unsupported local content.
The engine reconciles those findings through one `ObservationFindingsBatch`
boundary, and `observation_findings.go` is the single constructor path for
those batches. Whole-drive scans replace the managed family set for those
findings, which is the only batch shape in use now that path-scoped
single-path observation has been removed.

Observation-owned read-denied subtree boundaries stay boundary-scoped. The
durable batch records one boundary issue tagged with the matching read
`ScopeKey`; descendants are made unavailable from that boundary fact during
planning instead of receiving synthetic per-descendant issue rows.

Observation-owned local read findings use these rules:

- unreadable file -> `observation_issue(path, local_read_denied)` only
- unreadable directory boundary ->
  `observation_issue(boundary, local_read_denied)` with boundary `ScopeKey`
  `perm:dir:read:<boundary>`
- invalid or unsyncable local content -> observation issue only

Single-path observation was removed. Retry and trial flows once rebuilt local
truth for one path through an `observeSinglePathWithFilter` family, wired from
`engine_retry_trial.go`. The linear-runtime refactor replaced that model: the
runtime now replans from committed observation in the store and matches
`retry_work` rows against the resulting action set, while staleness is caught
at the side-effect boundary by executor live preconditions. The single-path
family survived that change with no production caller and stayed alive only
because its own tests referenced it, which is why `unused` never reported it.
It is deleted; replanning from committed truth is the single owner.

`Scanner.FullScan()` now also returns direct `localStateRow` values for every
admissible currently observed local path. `local_state` persistence therefore
comes from current disk truth, not by replaying local change events against
baseline.

Dry-run uses those same direct `localStateRow` values, but commits them only to
an isolated scratch store before SQLite comparison/reconciliation. Observation
findings discovered during preview also reconcile only into that scratch store.
The durable runtime store keeps its prior committed snapshots, observation
issues, and block scopes unchanged during preview.

### Unavailable Is Not Absent

`local_state` holds only what the scan could represent, and SQLite reads a
synced path missing from it as deleted. `scanner_unavailable.go` therefore
turns every "could not represent" case into an observation issue on the
*synced* path; the planner's truth overlay treats it as unavailable local truth,
and a folder issue is a read boundary (`perm:dir:read` scope) covering the
subtree:

- a synced path now occupied by a symlink sync does not follow, or by a FIFO,
  socket or device: `local_unsyncable_replacement` at that path (D-004);
- a synced item renamed to a name OneDrive refuses, a too-long path, or an
  excluded name (a dotfile under `ignore_dotfiles`, P-5):
  `local_unsyncable_replacement` at the old synced path, found by matching the
  skipped entry's (device, inode) against baseline identities; a regular file
  with other hard links is not evidence and is skipped;
- an entry the walk listed but could not stat (any error but ENOENT) and a
  file whose content could not be hashed: a read boundary or `hash_error`
  (D-005), never an empty hash that reads as a change;
- a folder whose device differs from its parent's, both read in the same scan,
  is a mount point; unless `cross_filesystems = true` it is not descended and
  is recorded as `local_mount_boundary` (D-003, P-11). Devices are only
  compared within one scan, because device numbers change between boots;
  the sync folder's own device is required, and a scan that cannot read it
  stops as an unavailable sync folder;
- a mount point crossed under `cross_filesystems` is recorded in
  `mount_boundaries` with the snapshot. A later scan that finds that folder on
  its parent's device treats the disk as not mounted: a `local_mount_boundary`
  read boundary, not deletions, however few files it held. A flagged folder
  still on its own device is the mounted disk, so emptying it is an ordinary
  delete. `drive rebind-root` forgets recorded mount points. Watch safety scans use the flags but
  only one-shot and bootstrap scans record new ones.

A synced item moved *into* an excluded folder is still invisible: the walk
does not descend excluded folders, so its identity is never seen. That case
remains a delete on OneDrive (recycle bin) until the comparison can carry
skipped identities (see the WS5 comparison work).

In watch mode a create event for an entry sync skips does not produce a row.
When that entry occupies a synced path, or carries a synced item's identity
(looked up in an index rebuilt at most once a minute), the handler runs a
safety scan immediately, so the issue reaches the store within the same
debounce window as the rename's delete half.

## Dirty Buffer

`dirtyBuffer` owns short-lived replan scheduling only:

- debounce bursts of local or remote dirty signals
- wait 5 seconds from the last local or remote observation before triggering replanning by default
- but never longer than `dirtyMaxWait` (30 seconds, or six debounce periods
  when the debounce is configured longer) from the first unflushed signal: a
  trailing debounce alone let sustained local churn postpone every replan, and
  with it every remote change, indefinitely
- keep one pending coarse dirty signal plus a full-refresh bit
- flush deterministically on shutdown/final drain

It is not a durable queue, not a semantic event journal, not a second planning
input, and not a second source of truth. Local watch prefiltering still has to
be directory-capable before stat/type resolution: when an fsnotify event arrives
with unknown kind, content filtering treats the path as visible if either a file
or a directory at that path could be sync-visible. That preserves timely watch
behavior for exact included directory roots and include ancestors while the later
stat/type-aware observation stage performs the authoritative file-vs-directory
decision.
