package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// retryBatchSize limits how many sync_failures are processed per retry sweep.
// Prevents the watch loop from stalling when thousands of items become
// retryable at once (e.g., after a scope clear).
const retryBatchSize = 1024

// trialPendingTTL is the maximum time a trial entry lingers in trialPending
// before being considered stale and cleaned up. 15× the debounce window.
const trialPendingTTL = 30 * time.Second

// resultClass categorizes a synctypes.WorkerResult for routing by processWorkerResult.
type resultClass int

const (
	resultSuccess    resultClass = iota // action succeeded
	resultRequeue                       // transient failure — re-queue with backoff
	resultScopeBlock                    // scope-level failure (429, 507, 5xx pattern)
	resultSkip                          // non-retryable — record and move on
	resultShutdown                      // context canceled — discard silently
	resultFatal                         // abort sync pass (401 unrecoverable auth)
)

type failureRecordMode int

const (
	recordFailureNone failureRecordMode = iota
	recordFailureActionable
	recordFailureReconcile
)

type permissionFlow int

const (
	permissionFlowNone permissionFlow = iota
	permissionFlowRemote403
	permissionFlowLocalPermission
)

// ResultDecision is the single classification output consumed by result
// routing. The decision is behavior-complete so downstream code does not
// re-derive policy from raw HTTP/local error facts.
type ResultDecision struct {
	Class             resultClass
	ScopeKey          synctypes.ScopeKey
	RecordMode        failureRecordMode
	PermissionFlow    permissionFlow
	RunScopeDetection bool
	RecordSuccess     bool
}

// classifyResult is a pure function that maps a synctypes.WorkerResult to a
// single ResultDecision. No side effects — classification is separate from
// routing ("functions do one thing").
func classifyResult(r *synctypes.WorkerResult) ResultDecision {
	if r.Success {
		return ResultDecision{
			Class:         resultSuccess,
			RecordSuccess: true,
		}
	}

	// Shutdown: context canceled or deadline exceeded — graceful drain.
	// NOT a failure — just a canceled operation. Don't record in sync_failures.
	if errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded) {
		return ResultDecision{Class: resultShutdown}
	}

	if decision, handled := classifyHTTPResult(r); handled {
		return decision
	}

	return classifyLocalResult(r)
}

func classifyHTTPResult(r *synctypes.WorkerResult) (ResultDecision, bool) {
	switch {
	case r.HTTPStatus == 0:
		return ResultDecision{}, false
	case r.HTTPStatus == http.StatusUnauthorized:
		return ResultDecision{
			Class:      resultFatal,
			RecordMode: recordFailureActionable,
		}, true
	case r.HTTPStatus == http.StatusForbidden:
		return ResultDecision{
			Class:          resultSkip,
			RecordMode:     recordFailureActionable,
			PermissionFlow: permissionFlowRemote403,
		}, true
	case r.HTTPStatus == http.StatusTooManyRequests:
		return ResultDecision{
			Class:             resultScopeBlock,
			ScopeKey:          synctypes.SKThrottleAccount(),
			RecordMode:        recordFailureReconcile,
			RunScopeDetection: true,
		}, true
	case r.HTTPStatus == http.StatusInsufficientStorage:
		return ResultDecision{
			Class:             resultScopeBlock,
			ScopeKey:          synctypes.ScopeKeyForStatus(r.HTTPStatus, r.ShortcutKey),
			RecordMode:        recordFailureReconcile,
			RunScopeDetection: true,
		}, true
	case r.HTTPStatus == http.StatusBadRequest && isOutagePattern(r.Err):
		return ResultDecision{
			Class:             resultRequeue,
			RecordMode:        recordFailureReconcile,
			RunScopeDetection: true,
		}, true
	case r.HTTPStatus >= http.StatusInternalServerError:
		return ResultDecision{
			Class:             resultRequeue,
			RecordMode:        recordFailureReconcile,
			RunScopeDetection: true,
		}, true
	case isRetryableHTTPStatus(r.HTTPStatus):
		return ResultDecision{
			Class:             resultRequeue,
			RecordMode:        recordFailureReconcile,
			RunScopeDetection: true,
		}, true
	default:
		return ResultDecision{
			Class:      resultSkip,
			RecordMode: recordFailureActionable,
		}, true
	}
}

func isRetryableHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusPreconditionFailed ||
		status == http.StatusNotFound ||
		status == http.StatusLocked
}

func classifyLocalResult(r *synctypes.WorkerResult) ResultDecision {
	switch {
	case errors.Is(r.Err, driveops.ErrDiskFull):
		return ResultDecision{
			Class:      resultScopeBlock,
			ScopeKey:   synctypes.SKDiskLocal(),
			RecordMode: recordFailureReconcile,
		}
	case errors.Is(r.Err, driveops.ErrFileTooLargeForSpace):
		return ResultDecision{
			Class:      resultSkip,
			RecordMode: recordFailureActionable,
		}
	case errors.Is(r.Err, os.ErrPermission):
		return ResultDecision{
			Class:          resultSkip,
			RecordMode:     recordFailureActionable,
			PermissionFlow: permissionFlowLocalPermission,
		}
	default:
		return ResultDecision{
			Class:      resultSkip,
			RecordMode: recordFailureActionable,
		}
	}
}

