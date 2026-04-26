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

// PublishInitialChildTopology runs the normal parent startup/current-plan path far
// enough to publish shortcut child topology from fresh local and remote truth.
// It intentionally uses the same observation and planning pipeline as a real
// pass; multisync consumes only the published topology before admitting child
// engines.
func (e *Engine) PublishInitialChildTopology(
	ctx context.Context,
	mode SyncMode,
	opts RunOptions,
) (ShortcutChildTopologySnapshot, error) {
	if ctx == nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: initial child topology publication context is required")
	}
	if e == nil || e.shortcutTopologyNamespaceID == "" {
		return ShortcutChildTopologySnapshot{}, nil
	}

	runner := newOneShotRunner(e)
	bl, err := e.runRunOnceStartup(ctx, runner)
	if err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: initial child topology publication startup: %w", err)
	}

	fullRefresh, err := e.shouldRunFullRemoteRefresh(ctx, opts.FullReconcile)
	if err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: initial child topology publication refresh cadence: %w", err)
	}
	opts.FullReconcile = fullRefresh

	runtime, err := runner.runLiveCurrentPlan(ctx, bl, mode, opts)
	if err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: initial child topology publication current plan: %w", err)
	}

	if runtime == nil {
		return ShortcutChildTopologySnapshot{
			NamespaceID: e.shortcutTopologyNamespaceID,
		}, nil
	}
	return runtime.ChildPublication, nil
}

func (e *Engine) AcknowledgeChildFinalDrain(
	ctx context.Context,
	ack ShortcutChildDrainAck,
) (ShortcutChildTopologySnapshot, error) {
	if ctx == nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: shortcut child drain ack context is required")
	}
	if e == nil || e.baseline == nil || e.shortcutTopologyNamespaceID == "" {
		return ShortcutChildTopologySnapshot{}, nil
	}
	if _, err := e.baseline.markShortcutChildFinalDrainReleasePending(ctx, ack); err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: mark shortcut child final drain release pending: %w", err)
	}
	if err := e.releaseShortcutRootProjectionAfterDrain(ctx, ack); err != nil {
		return ShortcutChildTopologySnapshot{}, err
	}
	if _, err := e.reconcileShortcutRootLocalState(ctx); err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: reconcile shortcut roots after child final drain: %w", err)
	}
	snapshot, err := e.baseline.ShortcutChildTopology(ctx, e.shortcutTopologyNamespaceID)
	if err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: read shortcut child topology after final drain: %w", err)
	}
	if e.shortcutChildTopologySink != nil {
		roots, rootErr := e.baseline.ListShortcutRoots(ctx)
		if rootErr != nil {
			return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: read parent shortcut roots after final drain: %w", rootErr)
		}
		if err := e.shortcutChildTopologySink(ctx, shortcutChildTopologyFromRoots(
			e.shortcutTopologyNamespaceID,
			roots,
		)); err != nil {
			return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: publish shortcut child topology after final drain: %w", err)
		}
	}
	return snapshot, nil
}

func (e *Engine) AcknowledgeChildArtifactsPurged(
	ctx context.Context,
	ack ShortcutChildArtifactCleanupAck,
) (ShortcutChildTopologySnapshot, error) {
	if ctx == nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: shortcut child artifact cleanup ack context is required")
	}
	if e == nil || e.baseline == nil || e.shortcutTopologyNamespaceID == "" {
		return ShortcutChildTopologySnapshot{}, nil
	}
	if _, err := e.baseline.acknowledgeShortcutChildArtifactsPurged(ctx, ack); err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: acknowledge shortcut child artifact cleanup: %w", err)
	}
	snapshot, err := e.baseline.ShortcutChildTopology(ctx, e.shortcutTopologyNamespaceID)
	if err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: read shortcut child topology after artifact cleanup: %w", err)
	}
	if e.shortcutChildTopologySink != nil {
		if err := e.shortcutChildTopologySink(ctx, snapshot); err != nil {
			return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: publish shortcut child topology after artifact cleanup: %w", err)
		}
	}
	return snapshot, nil
}
