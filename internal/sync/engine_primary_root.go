package sync

import (
	"context"
	"fmt"
)

type remoteObservationMode string

const (
	remoteObservationModeDelta     remoteObservationMode = "delta"
	remoteObservationModeEnumerate remoteObservationMode = "enumerate"
)

type pendingPrimaryCursorCommit struct {
	driveID               string
	rootID                string
	token                 string
	markFullRemoteRefresh bool
	observationMode       remoteObservationMode
}

type remoteObservationBatchSource string

const (
	remoteObservationBatchPrimaryWatch remoteObservationBatchSource = "primary_watch"
	remoteObservationBatchSharedRoot   remoteObservationBatchSource = "shared_root_watch"
	remoteObservationBatchFullRefresh  remoteObservationBatchSource = "full_refresh"
)

type remoteObservationBatch struct {
	source                remoteObservationBatchSource
	observationMode       remoteObservationMode
	observed              []ObservedItem
	emitted               []ChangeEvent
	pending               *pendingPrimaryCursorCommit
	findings              ObservationFindingsBatch
	armFullRefreshTimer   bool
	markFullRefreshIfIdle bool
	applyAck              chan error
}

type remoteFetchResult struct {
	observationMode remoteObservationMode
	events          []ChangeEvent
	pending         *pendingPrimaryCursorCommit
	findings        ObservationFindingsBatch
}

func (r *remoteFetchResult) hasObservationFindings() bool {
	if r == nil {
		return false
	}
	return len(r.findings.Issues) > 0 ||
		len(r.findings.ManagedIssueTypes) > 0
}

type primaryRootObservationKind string

const (
	primaryRootObservationDriveRoot  primaryRootObservationKind = "drive_root"
	primaryRootObservationSharedRoot primaryRootObservationKind = "shared_root"
)

type primaryRootObservationPlan struct {
	kind          primaryRootObservationKind
	fullReconcile bool
}

func (e *Engine) preferredSharedRootObservationMode() remoteObservationMode {
	if e.sharedRootDeltaSupported() && e.folderDelta != nil {
		return remoteObservationModeDelta
	}

	return remoteObservationModeEnumerate
}

func (flow *engineFlow) buildPrimaryRootObservationPlan(fullReconcile bool) primaryRootObservationPlan {
	plan := primaryRootObservationPlan{
		kind:          primaryRootObservationDriveRoot,
		fullReconcile: fullReconcile,
	}
	if flow.engine.hasSharedRoot() {
		plan.kind = primaryRootObservationSharedRoot
	}

	return plan
}

func (flow *engineFlow) executePrimaryRootObservation(
	ctx context.Context,
	bl *Baseline,
	plan primaryRootObservationPlan,
) (remoteFetchResult, error) {
	switch plan.kind {
	case primaryRootObservationDriveRoot:
		return flow.executeDriveRootObservation(ctx, bl, plan.fullReconcile)
	case primaryRootObservationSharedRoot:
		return flow.executeSharedRootObservation(ctx, bl, plan.fullReconcile)
	default:
		return remoteFetchResult{}, fmt.Errorf("unknown primary root observation kind %q", plan.kind)
	}
}

func (flow *engineFlow) executeDriveRootObservation(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (remoteFetchResult, error) {
	if fullReconcile {
		events, token, err := flow.observeRemoteFull(ctx, bl)
		if err != nil && isObservationRemoteReadDenied(err) {
			return remoteFetchResult{
				observationMode: remoteObservationModeDelta,
				findings:        rootRemoteReadDeniedObservationFindingsBatch(flow.engine.driveID),
			}, nil
		}
		return remoteFetchResult{
			observationMode: remoteObservationModeDelta,
			events:          events,
			pending:         primaryCursorCommit(token, flow.engine, true, len(events), remoteObservationModeDelta),
			findings:        newRemoteObservationFindingsBatch(),
		}, err
	}

	events, token, err := flow.observeRemote(ctx, bl)
	if err != nil && isObservationRemoteReadDenied(err) {
		return remoteFetchResult{
			observationMode: remoteObservationModeDelta,
			findings:        rootRemoteReadDeniedObservationFindingsBatch(flow.engine.driveID),
		}, nil
	}
	return remoteFetchResult{
		observationMode: remoteObservationModeDelta,
		events:          events,
		pending:         primaryCursorCommit(token, flow.engine, false, len(events), remoteObservationModeDelta),
		findings:        newRemoteObservationFindingsBatch(),
	}, err
}

func (flow *engineFlow) executeSharedRootObservation(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (remoteFetchResult, error) {
	events, token, mode, err := flow.observeSharedRootRemote(ctx, bl, fullReconcile)
	if err != nil && isObservationRemoteReadDenied(err) {
		return remoteFetchResult{
			observationMode: mode,
			findings:        rootRemoteReadDeniedObservationFindingsBatch(flow.engine.driveID),
		}, nil
	}
	return remoteFetchResult{
		observationMode: mode,
		events:          events,
		pending:         primaryCursorCommit(token, flow.engine, fullReconcile, len(events), mode),
		findings:        newRemoteObservationFindingsBatch(),
	}, err
}

func primaryCursorCommit(
	token string,
	eng *Engine,
	fullReconcile bool,
	eventCount int,
	mode remoteObservationMode,
) *pendingPrimaryCursorCommit {
	needsEnumerateClamp := mode == remoteObservationModeEnumerate
	if token == "" && !fullReconcile && !needsEnumerateClamp {
		return nil
	}
	if !fullReconcile && eventCount == 0 && !needsEnumerateClamp {
		return nil
	}

	rootID := ""
	if eng.hasSharedRoot() {
		rootID = eng.rootItemID
	}

	return &pendingPrimaryCursorCommit{
		driveID:               eng.driveID.String(),
		rootID:                rootID,
		token:                 token,
		markFullRemoteRefresh: fullReconcile,
		observationMode:       mode,
	}
}