func isDeleteLikeSyncStatus(status synctypes.SyncStatus) bool {
	return status == synctypes.SyncStatusDeleted ||
		status == synctypes.SyncStatusDeleting ||
		status == synctypes.SyncStatusDeleteFailed ||
		status == synctypes.SyncStatusPendingDelete
}

func isResolvedRemoteSyncStatus(status synctypes.SyncStatus) bool {
	return status == synctypes.SyncStatusSynced ||
		status == synctypes.SyncStatusDeleted ||
		status == synctypes.SyncStatusFiltered
}

// isOutagePattern returns true if the error matches known transient 400
// outage patterns. Per failure-redesign.md §7.6, some 400 errors (e.g.,
// "ObjectHandle is Invalid") are actually transient service outages.
func isOutagePattern(err error) bool {
	if err == nil {
		return false
	}

	var ge *graph.GraphError
	if !errors.As(err, &ge) {
		return false
	}

	return strings.Contains(ge.Message, "ObjectHandle is Invalid")
}

// processWorkerResult replaces processWorkerResult + routeReadyActions with
// failure-aware dependent dispatch. Returns actions for the outbox owned by
// the calling control flow instead of sending to readyCh directly.
//
// Dependent routing is structured at the Complete level:
//   - Parent success → children admitted via admitReady (active-scope check)
//   - Parent failure → children cascade-recorded as sync_failures
//   - Parent shutdown → children silently completed (no dispatch, no failure)
func (e *Engine) processWorkerResult(ctx context.Context, r *synctypes.WorkerResult, bl *synctypes.Baseline) []*synctypes.TrackedAction {
	// Trial results handled separately (early return).
	if r.IsTrial && !r.TrialScopeKey.IsZero() {
		return e.processTrialResult(ctx, r)
	}

	decision := classifyResult(r)

	// Graph completion — all result classes call Complete.
	ready, _ := e.depGraph.Complete(r.ActionID)

	// Dependent routing — based on result class.
	var dispatched []*synctypes.TrackedAction

	switch decision.Class {
	case resultSuccess:
		dispatched = e.admitReady(ctx, ready)

	case resultShutdown:
		// Context canceled — silently complete all dependents. Don't dispatch
		// (workers shutting down), don't record failures (not a failure).
		// BFS via completeSubtree prevents grandchild stranding.
		e.completeSubtree(ready)

	case resultRequeue, resultScopeBlock, resultSkip, resultFatal:
		// Parent failed — cascade-record children as sync_failures.
		// BFS via cascadeFailAndComplete prevents grandchild stranding.
		e.cascadeFailAndComplete(ctx, ready, r)
	}

	// Per-class side effects (after dependent routing).
	e.applyResultDecision(ctx, decision, r, bl)

	return dispatched
}

// applyResultDecision handles per-class side effects after dependent routing is
// complete. ResultDecision already encodes failure policy, permission routing,
// and scope-detection behavior so this function does not re-derive rules from
// raw HTTP/local error facts.
func (e *Engine) applyResultDecision(ctx context.Context, decision ResultDecision, r *synctypes.WorkerResult, bl *synctypes.Baseline) {
	switch decision.Class {
	case resultSuccess:
		if decision.RecordSuccess {
			e.succeeded.Add(1)
			e.clearFailureOnSuccess(ctx, r)
			if e.watch != nil {
				e.watch.scopeState.RecordSuccess(r)
			}
		}

	case resultSkip, resultShutdown, resultRequeue, resultScopeBlock, resultFatal:
		// no failure recording
	}

	if e.applyPermissionDecisionFlow(ctx, decision, r, bl) {
		e.recordError(r)
		return
	}

	e.applyFailureRecordMode(ctx, decision.RecordMode, r)

	if decision.RunScopeDetection {
		e.feedScopeDetection(ctx, r)
	} else if decision.Class == resultScopeBlock && !decision.ScopeKey.IsZero() {
		e.applyScopeBlock(ctx, synctypes.ScopeUpdateResult{
			Block:     true,
			ScopeKey:  decision.ScopeKey,
			IssueType: decision.ScopeKey.IssueType(),
		})
	}

	if decision.Class == resultScopeBlock {
		e.armTrialTimer()
	}
	if decision.RecordMode == recordFailureReconcile {
		e.armRetryTimer(ctx)
	}

	if decision.Class != resultShutdown && decision.Class != resultSuccess {
		e.recordError(r)
	}
}

