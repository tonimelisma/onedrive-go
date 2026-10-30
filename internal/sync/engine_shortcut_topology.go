package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func (e *Engine) publishShortcutChildWorkSnapshot(
	ctx context.Context,
	publication ShortcutChildWorkSnapshot,
) error {
	if e == nil || e.shortcutNamespaceID == "" || e.shortcutChildWorkSink == nil {
		return nil
	}
	if publication.NamespaceID == "" {
		publication.NamespaceID = e.shortcutNamespaceID
	}
	if err := e.shortcutChildWorkSink(ctx, publication); err != nil {
		return fmt.Errorf("sync: publish shortcut child work snapshot: %w", err)
	}
	return nil
}

func (e *Engine) ShortcutChildAckHandle() shortcutChildAckHandle {
	if e == nil {
		return shortcutChildAckHandle{}
	}
	return newShortcutChildAckHandle(
		e.acknowledgeChildFinalDrain,
		e.acknowledgeChildArtifactsPurged,
	)
}

func (e *Engine) acknowledgeChildFinalDrain(
	ctx context.Context,
	ack ShortcutChildDrainAck,
) (ShortcutChildWorkSnapshot, error) {
	if ctx == nil {
		return ShortcutChildWorkSnapshot{}, fmt.Errorf("sync: shortcut child drain ack context is required")
	}
	if e == nil || e.baseline == nil || e.shortcutNamespaceID == "" {
		return ShortcutChildWorkSnapshot{}, nil
	}
	if _, err := e.baseline.markShortcutChildFinalDrainReleasePending(ctx, ack); err != nil {
		return ShortcutChildWorkSnapshot{}, fmt.Errorf("sync: mark shortcut child final drain release pending: %w", err)
	}
	if err := e.releaseShortcutRootProjectionAfterDrain(ctx, ack); err != nil {
		return ShortcutChildWorkSnapshot{}, err
	}
	if _, err := e.reconcileShortcutRootLocalState(ctx); err != nil {
		return ShortcutChildWorkSnapshot{}, fmt.Errorf("sync: reconcile shortcut roots after child final drain: %w", err)
	}
	snapshot, err := e.baseline.ShortcutChildWorkSnapshot(ctx, e.shortcutNamespaceID, e.syncRoot)
	if err != nil {
		return ShortcutChildWorkSnapshot{}, fmt.Errorf("sync: read shortcut child work snapshot after final drain: %w", err)
	}
	if e.shortcutChildWorkSink != nil {
		roots, rootErr := e.baseline.listShortcutRoots(ctx)
		if rootErr != nil {
			return ShortcutChildWorkSnapshot{}, fmt.Errorf("sync: read parent shortcut roots after final drain: %w", rootErr)
		}
		if err := e.shortcutChildWorkSink(ctx, shortcutChildWorkSnapshotFromRootsWithParentRoot(
			e.shortcutNamespaceID,
			e.syncRoot,
			e.contentFilter,
			roots,
		)); err != nil {
			return ShortcutChildWorkSnapshot{}, fmt.Errorf("sync: publish shortcut child work snapshot after final drain: %w", err)
		}
	}
	return snapshot, nil
}

func (e *Engine) acknowledgeChildArtifactsPurged(
	ctx context.Context,
	ack ShortcutChildArtifactCleanupAck,
) (ShortcutChildWorkSnapshot, error) {
	if ctx == nil {
		return ShortcutChildWorkSnapshot{}, fmt.Errorf("sync: shortcut child artifact cleanup ack context is required")
	}
	if e == nil || e.baseline == nil || e.shortcutNamespaceID == "" {
		return ShortcutChildWorkSnapshot{}, nil
	}
	if _, err := e.baseline.acknowledgeShortcutChildArtifactsPurged(ctx, ack); err != nil {
		return ShortcutChildWorkSnapshot{}, fmt.Errorf("sync: acknowledge shortcut child artifact cleanup: %w", err)
	}
	snapshot, err := e.baseline.ShortcutChildWorkSnapshot(ctx, e.shortcutNamespaceID, e.syncRoot)
	if err != nil {
		return ShortcutChildWorkSnapshot{}, fmt.Errorf("sync: read shortcut child work snapshot after artifact cleanup: %w", err)
	}
	if e.shortcutChildWorkSink != nil {
		if err := e.shortcutChildWorkSink(ctx, snapshot); err != nil {
			return ShortcutChildWorkSnapshot{}, fmt.Errorf("sync: publish shortcut child work snapshot after artifact cleanup: %w", err)
		}
	}
	return snapshot, nil
}

