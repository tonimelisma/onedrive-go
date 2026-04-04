package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/syncrecovery"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// forceSafetyMax is the maximum threshold used when --force is set,
// effectively disabling big-delete protection.
const forceSafetyMax = math.MaxInt32

// RunOnce executes a single sync pass:
//  1. Load baseline
//  2. Observe remote (skip if upload-only)
//  3. Observe local (skip if download-only)
//  4. Buffer and flush changes
//  5. Early return if no changes
//  6. Plan actions (flat list + dependency edges)
//  7. Return early if dry-run
//  8. Build DepGraph, start worker pool
//  9. Wait for completion, commit delta token
func (e *Engine) RunOnce(ctx context.Context, mode synctypes.SyncMode, opts synctypes.RunOpts) (*synctypes.SyncReport, error) {
	start := time.Now()
	runner := newOneShotRunner(e)

	e.logger.Info("sync pass starting",
		slog.String("mode", mode.String()),
		slog.Bool("dry_run", opts.DryRun),
		slog.Bool("force", opts.Force),
	)

	bl, shortcuts, err := runner.prepareRunOnceState(ctx)
	if err != nil {
		return nil, err
	}

	// Steps 2-4: Observe remote + local, buffer, and flush.
	// The pending delta token is returned but NOT committed yet — it is
	// deferred until after the planner approves the changes (step 6).
	changes, pendingDeltaToken, err := runner.observeChanges(ctx, nil, bl, mode, opts.DryRun, opts.FullReconcile)
	if err != nil {
		return nil, err
	}

	// Step 5: Early return if no changes.
	if len(changes) == 0 {
		e.logger.Info("sync pass complete: no changes detected",
			slog.Duration("duration", time.Since(start)),
		)

		report := &synctypes.SyncReport{
			Mode:     mode,
			DryRun:   opts.DryRun,
			Duration: time.Since(start),
		}
		// Persist sync metadata even when no changes detected.
		if metaErr := e.baseline.WriteSyncMetadata(ctx, report); metaErr != nil {
			e.logger.Warn("failed to write sync metadata", slog.String("error", metaErr.Error()))
		}

		return report, nil
	}

	// Step 6: Plan actions.
	safety := e.resolveSafetyConfig(opts.Force, false)
	denied := e.permHandler.DeniedPrefixes(ctx)

	plan, err := e.planner.Plan(changes, bl, mode, safety, denied)
	if err != nil {
		// Big-delete protection (or other planner errors) — the delta token
		// is NOT committed, so the next sync replays the same events.
		return nil, fmt.Errorf("sync: planning actions: %w", err)
	}

	// Planner approved — commit the deferred delta token now.
	if err := runner.commitDeferredDeltaToken(ctx, pendingDeltaToken); err != nil {
		return nil, err
	}

	// Step 7: Build report from plan counts.
	counts := syncplan.CountByType(plan.Actions)
	report := buildReportFromCounts(counts, mode, opts)

	if opts.DryRun {
		report.Duration = time.Since(start)

		e.logger.Info("dry-run complete: no changes applied",
			slog.Duration("duration", report.Duration),
		)

		return report, nil
	}

	// Store shortcuts so result handling and permission rechecks can access
	// them during plan execution.
	runner.setShortcuts(shortcuts)

	// Execute plan: run workers, drain results (failures, 403s, upload issues).
	if err := runner.executePlan(ctx, plan, report, bl); err != nil {
		report.Duration = time.Since(start)
		return report, err
	}

	report.Duration = time.Since(start)

	e.logger.Info("sync pass complete",
		slog.Duration("duration", report.Duration),
		slog.Int("succeeded", report.Succeeded),
		slog.Int("failed", report.Failed),
	)

	runner.postSyncHousekeeping()

	// Persist sync metadata for status command queries.
	if metaErr := e.baseline.WriteSyncMetadata(ctx, report); metaErr != nil {
		e.logger.Warn("failed to write sync metadata", slog.String("error", metaErr.Error()))
	}

	return report, nil
}