// processTrialResult handles trial results using the new architecture.
// Returns actions for the caller-owned outbox.
func (e *Engine) processTrialResult(ctx context.Context, r *synctypes.WorkerResult) []*synctypes.TrackedAction {
	decision := classifyResult(r)

	// Complete the trial action in the graph.
	ready, _ := e.depGraph.Complete(r.ActionID)

	if decision.Class == resultSuccess {
		if err := e.releaseScope(ctx, r.TrialScopeKey); err != nil {
			e.logger.Warn("processTrialResult: failed to release scope",
				slog.String("scope_key", r.TrialScopeKey.String()),
				slog.String("error", err.Error()),
			)
		}
		e.succeeded.Add(1)
		if e.watch != nil {
			e.watch.scopeState.RecordSuccess(r)
		}
		// Dispatch any dependents that were waiting on the trial.
		return e.admitReady(ctx, ready)
	}

	if decision.Class == resultShutdown {
		// BFS via completeSubtree prevents grandchild stranding.
		e.completeSubtree(ready)
		return nil
	}

	// Trial failure: extend interval. Scope detection is NOT called — the
	// scope is already blocked, and re-detecting would overwrite the doubled
	// interval with a fresh initial interval (A2 bug prevention).
	e.extendScopeTrial(ctx, r.TrialScopeKey, r.RetryAfter)

	e.applyFailureRecordMode(ctx, decision.RecordMode, r)
	e.recordError(r)

	// Cascade-record dependents as failures.
	// BFS via cascadeFailAndComplete prevents grandchild stranding.
	e.cascadeFailAndComplete(ctx, ready, r)

	e.armRetryTimer(ctx)

	return nil
}

// armRetryTimer arms the retry timer for the next retrier sweep. Queries
// the earliest next_retry_at from sync_failures and sets the timer. If the
// retry timer channel is already signaled (non-blocking send to buffered(1)
// channel), the next owning loop iteration processes it.
func (e *Engine) armRetryTimer(ctx context.Context) {
	if e.watch == nil {
		return
	}

	earliest, err := e.baseline.EarliestSyncFailureRetryAt(ctx, e.nowFunc())
	if err != nil || earliest.IsZero() {
		return
	}

	delay := time.Until(earliest)
	if delay <= 0 {
		e.kickRetrySweepNow()
		return
	}

	if e.watch.retryTimer != nil {
		e.watch.retryTimer.Stop()
	}
	e.watch.retryTimer = time.AfterFunc(delay, func() {
		e.kickRetrySweepNow()
	})
}

func (e *Engine) stopRetryTimer() {
	if e.watch == nil {
		return
	}

	if e.watch.retryTimer != nil {
		e.watch.retryTimer.Stop()
		e.watch.retryTimer = nil
	}
}

// kickRetrySweepNow is the single immediate wakeup path for the watch-mode
// retrier. Centralizing the non-blocking send keeps retry timer ownership
// explicit and avoids scattering direct channel writes across the engine.
func (e *Engine) kickRetrySweepNow() {
	if e.watch == nil {
		return
	}

	select {
	case e.watch.retryTimerCh <- struct{}{}:
		e.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetryKicked})
	default:
	}
}

// retryTimerChan returns the retry timer notification channel. Returns a nil
// channel when retryTimerCh is not initialized (one-shot mode), which blocks
// forever in a select — effectively disabling the case.
func (e *Engine) retryTimerChan() <-chan struct{} {
	if e.watch == nil {
		return nil // nil channel blocks in select — disables retry case
	}

	return e.watch.retryTimerCh
}

// runTrialDispatch handles due scope trials. For each due scope, picks the
// oldest scope-blocked failure from sync_failures and synthesizes a
// re-observation event into the buffer. If no candidates exist for a scope,
// the scope is cleared (no items left to trial — condition resolved).
//
// Uses DueTrials to snapshot all due scopes at once, then iterates each
// exactly once. This is structurally incapable of infinite iteration —
// unlike the old NextDueTrial-in-a-loop approach which required state
// mutation (extendTrialInterval) as iteration control.
func (e *Engine) runTrialDispatch(ctx context.Context) {
	now := e.nowFunc()

	// Clean stale trial entries before dispatching new ones.
	e.cleanStaleTrialPending(ctx, now)

	// Snapshot all due scopes — each visited exactly once.
	for _, key := range syncdispatch.DueTrials(e.watch.activeScopes, now) {
		// Pick oldest scope-blocked failure for this scope.
		row, found, err := e.baseline.PickTrialCandidate(ctx, key)
		if err != nil {
			e.logger.Warn("runTrialDispatch: failed to pick trial candidate",
				slog.String("scope_key", key.String()),
				slog.String("error", err.Error()),
			)

			break
		}

		if !found {
			// No candidates — scope condition resolved externally.
			if err := e.releaseScope(ctx, key); err != nil {
				e.logger.Warn("runTrialDispatch: failed to release empty scope",
					slog.String("scope_key", key.String()),
					slog.String("error", err.Error()),
				)
			}

			continue
		}

		// Register the trial in trialPending so admitAndDispatch/admitReady
		// can intercept the resulting action from the planner.
		e.watch.trialPending[row.Path] = trialEntry{
			scopeKey: key,
			created:  now,
		}

		// Re-observe the item with a real API call / FS access to confirm
		// whether the scope condition has cleared. Unlike the retrier (which
		// uses cached DB state), trials hit the live source of truth.
		ev, retryAfter := e.reobserve(ctx, row)
		if ev == nil {
			// Scope condition persists — extend the trial interval using
			// the server's Retry-After if provided (R-2.10.7), otherwise
			// exponential backoff.
			e.extendScopeTrial(ctx, key, retryAfter)

			continue
		}

		if e.watch != nil {
			e.watch.buf.Add(ev)
		}
		e.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventTrialDispatched,
			ScopeKey: key,
			Path:     row.Path,
		})

		e.logger.Debug("trial dispatched",
			slog.String("scope_key", key.String()),
			slog.String("path", row.Path),
		)
	}

	e.armTrialTimer()
}