// confirmShortcutCompleteOmissions checks, before a complete observation is
// applied, every active shortcut binding the observation did not return.
// The planner turns a complete omission into a final drain, and releasing the
// root removes its whole local projection, so one enumeration that dropped a
// placeholder (a known transient Graph omission) deleted every file under it
// (D-007). Here each omitted binding is looked up by ID:
//   - still a live shortcut to the same target: it is re-added as observed and
//     stays active;
//   - 404, deleted, or now pointing elsewhere: it stays omitted and drains;
//   - the lookup failed: it is marked target-unavailable, which keeps local
//     data until a later observation settles it.
func (e *Engine) confirmShortcutCompleteOmissions(ctx context.Context, topology *shortcutTopologyBatch) error {
	if e == nil || topology == nil || topology.Kind != shortcutTopologyObservationComplete || e.itemsClient == nil {
		return nil
	}
	roots, err := e.baseline.listShortcutRoots(ctx)
	if err != nil {
		return fmt.Errorf("sync: read shortcut roots to confirm omissions: %w", err)
	}
	seen := shortcutRootCompleteBatchSeenBindings(*topology)
	for i := range roots {
		root := &roots[i]
		if _, ok := seen[root.BindingItemID]; ok || shortcutRootCompleteOmissionKeepsState(root.State) {
			continue
		}
		if root.NamespaceID != "" && topology.NamespaceID != "" && root.NamespaceID != topology.NamespaceID {
			continue
		}
		e.confirmShortcutOmission(ctx, topology, root)
	}

	return nil
}

func (e *Engine) confirmShortcutOmission(ctx context.Context, topology *shortcutTopologyBatch, root *ShortcutRootRecord) {
	live, err := e.itemsClient.GetItem(ctx, e.driveID, root.BindingItemID)
	switch {
	case errors.Is(err, graph.ErrNotFound):
		return
	case err != nil:
		e.logger.Debug("could not confirm an omitted shortcut is gone; keeping its files",
			slog.String("path", root.RelativeLocalPath), slog.Any("error", err))
		topology.Unavailable = append(topology.Unavailable, shortcutBindingUnavailable{
			BindingItemID:     root.BindingItemID,
			RelativeLocalPath: root.RelativeLocalPath,
			LocalAlias:        root.LocalAlias,
			RemoteDriveID:     root.RemoteDriveID.String(),
			RemoteItemID:      root.RemoteItemID,
			RemoteIsFolder:    root.RemoteIsFolder,
			Reason:            "the shortcut could not be checked after a listing omitted it",
		})
		return
	case live == nil || live.IsDeleted:
		return
	case live.RemoteItemID != root.RemoteItemID ||
		(live.RemoteDriveID != "" && !driveid.New(live.RemoteDriveID).Equal(root.RemoteDriveID)):
		return
	}

	e.logger.Info("OneDrive's listing omitted a shared-folder shortcut that still exists; keeping it",
		slog.String("path", root.RelativeLocalPath))
	topology.Upserts = append(topology.Upserts, shortcutBindingUpsert{
		BindingItemID:     root.BindingItemID,
		RelativeLocalPath: root.RelativeLocalPath,
		LocalAlias:        root.LocalAlias,
		RemoteDriveID:     root.RemoteDriveID.String(),
		RemoteItemID:      root.RemoteItemID,
		RemoteIsFolder:    root.RemoteIsFolder,
		Complete:          true,
	})
}
