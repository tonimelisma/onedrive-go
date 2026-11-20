package sync

import (
	"context"
	"errors"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func buildRemoteObservationBatch(
	engine *Engine,
	mode syncdomain.RemoteObservationMode,
	events []syncdomain.RemoteItem,
	token string,
	markFullRemoteRefresh bool,
	findings syncdomain.ObservationFindingsBatch,
) syncobserve.RemoteObservationBatch {
	projected := syncobserve.ProjectRemoteObservations(engine.logger, events)

	return syncobserve.RemoteObservationBatch{
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
	mode syncdomain.RemoteObservationMode,
) string {
	needsEnumerateClamp := mode == syncdomain.RemoteObservationModeEnumerate
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
	bl syncdomain.BaselineView,
	primaryEvents []syncdomain.RemoteItem,
	newToken string,
) syncobserve.RemoteObservationBatch {
	batch := buildRemoteObservationBatch(
		engine,
		syncdomain.RemoteObservationModeDelta,
		primaryEvents,
		newToken,
		false,
		syncdomain.NewRemoteObservationFindingsBatch(),
	)
	batch.FullRefreshDueSoon = deltaWindowDeletesSyncedFolder(bl, primaryEvents)
	batch.Source = syncobserve.RemoteObservationBatchPrimaryWatch
	batch.ApplyAck = make(chan error, 1)

	return batch
}

func (e *Engine) preferredMountRootObservationMode() syncdomain.RemoteObservationMode {
	if e.mountRootDeltaSupported() {
		return syncdomain.RemoteObservationModeDelta
	}

	return syncdomain.RemoteObservationModeEnumerate
}

func (flow *engineFlow) executePrimaryRootObservation(
	ctx context.Context,
	bl syncdomain.BaselineView,
	fullReconcile bool,
) (syncobserve.RemoteObservationBatch, error) {
	if flow.engine.hasRemoteMountRoot() {
		return flow.executeMountRootObservation(ctx, bl, fullReconcile)
	}

	return flow.executeDriveRootObservation(ctx, bl, fullReconcile)
}

// rootReadFailureBatch is the observation batch of a root OneDrive refused
// to read: no events and no new cursor, only the root's read issue, so the
// whole tree is unavailable rather than absent.
func (flow *engineFlow) rootReadFailureBatch(
	mode syncdomain.RemoteObservationMode, issueType syncdomain.IssueType,
) syncobserve.RemoteObservationBatch {
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
	bl syncdomain.BaselineView,
	fullReconcile bool,
) (syncobserve.RemoteObservationBatch, error) {
	if fullReconcile {
		return flow.executeDriveRootFullObservation(ctx, bl, syncdomain.ResyncKindApplyDifferences)
	}

	events, token, topology, err := flow.observeRemoteWithShortcutTopology(ctx, bl)
	if errors.Is(err, syncdomain.ErrDeltaExpired) {
		// Info: self-healing. Recovery is a full refresh with orphan
		// detection, not a bare re-enumeration: a delete that happened while
		// the token aged out is in no delta window (R-2.15.3).
		flow.engine.logger.Info("OneDrive change history expired; rescanning the whole drive (this may take a while)")
		return flow.executeDriveRootFullObservation(ctx, bl, syncdomain.ResyncKindOf(err))
	}
	if issueType, ok := rootReadFailureIssueType(err); ok {
		return flow.rootReadFailureBatch(syncdomain.RemoteObservationModeDelta, issueType), nil
	}

	batch := buildRemoteObservationBatch(
		flow.engine,
		syncdomain.RemoteObservationModeDelta,
		events,
		token,
		false,
		syncdomain.NewRemoteObservationFindingsBatch(),
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
func deltaWindowDeletesSyncedFolder(bl syncdomain.BaselineView, events []syncdomain.RemoteItem) bool {
	if bl == nil {
		return false
	}
	for i := range events {
		if !events[i].IsDeleted || events[i].ItemID == "" {
			continue
		}
		if entry, ok := bl.GetByID(events[i].ItemID); ok && entry.ItemType == syncdomain.ItemTypeFolder {
			return true
		}
	}

	return false
}

// executeDriveRootFullObservation runs the full refresh with orphan detection
// and marks it as one, which also restarts the full-refresh cadence.
func (flow *engineFlow) executeDriveRootFullObservation(
	ctx context.Context,
	bl syncdomain.BaselineView,
	kind syncdomain.ResyncKind,
) (syncobserve.RemoteObservationBatch, error) {
	full, err := flow.observeRemoteFullWithShortcutTopology(ctx, bl, kind)
	// A later page whose position OneDrive refused twice is an unusable
	// position (ErrDeltaExpired) that unwraps to the refusal, which may be
	// "not found": it says nothing about the root, so it is returned like any
	// other failure.
	if issueType, ok := rootReadFailureIssueType(err); ok && !errors.Is(err, syncdomain.ErrDeltaExpired) {
		return flow.rootReadFailureBatch(syncdomain.RemoteObservationModeDelta, issueType), nil
	}

	batch := buildRemoteObservationBatch(
		flow.engine,
		syncdomain.RemoteObservationModeDelta,
		full.events,
		full.token,
		true,
		syncdomain.NewRemoteObservationFindingsBatch(),
	)
	batch.ShortcutTopology = full.topology
	batch.OrphansUndecided = full.orphansUndecided
	return batch, err
}

func (flow *engineFlow) executeMountRootObservation(
	ctx context.Context,
	bl syncdomain.BaselineView,
	fullReconcile bool,
) (syncobserve.RemoteObservationBatch, error) {
	observed, err := flow.observeMountRootRemote(ctx, bl, fullReconcile)
	if issueType, ok := rootReadFailureIssueType(err); ok {
		return flow.rootReadFailureBatch(observed.mode, issueType), nil
	}

	// A complete folder delta (requested, or after an expired token) is a
	// full refresh like the drive root's: it restarts the cadence, and an
	// orphan it could not confirm, which the saved position never reports
	// again, brings the next one forward on the undecided-refresh backoff.
	completeDelta := observed.mode == syncdomain.RemoteObservationModeDelta && observed.complete
	batch := buildRemoteObservationBatch(
		flow.engine,
		observed.mode,
		observed.events,
		observed.token,
		fullReconcile || completeDelta,
		syncdomain.NewRemoteObservationFindingsBatch(),
	)
	batch.OrphansUndecided = completeDelta && observed.orphansUndecided

	return batch, err
}
