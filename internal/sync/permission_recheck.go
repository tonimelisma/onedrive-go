package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

const permissionMaintenanceInterval = 60 * time.Second

type permissionMaintenanceReason int

const (
	permissionMaintenanceStartup permissionMaintenanceReason = iota + 1
	permissionMaintenancePeriodic
	permissionMaintenanceLocalObservation
)

type permissionMaintenanceRequest struct {
	Reason       permissionMaintenanceReason
	ChangedPaths map[string]bool
}

type permissionRetryWorkReadyPlan struct {
	Work     RetryWorkKey
	ScopeKey ScopeKey
	Path     string
}

type permissionMaintenanceSnapshot struct {
	lastCheckedAt         time.Time
	trackLastCheckedAt    bool
	observationSuppressed bool
}

type permissionBlockedRetryState struct {
	row      RetryWorkRow
	resolved bool
}

type permissionMaintenanceState struct {
	blockScopes      []*BlockScope
	blockedRetryWork []permissionBlockedRetryState
}

type permissionMaintenancePlan struct {
	UpdateLastCheckedAt bool
	CheckedAt           time.Time
	Decisions           []PermissionRecheckDecision
	RetryWorkReadies    []permissionRetryWorkReadyPlan
}

// runPermissionMaintenance is the engine-facing entrypoint for permission-
// specific maintenance. Permission code owns deciding what to recheck and
// when remote-write blocked retry rows are stale; the scope controller still
// owns all durable mutations.
func (controller *scopeController) runPermissionMaintenance(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
	request permissionMaintenanceRequest,
) {
	snapshot := controller.permissionMaintenanceSnapshot(watch)
	state := controller.loadPermissionMaintenanceState(ctx, bl, request)
	plan := buildPermissionMaintenancePlan(ctx, controller.flow.engine.permHandler, bl, request, snapshot, state)
	controller.applyPermissionMaintenancePlan(ctx, watch, plan)
}

func (controller *scopeController) permissionMaintenanceSnapshot(
	watch *watchRuntime,
) permissionMaintenanceSnapshot {
	if watch == nil {
		return permissionMaintenanceSnapshot{}
	}

	return permissionMaintenanceSnapshot{
		lastCheckedAt:         watch.lastPermRecheck,
		trackLastCheckedAt:    true,
		observationSuppressed: controller.isObservationSuppressed(watch),
	}
}

func (controller *scopeController) loadPermissionMaintenanceState(
	ctx context.Context,
	bl *Baseline,
	request permissionMaintenanceRequest,
) permissionMaintenanceState {
	state := permissionMaintenanceState{
		blockScopes: controller.loadPermissionMaintenanceBlockScopes(ctx),
	}

	switch request.Reason {
	case permissionMaintenanceStartup, permissionMaintenanceLocalObservation:
		state.blockedRetryWork = controller.loadPermissionBlockedRetryState(ctx, bl)
	case permissionMaintenancePeriodic:
		// Periodic maintenance reuses persisted permission scopes only; it does
		// not need retry_work evidence.
	}

	return state
}

func buildPermissionMaintenancePlan(
	ctx context.Context,
	ph *PermissionHandler,
	bl *Baseline,
	request permissionMaintenanceRequest,
	snapshot permissionMaintenanceSnapshot,
	state permissionMaintenanceState,
) permissionMaintenancePlan {
	switch request.Reason {
	case permissionMaintenanceStartup:
		plan := permissionMaintenancePlan{
			RetryWorkReadies: startupRemoteWriteRetryWorkReadyPlans(state.blockedRetryWork),
		}
		if ph == nil {
			return plan
		}
		plan.Decisions = ph.startupRecheckDecisionsFromBlocks(ctx, bl, state.blockScopes)
		if snapshot.trackLastCheckedAt {
			plan.UpdateLastCheckedAt = true
			plan.CheckedAt = ph.nowFn()
		}
		return plan
	case permissionMaintenancePeriodic:
		if ph == nil || !snapshot.trackLastCheckedAt {
			return permissionMaintenancePlan{}
		}
		return ph.periodicMaintenancePlanFromBlocks(ctx, bl, snapshot, state.blockScopes)
	case permissionMaintenanceLocalObservation:
		return permissionMaintenancePlan{
			RetryWorkReadies: remoteWriteRetryWorkReadyPlansForChangedPaths(
				state.blockedRetryWork,
				request.ChangedPaths,
			),
		}
	default:
		panic("unknown permission maintenance reason")
	}
}