// runRetrierSweep processes a batch of due sync_failures, re-injecting them
// into the pipeline via the buffer. Runs inline in the owning control flow
// for direct depGraph.HasInFlight access without coordination problems (D-11).
//
// Batch-limited to retryBatchSize to prevent the owning loop from stalling when many
// items become retryable at once (e.g., after a scope clear).
func (e *Engine) runRetrierSweep(ctx context.Context) {
	now := e.nowFunc()

	rows, err := e.baseline.ListSyncFailuresForRetry(ctx, now)
	if err != nil {
		e.logger.Warn("retrier sweep: failed to list retriable items",
			slog.String("error", err.Error()),
		)

		return
	}

	if len(rows) == 0 {
		return
	}

	dispatched := 0

	for i := range rows {
		if dispatched >= retryBatchSize {
			// More items remain — re-arm immediately so the owning loop picks up
			// the next batch on the next iteration.
			e.kickRetrySweepNow()

			break
		}

		row := &rows[i]

		// Skip items already being processed by a worker.
		if e.depGraph.HasInFlight(row.Path) {
			continue
		}

		// D-11: skip stale failures whose underlying condition has resolved
		// through the normal pipeline (e.g., delta poll downloaded the file,
		// local file was deleted). Clears the sync_failure from DB.
		if e.isFailureResolved(ctx, row) {
			continue
		}

		// D-9: build a full-fidelity event from DB state (remote_state for
		// downloads, local FS for uploads) instead of sparse synthesized events.
		ev := e.createEventFromDB(ctx, row)
		if ev == nil {
			continue
		}

		if e.watch != nil {
			e.watch.buf.Add(ev)
		}

		dispatched++
	}

	if dispatched > 0 {
		e.logger.Info("retrier sweep",
			slog.Int("dispatched", dispatched),
		)
	}

	// Re-arm for the next due item.
	e.armRetryTimer(ctx)
}

// remoteStateToChangeEvent converts a synctypes.RemoteStateRow into a full-fidelity
// synctypes.ChangeEvent. Pure function — no I/O. Used by createEventFromDB for
// download/delete failures where the DB-cached remote_state provides all
// fields the planner needs (D-9 fix).
func remoteStateToChangeEvent(rs *synctypes.RemoteStateRow, path string) *synctypes.ChangeEvent {
	// Determine change type from sync_status: delete-family statuses
	// become synctypes.ChangeDelete, everything else becomes synctypes.ChangeModify.
	ct := synctypes.ChangeModify
	isDeleted := false

	if isDeleteLikeSyncStatus(rs.SyncStatus) {
		ct = synctypes.ChangeDelete
		isDeleted = true
	}

	return &synctypes.ChangeEvent{
		Source:    synctypes.SourceRemote,
		Type:      ct,
		Path:      path,
		ItemID:    rs.ItemID,
		ParentID:  rs.ParentID,
		DriveID:   rs.DriveID,
		ItemType:  rs.ItemType,
		Name:      filepath.Base(path),
		Size:      rs.Size,
		Hash:      rs.Hash,
		Mtime:     rs.Mtime,
		ETag:      rs.ETag,
		IsDeleted: isDeleted,
	}
}

// observeLocalFile stats and hashes a local file, returning a synctypes.ChangeEvent.
// File gone → synctypes.ChangeDelete. Transient FS error → nil. Used by both
// createEventFromDB and reobserve to avoid duplicating the upload path.
func (e *Engine) baselineEntryForPath(ctx context.Context, path string, driveID driveid.ID) *synctypes.BaselineEntry {
	bl := e.baseline.Baseline()
	if bl == nil {
		var err error
		bl, err = e.baseline.Load(ctx)
		if err != nil {
			e.logger.Debug("baselineEntryForPath: failed to load baseline",
				slog.String("path", path),
				slog.String("error", err.Error()),
			)

			return nil
		}
	}

	entry, ok := bl.GetByPath(path)
	if !ok || entry.DriveID != driveID {
		return nil
	}

	return entry
}

func (e *Engine) observeLocalFile(path, caller string, base *synctypes.BaselineEntry) *synctypes.ChangeEvent {
	absPath := filepath.Join(e.syncRoot, path)

	info, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &synctypes.ChangeEvent{
				Source:    synctypes.SourceLocal,
				Type:      synctypes.ChangeDelete,
				Path:      path,
				Name:      filepath.Base(path),
				ItemType:  synctypes.ItemTypeFile,
				IsDeleted: true,
			}
		}

		e.logger.Debug(caller+": stat failed",
			slog.String("path", path),
			slog.String("error", err.Error()),
		)

		return nil
	}

	it := synctypes.ItemTypeFile
	if info.IsDir() {
		it = synctypes.ItemTypeFolder
	}

	var hash string
	if it == synctypes.ItemTypeFile {
		if syncobserve.CanReuseBaselineHash(info, base, e.nowFunc().UnixNano()) {
			hash = base.LocalHash
		} else {
			hash, err = syncobserve.ComputeStableHash(absPath)
			if err != nil {
				e.logger.Debug(caller+": hash failed",
					slog.String("path", path),
					slog.String("error", err.Error()),
				)

				return nil
			}
		}
	}

	return &synctypes.ChangeEvent{
		Source:   synctypes.SourceLocal,
		Type:     synctypes.ChangeModify,
		Path:     path,
		Name:     filepath.Base(path),
		ItemType: it,
		Size:     info.Size(),
		Hash:     hash,
		Mtime:    info.ModTime().UnixNano(),
	}
}