func (r *oneShotRunner) prepareRunOnceState(ctx context.Context) (*synctypes.Baseline, []synctypes.Shortcut, error) {
	eng := r.engine
	flow := &r.engineFlow

	proof, proofErr := eng.proveDriveIdentity(ctx)
	if proofErr != nil {
		hasAuthScope, err := eng.hasPersistedAuthScope(ctx)
		if err != nil {
			return nil, nil, err
		}
		if !hasAuthScope {
			return nil, nil, proofErr
		}
	}

	if err := flow.scopeController().repairPersistedScopes(ctx, nil, proof, proofErr); err != nil {
		return nil, nil, fmt.Errorf("sync: repairing persisted scopes: %w", err)
	}
	if proofErr != nil {
		return nil, nil, proofErr
	}
	eng.logVerifiedDrive(proof)

	// Crash recovery: reset any in-progress states from a previous crash.
	// Also creates sync_failures entries so the retrier can rediscover items
	// that were mid-execution when the crash occurred.
	if err := syncrecovery.ResetInProgressStates(ctx, eng.baseline, eng.syncTree, retry.ReconcilePolicy().Delay, eng.logger); err != nil {
		eng.logger.Warn("failed to reset in-progress states", slog.String("error", err.Error()))
	}

	bl, err := eng.baseline.Load(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("sync: loading baseline: %w", err)
	}

	shortcuts, scErr := eng.baseline.ListShortcuts(ctx)
	if scErr != nil {
		eng.logger.Warn("failed to load shortcuts", slog.String("error", scErr.Error()))
	}

	// Recheck permissions — clear any permission_denied issues
	// for folders that have become writable since the last pass.
	if eng.permHandler.HasPermChecker() && scErr == nil {
		requestedKeys, reqErr := eng.baseline.ListRequestedScopeRechecks(ctx)
		if reqErr != nil {
			eng.logger.Warn("failed to list requested permission rechecks", slog.String("error", reqErr.Error()))
		}
		decisions := eng.permHandler.recheckPermissions(ctx, bl, shortcuts)
		flow.scopeController().applyPermissionRecheckDecisions(ctx, nil, decisions)
		if len(requestedKeys) > 0 {
			clearRequestedScopeRechecks(ctx, eng.baseline, eng.logger, requestedKeys)
		}
	}

	// Recheck local permission denials — clear scope blocks for
	// directories that have become accessible since the last pass (R-2.10.13).
	flow.scopeController().applyPermissionRecheckDecisions(ctx, nil, eng.permHandler.recheckLocalPermissions(ctx))

	return bl, shortcuts, nil
}

// postSyncHousekeeping runs non-critical cleanup after a sync pass:
// .partial deletion and session file cleanup. Synchronous — completes
// before RunOnce returns to guarantee cleanup on process exit.
func (flow *engineFlow) postSyncHousekeeping() {
	driveops.CleanTransferArtifacts(flow.engine.syncTree, flow.engine.sessionStore, flow.engine.logger)
}

