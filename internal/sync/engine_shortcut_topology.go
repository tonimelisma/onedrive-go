package sync

import (
	"context"
	"fmt"
)

func (e *Engine) SetShortcutChildTopologySink(sink ShortcutChildTopologySink) {
	if e == nil {
		return
	}
	e.shortcutChildTopologySink = sink
}

func (e *Engine) publishShortcutChildTopology(
	ctx context.Context,
	publication ShortcutChildTopologyPublication,
) error {
	if e == nil || e.shortcutTopologyNamespaceID == "" || e.shortcutChildTopologySink == nil {
		return nil
	}
	if publication.NamespaceID == "" {
		publication.NamespaceID = e.shortcutTopologyNamespaceID
	}
	if err := e.shortcutChildTopologySink(ctx, publication); err != nil {
		return fmt.Errorf("sync: publish shortcut child topology: %w", err)
	}
	return nil
}

func (e *Engine) AcknowledgeChildFinalDrain(
	ctx context.Context,
	ack ShortcutChildDrainAck,
) (ShortcutChildTopologyPublication, error) {
	if ctx == nil {
		return ShortcutChildTopologyPublication{}, fmt.Errorf("sync: shortcut child drain ack context is required")
	}
	if e == nil || e.baseline == nil || e.shortcutTopologyNamespaceID == "" {
		return ShortcutChildTopologyPublication{}, nil
	}
	if _, err := e.baseline.markShortcutChildFinalDrainReleasePending(ctx, ack); err != nil {
		return ShortcutChildTopologyPublication{}, fmt.Errorf("sync: mark shortcut child final drain release pending: %w", err)
	}
	if err := e.releaseShortcutRootProjectionAfterDrain(ctx, ack); err != nil {
		return ShortcutChildTopologyPublication{}, err
	}
	if _, err := e.reconcileShortcutRootLocalState(ctx); err != nil {
		return ShortcutChildTopologyPublication{}, fmt.Errorf("sync: reconcile shortcut roots after child final drain: %w", err)
	}
	snapshot, err := e.baseline.ShortcutChildTopology(ctx, e.shortcutTopologyNamespaceID)
	if err != nil {
		return ShortcutChildTopologyPublication{}, fmt.Errorf("sync: read shortcut child topology after final drain: %w", err)
	}
	if e.shortcutChildTopologySink != nil {
		roots, rootErr := e.baseline.ListShortcutRoots(ctx)
		if rootErr != nil {
			return ShortcutChildTopologyPublication{}, fmt.Errorf("sync: read parent shortcut roots after final drain: %w", rootErr)
		}
		if err := e.shortcutChildTopologySink(ctx, shortcutChildTopologyFromRoots(
			e.shortcutTopologyNamespaceID,
			roots,
		)); err != nil {
			return ShortcutChildTopologyPublication{}, fmt.Errorf("sync: publish shortcut child topology after final drain: %w", err)
		}
	}
	return snapshot, nil
}

func (e *Engine) AcknowledgeChildArtifactsPurged(
	ctx context.Context,
	ack ShortcutChildArtifactCleanupAck,
) (ShortcutChildTopologyPublication, error) {
	if ctx == nil {
		return ShortcutChildTopologyPublication{}, fmt.Errorf("sync: shortcut child artifact cleanup ack context is required")
	}
	if e == nil || e.baseline == nil || e.shortcutTopologyNamespaceID == "" {
		return ShortcutChildTopologyPublication{}, nil
	}
	if _, err := e.baseline.acknowledgeShortcutChildArtifactsPurged(ctx, ack); err != nil {
		return ShortcutChildTopologyPublication{}, fmt.Errorf("sync: acknowledge shortcut child artifact cleanup: %w", err)
	}
	snapshot, err := e.baseline.ShortcutChildTopology(ctx, e.shortcutTopologyNamespaceID)
	if err != nil {
		return ShortcutChildTopologyPublication{}, fmt.Errorf("sync: read shortcut child topology after artifact cleanup: %w", err)
	}
	if e.shortcutChildTopologySink != nil {
		if err := e.shortcutChildTopologySink(ctx, snapshot); err != nil {
			return ShortcutChildTopologyPublication{}, fmt.Errorf("sync: publish shortcut child topology after artifact cleanup: %w", err)
		}
	}
	return snapshot, nil
}