// createEventFromDB builds a full-fidelity synctypes.ChangeEvent from database state
// and the local filesystem. Direction-based dispatch:
//   - Upload: stat + hash the local file. File gone → synctypes.ChangeDelete. Error → nil.
//   - Download/Delete: query remote_state from DB. Nil → nil (resolved).
//     Otherwise remoteStateToChangeEvent.
//
// No API calls — uploads use the local FS; downloads use DB-cached
// remote_state (kept fresh by delta polls). Fixes D-9: the planner receives
// complete PathViews with hash, size, mtime, etag, and name.
func (e *Engine) createEventFromDB(ctx context.Context, row *synctypes.SyncFailureRow) *synctypes.ChangeEvent {
	if row.Direction == synctypes.DirectionUpload {
		return e.observeLocalFile(
			row.Path,
			"createEventFromDB",
			e.baselineEntryForPath(ctx, row.Path, row.DriveID),
		)
	}

	rs, found, err := e.baseline.GetRemoteStateByPath(ctx, row.Path, row.DriveID)
	if err != nil {
		e.logger.Debug("createEventFromDB: remote state lookup failed",
			slog.String("path", row.Path),
			slog.String("error", err.Error()),
		)

		return nil
	}

	if !found {
		// No active remote_state → item was resolved through the normal
		// pipeline (delta poll downloaded it, or it was deleted remotely).
		return nil
	}

	return remoteStateToChangeEvent(rs, row.Path)
}

// isFailureResolved checks whether a sync_failure's underlying condition has
// been resolved through the normal pipeline, making the failure stale. When
// resolved, the failure is cleared from the database. Fixes D-11: prevents
// the retrier from re-injecting events for items that no longer need action.
//
// Resolution conditions by direction:
//   - Download: remote_state is nil (deleted) OR sync_status is synced/deleted/filtered.
//   - Upload: local file no longer exists (os.ErrNotExist).
//   - Delete: no baseline entry exists for the path (already cleaned up).
func (e *Engine) isFailureResolved(ctx context.Context, row *synctypes.SyncFailureRow) bool {
	var resolved bool

	switch row.Direction {
	case synctypes.DirectionDownload:
		rs, found, err := e.baseline.GetRemoteStateByPath(ctx, row.Path, row.DriveID)
		if err != nil {
			// DB error — can't determine resolution, treat as unresolved.
			return false
		}

		// No active row means the item was deleted or never existed.
		if !found {
			resolved = true
		} else if isResolvedRemoteSyncStatus(rs.SyncStatus) {
			resolved = true
		}

	case synctypes.DirectionUpload:
		absPath := filepath.Join(e.syncRoot, row.Path)

		_, err := os.Stat(absPath)
		if errors.Is(err, os.ErrNotExist) {
			resolved = true
		}

	case synctypes.DirectionDelete:
		// Load baseline to check if the entry still exists.
		bl, err := e.baseline.Load(ctx)
		if err != nil {
			return false
		}

		_, exists := bl.GetByPath(row.Path)
		if !exists {
			resolved = true
		}
	}

	if resolved {
		if err := e.baseline.ClearSyncFailure(ctx, row.Path, row.DriveID); err != nil {
			e.logger.Debug("isFailureResolved: failed to clear resolved failure",
				slog.String("path", row.Path),
				slog.String("error", err.Error()),
			)
		}
	}

	return resolved
}