// executePlan populates the dependency graph and runs the worker pool.
// The engine processes results concurrently while workers run, classifying
// each result and calling depGraph.Complete (R-6.8.9).
//
// One-shot mode has NO watch-mode active-scope admission loop — all actions
// with satisfied deps go directly to readyCh. Scope detection (ScopeState) is
// absent in one-shot; watch-only lifecycle paths are nil-guarded → no-op.
func (r *oneShotRunner) executePlan(
	ctx context.Context, plan *synctypes.ActionPlan, report *synctypes.SyncReport,
	bl *synctypes.Baseline,
) error {
	if len(plan.Actions) == 0 {
		return nil
	}

	// Invariant: Planner.Plan() always builds Deps with len(Actions).
	// Assert here to catch any future regression that breaks this contract.
	if len(plan.Actions) != len(plan.Deps) {
		r.engine.logger.Error("plan invariant violation: Actions/Deps length mismatch",
			slog.Int("actions", len(plan.Actions)),
			slog.Int("deps", len(plan.Deps)),
		)

		report.Failed = len(plan.Actions)
		report.Errors = append(report.Errors,
			fmt.Errorf("plan invariant violation: %d actions but %d deps", len(plan.Actions), len(plan.Deps)))

		return nil
	}

	// Reset engine counters for this pass.
	r.resetResultStats()

	// One-shot mode: DepGraph + readyCh, no watch-mode active-scope admission
	// loop (e.watch == nil). Actions that pass dependency resolution go
	// straight to workers. Scope blocking is watch-mode only (§2.3).
	depGraph := syncdispatch.NewDepGraph(r.engine.logger)
	r.depGraph = depGraph
	r.readyCh = make(chan *synctypes.TrackedAction, len(plan.Actions))

	// Two-phase graph population: Register all actions first, then wire
	// dependencies. This avoids forward-reference issues where a parent
	// folder delete at index 0 depends on a child file delete at index 5 —
	// single-pass Add would silently drop the unregistered depID.
	for i := range plan.Actions {
		r.setDispatch(ctx, &plan.Actions[i])
		depGraph.Register(&plan.Actions[i], int64(i))
	}

	for i := range plan.Actions {
		var depIDs []int64
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, int64(depIdx))
		}

		if ta := depGraph.WireDeps(int64(i), depIDs); ta != nil {
			r.readyCh <- ta
		}
	}

	pool := syncexec.NewWorkerPool(r.engine.execCfg, r.readyCh, depGraph.Done(), r.engine.baseline, r.engine.logger, len(plan.Actions))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pool.Start(runCtx, r.engine.transferWorkers)

	// Process results concurrently — engine classifies and calls Complete.
	// The one-shot engine loop reads from the results channel while workers
	// run. drainDone signals when it has finished processing all results,
	// including side effects (counter updates, failure recording). Without
	// this barrier, resultStats() could race with result processing.
	drainDone := make(chan struct{})
	var drainErr error
	go func() {
		defer close(drainDone)
		drainErr = r.runResultsLoop(runCtx, cancel, bl, pool.Results())
	}()

	pool.Wait() // blocks until depGraph.Done() (all actions at terminal state)
	pool.Stop() // cancels workers and closes results once workers exit
	<-drainDone // wait for the one-shot engine loop to finish all side effects
	if drainErr != nil {
		report.Succeeded, report.Failed, report.Errors = r.resultStats()
		report.Errors = append(report.Errors, drainErr)
		return drainErr
	}

	// End-of-pass failure summary — aggregates failures by issue type so
	// bulk sync produces WARN summaries instead of per-item noise (R-6.6.12).
	r.logFailureSummary()

	report.Succeeded, report.Failed, report.Errors = r.resultStats()
	return nil
}

// buildReportFromCounts populates a synctypes.SyncReport with plan counts.
func buildReportFromCounts(counts map[synctypes.ActionType]int, mode synctypes.SyncMode, opts synctypes.RunOpts) *synctypes.SyncReport {
	return &synctypes.SyncReport{
		Mode:          mode,
		DryRun:        opts.DryRun,
		FolderCreates: counts[synctypes.ActionFolderCreate],
		Moves:         counts[synctypes.ActionLocalMove] + counts[synctypes.ActionRemoteMove],
		Downloads:     counts[synctypes.ActionDownload],
		Uploads:       counts[synctypes.ActionUpload],
		LocalDeletes:  counts[synctypes.ActionLocalDelete],
		RemoteDeletes: counts[synctypes.ActionRemoteDelete],
		Conflicts:     counts[synctypes.ActionConflict],
		SyncedUpdates: counts[synctypes.ActionUpdateSynced],
		Cleanups:      counts[synctypes.ActionCleanup],
	}
}

