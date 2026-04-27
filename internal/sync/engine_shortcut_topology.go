package sync

import (
	"context"
	"fmt"
)

func (e *Engine) SetShortcutChildRunnerSink(sink ShortcutChildRunnerSink) {
	if e == nil {
		return
	}
	e.shortcutChildRunnerSink = sink
}

func (e *Engine) publishShortcutChildRunnerPublication(
	ctx context.Context,
	publication ShortcutChildRunnerPublication,
) error {
	if e == nil || e.shortcutNamespaceID == "" || e.shortcutChildRunnerSink == nil {
		return nil
	}
	if publication.NamespaceID == "" {
		publication.NamespaceID = e.shortcutNamespaceID
	}
	if err := e.shortcutChildRunnerSink(ctx, publication); err != nil {
		return fmt.Errorf("sync: publish shortcut child runner publication: %w", err)
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
) (ShortcutChildRunnerPublication, error) {
	if ctx == nil {
		return ShortcutChildRunnerPublication{}, fmt.Errorf("sync: shortcut child drain ack context is required")
	}
	if e == nil || e.baseline == nil || e.shortcutNamespaceID == "" {
		return ShortcutChildRunnerPublication{}, nil
	}
	if _, err := e.baseline.markShortcutChildFinalDrainReleasePending(ctx, ack); err != nil {
		return ShortcutChildRunnerPublication{}, fmt.Errorf("sync: mark shortcut child final drain release pending: %w", err)
	}
	if err := e.releaseShortcutRootProjectionAfterDrain(ctx, ack); err != nil {
		return ShortcutChildRunnerPublication{}, err
	}
	if _, err := e.reconcileShortcutRootLocalState(ctx); err != nil {
		return ShortcutChildRunnerPublication{}, fmt.Errorf("sync: reconcile shortcut roots after child final drain: %w", err)
	}
	snapshot, err := e.baseline.ShortcutChildRunner(ctx, e.shortcutNamespaceID, e.syncRoot)
	if err != nil {
		return ShortcutChildRunnerPublication{}, fmt.Errorf("sync: read shortcut child runner publication after final drain: %w", err)
	}
	if e.shortcutChildRunnerSink != nil {
		roots, rootErr := e.baseline.listShortcutRoots(ctx)
		if rootErr != nil {
			return ShortcutChildRunnerPublication{}, fmt.Errorf("sync: read parent shortcut roots after final drain: %w", rootErr)
		}
		if err := e.shortcutChildRunnerSink(ctx, shortcutChildRunnerPublicationFromRootsWithParentRoot(
			e.shortcutNamespaceID,
			e.syncRoot,
			roots,
		)); err != nil {
			return ShortcutChildRunnerPublication{}, fmt.Errorf("sync: publish shortcut child runner publication after final drain: %w", err)
		}
	}
	return snapshot, nil
}

func (e *Engine) acknowledgeChildArtifactsPurged(
	ctx context.Context,
	ack ShortcutChildArtifactCleanupAck,
) (ShortcutChildRunnerPublication, error) {
	if ctx == nil {
		return ShortcutChildRunnerPublication{}, fmt.Errorf("sync: shortcut child artifact cleanup ack context is required")
	}
	if e == nil || e.baseline == nil || e.shortcutNamespaceID == "" {
		return ShortcutChildRunnerPublication{}, nil
	}
	if _, err := e.baseline.acknowledgeShortcutChildArtifactsPurged(ctx, ack); err != nil {
		return ShortcutChildRunnerPublication{}, fmt.Errorf("sync: acknowledge shortcut child artifact cleanup: %w", err)
	}
	snapshot, err := e.baseline.ShortcutChildRunner(ctx, e.shortcutNamespaceID, e.syncRoot)
	if err != nil {
		return ShortcutChildRunnerPublication{}, fmt.Errorf("sync: read shortcut child runner publication after artifact cleanup: %w", err)
	}
	if e.shortcutChildRunnerSink != nil {
		if err := e.shortcutChildRunnerSink(ctx, snapshot); err != nil {
			return ShortcutChildRunnerPublication{}, fmt.Errorf("sync: publish shortcut child runner publication after artifact cleanup: %w", err)
		}
	}
	return snapshot, nil
}