// reobserve makes a real API call or filesystem access to re-observe an item
// for trial dispatch. Unlike createEventFromDB (which reads cached DB state),
// reobserve hits the live source of truth to confirm whether a scope condition
// has actually cleared.
//
// Direction-based dispatch:
//   - Download/Delete: GetItem API call. 200 → full synctypes.ChangeEvent. 404 → synctypes.ChangeDelete.
//     429/507/5xx → nil (scope still blocked).
//   - Upload: stat + hash local file. Exists → full synctypes.ChangeEvent. Gone → synctypes.ChangeDelete.
//     Error → nil.
//
// Returns (nil, retryAfter) when the scope condition persists — caller
// forwards retryAfter to extendTrialInterval for server-driven backoff.
// Returns (event, 0) on success or when the item is gone.
func (e *Engine) reobserve(ctx context.Context, row *synctypes.SyncFailureRow) (*synctypes.ChangeEvent, time.Duration) {
	if row.Direction == synctypes.DirectionUpload {
		// Local FS — no RetryAfter concept.
		return e.observeLocalFile(
			row.Path,
			"reobserve",
			e.baselineEntryForPath(ctx, row.Path, row.DriveID),
		), 0
	}

	item, err := e.execCfg.Items().GetItem(ctx, row.DriveID, row.ItemID)
	if err != nil {
		// Classify the error to determine whether the scope is still blocked
		// or the item is truly gone.
		var ge *graph.GraphError
		if errors.As(err, &ge) {
			if errors.Is(ge.Err, graph.ErrNotFound) {
				// Item was deleted remotely — return a delete event.
				return &synctypes.ChangeEvent{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      row.Path,
					ItemID:    row.ItemID,
					DriveID:   row.DriveID,
					ItemType:  synctypes.ItemTypeFile,
					Name:      filepath.Base(row.Path),
					IsDeleted: true,
				}, 0
			}

			// 429, 507, 5xx — scope condition persists; return nil so the
			// caller extends the trial interval. Forward RetryAfter from
			// the server response (R-2.10.7) so the caller uses the
			// server-mandated wait instead of exponential backoff.
			if errors.Is(ge.Err, graph.ErrThrottled) || errors.Is(ge.Err, graph.ErrServerError) ||
				ge.StatusCode == http.StatusInsufficientStorage {
				e.logger.Debug("reobserve: scope condition persists",
					slog.String("path", row.Path),
					slog.Int("status", ge.StatusCode),
					slog.Duration("retry_after", ge.RetryAfter),
				)

				return nil, ge.RetryAfter
			}
		}

		// Unexpected error — log and return nil (skip this trial).
		e.logger.Debug("reobserve: GetItem failed",
			slog.String("path", row.Path),
			slog.String("error", err.Error()),
		)

		return nil, 0
	}

	// 200 — build a full synctypes.ChangeEvent from the live API response.
	it := synctypes.ItemTypeFile
	if item.IsFolder {
		it = synctypes.ItemTypeFolder
	} else if item.IsRoot {
		it = synctypes.ItemTypeRoot
	}

	ct := synctypes.ChangeModify
	isDeleted := false

	if item.IsDeleted {
		ct = synctypes.ChangeDelete
		isDeleted = true
	}

	return &synctypes.ChangeEvent{
		Source:    synctypes.SourceRemote,
		Type:      ct,
		Path:      row.Path,
		ItemID:    item.ID,
		ParentID:  item.ParentID,
		DriveID:   item.DriveID,
		ItemType:  it,
		Name:      item.Name,
		Size:      item.Size,
		Hash:      item.QuickXorHash,
		Mtime:     item.ModifiedAt.UnixNano(),
		ETag:      item.ETag,
		IsDeleted: isDeleted,
	}, 0
}

// cleanStaleTrialPending removes stale entries from trialPending. Entries
// older than trialPendingTTL are cleared and the corresponding sync_failure
// is deleted (item is stale).
func (e *Engine) cleanStaleTrialPending(ctx context.Context, now time.Time) {
	for path, entry := range e.watch.trialPending {
		if now.Sub(entry.created) > trialPendingTTL {
			delete(e.watch.trialPending, path)
			// Clear the stale sync_failure — this trial candidate was never
			// intercepted by the planner (item may have been deleted). Best-effort.
			if err := e.baseline.ClearSyncFailureByPath(ctx, path); err != nil {
				e.logger.Debug("cleanStaleTrialPending: failed to clear stale failure",
					slog.String("path", path),
					slog.String("error", err.Error()),
				)
			}
		}
	}
}

// recordError increments the failed counter and appends the error to the
// diagnostic error list.
func (e *Engine) recordError(r *synctypes.WorkerResult) {
	e.failed.Add(1)
	if r.Err != nil {
		e.syncErrorsMu.Lock()
		e.syncErrors = append(e.syncErrors, r.Err)
		e.syncErrorsMu.Unlock()
	}
}

// logFailureSummary logs an aggregated summary of sync errors from the
// current pass. Groups errors by message prefix (first 80 chars) and logs
// one WARN per group with count + sample paths when count > 10, or per-item
// WARN otherwise. Mirrors the scanner aggregation pattern in
// recordSkippedItems (R-6.6.12). Resets syncErrors after logging.
func (e *Engine) logFailureSummary() {
	e.syncErrorsMu.Lock()
	errs := e.syncErrors
	e.syncErrors = nil
	e.syncErrorsMu.Unlock()

	if len(errs) == 0 {
		return
	}

	// Group by error message for aggregation. Use the first errorGroupKeyLen
	// chars of the error message as the group key — detailed enough to
	// distinguish issue types without creating too many groups.
	const errorGroupKeyLen = 80
	type group struct {
		msgs  []string
		count int
	}
	groups := make(map[string]*group)
	for _, err := range errs {
		msg := err.Error()
		key := msg
		if len(key) > errorGroupKeyLen {
			key = key[:errorGroupKeyLen]
		}
		g, ok := groups[key]
		if !ok {
			g = &group{}
			groups[key] = g
		}
		g.count++
		// Keep first 3 unique messages as samples.
		const sampleCount = 3
		if len(g.msgs) < sampleCount {
			g.msgs = append(g.msgs, msg)
		}
	}

	const aggregateThreshold = 10
	for key, g := range groups {
		if g.count > aggregateThreshold {
			e.logger.Warn("sync failures (aggregated)",
				slog.String("error_prefix", key),
				slog.Int("count", g.count),
				slog.Any("samples", g.msgs),
			)
		} else {
			for _, msg := range g.msgs {
				e.logger.Warn("sync failure",
					slog.String("error", msg),
				)
			}
		}
	}
}

