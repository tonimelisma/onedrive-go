package sync

import (
	"context"
	"fmt"
)

type sharedRootObservationMode string

const (
	sharedRootObservationDelta     sharedRootObservationMode = "delta"
	sharedRootObservationEnumerate sharedRootObservationMode = "enumerate"
)

type pendingPrimaryCursorCommit struct {
	driveID               string
	rootID                string
	token                 string
	markFullRemoteRefresh bool
}

type remoteFetchResult struct {
	events   []ChangeEvent
	pending  *pendingPrimaryCursorCommit
	findings ObservationFindingsBatch
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

func (e *Engine) sharedRootObservationMode() sharedRootObservationMode {
	if e.folderDelta != nil {
		return sharedRootObservationDelta
	}

	return sharedRootObservationEnumerate
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
				findings: rootRemoteReadDeniedObservationFindingsBatch(flow.engine.driveID, err),
			}, nil
		}
		return remoteFetchResult{
			events:   events,
			pending:  primaryCursorCommit(token, flow.engine, true, len(events)),
			findings: newRemoteObservationFindingsBatch(),
		}, err
	}

	events, token, err := flow.observeRemote(ctx, bl)
	if err != nil && isObservationRemoteReadDenied(err) {
		return remoteFetchResult{
			findings: rootRemoteReadDeniedObservationFindingsBatch(flow.engine.driveID, err),
		}, nil
	}
	return remoteFetchResult{
		events:   events,
		pending:  primaryCursorCommit(token, flow.engine, false, len(events)),
		findings: newRemoteObservationFindingsBatch(),
	}, err
}

func (flow *engineFlow) executeSharedRootObservation(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (remoteFetchResult, error) {
	events, token, err := flow.observeSharedRootRemote(ctx, bl, fullReconcile)
	if err != nil && isObservationRemoteReadDenied(err) {
		return remoteFetchResult{
			findings: rootRemoteReadDeniedObservationFindingsBatch(flow.engine.driveID, err),
		}, nil
	}
	return remoteFetchResult{
		events:   events,
		pending:  primaryCursorCommit(token, flow.engine, fullReconcile, len(events)),
		findings: newRemoteObservationFindingsBatch(),
	}, err
}

func primaryCursorCommit(
	token string,
	eng *Engine,
	fullReconcile bool,
	eventCount int,
) *pendingPrimaryCursorCommit {
	if token == "" && !fullReconcile {
		return nil
	}
	if !fullReconcile && eventCount == 0 {
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
	}
}
