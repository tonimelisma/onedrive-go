package multisync

import (
	"context"
	"fmt"
	"log/slog"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type parentChildTopologyPublisher interface {
	PublishInitialChildTopology(
		context.Context,
		syncengine.SyncMode,
		syncengine.RunOptions,
	) (syncengine.ShortcutChildTopologySnapshot, error)
}

type startupParentEngines map[mountID]engineRunner

func (o *Orchestrator) publishParentStartupChildTopology(
	ctx context.Context,
	compiled *compiledMountSet,
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
	existingWatchRunners map[mountID]*watchRunner,
	mode syncengine.SyncMode,
	opts syncengine.RunOptions,
) (*compiledMountSet, startupParentEngines, error) {
	startup := make(startupParentEngines)
	if compiled == nil || o == nil || o.cfg == nil || o.cfg.disableParentStartupChildTopology {
		return compiled, startup, nil
	}

	changed := false
	for i := range compiled.Mounts {
		mount := compiled.Mounts[i]
		if mount == nil || mount.paused || mount.projectionKind != MountProjectionStandalone {
			continue
		}
		if canReuseWatchRunnerForParentStartupPublication(existingWatchRunners, mount) {
			continue
		}

		parentChanged, engine, err := o.publishStartupChildTopologyForParent(ctx, mount, mode, opts)
		if err != nil {
			o.closeStartupParentEngines(ctx, startup)
			return compiled, nil, fmt.Errorf("publish parent startup child topology for mount %s: %w", mount.label(), err)
		}
		if engine != nil {
			startup[mount.mountID] = engine
		}
		changed = changed || parentChanged
	}
	if !changed {
		return compiled, startup, nil
	}

	refreshed, err := o.buildRuntimeMountSet(ctx, standaloneMounts, initialStartup)
	if err != nil {
		o.closeStartupParentEngines(ctx, startup)
		return compiled, nil, fmt.Errorf("rebuilding mount specs after parent startup child topology publication: %w", err)
	}
	return refreshed, startup, nil
}

func canReuseWatchRunnerForParentStartupPublication(
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

func (o *Orchestrator) publishStartupChildTopologyForParent(
	ctx context.Context,
	parent *mountSpec,
	mode syncengine.SyncMode,
	opts syncengine.RunOptions,
) (bool, engineRunner, error) {
	if o == nil || o.cfg == nil || parent == nil {
		return false, nil, nil
	}

	session, err := o.cfg.Runtime.SyncSession(ctx, parent.syncSessionConfig())
	if err != nil {
		return false, nil, fmt.Errorf("session error for mount %s: %w", parent.label(), err)
	}

	startupParent := *parent
	mountCollector := o.registerMountPerfCollector(parent.mountID.String())
	engine, err := o.engineFactory(ctx, engineFactoryRequest{
		Session:       session,
		Mount:         &startupParent,
		Logger:        o.logger,
		VerifyDrive:   true,
		PerfCollector: mountCollector,
	})
	if err != nil {
		o.removeMountPerfCollector(parent.mountID.String())
		return false, nil, fmt.Errorf("engine creation failed for mount %s: %w", parent.label(), err)
	}

	publisher, ok := engine.(parentChildTopologyPublisher)
	if !ok {
		return false, engine, nil
	}
	snapshot, err := publisher.PublishInitialChildTopology(ctx, mode, opts)
	if err != nil {
		o.closeStartupParentEngine(ctx, parent.mountID, engine)
		return false, nil, fmt.Errorf("publishing parent initial child topology: %w", err)
	}
	changed := o.storeParentShortcutTopology(parent.mountID, snapshot)

	return changed, engine, nil
}

func (o *Orchestrator) closeStartupParentEngines(
	ctx context.Context,
	startup startupParentEngines,
) {
	for id, engine := range startup {
		o.closeStartupParentEngine(ctx, id, engine)
		delete(startup, id)
	}
}

func (o *Orchestrator) closeStartupParentEngine(
	ctx context.Context,
	id mountID,
	engine engineRunner,
) {
	if engine == nil {
		return
	}
	defer o.removeMountPerfCollector(id.String())
	if closeErr := engine.Close(ctx); closeErr != nil && o.logger != nil {
		o.logger.Warn("engine close error after parent startup child topology publication",
			slog.String("mount_id", id.String()),
			slog.String("error", closeErr.Error()),
		)
	}
}