// observeRemote fetches delta changes from the Graph API. Automatically
// retries with an empty token if synctypes.ErrDeltaExpired is returned (full resync).
func (flow *engineFlow) observeRemote(ctx context.Context, bl *synctypes.Baseline) ([]synctypes.ChangeEvent, string, error) {
	eng := flow.engine
	if eng.hasScopedRoot() {
		return flow.observeScopedRemote(ctx, bl, false)
	}

	savedToken, err := eng.baseline.GetDeltaToken(ctx, eng.driveID.String(), "")
	if err != nil {
		return nil, "", fmt.Errorf("sync: getting delta token: %w", err)
	}

	obs := syncobserve.NewRemoteObserver(eng.fetcher, bl, eng.driveID, eng.logger)

	events, token, err := obs.FullDelta(ctx, savedToken)
	if err != nil {
		if !errors.Is(err, synctypes.ErrDeltaExpired) {
			return nil, "", fmt.Errorf("sync: observing remote delta: %w", err)
		}

		// Delta token expired — retry with empty token for full resync.
		eng.logger.Warn("delta token expired, performing full resync")

		events, token, err = obs.FullDelta(ctx, "")
		if err != nil {
			return nil, "", fmt.Errorf("sync: full resync after delta expiry: %w", err)
		}
	}

	return events, token, nil
}

// observeLocal scans the local filesystem for changes and collects skipped
// items (invalid names, path too long, file too large) for failure recording.
// The observer also receives platform-derived naming rules from the engine so
// SharePoint-specific validation stays aligned across one-shot, watch, and
// retry/trial observation paths.
func (flow *engineFlow) observeLocal(ctx context.Context, bl *synctypes.Baseline) (synctypes.ScanResult, error) {
	eng := flow.engine

	obs := syncobserve.NewLocalObserver(bl, eng.logger, eng.checkWorkers)
	obs.SetFilterConfig(eng.localFilter)
	obs.SetObservationRules(eng.localRules)

	result, err := obs.FullScan(ctx, eng.syncTree)
	if err != nil {
		return synctypes.ScanResult{}, fmt.Errorf("sync: local scan: %w", err)
	}

	return result, nil
}

// observeChanges runs remote and local observers based on mode, buffers their
// events, and returns the flushed change set plus a pending delta token.
//
// Observations (remote_state rows) are committed immediately. The delta token
// is returned but NOT committed — the caller must commit it only after the
// planner approves the changes (prevents big-delete protection from
// permanently consuming deletion events). Skipped entirely for dry-run.
//
// When fullReconcile is true, runs a fresh delta with empty token (enumerates
// ALL remote items) and detects orphans — baseline entries not in the full
// enumeration, representing missed delta deletions.
func (flow *engineFlow) observeChanges(
	ctx context.Context,
	watch *watchRuntime,
	bl *synctypes.Baseline,
	mode synctypes.SyncMode,
	dryRun, fullReconcile bool,
) ([]synctypes.PathChanges, string, error) {
	eng := flow.engine
	var remoteEvents []synctypes.ChangeEvent
	var pendingDeltaToken string

	var err error

	if mode != synctypes.SyncUploadOnly {
		if fullReconcile {
			eng.logger.Info("full reconciliation: enumerating all remote items")
			remoteEvents, pendingDeltaToken, err = flow.observeAndCommitRemoteFull(ctx, bl)
		} else if dryRun {
			// Dry-run: observe without committing delta token or observations.
			// A subsequent real sync must see the same remote changes.
			remoteEvents, _, err = flow.observeRemote(ctx, bl)
		} else {
			remoteEvents, pendingDeltaToken, err = flow.observeAndCommitRemote(ctx, bl)
		}

		if err != nil {
			return nil, "", err
		}
	}

	// Process shortcuts: register new ones, remove deleted ones, observe content.
	// During throttle:account or service scope blocks, suppress shortcut
	// observation to avoid wasting API calls (R-2.10.30).
	var shortcutEvents []synctypes.ChangeEvent

	if flow.scopeController().isObservationSuppressed(watch) {
		eng.logger.Debug("suppressing shortcut observation — global scope block active")
	} else {
		shortcutEvents, err = flow.shortcutCoordinator().processShortcuts(ctx, remoteEvents, bl, dryRun)
		if err != nil {
			eng.logger.Warn("shortcut processing failed, continuing without shortcut content",
				slog.String("error", err.Error()),
			)
		}
	}

	// Filter out synctypes.ChangeShortcut events from primary events — they were consumed
	// by processShortcuts and should not enter the planner as regular events.
	remoteEvents = filterOutShortcuts(remoteEvents)

	var localResult synctypes.ScanResult

	if mode != synctypes.SyncDownloadOnly {
		localResult, err = flow.observeLocal(ctx, bl)
		if err != nil {
			return nil, "", err
		}

		// Record observation-time issues (invalid names, path too long, file too large).
		flow.recordSkippedItems(ctx, localResult.Skipped)
		flow.clearResolvedSkippedItems(ctx, localResult.Skipped)

		// R-2.10.10: If the scanner observed paths that were previously blocked
		// by local permission denials, clear the failures (scanner success = proof
		// of accessibility).
		flow.scopeController().applyPermissionRecheckDecisions(
			ctx,
			watch,
			eng.permHandler.clearScannerResolvedPermissions(ctx, pathSetFromEvents(localResult.Events)),
		)
		flow.scopeController().clearResolvedRemoteBlockedFailures(ctx, watch, pathSetFromEvents(localResult.Events))
	}

	buf := syncobserve.NewBuffer(eng.logger)
	buf.AddAll(remoteEvents)
	buf.AddAll(shortcutEvents)
	buf.AddAll(localResult.Events)

	return buf.FlushImmediate(), pendingDeltaToken, nil
}

