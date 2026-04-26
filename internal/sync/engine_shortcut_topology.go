package sync

import (
	"context"
	"fmt"
)

// RefreshShortcutTopology asks the parent-drive observer to publish shortcut
// topology facts without committing content observations or advancing the
// remote cursor. Multisync uses this during parent bootstrap so child mounts
// are compiled from parent-observed Graph state before child engines run.
func (e *Engine) RefreshShortcutTopology(ctx context.Context) error {
	_, err := e.PrepareShortcutChildren(ctx)
	return err
}

func (e *Engine) SetShortcutTopologyHandler(handler ShortcutTopologyHandler) {
	if e == nil {
		return
	}
	e.shortcutTopologyHandler = handler
}

func (e *Engine) PrepareShortcutChildren(ctx context.Context) (ShortcutChildTopologySnapshot, error) {
	if ctx == nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: shortcut topology refresh context is required")
	}
	if e == nil || e.hasRemoteMountRoot() {
		return ShortcutChildTopologySnapshot{}, nil
	}

	flow := newEngineFlow(e)
	bl, err := flow.runStartupStage(ctx, nil)
	if err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: shortcut topology startup: %w", err)
	}

	fullRefresh, err := e.shouldRunFullRemoteRefresh(ctx, false)
	if err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: shortcut topology refresh cadence: %w", err)
	}

	var topology ShortcutTopologyBatch
	if fullRefresh {
		_, _, topology, err = flow.observeRemoteFullWithShortcutTopology(ctx, bl)
	} else {
		_, _, topology, err = flow.observeRemoteWithShortcutTopology(ctx, bl)
	}
	if err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: shortcut topology remote observation: %w", err)
	}

	applyErr := flow.applyShortcutTopologyBatch(ctx, &remoteObservationBatch{
		shortcutTopology: topology,
	})
	if applyErr != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: shortcut topology apply: %w", applyErr)
	}

	snapshot, err := e.baseline.ShortcutChildTopology(ctx, e.shortcutTopologyNamespaceID)
	if err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: read shortcut child topology: %w", err)
	}
	return snapshot, nil
}

func (e *Engine) AcknowledgeChildFinalDrain(
	ctx context.Context,
	ack ShortcutChildDrainAck,
) (ShortcutChildTopologySnapshot, error) {
	if ctx == nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: shortcut child drain ack context is required")
	}
	if e == nil || e.baseline == nil || e.hasRemoteMountRoot() {
		return ShortcutChildTopologySnapshot{}, nil
	}
	if err := e.releaseShortcutRootProjectionAfterDrain(ctx, ack); err != nil {
		return ShortcutChildTopologySnapshot{}, err
	}
	if _, err := e.baseline.AcknowledgeShortcutChildFinalDrain(ctx, ack); err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: acknowledge shortcut child final drain: %w", err)
	}
	if _, err := e.reconcileShortcutRootLocalState(ctx); err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: reconcile shortcut roots after child final drain: %w", err)
	}
	snapshot, err := e.baseline.ShortcutChildTopology(ctx, e.shortcutTopologyNamespaceID)
	if err != nil {
		return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: read shortcut child topology after final drain: %w", err)
	}
	if e.shortcutTopologyHandler != nil {
		roots, rootErr := e.baseline.ListShortcutRoots(ctx)
		if rootErr != nil {
			return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: read parent shortcut roots after final drain: %w", rootErr)
		}
		if err := e.shortcutTopologyHandler(ctx, shortcutChildTopologyFromRoots(
			e.shortcutTopologyNamespaceID,
			roots,
		)); err != nil {
			return ShortcutChildTopologySnapshot{}, fmt.Errorf("sync: publish shortcut topology after final drain: %w", err)
		}
	}
	return snapshot, nil
}
