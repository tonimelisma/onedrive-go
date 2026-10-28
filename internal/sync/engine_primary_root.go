package sync

import (
	"context"
	"errors"
)

func (batch *remoteObservationBatch) hasObservationFindings() bool {
	if batch == nil {
		return false
	}

	return len(batch.findings.Issues) > 0 ||
		len(batch.findings.ManagedIssueTypes) > 0
}

func (batch *remoteObservationBatch) deferredProgress() *remoteObservationBatch {
	if batch == nil ||
		(batch.cursorToken == "" &&
			!batch.markFullRemoteRefresh &&
			!batch.fullRefreshDueNow &&
			batch.observationMode != remoteObservationModeEnumerate) {
		return nil
	}

	clone := *batch
	clone.source = ""
	clone.observed = nil
	clone.emitted = nil
	clone.findings = ObservationFindingsBatch{}
	clone.shortcutTopology = shortcutTopologyBatch{}
	clone.armFullRefreshTimer = false
	clone.markFullRefreshIfIdle = false
	clone.applyAck = nil

	return &clone
}

func buildRemoteObservationBatch(
	engine *Engine,
	mode remoteObservationMode,
	events []changeEvent,
	token string,
	markFullRemoteRefresh bool,
	findings ObservationFindingsBatch,
) remoteObservationBatch {
	projected := projectRemoteObservations(engine.logger, events)

	return remoteObservationBatch{
		observationMode:       mode,
		observed:              projected.observed,
		emitted:               append([]changeEvent(nil), projected.emitted...),
		cursorToken:           primaryObservationCursorToken(token, markFullRemoteRefresh, len(projected.emitted), mode),
		markFullRemoteRefresh: markFullRemoteRefresh,
		findings:              findings,
	}
}

func primaryObservationCursorToken(
	token string,
	markFullRemoteRefresh bool,
	eventCount int,
	mode remoteObservationMode,
) string {
	needsEnumerateClamp := mode == remoteObservationModeEnumerate
	if token == "" && !markFullRemoteRefresh && !needsEnumerateClamp {
		return ""
	}
	if !markFullRemoteRefresh && eventCount == 0 && !needsEnumerateClamp {
		return ""
	}

	return token
}

func buildPrimaryWatchBatch(
	engine *Engine,
	bl *Baseline,
	primaryEvents []changeEvent,
	newToken string,
) remoteObservationBatch {
	batch := buildRemoteObservationBatch(
		engine,
		remoteObservationModeDelta,
		primaryEvents,
		newToken,
		false,
		newRemoteObservationFindingsBatch(),
	)
	batch.fullRefreshDueNow = deltaWindowDeletesSyncedFolder(bl, primaryEvents)
	batch.source = remoteObservationBatchPrimaryWatch
	batch.applyAck = make(chan error, 1)

	return batch
}

func (e *Engine) preferredMountRootObservationMode() remoteObservationMode {
	if e.mountRootDeltaSupported() {
		return remoteObservationModeDelta
	}

	return remoteObservationModeEnumerate
}

func (flow *engineFlow) executePrimaryRootObservation(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (remoteObservationBatch, error) {
	if flow.engine.hasRemoteMountRoot() {
		return flow.executeMountRootObservation(ctx, bl, fullReconcile)
	}

	return flow.executeDriveRootObservation(ctx, bl, fullReconcile)
}

func (flow *engineFlow) executeDriveRootObservation(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (remoteObservationBatch, error) {
	if fullReconcile {
		return flow.executeDriveRootFullObservation(ctx, bl, resyncKindApplyDifferences)
	}

	events, token, topology, err := flow.observeRemoteWithShortcutTopology(ctx, bl)
	if errors.Is(err, errDeltaExpired) {
		// Info: self-healing. Recovery is a full refresh with orphan
		// detection, not a bare re-enumeration: a delete that happened while
		// the token aged out is in no delta window (R-2.15.3).
		flow.engine.logger.Info("OneDrive change history expired; rescanning the whole drive (this may take a while)")
		return flow.executeDriveRootFullObservation(ctx, bl, resyncKindOf(err))
	}
	if err != nil && isObservationRemoteReadDenied(err) {
		return buildRemoteObservationBatch(
			flow.engine,
			remoteObservationModeDelta,
			nil,
			"",
			false,
			rootRemoteReadDeniedObservationFindingsBatch(flow.engine.driveID),
		), nil
	}

	batch := buildRemoteObservationBatch(
		flow.engine,
		remoteObservationModeDelta,
		events,
		token,
		false,
		newRemoteObservationFindingsBatch(),
	)
	batch.fullRefreshDueNow = deltaWindowDeletesSyncedFolder(bl, events)
	batch.shortcutTopology = topology
	return batch, err
}

// deltaWindowDeletesSyncedFolder reports whether an incremental delta window
// deleted a folder the baseline holds. Delta reports a deleted folder alone,
// never its descendants (R-2.17.2), and that silence also covers an item moved
// into the folder in the same window: the move is part of a subtree delta
// never repeats, so the item's row stays at its old path as a ghost and its
// synced local copy is never deleted. Only a full enumeration can see that
// such an item is gone, so the folder's tombstone makes the full refresh due
// at once; its orphans are confirmed by ID before anything is deleted
// (confirmEnumerationOrphans).
//
// A folder this client deleted itself is no longer in the baseline when its
// tombstone echoes back, so propagating a local folder delete does not cost a
// full enumeration.
func deltaWindowDeletesSyncedFolder(bl *Baseline, events []changeEvent) bool {
	if bl == nil {
		return false
	}
	for i := range events {
		if !events[i].IsDeleted || events[i].ItemID == "" {
			continue
		}
		if entry, ok := bl.GetByID(events[i].ItemID); ok && entry.ItemType == ItemTypeFolder {
			return true
		}
	}

	return false
}

// executeDriveRootFullObservation runs the full refresh with orphan detection
// and marks it as one, which also restarts the full-refresh cadence.
func (flow *engineFlow) executeDriveRootFullObservation(
	ctx context.Context,
	bl *Baseline,
	kind resyncKind,
) (remoteObservationBatch, error) {
	events, token, topology, err := flow.observeRemoteFullWithShortcutTopology(ctx, bl, kind)
	if err != nil && isObservationRemoteReadDenied(err) {
		return buildRemoteObservationBatch(
			flow.engine,
			remoteObservationModeDelta,
			nil,
			"",
			false,
			rootRemoteReadDeniedObservationFindingsBatch(flow.engine.driveID),
		), nil
	}

	batch := buildRemoteObservationBatch(
		flow.engine,
		remoteObservationModeDelta,
		events,
		token,
		true,
		newRemoteObservationFindingsBatch(),
	)
	batch.shortcutTopology = topology
	return batch, err
}

func (flow *engineFlow) executeMountRootObservation(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (remoteObservationBatch, error) {
	events, token, mode, err := flow.observeMountRootRemote(ctx, bl, fullReconcile)
	if err != nil && isObservationRemoteReadDenied(err) {
		return buildRemoteObservationBatch(
			flow.engine,
			mode,
			nil,
			"",
			false,
			rootRemoteReadDeniedObservationFindingsBatch(flow.engine.driveID),
		), nil
	}

	return buildRemoteObservationBatch(
		flow.engine,
		mode,
		events,
		token,
		fullReconcile,
		newRemoteObservationFindingsBatch(),
	), err
}