// clearFailureOnSuccess removes the sync_failures row for a successfully
// completed action. The engine owns failure lifecycle — store_baseline's
// CommitOutcome handles only baseline/remote_state updates (D-6).
func (e *Engine) clearFailureOnSuccess(ctx context.Context, r *synctypes.WorkerResult) {
	driveID := r.DriveID
	if driveID.String() == "" {
		driveID = e.driveID
	}

	if clearErr := e.baseline.ClearSyncFailure(ctx, r.Path, driveID); clearErr != nil {
		e.logger.Warn("failed to clear sync failure on success",
			slog.String("path", r.Path),
			slog.String("error", clearErr.Error()),
		)
	}
}

func (e *Engine) applyFailureRecordMode(
	ctx context.Context,
	mode failureRecordMode,
	r *synctypes.WorkerResult,
) {
	switch mode {
	case recordFailureNone:
		return
	case recordFailureActionable:
		e.recordFailure(ctx, r, nil)
	case recordFailureReconcile:
		e.recordFailure(ctx, r, retry.ReconcilePolicy().Delay)
	default:
		panic(fmt.Sprintf("unknown failure record mode %d", mode))
	}
}

func (e *Engine) applyPermissionDecisionFlow(
	ctx context.Context,
	decision ResultDecision,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) bool {
	switch decision.PermissionFlow {
	case permissionFlowNone:
		return false
	case permissionFlowRemote403:
		if !e.permHandler.HasPermChecker() {
			return false
		}
		decision := e.permHandler.handle403(ctx, bl, r.Path, e.getShortcuts())
		return e.applyPermissionCheckDecision(
			ctx,
			permissionFlowRemote403,
			&decision,
		)
	case permissionFlowLocalPermission:
		decision := e.permHandler.handleLocalPermission(ctx, r)
		return e.applyPermissionCheckDecision(
			ctx,
			permissionFlowLocalPermission,
			&decision,
		)
	default:
		panic(fmt.Sprintf("unknown permission flow %d", decision.PermissionFlow))
	}
}

// recordFailure writes a failure to sync_failures with the given delay
// function for computing next_retry_at. For transient failures, pass
// retry.Reconcile.Delay; for actionable/fatal, pass nil (no retry).
func (e *Engine) recordFailure(ctx context.Context, r *synctypes.WorkerResult, delayFn func(int) time.Duration) {
	direction := directionFromAction(r.ActionType)

	driveID := r.DriveID
	if driveID.String() == "" {
		driveID = e.driveID
	}

	// The engine's routing already classifies each result — delayFn is non-nil
	// for transient failures (retryable) and nil for actionable/fatal ones.
	category := synctypes.CategoryTransient
	if delayFn == nil {
		category = synctypes.CategoryActionable
	}

	issueType := issueTypeForHTTPStatus(r.HTTPStatus, r.Err)
	scopeKey := deriveScopeKey(r)

	if recErr := e.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       r.Path,
		DriveID:    driveID,
		Direction:  direction,
		Role:       synctypes.FailureRoleItem,
		Category:   category,
		IssueType:  issueType,
		ErrMsg:     r.ErrMsg,
		HTTPStatus: r.HTTPStatus,
		ScopeKey:   scopeKey,
	}, delayFn); recErr != nil {
		e.logger.Warn("failed to record failure",
			slog.String("path", r.Path),
			slog.String("error", recErr.Error()),
		)

		return
	}

	// Per-item failure detail at DEBUG. Bulk sync logs individual items at
	// DEBUG and aggregates at WARN in logFailureSummary (R-6.6.10).
	e.logger.Debug("sync failure recorded",
		slog.String("path", r.Path),
		slog.String("action", r.ActionType.String()),
		slog.Int("http_status", r.HTTPStatus),
		slog.String("error", r.ErrMsg),
		slog.String("scope_key", scopeKey.String()),
	)
}

// deriveScopeKey returns the scope key for a failed worker result.
// deriveScopeKey maps a worker result to its typed scope key. Delegates to
// synctypes.ScopeKeyForStatus — single source of truth for HTTP status → scope key
// mapping. Returns the zero-value synctypes.ScopeKey for non-scope statuses.
func deriveScopeKey(r *synctypes.WorkerResult) synctypes.ScopeKey {
	return synctypes.ScopeKeyForStatus(r.HTTPStatus, r.ShortcutKey)
}