// observeAndCommitRemote wraps observeRemote to persist observations
// and return the pending delta token for deferred commitment.
//
// Observations (remote_state rows) are committed immediately so the baseline
// reflects the current remote state. The delta token is NOT committed here —
// it is returned to the caller, who must commit it only after the planner
// approves the changes. This prevents big-delete protection from permanently
// consuming deletion events: if the planner rejects the plan, the token stays
// at its old position and the next sync replays the same delta window.
//
// When delta returns 0 events, the token is NOT advanced. The old token
// still covers the same window — replaying it costs nothing (O(1)). But if
// a deletion was still propagating to the Graph change log, advancing would
// permanently skip it. Deletions are delivered exactly once in a narrow
// window (ci_issues.md §20).
func (flow *engineFlow) observeAndCommitRemote(ctx context.Context, bl *synctypes.Baseline) ([]synctypes.ChangeEvent, string, error) {
	eng := flow.engine

	events, deltaToken, err := flow.observeRemote(ctx, bl)
	if err != nil {
		return nil, "", err
	}

	// Skip token advancement when no events were returned. The old token
	// replays the same empty window at zero cost, but avoids advancing
	// past deletions still propagating through the Graph change log.
	if len(events) == 0 {
		eng.logger.Debug("delta returned 0 events, skipping token advancement")
		return events, "", nil
	}

	// Commit observations WITHOUT the delta token. The token is deferred
	// until after the planner approves the changes.
	if commitErr := flow.commitObservedRemote(ctx, events, ""); commitErr != nil {
		return nil, "", commitErr
	}

	return events, deltaToken, nil
}

// commitDeferredDeltaToken advances the delta token after the planner approves
// the changes. No-op when token is empty (upload-only mode, 0-event delta).
// If the process crashes between this call and execution, the next sync
// replays the same delta window — the state machine handles re-observation
// idempotently (same hash → no-op, same delete → no-op).
func (flow *engineFlow) commitDeferredDeltaToken(ctx context.Context, token string) error {
	eng := flow.engine

	if token == "" {
		return nil
	}

	scopeID := ""
	if eng.hasScopedRoot() {
		scopeID = eng.rootItemID
	}

	if err := eng.baseline.CommitDeltaToken(
		ctx, token, eng.driveID.String(), scopeID, eng.driveID.String(),
	); err != nil {
		return fmt.Errorf("sync: committing deferred delta token: %w", err)
	}

	return nil
}

