package sync

import (
	"context"
	"fmt"
)

func (e *Engine) SetShortcutChildProcessSink(sink ShortcutChildProcessSink) {
	if e == nil {
		return
	}
	e.shortcutChildProcessSink = sink
}

func (e *Engine) publishShortcutChildProcessSnapshot(
	ctx context.Context,
	publication ShortcutChildProcessSnapshot,
) error {
	if e == nil || e.shortcutNamespaceID == "" || e.shortcutChildProcessSink == nil {
		return nil
	}
	if publication.NamespaceID == "" {
		publication.NamespaceID = e.shortcutNamespaceID
	}
	if err := e.shortcutChildProcessSink(ctx, publication); err != nil {
		return fmt.Errorf("sync: publish shortcut child process snapshot: %w", err)
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
) (ShortcutChildProcessSnapshot, error) {
	if ctx == nil {
		return ShortcutChildProcessSnapshot{}, fmt.Errorf("sync: shortcut child drain ack context is required")
	}
	if e == nil || e.baseline == nil || e.shortcutNamespaceID == "" {
		return ShortcutChildProcessSnapshot{}, nil
	}
	if _, err := e.baseline.markShortcutChildFinalDrainReleasePending(ctx, ack); err != nil {
		return ShortcutChildProcessSnapshot{}, fmt.Errorf("sync: mark shortcut child final drain release pending: %w", err)
	}
	if err := e.releaseShortcutRootProjectionAfterDrain(ctx, ack); err != nil {
		return ShortcutChildProcessSnapshot{}, err
	}
	if _, err := e.reconcileShortcutRootLocalState(ctx); err != nil {
		return ShortcutChildProcessSnapshot{}, fmt.Errorf("sync: reconcile shortcut roots after child final drain: %w", err)
	}
	snapshot, err := e.baseline.ShortcutChildProcessSnapshot(ctx, e.shortcutNamespaceID, e.syncRoot)
	if err != nil {
		return ShortcutChildProcessSnapshot{}, fmt.Errorf("sync: read shortcut child process snapshot after final drain: %w", err)
	}
	if e.shortcutChildProcessSink != nil {
		roots, rootErr := e.baseline.listShortcutRoots(ctx)
		if rootErr != nil {
			return ShortcutChildProcessSnapshot{}, fmt.Errorf("sync: read parent shortcut roots after final drain: %w", rootErr)
		}
		if err := e.shortcutChildProcessSink(ctx, shortcutChildProcessSnapshotFromRootsWithParentRoot(
			e.shortcutNamespaceID,
			e.syncRoot,
			roots,
		)); err != nil {
			return ShortcutChildProcessSnapshot{}, fmt.Errorf("sync: publish shortcut child process snapshot after final drain: %w", err)
		}
	}
	return snapshot, nil
}

func (e *Engine) acknowledgeChildArtifactsPurged(
	ctx context.Context,
	ack ShortcutChildArtifactCleanupAck,
) (ShortcutChildProcessSnapshot, error) {
	if ctx == nil {
		return ShortcutChildProcessSnapshot{}, fmt.Errorf("sync: shortcut child artifact cleanup ack context is required")
	}
	if e == nil || e.baseline == nil || e.shortcutNamespaceID == "" {
		return ShortcutChildProcessSnapshot{}, nil
	}
	if _, err := e.baseline.acknowledgeShortcutChildArtifactsPurged(ctx, ack); err != nil {
		return ShortcutChildProcessSnapshot{}, fmt.Errorf("sync: acknowledge shortcut child artifact cleanup: %w", err)
	}
	snapshot, err := e.baseline.ShortcutChildProcessSnapshot(ctx, e.shortcutNamespaceID, e.syncRoot)
	if err != nil {
		return ShortcutChildProcessSnapshot{}, fmt.Errorf("sync: read shortcut child process snapshot after artifact cleanup: %w", err)
	}
	if e.shortcutChildProcessSink != nil {
		if err := e.shortcutChildProcessSink(ctx, snapshot); err != nil {
			return ShortcutChildProcessSnapshot{}, fmt.Errorf("sync: publish shortcut child process snapshot after artifact cleanup: %w", err)
		}
	}
	return snapshot, nil
}