func (controller *scopeController) applyPermissionMaintenancePlan(
	ctx context.Context,
	watch *watchRuntime,
	plan permissionMaintenancePlan,
) {
	for i := range plan.RetryWorkReadies {
		ready := plan.RetryWorkReadies[i]
		if err := controller.flow.engine.baseline.ClearBlockedRetryWork(ctx, ready.Work, ready.ScopeKey); err != nil {
			controller.flow.engine.logger.Debug("permission maintenance: failed to clear blocked retry work",
				slog.String("path", ready.Path),
				slog.String("scope_key", ready.ScopeKey.String()),
				slog.String("error", err.Error()),
			)
		}
	}

	if len(plan.Decisions) > 0 {
		controller.applyPermissionRecheckDecisions(ctx, watch, plan.Decisions)
	}
	if watch != nil && plan.UpdateLastCheckedAt {
		watch.lastPermRecheck = plan.CheckedAt
	}
}

func (controller *scopeController) loadPermissionMaintenanceBlockScopes(ctx context.Context) []*BlockScope {
	blocks, err := controller.flow.engine.baseline.ListBlockScopes(ctx)
	if err != nil {
		controller.flow.engine.logger.Warn("permission maintenance: failed to list block scopes",
			slog.String("error", err.Error()),
		)
		return nil
	}

	return blocks
}

func (controller *scopeController) loadPermissionBlockedRetryState(
	ctx context.Context,
	bl *Baseline,
) []permissionBlockedRetryState {
	rows, err := controller.flow.engine.baseline.ListBlockedRetryWork(ctx)
	if err != nil {
		controller.flow.engine.logger.Warn("permission maintenance: failed to list blocked retry_work",
			slog.String("error", err.Error()),
		)
		return nil
	}

	return controller.classifyPermissionBlockedRetryState(ctx, bl, rows)
}

func (controller *scopeController) classifyPermissionBlockedRetryState(
	ctx context.Context,
	bl *Baseline,
	rows []RetryWorkRow,
) []permissionBlockedRetryState {
	if len(rows) == 0 {
		return nil
	}

	flow := controller.flow
	driveID, driveErr := flow.retryWorkDriveID(ctx)
	if driveErr != nil {
		flow.engine.logger.Warn("permission maintenance: failed to load drive for retry_work classification",
			slog.String("error", driveErr.Error()),
		)
		return nil
	}

	classified := make([]permissionBlockedRetryState, 0, len(rows))
	for i := range rows {
		state := permissionBlockedRetryState{row: rows[i]}
		if rows[i].ScopeKey.IsPermRemoteWrite() {
			state.resolved = flow.buildRetryCandidateFromRetryWork(ctx, bl, &rows[i], driveID).resolved
		}
		classified = append(classified, state)
	}

	return classified
}

// startupRecheckDecisions is the single startup entrypoint for persisted
// permission-scope maintenance. The engine owns applying the decisions, but
// permission policy owns deciding which persisted permission scopes should be
// released or retained after current truth is re-probed.
func (ph *PermissionHandler) startupRecheckDecisions(
	ctx context.Context,
	bl *Baseline,
) []PermissionRecheckDecision {
	blocks, err := ph.store.ListBlockScopes(ctx)
	if err != nil {
		return nil
	}

	return ph.startupRecheckDecisionsFromBlocks(ctx, bl, blocks)
}

func (ph *PermissionHandler) startupRecheckDecisionsFromBlocks(
	ctx context.Context,
	bl *Baseline,
	blocks []*BlockScope,
) []PermissionRecheckDecision {
	return ph.permissionRecheckDecisionsFromBlocks(ctx, bl, blocks, true)
}

func (ph *PermissionHandler) periodicRecheckDecisionsFromBlocks(
	ctx context.Context,
	bl *Baseline,
	blocks []*BlockScope,
	includeRemote bool,
) []PermissionRecheckDecision {
	return ph.permissionRecheckDecisionsFromBlocks(ctx, bl, blocks, includeRemote)
}

func (ph *PermissionHandler) periodicMaintenancePlan(
	ctx context.Context,
	bl *Baseline,
	lastRecheck time.Time,
	observationSuppressed bool,
) permissionMaintenancePlan {
	blocks, err := ph.store.ListBlockScopes(ctx)
	if err != nil {
		return permissionMaintenancePlan{}
	}

	return ph.periodicMaintenancePlanFromBlocks(ctx, bl, permissionMaintenanceSnapshot{
		lastCheckedAt:         lastRecheck,
		trackLastCheckedAt:    true,
		observationSuppressed: observationSuppressed,
	}, blocks)
}

func (ph *PermissionHandler) periodicMaintenancePlanFromBlocks(
	ctx context.Context,
	bl *Baseline,
	snapshot permissionMaintenanceSnapshot,
	blockScopes []*BlockScope,
) permissionMaintenancePlan {
	now := ph.nowFn()
	if now.Sub(snapshot.lastCheckedAt) < permissionMaintenanceInterval {
		return permissionMaintenancePlan{}
	}

	includeRemote := ph.includeRemotePeriodicRechecks(snapshot.observationSuppressed)
	return permissionMaintenancePlan{
		UpdateLastCheckedAt: true,
		CheckedAt:           now,
		Decisions:           ph.periodicRecheckDecisionsFromBlocks(ctx, bl, blockScopes, includeRemote),
	}
}