// observeRemoteFull runs a fresh delta with empty token (enumerates ALL remote
// items) and compares against the baseline to find orphans: items in baseline
// but not in the full enumeration = deleted remotely but missed by incremental
// delta. Returns all events (creates/modifies from the full enumeration +
// synthesized deletes for orphans) and the new delta token.
func (flow *engineFlow) observeRemoteFull(ctx context.Context, bl *synctypes.Baseline) ([]synctypes.ChangeEvent, string, error) {
	eng := flow.engine
	if eng.hasScopedRoot() {
		return flow.observeScopedRemote(ctx, bl, true)
	}

	obs := syncobserve.NewRemoteObserver(eng.fetcher, bl, eng.driveID, eng.logger)

	// Full enumeration: empty token returns ALL items as create/modify events.
	events, token, err := obs.FullDelta(ctx, "")
	if err != nil {
		return nil, "", fmt.Errorf("sync: full reconciliation delta: %w", err)
	}

	// Build seen set from all non-deleted events in the full enumeration.
	seen := make(map[driveid.ItemKey]struct{}, len(events))
	for i := range events {
		if events[i].IsDeleted {
			continue
		}

		key := driveid.NewItemKey(events[i].DriveID, events[i].ItemID)
		seen[key] = struct{}{}
	}

	// Detect orphans: baseline entries whose ItemID is not in the seen set.
	orphans := bl.FindOrphans(seen, eng.driveID, "")

	if len(orphans) > 0 {
		eng.logger.Info("full reconciliation: detected orphaned items",
			slog.Int("orphans", len(orphans)),
		)

		events = append(events, orphans...)
	}

	eng.logger.Info("full reconciliation complete",
		slog.Int("total_events", len(events)),
		slog.Int("orphans", len(orphans)),
	)

	return events, token, nil
}

// observeAndCommitRemoteFull wraps observeRemoteFull to persist observations
// and return the pending delta token for deferred commitment (same deferral
// pattern as observeAndCommitRemote — see its doc comment for rationale).
func (flow *engineFlow) observeAndCommitRemoteFull(ctx context.Context, bl *synctypes.Baseline) ([]synctypes.ChangeEvent, string, error) {
	events, deltaToken, err := flow.observeRemoteFull(ctx, bl)
	if err != nil {
		return nil, "", err
	}

	// Commit observations without the delta token — token deferred to caller.
	if commitErr := flow.commitObservedRemote(ctx, events, ""); commitErr != nil {
		return nil, "", commitErr
	}

	return events, deltaToken, nil
}

// changeEventsToObservedItems converts remote ChangeEvents into ObservedItems
// for CommitObservation. Filters out local-source events and events with
// empty ItemIDs (defensive guard against malformed API responses).
func changeEventsToObservedItems(logger *slog.Logger, events []synctypes.ChangeEvent) []synctypes.ObservedItem {
	var items []synctypes.ObservedItem

	for i := range events {
		if events[i].Source != synctypes.SourceRemote {
			continue
		}

		if events[i].ItemID == "" {
			logger.Warn("changeEventsToObservedItems: skipping event with empty ItemID",
				slog.String("path", events[i].Path),
			)

			continue
		}

		items = append(items, synctypes.ObservedItem{
			DriveID:   events[i].DriveID,
			ItemID:    events[i].ItemID,
			ParentID:  events[i].ParentID,
			Path:      events[i].Path,
			ItemType:  events[i].ItemType,
			Hash:      events[i].Hash,
			Size:      events[i].Size,
			Mtime:     events[i].Mtime,
			ETag:      events[i].ETag,
			IsDeleted: events[i].IsDeleted,
		})
	}

	return items
}

// resolveSafetyConfig returns the appropriate synctypes.SafetyConfig. The planner-level
// big-delete check is disabled (threshold=MaxInt32) when force is set or when
// the engine has a deleteCounter (watch mode — the rolling counter handles
// big-delete protection instead).
func (e *Engine) resolveSafetyConfig(force, watchMode bool) *synctypes.SafetyConfig {
	if force || watchMode {
		return &synctypes.SafetyConfig{
			BigDeleteThreshold: forceSafetyMax,
		}
	}

	return &synctypes.SafetyConfig{
		BigDeleteThreshold: e.bigDeleteThreshold,
	}
}
