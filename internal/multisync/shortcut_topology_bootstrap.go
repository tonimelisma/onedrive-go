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

type bootstrappedParentEngines map[mountID]engineRunner

func (o *Orchestrator) bootstrapShortcutTopology(
	ctx context.Context,
	compiled *compiledMountSet,
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
	existingWatchRunners map[mountID]*watchRunner,
) (*compiledMountSet, bootstrappedParentEngines, error) {
	bootstrapped := make(bootstrappedParentEngines)
	if compiled == nil || o == nil || o.cfg == nil || o.cfg.disableTopologyBootstrap {
		return compiled, bootstrapped, nil
	}

	changed := false
	for i := range compiled.Mounts {
		mount := compiled.Mounts[i]
		if mount == nil || mount.paused || mount.projectionKind != MountProjectionStandalone {
			continue
		}
		if canReuseWatchRunnerForParentBootstrap(existingWatchRunners, mount) {
			continue
		}

		parentChanged, engine, err := o.bootstrapShortcutTopologyForParent(ctx, mount)
		if err != nil {
			o.closeBootstrappedShortcutParentEngines(ctx, bootstrapped)
			return compiled, nil, fmt.Errorf("bootstrap shortcut topology for mount %s: %w", mount.label(), err)
		}
		if engine != nil {
			bootstrapped[mount.mountID] = engine
		}
		changed = changed || parentChanged
	}
	if !changed {
		return compiled, bootstrapped, nil
	}

	refreshed, err := o.buildRuntimeMountSet(ctx, standaloneMounts, initialStartup)
	if err != nil {
		o.closeBootstrappedShortcutParentEngines(ctx, bootstrapped)
		return compiled, nil, fmt.Errorf("rebuilding mount specs after shortcut topology bootstrap: %w", err)
	}
	o.closeBootstrappedShortcutParentEngines(ctx, bootstrapped)
	return refreshed, make(bootstrappedParentEngines), nil
}

func canReuseWatchRunnerForParentBootstrap(
	existingWatchRunners map[mountID]*watchRunner,
	parent *mountSpec,
) bool {
	if parent == nil || len(existingWatchRunners) == 0 {
		return false
	}
	runner := existingWatchRunners[parent.mountID]
	if runner == nil || runner.mount == nil {
		return false
	}
	return mountSpecCoreEquivalent(runner.mount, parent)
}

func (o *Orchestrator) bootstrapShortcutTopologyForParent(
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
	bootstrapParent := *parent
	mountCollector := o.registerMountPerfCollector(parent.mountID.String())

	engine, err := o.engineFactory(ctx, engineFactoryRequest{
		Session:       session,
		Mount:         &bootstrapParent,
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
		o.closeBootstrappedShortcutParentEngine(ctx, parent.mountID, engine)
		return false, nil, fmt.Errorf("preparing shortcut children: %w", err)
	}
	changed = o.storeParentShortcutTopology(parent.mountID, snapshot)

	return changed, engine, nil
}

func (o *Orchestrator) closeBootstrappedShortcutParentEngines(
	ctx context.Context,
	bootstrapped bootstrappedParentEngines,
) {
	for id, engine := range bootstrapped {
		o.closeBootstrappedShortcutParentEngine(ctx, id, engine)
		delete(bootstrapped, id)
	}
}

func (o *Orchestrator) closeBootstrappedShortcutParentEngine(
	ctx context.Context,
	id mountID,
	engine engineRunner,
) {
	if engine == nil {
		return
	}
	defer o.removeMountPerfCollector(id.String())
	if closeErr := engine.Close(ctx); closeErr != nil && o.logger != nil {
		o.logger.Warn("engine close error after shortcut topology bootstrap",
			slog.String("mount_id", id.String()),
			slog.String("error", closeErr.Error()),
		)
	}
}
