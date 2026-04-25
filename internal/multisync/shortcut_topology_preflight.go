package multisync

import (
	"context"
	"fmt"
	"log/slog"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type shortcutTopologyRefresher interface {
	RefreshShortcutTopology(context.Context) error
}

func (o *Orchestrator) preflightShortcutTopology(
	ctx context.Context,
	compiled *compiledMountSet,
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
	phase string,
) (*compiledMountSet, error) {
	if compiled == nil || o == nil || o.cfg == nil || o.cfg.disableTopologyPreflight {
		return compiled, nil
	}

	changed := false
	for i := range compiled.Mounts {
		mount := compiled.Mounts[i]
		if mount == nil || mount.paused || mount.projectionKind != MountProjectionStandalone {
			continue
		}

		parentChanged, err := o.preflightShortcutTopologyForParent(ctx, mount)
		if err != nil {
			return compiled, fmt.Errorf("preflight shortcut topology for mount %s: %w", mount.label(), err)
		}
		changed = changed || parentChanged
	}
	if !changed {
		return compiled, nil
	}

	refreshed, err := o.buildRuntimeMountSet(ctx, standaloneMounts, initialStartup)
	if err != nil {
		return compiled, fmt.Errorf("rebuilding mount specs after shortcut topology preflight: %w", err)
	}

	return o.finalizeRuntimeMountSetLifecycle(
		ctx,
		refreshed,
		standaloneMounts,
		initialStartup,
		phase,
		true,
	)
}

func (o *Orchestrator) preflightShortcutTopologyForParent(
	ctx context.Context,
	parent *mountSpec,
) (bool, error) {
	if o == nil || o.cfg == nil || parent == nil {
		return false, nil
	}

	session, err := o.cfg.Runtime.SyncSession(ctx, parent.syncSessionConfig())
	if err != nil {
		return false, fmt.Errorf("session error for mount %s: %w", parent.label(), err)
	}

	changed := false
	preflightParent := *parent
	preflightParent.shortcutTopologyHandler = func(ctx context.Context, batch syncengine.ShortcutTopologyBatch) error {
		batchChanged, applyErr := o.applyShortcutTopologyBatch(ctx, &preflightParent, batch)
		changed = changed || batchChanged
		return applyErr
	}

	engine, err := o.engineFactory(ctx, engineFactoryRequest{
		Session:     session,
		Mount:       &preflightParent,
		Logger:      o.logger,
		VerifyDrive: true,
	})
	if err != nil {
		return false, fmt.Errorf("engine creation failed for mount %s: %w", parent.label(), err)
	}
	defer func() {
		if closeErr := engine.Close(ctx); closeErr != nil && o.logger != nil {
			o.logger.Warn("engine close error after shortcut topology preflight",
				slog.String("mount_id", parent.mountID.String()),
				slog.String("error", closeErr.Error()),
			)
		}
	}()

	refresher, ok := engine.(shortcutTopologyRefresher)
	if !ok {
		return false, nil
	}
	if err := refresher.RefreshShortcutTopology(ctx); err != nil {
		return false, fmt.Errorf("refreshing shortcut topology: %w", err)
	}

	return changed, nil
}
