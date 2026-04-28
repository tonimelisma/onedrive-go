package sync

import (
	"context"
	"fmt"
)

func (e *Engine) SetShortcutChildWorkSink(sink ShortcutChildWorkSink) {
	if e == nil {
		return
	}
	e.shortcutChildWorkSink = sink
}

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

func (e *Engine) ShortcutChildAckHandle() ShortcutChildAckHandle {
	if e == nil {
		return ShortcutChildAckHandle{}
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
