package multisync

import (
	"context"
	"fmt"
	"log/slog"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type shortcutTopologyRefresher interface {
	PrepareShortcutChildren(context.Context) (syncengine.ShortcutChildTopologySnapshot, error)
}

type preparedShortcutParentEngines map[mountID]engineRunner

func (o *Orchestrator) preflightShortcutTopology(
	ctx context.Context,
	compiled *compiledMountSet,
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
) (*compiledMountSet, preparedShortcutParentEngines, error) {
	prepared := make(preparedShortcutParentEngines)
	if compiled == nil || o == nil || o.cfg == nil || o.cfg.disableTopologyPreflight {
		return compiled, prepared, nil
	}

	changed := false
	for i := range compiled.Mounts {
		mount := compiled.Mounts[i]
		if mount == nil || mount.paused || mount.projectionKind != MountProjectionStandalone {
			continue
		}

		parentChanged, engine, err := o.preflightShortcutTopologyForParent(ctx, mount)
		if err != nil {
			o.closePreparedShortcutParentEngines(ctx, prepared)
			return compiled, nil, fmt.Errorf("preflight shortcut topology for mount %s: %w", mount.label(), err)
		}
		if engine != nil {
			prepared[mount.mountID] = engine
		}
		changed = changed || parentChanged
	}
	if !changed {
		return compiled, prepared, nil
	}

	refreshed, err := o.buildRuntimeMountSet(ctx, standaloneMounts, initialStartup)
	if err != nil {
		o.closePreparedShortcutParentEngines(ctx, prepared)
		return compiled, nil, fmt.Errorf("rebuilding mount specs after shortcut topology preflight: %w", err)
	}
	return refreshed, prepared, nil
}

func (o *Orchestrator) preflightShortcutTopologyForParent(
	ctx context.Context,
	parent *mountSpec,
) (bool, engineRunner, error) {
	if o == nil || o.cfg == nil || parent == nil {
		return false, nil, nil
	}

	session, err := o.cfg.Runtime.SyncSession(ctx, parent.syncSessionConfig())
	if err != nil {
		return false, nil, fmt.Errorf("session error for mount %s: %w", parent.label(), err)
	}

	changed := false
	preflightParent := *parent
	mountCollector := o.registerMountPerfCollector(parent.mountID.String())

	engine, err := o.engineFactory(ctx, engineFactoryRequest{
		Session:       session,
		Mount:         &preflightParent,
		Logger:        o.logger,
		VerifyDrive:   true,
		PerfCollector: mountCollector,
	})
	if err != nil {
		o.removeMountPerfCollector(parent.mountID.String())
		return false, nil, fmt.Errorf("engine creation failed for mount %s: %w", parent.label(), err)
	}

	refresher, ok := engine.(shortcutTopologyRefresher)
	if !ok {
		return false, engine, nil
	}
	snapshot, err := refresher.PrepareShortcutChildren(ctx)
	if err != nil {
		o.closePreparedShortcutParentEngine(ctx, parent.mountID, engine)
		return false, nil, fmt.Errorf("preparing shortcut children: %w", err)
	}
	changed = o.storeShortcutChildTopology(parent.mountID, snapshot)

	return changed, engine, nil
}

func (o *Orchestrator) closePreparedShortcutParentEngines(
	ctx context.Context,
	prepared preparedShortcutParentEngines,
) {
	for id, engine := range prepared {
		o.closePreparedShortcutParentEngine(ctx, id, engine)
		delete(prepared, id)
	}
}

func (o *Orchestrator) closePreparedShortcutParentEngine(
	ctx context.Context,
	id mountID,
	engine engineRunner,
) {
	if engine == nil {
		return
	}
	defer o.removeMountPerfCollector(id.String())
	if closeErr := engine.Close(ctx); closeErr != nil && o.logger != nil {
		o.logger.Warn("engine close error after shortcut topology preflight",
			slog.String("mount_id", id.String()),
			slog.String("error", closeErr.Error()),
		)
	}
}
