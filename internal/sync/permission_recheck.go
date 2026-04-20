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
	plan := controller.buildPermissionMaintenancePlan(ctx, watch, bl, request)
	controller.applyPermissionMaintenancePlan(ctx, watch, plan)
}

func (controller *scopeController) buildPermissionMaintenancePlan(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
	request permissionMaintenanceRequest,
) permissionMaintenancePlan {
	ph := controller.flow.engine.permHandler

	switch request.Reason {
	case permissionMaintenanceStartup:
		plan := permissionMaintenancePlan{
			RetryWorkReadies: controller.startupRemoteWriteRetryWorkReadyPlans(ctx, bl),
		}
		if ph == nil {
			return plan
		}
		plan.Decisions = ph.startupRecheckDecisions(ctx, bl)
		if watch != nil {
			plan.UpdateLastCheckedAt = true
			plan.CheckedAt = ph.nowFn()
		}
		return plan
	case permissionMaintenancePeriodic:
		if ph == nil || watch == nil {
			return permissionMaintenancePlan{}
		}
		return ph.periodicMaintenancePlan(
			ctx,
			bl,
			watch.lastPermRecheck,
			controller.isObservationSuppressed(watch),
		)
	case permissionMaintenanceLocalObservation:
		return permissionMaintenancePlan{
			RetryWorkReadies: controller.remoteWriteRetryWorkReadyPlansForChangedPaths(ctx, bl, request.ChangedPaths),
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

// startupRecheckDecisions is the single startup entrypoint for persisted
// permission-scope maintenance. The engine owns applying the decisions, but
// permission policy owns deciding which persisted permission scopes should be
// released or retained after current truth is re-probed.
func (ph *PermissionHandler) startupRecheckDecisions(
	ctx context.Context,
	bl *Baseline,
) []PermissionRecheckDecision {
	return ph.permissionRecheckDecisions(ctx, bl, true)
}

// periodicRecheckDecisions is the steady-state permission-maintenance entrypoint.
// The engine may suppress remote Graph probing during broader observation
// suppression, but local filesystem rechecks still run because they are direct
// local observation of current truth.
func (ph *PermissionHandler) periodicRecheckDecisions(
	ctx context.Context,
	bl *Baseline,
	includeRemote bool,
) []PermissionRecheckDecision {
	return ph.permissionRecheckDecisions(ctx, bl, includeRemote)
}

func (ph *PermissionHandler) periodicMaintenancePlan(
	ctx context.Context,
	bl *Baseline,
	lastRecheck time.Time,
	observationSuppressed bool,
) permissionMaintenancePlan {
	now := ph.nowFn()
	if now.Sub(lastRecheck) < permissionMaintenanceInterval {
		return permissionMaintenancePlan{}
	}

	includeRemote := ph.includeRemotePeriodicRechecks(observationSuppressed)
	return permissionMaintenancePlan{
		UpdateLastCheckedAt: true,
		CheckedAt:           now,
		Decisions:           ph.periodicRecheckDecisions(ctx, bl, includeRemote),
	}
}

// includeRemotePeriodicRechecks returns whether periodic maintenance should
// probe remote write scopes on this pass. Remote permission probes can be
// suppressed when broader remote observation is intentionally skipped, while
// local read/write rechecks still run as direct filesystem observation.
func (ph *PermissionHandler) includeRemotePeriodicRechecks(observationSuppressed bool) bool {
	return ph.HasPermChecker() && !observationSuppressed
}

func (ph *PermissionHandler) permissionRecheckDecisions(
	ctx context.Context,
	bl *Baseline,
	includeRemote bool,
) []PermissionRecheckDecision {
	var decisions []PermissionRecheckDecision
	if includeRemote && ph.HasPermChecker() {
		decisions = append(decisions, ph.recheckPermissions(ctx, bl)...)
	}
	return append(decisions, ph.recheckLocalPermissions(ctx)...)
}

// recheckPermissions rechecks persisted remote write-denial scopes at the
// start of each sync pass. Read-denial scopes are observation-owned and clear
// only through observation reconciliation.
func (ph *PermissionHandler) recheckPermissions(
	ctx context.Context,
	bl *Baseline,
) []PermissionRecheckDecision {
	return ph.recheckPermissionsForScopeKeys(ctx, bl, nil)
}

func (ph *PermissionHandler) recheckPermissionsForScopeKeys(
	ctx context.Context,
	bl *Baseline,
	scopeFilter map[ScopeKey]bool,
) []PermissionRecheckDecision {
	if ph.permChecker == nil {
		return nil
	}

	blocks, err := ph.store.ListBlockScopes(ctx)
	if err != nil || len(blocks) == 0 {
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
	if err != nil || len(blocks) == 0 {
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
func (controller *scopeController) startupRemoteWriteRetryWorkReadyPlans(
	ctx context.Context,
	bl *Baseline,
) []permissionRetryWorkReadyPlan {
	if bl == nil {
		return nil
	}

	flow := controller.flow
	rows, err := flow.engine.baseline.ListBlockedRetryWork(ctx)
	if err != nil {
		flow.engine.logger.Warn("failed to list blocked retry_work rows for startup remote write cleanup",
			slog.String("error", err.Error()),
		)
		return nil
	}

	return controller.resolvedRemoteWriteRetryWorkReadyPlans(
		ctx,
		bl,
		rows,
		func(row *RetryWorkRow) bool {
			return row != nil && row.ScopeKey.IsPermRemoteWrite()
		},
	)
}

// remoteWriteRetryWorkReadyPlansForChangedPaths returns remote-write blocked
// retry rows whose exact work is already resolved after local observation
// changed a relevant subtree. Permission maintenance owns selecting these
// rows; it must not release the persisted permission scope on its own.
func (controller *scopeController) remoteWriteRetryWorkReadyPlansForChangedPaths(
	ctx context.Context,
	bl *Baseline,
	changedPaths map[string]bool,
) []permissionRetryWorkReadyPlan {
	if bl == nil || len(changedPaths) == 0 {
		return nil
	}

	flow := controller.flow
	rows, err := flow.engine.baseline.ListBlockedRetryWork(ctx)
	if err != nil {
		flow.engine.logger.Warn("failed to list blocked retry_work rows for remote write cleanup",
			slog.String("error", err.Error()),
		)
		return nil
	}

	return controller.resolvedRemoteWriteRetryWorkReadyPlans(
		ctx,
		bl,
		rows,
		func(row *RetryWorkRow) bool {
			return remoteWriteBlockedRetryRelevant(row, changedPaths)
		},
	)
}

func (controller *scopeController) resolvedRemoteWriteRetryWorkReadyPlans(
	ctx context.Context,
	bl *Baseline,
	rows []RetryWorkRow,
	relevant func(*RetryWorkRow) bool,
) []permissionRetryWorkReadyPlan {
	flow := controller.flow
	driveID, driveErr := flow.retryWorkDriveID(ctx)
	if driveErr != nil {
		flow.engine.logger.Warn("permission maintenance: failed to load drive for remote write cleanup",
			slog.String("error", driveErr.Error()),
		)
		return nil
	}

	var readyPlans []permissionRetryWorkReadyPlan
	for i := range rows {
		if relevant != nil && !relevant(&rows[i]) {
			continue
		}
		if flow.buildRetryCandidateFromRetryWork(ctx, bl, &rows[i], driveID).resolved {
			readyPlans = append(readyPlans, permissionRetryWorkReadyPlan{
				Work:     retryWorkKeyForRetryWork(&rows[i]),
				ScopeKey: rows[i].ScopeKey,
				Path:     rows[i].Path,
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