// issueTypeForHTTPStatus maps an HTTP status code and error to a sync
// failure issue type. Used by recordFailure to populate the issue_type
// column. Returns empty string for generic/unknown failures.
func issueTypeForHTTPStatus(httpStatus int, err error) string {
	switch {
	case httpStatus == http.StatusTooManyRequests:
		return synctypes.IssueRateLimited
	case httpStatus == http.StatusInsufficientStorage:
		return synctypes.IssueQuotaExceeded
	case httpStatus == http.StatusForbidden:
		return synctypes.IssuePermissionDenied
	case httpStatus == http.StatusBadRequest && isOutagePattern(err):
		return synctypes.IssueServiceOutage
	case httpStatus >= http.StatusInternalServerError:
		return synctypes.IssueServiceOutage
	case httpStatus == http.StatusRequestTimeout:
		return "request_timeout"
	case httpStatus == http.StatusPreconditionFailed:
		return "transient_conflict"
	case httpStatus == http.StatusNotFound:
		return "transient_not_found"
	case httpStatus == http.StatusLocked:
		return "resource_locked"
	case errors.Is(err, driveops.ErrDiskFull):
		return synctypes.IssueDiskFull
	case errors.Is(err, driveops.ErrFileTooLargeForSpace):
		return synctypes.IssueFileTooLargeForSpace
	case errors.Is(err, os.ErrPermission):
		return synctypes.IssueLocalPermissionDenied
	default:
		return ""
	}
}

// nowFunc returns the current time from the engine's injectable clock.
// Always set by NewEngine; tests overwrite with a controllable clock.
func (e *Engine) nowFunc() time.Time {
	return e.nowFn()
}

// resultStats returns the engine-owned counters and error list.
func (e *Engine) resultStats() (succeeded, failed int, errs []error) {
	e.syncErrorsMu.Lock()
	errs = make([]error, len(e.syncErrors))
	copy(errs, e.syncErrors)
	e.syncErrorsMu.Unlock()
	return int(e.succeeded.Load()), int(e.failed.Load()), errs
}

// resetResultStats resets the engine-owned counters for a new pass.
func (e *Engine) resetResultStats() {
	e.succeeded.Store(0)
	e.failed.Store(0)
	e.syncErrorsMu.Lock()
	e.syncErrors = nil
	e.syncErrorsMu.Unlock()
}

// directionFromAction maps a synctypes.ActionType to a typed Direction enum.
// All ActionType values are explicitly covered — no default case, so the
// exhaustive linter catches any new ActionType values added in the future.
func directionFromAction(at synctypes.ActionType) synctypes.Direction {
	switch at {
	case synctypes.ActionUpload:
		return synctypes.DirectionUpload
	case synctypes.ActionDownload, synctypes.ActionFolderCreate, synctypes.ActionConflict:
		return synctypes.DirectionDownload
	case synctypes.ActionLocalDelete, synctypes.ActionRemoteDelete:
		return synctypes.DirectionDelete
	case synctypes.ActionLocalMove, synctypes.ActionRemoteMove,
		synctypes.ActionUpdateSynced, synctypes.ActionCleanup:
		// Metadata ops default to download direction for failure recording.
		return synctypes.DirectionDownload
	}
	// Unreachable — all ActionType values are covered above.
	return synctypes.DirectionDownload
}

// armTrialTimer sets (or resets) the trial timer to fire at the earliest
// NextTrialAt across all scope blocks. Uses time.AfterFunc to send to the
// persistent trialCh channel, avoiding a race where the watch loop's select
// watches the old timer's channel after replacement. Called after scope blocks
// are created, trials dispatched, or trial results processed (R-2.10.5).
func (e *Engine) armTrialTimer() {
	if e.watch == nil {
		return
	}

	if e.watch.trialTimer != nil {
		e.watch.trialTimer.Stop()
		e.watch.trialTimer = nil
	}

	earliest, ok := syncdispatch.EarliestTrialAt(e.watch.activeScopes)
	if !ok {
		return
	}

	delay := time.Until(earliest)
	if delay <= 0 {
		delay = 1 * time.Millisecond // fire immediately
	}

	// Non-blocking send to the buffered(1) channel. If a signal is already
	// pending, the new one is coalesced (dropped). This is self-healing:
	// the watch loop calls DueTrials, so even if a second AfterFunc
	// fires while a signal is pending, all due scopes are still processed
	// on the next loop iteration.
	e.watch.trialTimer = time.AfterFunc(delay, func() {
		select {
		case e.trialCh <- struct{}{}:
		default:
		}
	})
}

// trialTimerChan returns the persistent trial notification channel.
// time.AfterFunc sends to this channel when a trial timer fires.
// The channel is always non-nil after NewEngine.
func (e *Engine) trialTimerChan() <-chan struct{} {
	return e.trialCh
}

// stopTrialTimer stops and clears the trial timer. Called on shutdown.
func (e *Engine) stopTrialTimer() {
	if e.watch == nil {
		return
	}

	if e.watch.trialTimer != nil {
		e.watch.trialTimer.Stop()
		e.watch.trialTimer = nil
	}
}

// handleTrialTimer dispatches due scope trials via the active-scope working
// set. In one-shot mode this is a no-op.
func (e *Engine) handleTrialTimer(ctx context.Context) {
	if e.watch != nil {
		e.runTrialDispatch(ctx)
	}
}

// handleRetryTimer runs a retrier sweep for due sync_failures. Only active
// when depGraph is initialized (watch mode with new architecture).
func (e *Engine) handleRetryTimer(ctx context.Context) {
	if e.depGraph != nil {
		e.runRetrierSweep(ctx)
	}
}