// includeRemotePeriodicRechecks returns whether periodic maintenance should
// probe remote write scopes on this pass. Remote permission probes can be
// suppressed when broader remote observation is intentionally skipped, while
// local read/write rechecks still run as direct filesystem observation.
func (ph *PermissionHandler) includeRemotePeriodicRechecks(observationSuppressed bool) bool {
	return ph.HasPermChecker() && !observationSuppressed
}

func (ph *PermissionHandler) permissionRecheckDecisionsFromBlocks(
	ctx context.Context,
	bl *Baseline,
	blocks []*BlockScope,
	includeRemote bool,
) []PermissionRecheckDecision {
	var decisions []PermissionRecheckDecision
	if includeRemote && ph.HasPermChecker() {
		decisions = append(decisions, ph.recheckPermissionsFromBlocks(ctx, bl, blocks, nil)...)
	}
	return append(decisions, ph.recheckLocalPermissionsFromBlocks(blocks)...)
}

// recheckPermissions rechecks persisted remote write-denial scopes at the
// start of each sync pass. Read-denial scopes are observation-owned and clear
// only through observation reconciliation.
func (ph *PermissionHandler) recheckPermissions(
	ctx context.Context,
	bl *Baseline,
) []PermissionRecheckDecision {
	blocks, err := ph.store.ListBlockScopes(ctx)
	if err != nil {
		return nil
	}

	return ph.recheckPermissionsFromBlocks(ctx, bl, blocks, nil)
}

func (ph *PermissionHandler) recheckPermissionsFromBlocks(
	ctx context.Context,
	bl *Baseline,
	blocks []*BlockScope,
	scopeFilter map[ScopeKey]bool,
) []PermissionRecheckDecision {
	if ph.permChecker == nil || len(blocks) == 0 {
		return nil
	}

	var decisions []PermissionRecheckDecision
	seen := make(map[ScopeKey]bool, len(blocks))

	for i := range blocks {
		block := blocks[i]
		if block == nil || !block.Key.IsPermRemoteWrite() {
			continue
		}
		if seen[block.Key] {
			continue
		}
		if len(scopeFilter) > 0 && !scopeFilter[block.Key] {
			continue
		}
		seen[block.Key] = true
		decisions = append(decisions, ph.recheckRemotePermissionBlock(ctx, bl, block))
	}

	return decisions
}

func (ph *PermissionHandler) recheckRemotePermissionBlock(
	ctx context.Context,
	bl *Baseline,
	block *BlockScope,
) PermissionRecheckDecision {
	boundaryPath := block.CoveredPath()

	root := ph.permissionRoot()
	if root == nil {
		return PermissionRecheckDecision{
			Kind:     permissionRecheckKeepScope,
			Path:     boundaryPath,
			ScopeKey: block.Key,
			Reason:   "configured remote root unavailable; keeping remote permission scope until probe succeeds",
		}
	}

	remoteDriveID := driveid.New(root.remoteDrive)
	remoteItemID := resolveBoundaryRemoteItemID(bl, boundaryPath, remoteDriveID, root)
	if remoteItemID == "" {
		return PermissionRecheckDecision{
			Kind:     permissionRecheckKeepScope,
			Path:     boundaryPath,
			ScopeKey: block.Key,
			Reason:   "remote permission boundary not resolvable yet; keeping scope",
		}
	}

	perms, permErr := ph.permChecker.ListItemPermissions(ctx, remoteDriveID, remoteItemID)
	if permErr != nil {
		return PermissionRecheckDecision{
			Kind:     permissionRecheckKeepScope,
			Path:     boundaryPath,
			ScopeKey: block.Key,
			Reason:   "permission recheck failed; keeping remote permission scope",
		}
	}

	switch graph.EvaluateWriteAccess(perms, ph.accountEmail) {
	case graph.PermissionWriteAccessWritable:
		return PermissionRecheckDecision{
			Kind:     permissionRecheckReleaseScope,
			Path:     boundaryPath,
			ScopeKey: block.Key,
			Reason:   "remote write permission granted; releasing remote permission scope",
		}
	case graph.PermissionWriteAccessInconclusive:
		return PermissionRecheckDecision{
			Kind:     permissionRecheckKeepScope,
			Path:     boundaryPath,
			ScopeKey: block.Key,
			Reason:   "permission recheck inconclusive; keeping remote permission scope",
		}
	case graph.PermissionWriteAccessReadOnly:
		return PermissionRecheckDecision{
			Kind:     permissionRecheckKeepScope,
			Path:     boundaryPath,
			ScopeKey: block.Key,
			Reason:   "remote permission boundary still denied",
		}
	}

	return PermissionRecheckDecision{
		Kind:     permissionRecheckKeepScope,
		Path:     boundaryPath,
		ScopeKey: block.Key,
		Reason:   "permission recheck inconclusive; keeping remote permission scope",
	}
}

