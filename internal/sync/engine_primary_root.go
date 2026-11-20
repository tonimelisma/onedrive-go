package sync

import (
	"context"
	"errors"
)

func (batch *RemoteObservationBatch) HasObservationFindings() bool {
	if batch == nil {
		return false
	}

	return len(batch.Findings.Issues) > 0 ||
		len(batch.Findings.ManagedIssueTypes) > 0
}

func (batch *RemoteObservationBatch) DeferredProgress() *RemoteObservationBatch {
	if batch == nil ||
		(batch.CursorToken == "" &&
			!batch.MarkFullRemoteRefresh &&
			!batch.FullRefreshDueSoon &&
			!batch.OrphansUndecided &&
			batch.ObservationMode != RemoteObservationModeEnumerate) {
		return nil
	}

	clone := *batch
	clone.Source = ""
	clone.Observed = nil
	clone.Changes = nil
	clone.Findings = ObservationFindingsBatch{}
	clone.ShortcutTopology = ShortcutTopologyBatch{}
	clone.ArmFullRefreshTimer = false
	clone.MarkFullRefreshIfIdle = false
	clone.ApplyAck = nil

	return &clone
}

func buildRemoteObservationBatch(
	engine *Engine,
	mode RemoteObservationMode,
	events []RemoteItem,
	token string,
	markFullRemoteRefresh bool,
	findings ObservationFindingsBatch,
) RemoteObservationBatch {
	projected := ProjectRemoteObservations(engine.logger, events)

	return RemoteObservationBatch{
		ObservationMode:       mode,
		Observed:              projected.Observed,
		Changes:               projected.Changes,
		CursorToken:           primaryObservationCursorToken(token, markFullRemoteRefresh, len(projected.Changes), mode),
		MarkFullRemoteRefresh: markFullRemoteRefresh,
		Findings:              findings,
	}
}

func primaryObservationCursorToken(
	token string,
	markFullRemoteRefresh bool,
	changeCount int,
	mode RemoteObservationMode,
) string {
	needsEnumerateClamp := mode == RemoteObservationModeEnumerate
	if token == "" && !markFullRemoteRefresh && !needsEnumerateClamp {
		return ""
	}
	if !markFullRemoteRefresh && changeCount == 0 && !needsEnumerateClamp {
		return ""
	}

	return token
}

func buildPrimaryWatchBatch(
	engine *Engine,
	bl *Baseline,
	primaryEvents []RemoteItem,
	newToken string,
) RemoteObservationBatch {
	batch := buildRemoteObservationBatch(
		engine,
		RemoteObservationModeDelta,
		primaryEvents,
		newToken,
		false,
		NewRemoteObservationFindingsBatch(),
	)
	batch.FullRefreshDueSoon = deltaWindowDeletesSyncedFolder(bl, primaryEvents)
	batch.Source = RemoteObservationBatchPrimaryWatch
	batch.ApplyAck = make(chan error, 1)

	return batch
}

func (e *Engine) preferredMountRootObservationMode() RemoteObservationMode {
	if e.mountRootDeltaSupported() {
		return RemoteObservationModeDelta
	}

	return RemoteObservationModeEnumerate
}

