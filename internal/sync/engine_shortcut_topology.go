package sync

import (
	"context"
	"fmt"
)

// RefreshShortcutTopology asks the parent-drive observer to publish shortcut
// topology facts without committing content observations or advancing the
// remote cursor. Multisync uses this as a startup/reload preflight so child
// mounts are compiled from parent-observed Graph state before child engines run.
func (e *Engine) RefreshShortcutTopology(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("sync: shortcut topology refresh context is required")
	}
	if e == nil || e.shortcutTopologyHandler == nil || e.hasRemoteMountRoot() {
		return nil
	}

	flow := newEngineFlow(e)
	bl, err := flow.runStartupStage(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: shortcut topology startup: %w", err)
	}

	fullRefresh, err := e.shouldRunFullRemoteRefresh(ctx, false)
	if err != nil {
		return fmt.Errorf("sync: shortcut topology refresh cadence: %w", err)
	}

	var topology ShortcutTopologyBatch
	if fullRefresh {
		_, _, topology, err = flow.observeRemoteFullWithShortcutTopology(ctx, bl)
	} else {
		_, _, topology, err = flow.observeRemoteWithShortcutTopology(ctx, bl)
	}
	if err != nil {
		return fmt.Errorf("sync: shortcut topology remote observation: %w", err)
	}

	if err := flow.applyShortcutTopologyBatch(ctx, &remoteObservationBatch{
		shortcutTopology: topology,
	}); err != nil {
		return fmt.Errorf("sync: shortcut topology apply: %w", err)
	}

	return nil
}