// recheckLocalPermissions revalidates persisted local permission scopes. Local
// write scopes clear on affirmative write-probe success. Local read scopes may
// also clear on direct accessibility revalidation because that check is part of
// observation.
func (ph *PermissionHandler) recheckLocalPermissions(ctx context.Context) []PermissionRecheckDecision {
	blocks, err := ph.store.ListBlockScopes(ctx)
	if err != nil {
		return nil
	}

	return ph.recheckLocalPermissionsFromBlocks(blocks)
}

func (ph *PermissionHandler) recheckLocalPermissionsFromBlocks(
	blocks []*BlockScope,
) []PermissionRecheckDecision {
	if len(blocks) == 0 {
		return nil
	}

	var decisions []PermissionRecheckDecision

	for i := range blocks {
		block := blocks[i]
		if block == nil || !block.Key.IsPermDir() {
			continue
		}

		dirPath := block.CoveredPath()
		clearable := false
		switch {
		case block.Key.IsPermLocalRead():
			clearable = isDirAccessible(ph.syncTree, dirPath)
		case block.Key.IsPermLocalWrite():
			clearable = isDirWritable(ph.syncTree, dirPath)
		}
		if !clearable {
			decisions = append(decisions, PermissionRecheckDecision{
				Kind:     permissionRecheckKeepScope,
				Path:     dirPath,
				ScopeKey: block.Key,
				Reason:   "local permission denial still active",
			})
			continue
		}

		decisions = append(decisions, PermissionRecheckDecision{
			Kind:     permissionRecheckReleaseScope,
			Path:     dirPath,
			ScopeKey: block.Key,
			Reason:   "local permission restored, clearing denial",
		})
	}

	return decisions
}

// startupRemoteWriteRetryWorkReadyPlans returns persisted blocked retry rows
// whose remote-write-denied exact work is already resolved by the current
// baseline at startup. Permission maintenance owns selecting these rows, while
// the generic apply step owns the durable mutation.
func startupRemoteWriteRetryWorkReadyPlans(
	blockedRetryWork []permissionBlockedRetryState,
) []permissionRetryWorkReadyPlan {
	return resolvedRemoteWriteRetryWorkReadyPlans(
		blockedRetryWork,
		func(state permissionBlockedRetryState) bool {
			return state.row.ScopeKey.IsPermRemoteWrite()
		},
	)
}

// remoteWriteRetryWorkReadyPlansForChangedPaths returns remote-write blocked
// retry rows whose exact work is already resolved after local observation
// changed a relevant subtree. Permission maintenance owns selecting these
// rows; it must not release the persisted permission scope on its own.
func remoteWriteRetryWorkReadyPlansForChangedPaths(
	blockedRetryWork []permissionBlockedRetryState,
	changedPaths map[string]bool,
) []permissionRetryWorkReadyPlan {
	if len(blockedRetryWork) == 0 || len(changedPaths) == 0 {
		return nil
	}

	return resolvedRemoteWriteRetryWorkReadyPlans(
		blockedRetryWork,
		func(state permissionBlockedRetryState) bool {
			return remoteWriteBlockedRetryRelevant(&state.row, changedPaths)
		},
	)
}

func resolvedRemoteWriteRetryWorkReadyPlans(
	rows []permissionBlockedRetryState,
	relevant func(permissionBlockedRetryState) bool,
) []permissionRetryWorkReadyPlan {
	var readyPlans []permissionRetryWorkReadyPlan
	for i := range rows {
		if relevant != nil && !relevant(rows[i]) {
			continue
		}
		if rows[i].resolved {
			readyPlans = append(readyPlans, permissionRetryWorkReadyPlan{
				Work:     retryWorkKeyForRetryWork(&rows[i].row),
				ScopeKey: rows[i].row.ScopeKey,
				Path:     rows[i].row.Path,
			})
		}
	}

	return readyPlans
}

func remoteWriteBlockedRetryRelevant(
	row *RetryWorkRow,
	changedPaths map[string]bool,
) bool {
	if row == nil || !row.ScopeKey.IsPermRemoteWrite() {
		return false
	}

	boundary := row.ScopeKey.RemotePath()
	for changedPath := range changedPaths {
		if scopePathMatches(row.Path, changedPath) ||
			scopePathMatches(changedPath, row.Path) ||
			scopePathMatches(boundary, changedPath) ||
			scopePathMatches(changedPath, boundary) {
			return true
		}
	}

	return false
}