func (flow *engineFlow) executePrimaryRootObservation(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (RemoteObservationBatch, error) {
	if flow.engine.hasRemoteMountRoot() {
		return flow.executeMountRootObservation(ctx, bl, fullReconcile)
	}

	return flow.executeDriveRootObservation(ctx, bl, fullReconcile)
}

// rootReadFailureBatch is the observation batch of a root OneDrive refused
// to read: no events and no new cursor, only the root's read issue, so the
// whole tree is unavailable rather than absent.
func (flow *engineFlow) rootReadFailureBatch(mode RemoteObservationMode, issueType IssueType) RemoteObservationBatch {
	return buildRemoteObservationBatch(
		flow.engine,
		mode,
		nil,
		"",
		false,
		rootReadFailureObservationFindingsBatch(flow.engine.driveID, issueType),
	)
}

func (flow *engineFlow) executeDriveRootObservation(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (RemoteObservationBatch, error) {
	if fullReconcile {
		return flow.executeDriveRootFullObservation(ctx, bl, ResyncKindApplyDifferences)
	}

	events, token, topology, err := flow.observeRemoteWithShortcutTopology(ctx, bl)
	if errors.Is(err, ErrDeltaExpired) {
		// Info: self-healing. Recovery is a full refresh with orphan
		// detection, not a bare re-enumeration: a delete that happened while
		// the token aged out is in no delta window (R-2.15.3).
		flow.engine.logger.Info("OneDrive change history expired; rescanning the whole drive (this may take a while)")
		return flow.executeDriveRootFullObservation(ctx, bl, ResyncKindOf(err))
	}
	if issueType, ok := rootReadFailureIssueType(err); ok {
		return flow.rootReadFailureBatch(RemoteObservationModeDelta, issueType), nil
	}

	batch := buildRemoteObservationBatch(
		flow.engine,
		RemoteObservationModeDelta,
		events,
		token,
		false,
		NewRemoteObservationFindingsBatch(),
	)
	batch.FullRefreshDueSoon = deltaWindowDeletesSyncedFolder(bl, events)
	batch.ShortcutTopology = topology
	return batch, err
}

// deltaWindowDeletesSyncedFolder reports whether an incremental delta window
// deleted a folder the baseline holds. Delta reports a deleted folder alone,
// never its descendants (R-2.17.2), and that silence also covers an item moved
// into the folder in the same window: the move is part of a subtree delta
// never repeats, so the item's row stays at its old path as a ghost and its
// synced local copy is never deleted. Only a full enumeration can see that
// such an item is gone, so the folder's tombstone makes the full refresh due
// within FullRefreshAfterFolderDeleteDelay; its orphans are confirmed by ID
// before anything is deleted (confirmEnumerationOrphans).
//
// A folder this client deleted itself is no longer in the baseline when its
// tombstone echoes back, so propagating a local folder delete does not cost a
// full enumeration.
func deltaWindowDeletesSyncedFolder(bl *Baseline, events []RemoteItem) bool {
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
	kind ResyncKind,
) (RemoteObservationBatch, error) {
	full, err := flow.observeRemoteFullWithShortcutTopology(ctx, bl, kind)
	// A later page whose position OneDrive refused twice is an unusable
	// position (ErrDeltaExpired) that unwraps to the refusal, which may be
	// "not found": it says nothing about the root, so it is returned like any
	// other failure.
	if issueType, ok := rootReadFailureIssueType(err); ok && !errors.Is(err, ErrDeltaExpired) {
		return flow.rootReadFailureBatch(RemoteObservationModeDelta, issueType), nil
	}

	batch := buildRemoteObservationBatch(
		flow.engine,
		RemoteObservationModeDelta,
		full.events,
		full.token,
		true,
		NewRemoteObservationFindingsBatch(),
	)
	batch.ShortcutTopology = full.topology
	batch.OrphansUndecided = full.orphansUndecided
	return batch, err
}

func (flow *engineFlow) executeMountRootObservation(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (RemoteObservationBatch, error) {
	observed, err := flow.observeMountRootRemote(ctx, bl, fullReconcile)
	if issueType, ok := rootReadFailureIssueType(err); ok {
		return flow.rootReadFailureBatch(observed.mode, issueType), nil
	}

	// A complete folder delta (requested, or after an expired token) is a
	// full refresh like the drive root's: it restarts the cadence, and an
	// orphan it could not confirm, which the saved position never reports
	// again, brings the next one forward on the undecided-refresh backoff.
	completeDelta := observed.mode == RemoteObservationModeDelta && observed.complete
	batch := buildRemoteObservationBatch(
		flow.engine,
		observed.mode,
		observed.events,
		observed.token,
		fullReconcile || completeDelta,
		NewRemoteObservationFindingsBatch(),
	)
	batch.OrphansUndecided = completeDelta && observed.orphansUndecided

	return batch, err
}
